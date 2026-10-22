package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// trailerKey marks commits created by autogit. Only such commits are
// rebased onto the upstream when histories diverge.
const trailerKey = "Autogit"

// Repo holds the sync state of one repository. Its methods are not safe
// for concurrent use; the worker in repo.go serializes them.
type Repo struct {
	cfg      RepoConfig
	git      *Git
	log      *slog.Logger
	host     string
	excludes []string
	paths    RepoPaths

	st     RepoStatus
	paused string            // why pull/push need a human, set by Sync
	errs   map[string]string // transient errors by operation
	dirty  bool              // uncommitted tracked changes block the pull
	note   string            // informational reason while ok

	publish func(RepoStatus)
}

func NewRepo(cfg RepoConfig, log *slog.Logger, publish func(RepoStatus)) *Repo {
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(host, ".")
	if publish == nil {
		publish = func(RepoStatus) {}
	}
	return &Repo{
		cfg:      cfg,
		git:      &Git{Dir: cfg.Path, Timeout: cfg.GitTimeout},
		log:      log.With("repo", cfg.Name),
		host:     host,
		excludes: excludePathspecs(cfg.Ignore),
		st:       RepoStatus{Name: cfg.Name, Path: cfg.Path, State: StateOK},
		errs:     map[string]string{},
		publish:  publish,
	}
}

var errOrder = []string{"setup", "watch", "fetch", "pull", "commit", "push"}

// update derives the visible state and publishes it.
func (r *Repo) update() {
	r.st.Reason = ""
	switch {
	case r.paused != "":
		r.st.State, r.st.Reason = StatePaused, r.paused
	case len(r.errs) > 0:
		r.st.State = StateError
		for _, k := range errOrder {
			if msg, ok := r.errs[k]; ok {
				r.st.Reason = msg
				break
			}
		}
	case r.dirty:
		r.st.State, r.st.Reason = StateDirty, "uncommitted changes, pull skipped"
	default:
		r.st.State, r.st.Reason = StateOK, r.note
	}
	r.st.Updated = time.Now()
	r.publish(r.st)
}

func (r *Repo) setErr(op string, err error) {
	if err == nil {
		delete(r.errs, op)
		return
	}
	msg := fmt.Sprintf("%s: %v", op, err)
	if r.errs[op] != msg {
		r.log.Error(op+" failed", "err", err)
	}
	r.errs[op] = msg
}

func (r *Repo) setPaused(reason string) {
	if reason != "" && reason != r.paused {
		r.log.Warn("paused", "reason", reason)
		r.runOnPause(reason)
	} else if reason == "" && r.paused != "" {
		r.log.Info("resumed")
	}
	r.paused = reason
}

func (r *Repo) runOnPause(reason string) {
	if r.cfg.OnPause == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c := exec.CommandContext(ctx, "sh", "-c", r.cfg.OnPause)
		c.Dir = r.cfg.Path
		c.Env = append(os.Environ(),
			"AUTOGIT_REPO="+r.cfg.Name,
			"AUTOGIT_PATH="+r.cfg.Path,
			"AUTOGIT_REASON="+reason,
		)
		if out, err := c.CombinedOutput(); err != nil {
			r.log.Error("on_pause failed", "err", err, "output", strings.TrimSpace(string(out)))
		}
	}()
}

// setup resolves the git directories. It is retried until it succeeds.
func (r *Repo) setup(ctx context.Context) error {
	if _, err := os.Stat(r.cfg.Path); err != nil {
		r.setErr("setup", err)
		return err
	}
	p, err := r.git.Paths(ctx)
	if err == nil && !samePath(p.Top, r.cfg.Path) {
		err = fmt.Errorf("%s is inside the repository %s, not its root", r.cfg.Path, p.Top)
	}
	r.setErr("setup", err)
	if err != nil {
		return err
	}
	r.paths = p
	return nil
}

// ready checks the conditions under which autogit may touch the repo and
// returns the current branch, or a reason why it must not.
func (r *Repo) ready(ctx context.Context) (branch, reason string, err error) {
	if op := Operation(r.paths.GitDir); op != "" {
		return "", op + " in progress", nil
	}
	branch, err = r.git.Branch(ctx)
	if err != nil {
		return "", "", err
	}
	if branch == "" {
		return "", "detached HEAD", nil
	}
	return branch, "", nil
}

// MessageData is available to commit message templates.
type MessageData struct {
	Files  string // changed paths, shortened: "a, b and 3 more"
	Count  int
	Host   string
	Repo   string
	Branch string
	Time   string
}

func summarizeFiles(paths []string) string {
	switch n := len(paths); {
	case n == 0:
		return ""
	case n == 1:
		return paths[0]
	case n <= 3:
		return strings.Join(paths[:n-1], ", ") + " and " + paths[n-1]
	default:
		return fmt.Sprintf("%s, %s and %d more", paths[0], paths[1], n-2)
	}
}

// Commit stages and commits all changes. It does nothing when the worktree
// is clean or a git operation is in progress.
func (r *Repo) Commit(ctx context.Context) (bool, error) {
	if r.paths.GitDir == "" {
		return false, nil
	}
	committed, err := r.commit(ctx)
	r.setErr("commit", err)
	r.update()
	return committed, err
}

func (r *Repo) commit(ctx context.Context) (bool, error) {
	branch, reason, err := r.ready(ctx)
	if err != nil || reason != "" {
		if reason != "" {
			r.log.Debug("not committing", "reason", reason)
		}
		return false, err
	}
	changes, err := r.git.Status(ctx, true, r.excludes)
	if err != nil || len(changes) == 0 {
		return false, err
	}
	paths := make([]string, len(changes))
	for i, c := range changes {
		paths[i] = c.Path
	}
	slices.Sort(paths)

	var msg strings.Builder
	err = r.cfg.CommitMessage.Execute(&msg, MessageData{
		Files:  summarizeFiles(paths),
		Count:  len(paths),
		Host:   r.host,
		Repo:   r.cfg.Name,
		Branch: branch,
		Time:   time.Now().Format("2006-01-02 15:04"),
	})
	if err != nil {
		return false, fmt.Errorf("commit message template: %w", err)
	}

	add := append([]string{"add", "-A", "--", "."}, r.excludes...)
	if _, err := r.git.Run(ctx, add...); err != nil {
		return false, err
	}
	_, err = r.git.Run(ctx,
		"-c", fmt.Sprintf("commit.gpgsign=%t", r.cfg.Sign),
		"commit", "-q", "--no-edit",
		"-m", strings.TrimSpace(msg.String()),
		"--trailer", trailerKey+": "+r.host,
	)
	if err != nil {
		return false, err
	}
	r.st.LastCommit = time.Now()
	r.log.Info("committed", "files", len(paths))
	return true, nil
}

// Sync fetches, integrates upstream changes where that is safe and pushes.
// Anything that needs a human pauses pull and push until the next Sync
// finds the conditions resolved.
func (r *Repo) Sync(ctx context.Context) {
	if r.paths.GitDir == "" {
		return
	}
	reason, err := r.sync(ctx)
	if err != nil && reason == "" {
		// An error (e.g. offline) kept us from checking; stay paused.
		reason = r.paused
	}
	r.setPaused(reason)
	r.st.LastSync = time.Now()
	r.update()
	if err != nil {
		r.log.Debug("sync incomplete", "err", err)
	}
}

func (r *Repo) sync(ctx context.Context) (string, error) {
	r.note, r.dirty = "", false
	branch, reason, err := r.ready(ctx)
	r.st.Branch = branch
	if err != nil || reason != "" {
		return reason, err
	}
	if !r.git.HasCommits(ctx) {
		r.note = "no commits yet"
		return "", nil
	}
	upRef, upRemote, err := r.git.Upstream(ctx, branch)
	if err != nil {
		return "", err
	}
	r.st.Upstream = strings.TrimPrefix(upRef, "refs/remotes/")

	if err := r.fetch(ctx, upRemote); err != nil {
		return "", err
	}

	if r.cfg.AutoPull {
		if upRef == "" {
			r.note = "no upstream, pull skipped"
		} else if reason, err := r.pull(ctx, upRef); err != nil || reason != "" {
			r.counts(ctx)
			return reason, err
		}
	}

	if r.cfg.AutoPush {
		rejected, err := r.Push(ctx)
		if err != nil {
			return "", err
		}
		if len(rejected) > 0 {
			return fmt.Sprintf("push to %s rejected (remote has diverged)", strings.Join(rejected, ", ")), nil
		}
	}
	r.counts(ctx)
	return "", nil
}

// fetch updates the remote-tracking refs of the upstream and the push
// remotes.
func (r *Repo) fetch(ctx context.Context, upRemote string) error {
	var remotes []string
	if r.cfg.AutoPull && upRemote != "" {
		remotes = append(remotes, upRemote)
	}
	if r.cfg.AutoPush {
		remotes = append(remotes, r.cfg.PushRemotes...)
	}
	slices.Sort(remotes)
	remotes = slices.Compact(remotes)
	if len(remotes) == 0 {
		r.setErr("fetch", nil)
		return nil
	}
	known, err := r.git.Remotes(ctx)
	if err == nil {
		for _, rem := range remotes {
			if !slices.Contains(known, rem) {
				err = fmt.Errorf("remote %q is not configured", rem)
				break
			}
		}
	}
	if err == nil {
		_, err = r.git.Run(ctx, append([]string{"fetch", "-q", "--multiple", "--no-tags"}, remotes...)...)
	}
	r.setErr("fetch", err)
	return err
}

// pull brings the branch up to date with upRef: fast-forward, or rebase
// when every local commit was made by autogit and was not published yet.
func (r *Repo) pull(ctx context.Context, upRef string) (string, error) {
	tracked, err := r.git.Status(ctx, false, r.excludes)
	if err != nil {
		r.setErr("pull", err)
		return "", err
	}
	if len(tracked) > 0 {
		r.dirty = true
		r.setErr("pull", nil)
		return "", nil
	}
	ahead, behind, err := r.git.AheadBehind(ctx, "HEAD", upRef)
	if err != nil {
		r.setErr("pull", err)
		return "", err
	}
	up := strings.TrimPrefix(upRef, "refs/remotes/")
	switch {
	case behind == 0:
		r.setErr("pull", nil)
		return "", nil
	case ahead == 0:
		if _, err := r.git.Run(ctx, "merge", "-q", "--ff-only", upRef); err != nil {
			r.setErr("pull", nil)
			return fmt.Sprintf("fast-forward to %s failed: %v", up, firstLine(err)), nil
		}
		r.log.Info("fast-forwarded", "upstream", up, "commits", behind)
	default:
		if why, err := r.rebaseBlocker(ctx, upRef, ahead); err != nil || why != "" {
			r.setErr("pull", err)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("diverged from %s (%d local, %d remote): %s", up, ahead, behind, why), nil
		}
		_, err := r.git.Run(ctx,
			"-c", "rebase.autoStash=false",
			"-c", "rebase.updateRefs=false",
			"-c", fmt.Sprintf("commit.gpgsign=%t", r.cfg.Sign),
			"rebase", "-q", "--no-autosquash", upRef)
		if err != nil {
			if _, aerr := r.git.Run(ctx, "rebase", "--abort"); aerr != nil {
				r.log.Error("rebase --abort failed", "err", aerr)
			}
			r.setErr("pull", nil)
			return fmt.Sprintf("rebase onto %s failed: %v", up, firstLine(err)), nil
		}
		r.log.Info("rebased", "upstream", up, "local", ahead, "remote", behind)
	}
	r.setErr("pull", nil)
	r.st.LastPull = time.Now()
	return "", nil
}

// rebaseBlocker explains why the local commits ahead of upRef must not be
// rebased, or returns "" if they may.
func (r *Repo) rebaseBlocker(ctx context.Context, upRef string, ahead int) (string, error) {
	out, err := r.git.cmd(ctx, "", "log", "-z", "--format=%H %(trailers:key="+trailerKey+",valueonly)", upRef+"..HEAD")
	if err != nil {
		return "", err
	}
	for _, rec := range strings.Split(out, "\x00") {
		if rec = strings.TrimSpace(rec); rec == "" {
			continue
		}
		if f := strings.Fields(rec); len(f) < 2 {
			return fmt.Sprintf("local commit %.10s was not made by autogit", f[0]), nil
		}
	}
	unpublished, err := r.git.Run(ctx, "rev-list", "--count", "HEAD", "--not", "--remotes")
	if err != nil {
		return "", err
	}
	if unpublished != fmt.Sprint(ahead) {
		return "local commits were already pushed to another remote", nil
	}
	return "", nil
}

// Push pushes the current branch to every allowed remote that already has
// it and is behind. It returns the remotes that rejected the push.
func (r *Repo) Push(ctx context.Context) (rejected []string, err error) {
	if r.paths.GitDir == "" || !r.cfg.AutoPush {
		return nil, nil
	}
	defer func() { r.setErr("push", err) }()
	branch, err := r.git.Branch(ctx)
	if err != nil || branch == "" || !r.git.HasCommits(ctx) {
		return nil, err
	}
	var errs []error
	for _, remote := range r.cfg.PushRemotes {
		tracking := "refs/remotes/" + remote + "/" + branch
		if !r.git.RefExists(ctx, tracking) {
			r.log.Debug("branch not on remote, not pushing", "remote", remote, "branch", branch)
			continue
		}
		ahead, _, err := r.git.AheadBehind(ctx, "HEAD", tracking)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ahead == 0 {
			continue
		}
		ref := "refs/heads/" + branch
		out, err := r.git.cmd(ctx, "", "push", "--porcelain", remote, ref+":"+ref)
		if err != nil {
			if pushRejected(out) {
				r.log.Warn("push rejected", "remote", remote)
				rejected = append(rejected, remote)
				continue
			}
			errs = append(errs, err)
			continue
		}
		r.st.LastPush = time.Now()
		r.log.Info("pushed", "remote", remote, "commits", ahead)
	}
	return rejected, errors.Join(errs...)
}

// pushRejected reports whether git push --porcelain output contains a ref
// rejected by the remote (as opposed to a connection error).
func pushRejected(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "!\t") {
			return true
		}
	}
	return false
}

// counts updates ahead/behind against the upstream without fetching.
func (r *Repo) counts(ctx context.Context) {
	r.st.Ahead, r.st.Behind = 0, 0
	if r.st.Upstream == "" {
		return
	}
	if a, b, err := r.git.AheadBehind(ctx, "HEAD", "refs/remotes/"+r.st.Upstream); err == nil {
		r.st.Ahead, r.st.Behind = a, b
	}
}

func firstLine(err error) string {
	var ge *GitError
	msg := err.Error()
	if errors.As(err, &ge) && strings.TrimSpace(ge.Stderr) != "" {
		msg = strings.TrimSpace(ge.Stderr)
	}
	line, _, _ := strings.Cut(msg, "\n")
	return strings.TrimPrefix(line, "error: ")
}

func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}
