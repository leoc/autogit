package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Git runs git commands in one repository.
type Git struct {
	Dir     string
	Timeout time.Duration
}

// GitError carries the exit code and stderr of a failed git command.
type GitError struct {
	Args   []string
	Code   int
	Stdout string
	Stderr string
	Err    error
}

func (e *GitError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), msg)
}

func (e *GitError) Unwrap() error { return e.Err }

// exitCode returns the exit code of a failed git command, or -1.
func exitCode(err error) int {
	var ge *GitError
	if errors.As(err, &ge) {
		return ge.Code
	}
	return -1
}

func (g *Git) cmd(ctx context.Context, stdin string, args ...string) (string, error) {
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c := exec.CommandContext(ctx, "git", args...)
	c.Dir = g.Dir
	c.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_EDITOR=true",
		"GIT_MERGE_AUTOEDIT=no",
		"LC_ALL=C",
	)
	if stdin != "" {
		c.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	err := c.Run()
	if err != nil {
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		if ctx.Err() != nil {
			err = fmt.Errorf("%w (timeout %s)", ctx.Err(), timeout)
		}
		return stdout.String(), &GitError{Args: args, Code: code, Stdout: stdout.String(), Stderr: stderr.String(), Err: err}
	}
	return stdout.String(), nil
}

// Run executes git and returns its trimmed stdout.
func (g *Git) Run(ctx context.Context, args ...string) (string, error) {
	out, err := g.cmd(ctx, "", args...)
	return strings.TrimSpace(out), err
}

// RepoPaths are the locations git uses for a (possibly linked) worktree.
type RepoPaths struct {
	Top       string // worktree root
	GitDir    string // per-worktree git dir (HEAD, MERGE_HEAD, rebase state)
	CommonDir string // shared git dir (refs, packed-refs)
}

func (g *Git) Paths(ctx context.Context) (RepoPaths, error) {
	out, err := g.Run(ctx, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-dir", "--git-common-dir")
	if err != nil {
		return RepoPaths{}, err
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		return RepoPaths{}, fmt.Errorf("unexpected rev-parse output %q", out)
	}
	return RepoPaths{Top: lines[0], GitDir: lines[1], CommonDir: lines[2]}, nil
}

// Operation returns the name of an unfinished git operation, if any.
func Operation(gitDir string) string {
	for _, op := range []struct{ file, name string }{
		{"rebase-merge", "rebase"},
		{"rebase-apply", "rebase"},
		{"MERGE_HEAD", "merge"},
		{"CHERRY_PICK_HEAD", "cherry-pick"},
		{"REVERT_HEAD", "revert"},
		{"BISECT_LOG", "bisect"},
	} {
		if _, err := os.Lstat(filepath.Join(gitDir, op.file)); err == nil {
			return op.name
		}
	}
	return ""
}

// Branch returns the current branch name, or "" for a detached HEAD.
func (g *Git) Branch(ctx context.Context) (string, error) {
	out, err := g.Run(ctx, "symbolic-ref", "-q", "--short", "HEAD")
	if exitCode(err) == 1 {
		return "", nil
	}
	return out, err
}

// Branches returns the names of all local branches.
func (g *Git) Branches(ctx context.Context) ([]string, error) {
	out, err := g.Run(ctx, "for-each-ref", "--format=%(refname:lstrip=2)", "refs/heads/")
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

// HasCommits reports whether HEAD points to a commit (false in a fresh repo).
func (g *Git) HasCommits(ctx context.Context) bool {
	_, err := g.Run(ctx, "rev-parse", "-q", "--verify", "HEAD^{commit}")
	return err == nil
}

// Upstream returns the upstream ref (refs/remotes/<remote>/<branch>) and
// remote name of branch, or empty strings if none is configured.
func (g *Git) Upstream(ctx context.Context, branch string) (ref, remote string, err error) {
	out, err := g.Run(ctx, "for-each-ref", "--format=%(upstream)%00%(upstream:remotename)", "refs/heads/"+branch)
	if err != nil || out == "" {
		return "", "", err
	}
	ref, remote, _ = strings.Cut(out, "\x00")
	if !strings.HasPrefix(ref, "refs/remotes/") {
		// Upstream is a local branch; nothing to pull from a remote.
		return "", "", nil
	}
	return ref, remote, nil
}

// RefExists reports whether a fully qualified ref exists.
func (g *Git) RefExists(ctx context.Context, ref string) bool {
	_, err := g.Run(ctx, "show-ref", "-q", "--verify", ref)
	return err == nil
}

// AheadBehind counts commits in a but not b (ahead) and in b but not a (behind).
func (g *Git) AheadBehind(ctx context.Context, a, b string) (ahead, behind int, err error) {
	out, err := g.Run(ctx, "rev-list", "--left-right", "--count", a+"..."+b)
	if err != nil {
		return 0, 0, err
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("unexpected rev-list output %q", out)
	}
	ahead, _ = strconv.Atoi(f[0])
	behind, _ = strconv.Atoi(f[1])
	return ahead, behind, nil
}

// Remotes returns the configured remote names.
func (g *Git) Remotes(ctx context.Context) ([]string, error) {
	out, err := g.Run(ctx, "remote")
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

// Change is one entry of git status.
type Change struct {
	Code string // two-letter porcelain status, "??" for untracked
	Path string
}

// Status lists changes in the worktree, skipping paths matched by the
// exclude pathspecs.
func (g *Git) Status(ctx context.Context, untracked bool, excludes []string) ([]Change, error) {
	args := []string{"--no-optional-locks", "status", "--porcelain=v1", "-z"}
	if untracked {
		args = append(args, "--untracked-files=all")
	} else {
		args = append(args, "--untracked-files=no")
	}
	args = append(args, "--", ".")
	args = append(args, excludes...)
	out, err := g.cmd(ctx, "", args...)
	if err != nil {
		return nil, err
	}
	return parseStatus(out), nil
}

func parseStatus(out string) []Change {
	var changes []Change
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		c := Change{Code: e[:2], Path: e[3:]}
		changes = append(changes, c)
		if c.Code[0] == 'R' || c.Code[0] == 'C' {
			i++ // skip the original path of a rename or copy
		}
	}
	return changes
}

// excludePathspecs turns file name globs into git exclude pathspecs that
// match at any depth.
func excludePathspecs(globs []string) []string {
	var out []string
	for _, g := range globs {
		out = append(out, ":(exclude,glob)**/"+g)
	}
	return out
}

// CheckIgnore returns the subset of paths (relative to the worktree root)
// that git ignores.
func (g *Git) CheckIgnore(ctx context.Context, paths []string) (map[string]bool, error) {
	ignored := map[string]bool{}
	if len(paths) == 0 {
		return ignored, nil
	}
	out, err := g.cmd(ctx, strings.Join(paths, "\x00")+"\x00", "check-ignore", "--stdin", "-z")
	if exitCode(err) == 1 {
		return ignored, nil // nothing ignored
	}
	if err != nil {
		return nil, err
	}
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			ignored[p] = true
		}
	}
	return ignored, nil
}
