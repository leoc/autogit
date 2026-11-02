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
	t.Setenv("HOME", t.TempDir()) // the example glob must match nothing
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

// commitOn makes a commit by hand on branch and switches back to main.
func commitOn(t *testing.T, dir, branch, file string) {
	t.Helper()
	git(t, dir, "switch", "-q", branch)
	write(t, filepath.Join(dir, file), branch+"\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "on "+branch)
	git(t, dir, "switch", "-q", "main")
}

func pushOnly(s *Settings) {
	s.AutoCommit = ptr(false)
	s.AutoPull = ptr(false)
}

func TestPushBranches(t *testing.T) {
	w := newWorld(t)
	a := w.clone("a")
	for _, b := range []string{"feature/x", "wip"} {
		git(t, a, "branch", b)
		git(t, a, "push", "-q", "origin", b)
	}
	git(t, a, "branch", "feature/new") // not on the remote
	commitOn(t, a, "main", "m")
	commitOn(t, a, "feature/x", "x")
	commitOn(t, a, "wip", "w")
	commitOn(t, a, "feature/new", "n")

	r := newRepo(t, a, func(s *Settings) {
		pushOnly(s)
		s.PushBranches = []string{"main", "feature/*"}
	})
	r.Sync(context.Background())
	wantState(t, r, StateOK, "")

	for branch, want := range map[string]string{"main": "on main", "feature/x": "on feature/x", "wip": "initial"} {
		if got := git(t, w.remote, "log", "-1", "--format=%s", branch); got != want {
			t.Errorf("remote %s = %q, want %q", branch, got, want)
		}
	}
	if out := git(t, w.remote, "branch", "--list", "feature/new"); out != "" {
		t.Errorf("feature/new was created on the remote: %q", out)
	}
}

func TestPushBranchesNotCheckedOut(t *testing.T) {
	// With push_branches, main is pushed even while another branch is
	// checked out; without, only the current branch would be.
	w := newWorld(t)
	a := w.clone("a")
	commitOn(t, a, "main", "m")
	git(t, a, "switch", "-q", "-c", "topic")

	r := newRepo(t, a, pushOnly)
	r.Sync(context.Background())
	if got := remoteLog(t, w.remote)[0]; got != "initial" {
		t.Errorf("without push_branches main was pushed: %q", got)
	}

	r = newRepo(t, a, func(s *Settings) { pushOnly(s); s.PushBranches = []string{"main"} })
	r.Sync(context.Background())
	if got := remoteLog(t, w.remote)[0]; got != "on main" {
		t.Errorf("remote main = %q, want on main", got)
	}
}

// recordPushOptions makes the bare repository accept push options and
// record them in a file, as GitLab would read ci.skip.
func recordPushOptions(t *testing.T, bare string) string {
	t.Helper()
	git(t, bare, "config", "receive.advertisePushOptions", "true")
	out := strings.TrimSuffix(bare, ".git") + "-push-options"
	hook := filepath.Join(bare, "hooks", "pre-receive")
	write(t, hook, "#!/bin/sh\nprintf '%s %s\\n' \"$GIT_PUSH_OPTION_COUNT\" \"$GIT_PUSH_OPTION_0\" >> "+out+"\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPushOptions(t *testing.T) {
	w := newWorld(t)
	a := w.clone("a")
	backup := w0bare(t, w.root, "backup.git")
	git(t, a, "remote", "add", "backup", backup)
	git(t, a, "push", "-q", "backup", "main")
	originOpts := recordPushOptions(t, w.remote)
	backupOpts := recordPushOptions(t, backup)

	commitOn(t, a, "main", "m")
	r := newRepo(t, a, func(s *Settings) {
		pushOnly(s)
		s.PushRemotes = []string{"origin", "backup"}
		s.PushOptions = PushOptions{"*": {"ci.skip"}, "backup": {}}
	})
	r.Sync(context.Background())
	wantState(t, r, StateOK, "")

	if data, _ := os.ReadFile(originOpts); string(data) != "1 ci.skip\n" {
		t.Errorf("origin push options = %q", data)
	}
	if data, _ := os.ReadFile(backupOpts); string(data) != "0 \n" {
		t.Errorf("backup push options = %q", data)
	}
}

func TestPushOptionsUnsupportedIsAnError(t *testing.T) {
	w := newWorld(t)
	a := w.clone("a")
	commitOn(t, a, "main", "m")
	r := newRepo(t, a, func(s *Settings) { pushOnly(s); s.PushOptions = PushOptions{"*": {"ci.skip"}} })
	r.Sync(context.Background())
	wantState(t, r, StateError, "push")
	if got := remoteLog(t, w.remote)[0]; got != "initial" {
		t.Errorf("pushed without push options: %q", got)
	}
}

func TestPushOnlyNeverPullsOrCommits(t *testing.T) {
	w := newWorld(t)
	a, b := w.clone("a"), w.clone("b")
	rb := newRepo(t, b, nil)
	write(t, filepath.Join(b, "b.org"), "b\n")
	commitAndSync(t, rb)

	r := newRepo(t, a, pushOnly)
	write(t, filepath.Join(a, "draft"), "uncommitted\n")
	ctx := context.Background()
	if r.cfg.AutoCommit {
		t.Fatal("auto_commit on")
	}
	r.Sync(ctx)
	wantState(t, r, StateOK, "")
	if _, err := os.Stat(filepath.Join(a, "b.org")); err == nil {
		t.Error("pulled although auto_pull is off")
	}
	if got := git(t, a, "log", "-1", "--format=%s"); got != "initial" {
		t.Errorf("local head = %q", got)
	}

	// A local commit on a branch the remote moved ahead on is rejected
	// and pauses the repository instead of being merged or forced.
	commitOn(t, a, "main", "m")
	r.Sync(ctx)
	wantState(t, r, StatePaused, "push of origin/main rejected")
	if got := remoteLog(t, w.remote)[0]; !strings.Contains(got, "b.org") {
		t.Errorf("remote head = %q", got)
	}
}

// mkRepo creates a non-bare repository with the given files.
func mkRepo(t *testing.T, dir string, files ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q")
	for _, f := range files {
		write(t, filepath.Join(dir, f), "x\n")
	}
	return dir
}

func TestConfigGlobs(t *testing.T) {
	root := t.TempDir()
	mkRepo(t, filepath.Join(root, "src", "plain"))
	mkRepo(t, filepath.Join(root, "src", "gitlab"), ".gitlab-ci.yml")
	mkRepo(t, filepath.Join(root, "src", "actions"), ".github/workflows/test.yml")
	mkRepo(t, filepath.Join(root, "src", "excluded"))
	mkRepo(t, filepath.Join(root, "src", "explicit"))
	mkRepo(t, filepath.Join(root, "other", "plain"))
	os.MkdirAll(filepath.Join(root, "src", "not-a-repo"), 0o755)
	git(t, root, "init", "-q", "--bare", filepath.Join(root, "src", "bare.git"))

	path := filepath.Join(root, "config.yaml")
	write(t, path, `
defaults:
  push_remotes: [origin]
  push_branches: [main, master]
repos:
  - path: `+root+`/src/explicit
    name: mine
    auto_commit: true
  - path: `+root+`/src/*
    exclude: ["`+root+`/src/excl*"]
    auto_commit: false
    auto_pull: false
  - path: `+root+`/*/plain
  - path: `+root+`/src/actions
    push_options:
      origin: [ci.skip]
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	repos, err := cfg.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]RepoConfig{}
	for _, rc := range repos {
		got[rc.Name] = rc
	}
	if len(got) != 4 {
		t.Errorf("repos = %v", slicesKeys(got))
	}
	if rc, ok := got["mine"]; !ok || !rc.AutoCommit {
		t.Errorf("explicit entry lost or overridden by glob: %+v", rc)
	}
	if rc, ok := got["plain"]; !ok || rc.AutoCommit || rc.Path != filepath.Join(root, "src", "plain") {
		t.Errorf("plain = %+v", rc)
	}
	if rc, ok := got["other/plain"]; !ok || !rc.AutoCommit {
		t.Errorf("other/plain = %+v (duplicate base name should be qualified)", rc)
	}
	if rc, ok := got["actions"]; !ok || len(rc.PushOptions.For("origin")) != 1 || len(rc.PushBranches) != 2 {
		t.Errorf("actions = %+v (explicit entry with push_options)", rc)
	}
	if len(cfg.Skipped) != 1 || !strings.HasSuffix(cfg.Skipped[0].Path, "gitlab") ||
		!strings.Contains(cfg.Skipped[0].Reason, ".gitlab-ci.yml") {
		t.Errorf("skipped = %+v", cfg.Skipped)
	}

	// push_options make a CI repository eligible for a glob.
	write(t, path, `
defaults:
  push_remotes: [origin]
  push_options: [ci.skip]
repos:
  - path: `+root+`/src/*
`)
	cfg, _ = LoadConfig(path)
	if repos, err = cfg.Resolve(); err != nil {
		t.Fatal(err)
	}
	if len(repos) != 5 || len(cfg.Skipped) != 0 {
		t.Errorf("with push_options: %d repos, skipped %+v", len(repos), cfg.Skipped)
	}

	for _, bad := range []string{
		"repos:\n  - path: " + root + "/src/*\n    name: x\n",
		"repos:\n  - path: " + root + "/src/plain\n    exclude: [x]\n",
		"repos:\n  - path: " + root + "/nothing/*\n",
		"repos:\n  - path: /a\n    push_branches: [\"[\"]\n",
		"repos:\n  - path: /a\n    push_options: ci.skip\n",
		"repos:\n  - path: /a\n    push_options: [\"\"]\n",
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

func slicesKeys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestPushOptionsFor(t *testing.T) {
	p := PushOptions{"*": {"ci.skip"}, "github": {}}
	if got := p.For("origin"); len(got) != 1 || got[0] != "ci.skip" {
		t.Errorf("origin = %v", got)
	}
	if got := p.For("github"); len(got) != 0 {
		t.Errorf("github = %v", got)
	}
	if got := (PushOptions{}).For("origin"); got != nil {
		t.Errorf("empty = %v", got)
	}
}

func TestPushRejected(t *testing.T) {
	out := "To ../origin.git\n=\trefs/heads/a:refs/heads/a\t[up to date]\n!\trefs/heads/main:refs/heads/main\t[rejected] (fetch first)\nDone\n"
	if got := pushRejected(out); len(got) != 1 || got[0] != "refs/heads/main" {
		t.Errorf("pushRejected = %v", got)
	}
}
