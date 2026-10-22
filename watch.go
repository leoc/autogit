package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// batchWindow collects bursts of filesystem events before filtering them
// through git check-ignore.
const batchWindow = 100 * time.Millisecond

// TreeWatcher watches a worktree recursively and reports changed paths
// that git does not ignore. .git, nested repositories and ignored
// directories are not watched.
type TreeWatcher struct {
	root   string
	git    *Git
	ignore []string
	log    *slog.Logger
	w      *fsnotify.Watcher

	Changes chan []string // relative paths, after filtering
	Errors  chan error
}

func NewTreeWatcher(ctx context.Context, root string, git *Git, ignore []string, log *slog.Logger) (*TreeWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	t := &TreeWatcher{
		root: root, git: git, ignore: ignore, log: log, w: w,
		Changes: make(chan []string, 16),
		Errors:  make(chan error, 4),
	}
	if err := t.addTree(ctx, root); err != nil {
		w.Close()
		return nil, err
	}
	go t.loop(ctx)
	return t, nil
}

func (t *TreeWatcher) rel(p string) string {
	rel, err := filepath.Rel(t.root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return ""
	}
	return rel
}

// addTree watches dir and all non-ignored directories below it, one
// directory level per check-ignore call.
func (t *TreeWatcher) addTree(ctx context.Context, dir string) error {
	level := []string{dir}
	for len(level) > 0 {
		var next []string
		for _, d := range level {
			if err := t.w.Add(d); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return err
			}
			entries, err := os.ReadDir(d)
			if err != nil {
				continue // removed meanwhile
			}
			for _, e := range entries {
				if !e.IsDir() || e.Name() == ".git" {
					continue // symlinks are not followed
				}
				sub := filepath.Join(d, e.Name())
				if _, err := os.Lstat(filepath.Join(sub, ".git")); err == nil {
					continue // nested repository or submodule
				}
				next = append(next, sub)
			}
		}
		rels := make([]string, 0, len(next))
		for _, d := range next {
			rels = append(rels, t.rel(d))
		}
		ignored, err := t.git.CheckIgnore(ctx, rels)
		if err != nil {
			return err
		}
		level = level[:0]
		for i, d := range next {
			if !ignored[rels[i]] {
				level = append(level, d)
			}
		}
	}
	return nil
}

func (t *TreeWatcher) skip(rel string) bool {
	if rel == "" || rel == "." {
		return true
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == ".git" {
			return true
		}
	}
	base := filepath.Base(rel)
	for _, g := range t.ignore {
		if ok, _ := filepath.Match(g, base); ok {
			return true
		}
	}
	return false
}

func (t *TreeWatcher) loop(ctx context.Context) {
	defer t.w.Close()
	pending := map[string]bool{}
	created := map[string]bool{}
	flush := time.NewTimer(time.Hour)
	flush.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-t.w.Events:
			if !ok {
				return
			}
			if ev.Op == fsnotify.Chmod {
				continue
			}
			rel := t.rel(ev.Name)
			if t.skip(rel) {
				continue
			}
			pending[rel] = true
			if ev.Has(fsnotify.Create) {
				created[rel] = true
			}
			flush.Reset(batchWindow)
		case err, ok := <-t.w.Errors:
			if !ok {
				return
			}
			t.sendErr(err)
		case <-flush.C:
			t.flush(ctx, pending, created)
			clear(pending)
			clear(created)
		}
	}
}

func (t *TreeWatcher) flush(ctx context.Context, pending, created map[string]bool) {
	paths := make([]string, 0, len(pending))
	for p := range pending {
		paths = append(paths, p)
	}
	ignored, err := t.git.CheckIgnore(ctx, paths)
	if err != nil {
		t.sendErr(err)
		return
	}
	var changed []string
	for _, p := range paths {
		if ignored[p] {
			continue
		}
		changed = append(changed, p)
		if created[p] {
			abs := filepath.Join(t.root, p)
			if fi, err := os.Lstat(abs); err == nil && fi.IsDir() {
				if _, err := os.Lstat(filepath.Join(abs, ".git")); err == nil {
					continue
				}
				if err := t.addTree(ctx, abs); err != nil {
					t.sendErr(err)
				}
			}
		}
	}
	if len(changed) > 0 {
		t.log.Debug("worktree changed", "paths", changed)
		select {
		case t.Changes <- changed:
		case <-ctx.Done():
		}
	}
}

func (t *TreeWatcher) sendErr(err error) {
	select {
	case t.Errors <- err:
	default:
	}
}

// RefsWatcher signals when HEAD or a local branch changes.
type RefsWatcher struct {
	paths RepoPaths
	log   *slog.Logger
	w     *fsnotify.Watcher

	Changed chan struct{}
	Errors  chan error
}

func NewRefsWatcher(ctx context.Context, paths RepoPaths, log *slog.Logger) (*RefsWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	rw := &RefsWatcher{
		paths: paths, log: log, w: w,
		Changed: make(chan struct{}, 1),
		Errors:  make(chan error, 4),
	}
	err = w.Add(paths.GitDir)
	if err == nil && paths.CommonDir != paths.GitDir {
		err = w.Add(paths.CommonDir)
	}
	if err == nil {
		err = rw.addDirs(filepath.Join(paths.CommonDir, "refs", "heads"))
	}
	if err != nil {
		w.Close()
		return nil, err
	}
	go rw.loop(ctx)
	return rw, nil
}

func (rw *RefsWatcher) addDirs(root string) error {
	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return rw.w.Add(p)
		}
		return nil
	})
}

func (rw *RefsWatcher) relevant(ev fsnotify.Event) bool {
	name := filepath.Base(ev.Name)
	if strings.HasSuffix(name, ".lock") {
		return false
	}
	dir := filepath.Dir(ev.Name)
	switch {
	case dir == rw.paths.GitDir && name == "HEAD":
		return true
	case dir == rw.paths.CommonDir && name == "packed-refs":
		return true
	}
	heads := filepath.Join(rw.paths.CommonDir, "refs", "heads")
	return ev.Name == heads || strings.HasPrefix(ev.Name, heads+string(filepath.Separator))
}

func (rw *RefsWatcher) loop(ctx context.Context) {
	defer rw.w.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-rw.w.Events:
			if !ok {
				return
			}
			if !rw.relevant(ev) {
				continue
			}
			if ev.Has(fsnotify.Create) {
				if fi, err := os.Lstat(ev.Name); err == nil && fi.IsDir() {
					if err := rw.addDirs(ev.Name); err != nil {
						rw.sendErr(err)
					}
				}
			}
			rw.log.Debug("refs changed", "path", ev.Name)
			select {
			case rw.Changed <- struct{}{}:
			default:
			}
		case err, ok := <-rw.w.Errors:
			if !ok {
				return
			}
			rw.sendErr(err)
		}
	}
}

func (rw *RefsWatcher) sendErr(err error) {
	select {
	case rw.Errors <- err:
	default:
	}
}
