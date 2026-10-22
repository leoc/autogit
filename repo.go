package main

import (
	"context"
	"time"
)

// pushDelay groups ref updates (e.g. the steps of a rebase) into one push.
const pushDelay = time.Second

// Run is the event loop of one repository. All git operations on the
// repository happen on this goroutine.
func (r *Repo) Run(ctx context.Context, syncNow <-chan struct{}) {
	for r.setup(ctx) != nil {
		r.update()
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.cfg.PullInterval):
		case <-syncNow:
		}
	}

	var treeChanges <-chan []string
	var treeErrors <-chan error
	if r.cfg.AutoCommit {
		t, err := NewTreeWatcher(ctx, r.paths.Top, r.git, r.cfg.Ignore, r.log)
		r.setErr("watch", err)
		if err == nil {
			treeChanges, treeErrors = t.Changes, t.Errors
		}
	}
	var refsChanged <-chan struct{}
	var refsErrors <-chan error
	if r.cfg.AutoPush {
		rw, err := NewRefsWatcher(ctx, r.paths, r.log)
		if err != nil {
			r.setErr("watch", err)
		} else {
			refsChanged, refsErrors = rw.Changed, rw.Errors
		}
	}

	var (
		lastChange  time.Time // last worktree event
		commitDue   bool      // changes are waiting for the commit timer
		syncPending bool      // a sync waits for the pending commit
		commitTimer = time.NewTimer(time.Hour)
		pushTimer   = time.NewTimer(time.Hour)
		ticker      = time.NewTicker(r.cfg.PullInterval)
	)
	commitTimer.Stop()
	pushTimer.Stop()
	defer ticker.Stop()

	// scheduleCommit arms the timer for the earliest moment that respects
	// both the debounce and the minimum interval between commits.
	scheduleCommit := func() {
		at := lastChange.Add(r.cfg.Debounce)
		if min := r.st.LastCommit.Add(r.cfg.MinCommitInterval); min.After(at) {
			at = min
		}
		commitTimer.Reset(time.Until(at))
	}
	commit := func() {
		commitDue = false
		if _, err := r.Commit(ctx); err != nil {
			return
		}
		pushTimer.Reset(pushDelay)
	}
	sync := func() {
		if r.cfg.AutoCommit {
			commit()
		}
		r.Sync(ctx)
	}

	// Catch up on whatever happened while autogit was not running.
	sync()

	for {
		select {
		case <-ctx.Done():
			return

		case paths := <-treeChanges:
			lastChange = time.Now()
			commitDue = true
			r.log.Debug("change", "paths", len(paths))
			scheduleCommit()

		case err := <-treeErrors:
			r.setErr("watch", err)
			r.update()

		case <-commitTimer.C:
			if !commitDue {
				continue
			}
			if time.Since(lastChange) < r.cfg.Debounce || time.Since(r.st.LastCommit) < r.cfg.MinCommitInterval {
				scheduleCommit()
				continue
			}
			commit()
			if syncPending {
				syncPending = false
				r.Sync(ctx)
			}

		case <-refsChanged:
			pushTimer.Reset(pushDelay)

		case err := <-refsErrors:
			r.setErr("watch", err)
			r.update()

		case <-pushTimer.C:
			if r.paused != "" {
				// The user may just have resolved the problem: recheck.
				r.Sync(ctx)
				continue
			}
			rejected, _ := r.Push(ctx)
			if len(rejected) > 0 {
				// Someone else pushed: integrate their changes, then retry.
				r.Sync(ctx)
			} else {
				r.update()
			}

		case <-ticker.C:
			if commitDue {
				// Let the user finish editing; sync after that commit.
				syncPending = true
				continue
			}
			sync()

		case <-syncNow:
			sync()
		}
	}
}
