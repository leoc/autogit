package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain isolates git from the user's and the system's configuration.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "autogit-test")
	if err != nil {
		panic(err)
	}
	cfg := filepath.Join(dir, "gitconfig")
	err = os.WriteFile(cfg, []byte(`[user]
	name = Test
	email = test@example.com
[init]
	defaultBranch = main
[advice]
	detachedHead = false
`), 0o644)
	if err != nil {
		panic(err)
	}
	os.Setenv("GIT_CONFIG_GLOBAL", cfg)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// world is a bare remote plus helpers to clone it, as if from several
// machines.
type world struct {
	t      *testing.T
	root   string
	remote string
}

func newWorld(t *testing.T) *world {
	root := t.TempDir()
	w := &world{t: t, root: root, remote: w0bare(t, root, "origin.git")}
	seed := filepath.Join(root, "seed")
	git(t, root, "clone", "-q", w.remote, seed)
	write(t, filepath.Join(seed, "notes.org"), "line 1\n")
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-q", "-m", "initial")
	git(t, seed, "push", "-q", "origin", "main")
	return w
}

func w0bare(t *testing.T, root, name string) string {
	p := filepath.Join(root, name)
	git(t, root, "init", "-q", "--bare", p)
	return p
}

func (w *world) clone(name string) string {
	p := filepath.Join(w.root, name)
	git(w.t, w.root, "clone", "-q", w.remote, p)
	return p
}

// remoteLog returns the subjects on the remote's main branch, newest first.
func remoteLog(t *testing.T, bare string) []string {
	return strings.Split(git(t, bare, "log", "--format=%s", "main"), "\n")
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// newRepo builds a Repo for path with fast test defaults; mod may adjust
// the repository settings.
func newRepo(t *testing.T, path string, mod func(*Settings)) *Repo {
	t.Helper()
	cfg := &Config{
		GitTimeout: 30 * time.Second,
		Defaults: Settings{
			PushRemotes:       []string{"origin"},
			Debounce:          ptr(50 * time.Millisecond),
			MinCommitInterval: ptr(time.Duration(0)),
			PullInterval:      ptr(time.Hour),
		},
		Repos: []RepoEntry{{Path: path}},
	}
	if mod != nil {
		mod(&cfg.Repos[0].Settings)
	}
	rcs, err := cfg.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	r := NewRepo(rcs[0], testLogger(t), nil)
	r.host = "testhost"
	if err := r.setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r
}

func commitAndSync(t *testing.T, r *Repo) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r.Sync(ctx)
}

func wantState(t *testing.T, r *Repo, state, reasonPart string) {
	t.Helper()
	if r.st.State != state || !strings.Contains(r.st.Reason, reasonPart) {
		t.Fatalf("state = %s (%q), want %s containing %q", r.st.State, r.st.Reason, state, reasonPart)
	}
}

func TestCommitAndPush(t *testing.T) {
	w := newWorld(t)
	a := w.clone("a")
	r := newRepo(t, a, nil)

	write(t, filepath.Join(a, "notes.org"), "line 1\nline 2\n")
	write(t, filepath.Join(a, "sub", "todo.org"), "x\n")
	commitAndSync(t, r)

	wantState(t, r, StateOK, "")
	if got := remoteLog(t, w.remote)[0]; got != "autogit: notes.org and sub/todo.org on testhost" {
		t.Errorf("remote head subject = %q", got)
	}
	if got := git(t, a, "log", "-1", "--format=%(trailers:key=Autogit,valueonly)"); got != "testhost" {
		t.Errorf("trailer = %q", got)
	}
	if r.st.Ahead != 0 || r.st.Behind != 0 {
		t.Errorf("ahead/behind = %d/%d", r.st.Ahead, r.st.Behind)
	}
}

func TestCommitMessageTemplate(t *testing.T) {
	w := newWorld(t)
	a := w.clone("a")
	r := newRepo(t, a, func(s *Settings) {
		s.CommitMessage = ptr("{{.Repo}}: {{.Count}} files on {{.Branch}}")
	})
	write(t, filepath.Join(a, "x"), "1")
	commitAndSync(t, r)
	if got := git(t, a, "log", "-1", "--format=%s"); got != "a: 1 files on main" {
		t.Errorf("subject = %q", got)
	}
}

func TestSummarizeFiles(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{
		{[]string{"a"}, "a"},
		{[]string{"a", "b"}, "a and b"},
		{[]string{"a", "b", "c"}, "a, b and c"},
		{[]string{"a", "b", "c", "d", "e"}, "a, b and 3 more"},
	} {
		if got := summarizeFiles(c.in); got != c.want {
			t.Errorf("summarizeFiles(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIgnoredFilesAreNotCommitted(t *testing.T) {
	w := newWorld(t)
	a := w.clone("a")
	write(t, filepath.Join(a, ".gitignore"), "*.log\n")
	git(t, a, "add", ".gitignore")
	git(t, a, "commit", "-q", "-m", "ignore logs")
	r := newRepo(t, a, nil)

	write(t, filepath.Join(a, "debug.log"), "x")
	write(t, filepath.Join(a, ".#notes.org"), "lock") // editor lock file
	write(t, filepath.Join(a, "notes.org~"), "backup")
	committed, err := r.Commit(context.Background())
	if err != nil || committed {
		t.Fatalf("Commit = %v, %v; want nothing to commit", committed, err)
	}
}

func TestPushOnlyToAllowedRemotes(t *testing.T) {
	w := newWorld(t)
	a := w.clone("a")
	backup := w0bare(t, w.root, "backup.git")
	git(t, a, "remote", "add", "backup", backup)
	git(t, a, "push", "-q", "backup", "main")
	r := newRepo(t, a, func(s *Settings) { s.PushRemotes = []string{"backup"} })

	write(t, filepath.Join(a, "notes.org"), "changed\n")
	commitAndSync(t, r)

	wantState(t, r, StateOK, "")
	if got := remoteLog(t, backup)[0]; !strings.HasPrefix(got, "autogit:") {
		t.Errorf("backup head = %q, want autogit commit", got)
	}
	if got := remoteLog(t, w.remote)[0]; got != "initial" {
		t.Errorf("origin head = %q, want untouched", got)
	}
}

func TestNewBranchesAreNotPushed(t *testing.T) {
	w := newWorld(t)
	a := w.clone("a")
	git(t, a, "switch", "-q", "-c", "feature")
	r := newRepo(t, a, nil)
	write(t, filepath.Join(a, "f"), "x")
	commitAndSync(t, r)
	if out := git(t, w.remote, "branch", "--list", "feature"); out != "" {
		t.Errorf("feature was pushed: %q", out)
	}
}

func TestFastForwardPull(t *testing.T) {
	w := newWorld(t)
	a, b := w.clone("a"), w.clone("b")
	write(t, filepath.Join(b, "from-b"), "b\n")
	git(t, b, "add", ".")
	git(t, b, "commit", "-q", "-m", "from b")
	git(t, b, "push", "-q")

	r := newRepo(t, a, nil)
	commitAndSync(t, r)
	wantState(t, r, StateOK, "")
	if _, err := os.Stat(filepath.Join(a, "from-b")); err != nil {
		t.Errorf("not fast-forwarded: %v", err)
	}
}

func TestRebaseAutogitCommits(t *testing.T) {
	w := newWorld(t)
	a, b := w.clone("a"), w.clone("b")
	ra := newRepo(t, a, nil)
	rb := newRepo(t, b, nil)

	write(t, filepath.Join(a, "a.org"), "a\n")
	write(t, filepath.Join(b, "b.org"), "b\n")
	commitAndSync(t, rb) // b pushes first
	commitAndSync(t, ra) // a has diverged and rebases its autogit commit

	wantState(t, ra, StateOK, "")
	log := remoteLog(t, w.remote)
	if len(log) != 3 || !strings.Contains(log[0], "a.org") || !strings.Contains(log[1], "b.org") {
		t.Errorf("remote log = %q", log)
	}
	if merges := git(t, w.remote, "rev-list", "--merges", "main"); merges != "" {
		t.Errorf("history has merges: %s", merges)
	}
}

func TestConflictPausesAndLeavesWorktreeAlone(t *testing.T) {
	w := newWorld(t)
	a, b := w.clone("a"), w.clone("b")
	marker := filepath.Join(w.root, "paused")
	ra := newRepo(t, a, func(s *Settings) {
		s.OnPause = ptr(`printf '%s: %s' "$AUTOGIT_REPO" "$AUTOGIT_REASON" > ` + marker)
	})
	rb := newRepo(t, b, nil)

	write(t, filepath.Join(b, "notes.org"), "from b\n")
	commitAndSync(t, rb)
	write(t, filepath.Join(a, "notes.org"), "from a\n")
	ctx := context.Background()
	ra.Commit(ctx)
	head := git(t, a, "rev-parse", "HEAD")
	ra.Sync(ctx)

	wantState(t, ra, StatePaused, "rebase onto origin/main failed")
	if got := git(t, a, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved from %s to %s", head, got)
	}
	if Operation(ra.paths.GitDir) != "" {
		t.Error("rebase was left in progress")
	}
	if got := git(t, a, "status", "--porcelain"); got != "" {
		t.Errorf("worktree not clean: %q", got)
	}
	if got := remoteLog(t, w.remote)[0]; !strings.Contains(got, "testhost") {
		t.Errorf("remote head = %q", got)
	}
	waitFor(t, func() bool {
		data, _ := os.ReadFile(marker)
		return strings.HasPrefix(string(data), "a: rebase onto")
	})

	// The user resolves the conflict by hand; the next sync resumes.
	git(t, a, "fetch", "-q")
	c := exec.Command("git", "rebase", "origin/main")
	c.Dir = a
	c.Run() // conflicts
	write(t, filepath.Join(a, "notes.org"), "from a and b\n")
	git(t, a, "add", "notes.org")
	c = exec.Command("git", "rebase", "--continue")
	c.Dir = a
	c.Env = append(os.Environ(), "GIT_EDITOR=true")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("rebase --continue: %v\n%s", err, out)
	}
	ra.Sync(ctx)
	wantState(t, ra, StateOK, "")
	if got := git(t, w.remote, "show", "main:notes.org"); got != "from a and b" {
		t.Errorf("remote notes.org = %q", got)
	}
}

func TestManualCommitDivergedPauses(t *testing.T) {
	w := newWorld(t)
	a, b := w.clone("a"), w.clone("b")
	ra := newRepo(t, a, nil)
	rb := newRepo(t, b, nil)

	write(t, filepath.Join(b, "b.org"), "b\n")
	commitAndSync(t, rb)
	write(t, filepath.Join(a, "a.org"), "a\n")
	git(t, a, "add", ".")
	git(t, a, "commit", "-q", "-m", "manual")
	commitAndSync(t, ra)

	wantState(t, ra, StatePaused, "diverged from origin/main (1 local, 1 remote)")
	if !strings.Contains(ra.st.Reason, "not made by autogit") {
		t.Errorf("reason = %q", ra.st.Reason)
	}
	if got := git(t, a, "log", "-1", "--format=%s"); got != "manual" {
		t.Errorf("local head = %q", got)
	}
}

func TestDetachedHeadAndRebaseInProgressPause(t *testing.T) {
	w := newWorld(t)
	a := w.clone("a")
	r := newRepo(t, a, nil)
	ctx := context.Background()

	git(t, a, "checkout", "-q", "--detach")
	r.Sync(ctx)
	wantState(t, r, StatePaused, "detached HEAD")
	write(t, filepath.Join(a, "x"), "x")
	if committed, _ := r.Commit(ctx); committed {
		t.Error("committed on detached HEAD")
	}
	git(t, a, "checkout", "-q", "main")
	r.Sync(ctx)
	wantState(t, r, StateOK, "")

	os.MkdirAll(filepath.Join(r.paths.GitDir, "rebase-merge"), 0o755)
	r.Sync(ctx)
	wantState(t, r, StatePaused, "rebase in progress")
	os.RemoveAll(filepath.Join(r.paths.GitDir, "rebase-merge"))
	r.Sync(ctx)
	wantState(t, r, StateOK, "")
}

func TestDirtyWithoutAutoCommitSkipsPull(t *testing.T) {
	w := newWorld(t)
	a, b := w.clone("a"), w.clone("b")
	rb := newRepo(t, b, nil)
	write(t, filepath.Join(b, "b.org"), "b\n")
	commitAndSync(t, rb)

	r := newRepo(t, a, func(s *Settings) { s.AutoCommit = ptr(false) })
	write(t, filepath.Join(a, "notes.org"), "local edit\n")
	r.Sync(context.Background())
	wantState(t, r, StateDirty, "pull skipped")
	if _, err := os.Stat(filepath.Join(a, "b.org")); err == nil {
		t.Error("pulled into a dirty worktree")
	}
}

func TestUnreachableRemoteIsAnError(t *testing.T) {
	w := newWorld(t)
	a := w.clone("a")
	r := newRepo(t, a, nil)
	git(t, a, "remote", "set-url", "origin", filepath.Join(w.root, "gone.git"))
	write(t, filepath.Join(a, "x"), "x")
	commitAndSync(t, r)
	wantState(t, r, StateError, "fetch")
	if got := git(t, a, "log", "-1", "--format=%s"); !strings.HasPrefix(got, "autogit:") {
		t.Errorf("local commit missing: %q", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestDaemon runs the event loop with both watchers.
func TestDaemon(t *testing.T) {
	w := newWorld(t)
	a := w.clone("a")
	write(t, filepath.Join(a, ".gitignore"), "build/\n")
	git(t, a, "add", ".")
	git(t, a, "commit", "-q", "-m", "ignore build")
	git(t, a, "push", "-q")
	r := newRepo(t, a, func(s *Settings) { s.Debounce = ptr(300 * time.Millisecond) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx, nil); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	count := func() int { return len(remoteLog(t, w.remote)) }
	waitFor(t, func() bool { return count() == 2 }) // initial sync done
	time.Sleep(200 * time.Millisecond)

	// Several quick edits, including in a new directory, give one commit.
	write(t, filepath.Join(a, "notes.org"), "1\n")
	write(t, filepath.Join(a, "new", "deep", "file.org"), "1\n")
	time.Sleep(100 * time.Millisecond)
	write(t, filepath.Join(a, "notes.org"), "2\n")
	waitFor(t, func() bool { return count() == 3 })
	if got := remoteLog(t, w.remote)[0]; got != "autogit: new/deep/file.org and notes.org on testhost" {
		t.Errorf("subject = %q", got)
	}

	// The new directory is watched now.
	write(t, filepath.Join(a, "new", "deep", "file.org"), "2\n")
	waitFor(t, func() bool { return count() == 4 })

	// Ignored paths do not trigger commits.
	write(t, filepath.Join(a, "build", "out.bin"), "x")
	time.Sleep(time.Second)
	if n := count(); n != 4 {
		t.Errorf("ignored file caused a commit (%d commits)", n)
	}

	// A commit made by hand is pushed through the refs watcher.
	write(t, filepath.Join(a, ".gitignore"), "build/\n*.tmp\n")
	git(t, a, "commit", "-q", "-am", "by hand")
	waitFor(t, func() bool { return remoteLog(t, w.remote)[0] == "by hand" })
}

func TestConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write(t, path, `
defaults:
  push_remotes: [backup]
  debounce: 10s
repos:
  - path: /srv/notes
  - path: /srv/other
    name: o
    auto_pull: false
    push_remotes: []
    commit_message: "sync {{.Count}}"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	repos, err := cfg.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	n, o := repos[0], repos[1]
	if n.Name != "notes" || !n.AutoPull || n.Debounce != 10*time.Second || n.PullInterval != 2*time.Minute ||
		len(n.PushRemotes) != 1 || n.Sign {
		t.Errorf("notes = %+v", n)
	}
	if o.Name != "o" || o.AutoPull || len(o.PushRemotes) != 0 || !o.AutoCommit {
		t.Errorf("o = %+v", o)
	}

	for _, bad := range []string{
		"repos:\n  - path: /a\n    auto_pul: true\n",
		"repos:\n  - path: relative\n",
		"repos:\n  - path: /a\n  - path: /b/a\n",
		"repos:\n  - path: /a\n    ignore: [\"x/y\"]\n",
		"repos:\n  - path: /a\n    commit_message: \"{{.Nope\"\n",
		"",
	} {
		write(t, path, bad)
		cfg, err := LoadConfig(path)
		if err == nil {
			_, err = cfg.Resolve()
		}
		if err == nil {
			t.Errorf("config %q: expected error", bad)
		}
	}
}

func TestExampleConfig(t *testing.T) {
	cfg, err := LoadConfig("contrib/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	repos, err := cfg.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 3 || repos[1].Name != "dots" || repos[1].AutoCommit {
		t.Errorf("repos = %+v", repos)
	}
}

func TestParseStatus(t *testing.T) {
	got := parseStatus(" M a\x00R  new\x00old\x00?? c d\x00")
	want := []Change{{" M", "a"}, {"R ", "new"}, {"??", "c d"}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %v, want %v", i, got[i], want[i])
		}
	}
}
