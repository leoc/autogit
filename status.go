package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"text/tabwriter"
	"time"
)

const (
	StateOK     = "ok"
	StateDirty  = "dirty"  // uncommitted changes and auto_commit off
	StatePaused = "paused" // needs a human; pull and push are on hold
	StateError  = "error"  // transient failure, retried
)

type RepoStatus struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	State      string    `json:"state"`
	Reason     string    `json:"reason,omitempty"`
	Branch     string    `json:"branch,omitempty"`
	Upstream   string    `json:"upstream,omitempty"`
	Ahead      int       `json:"ahead"`
	Behind     int       `json:"behind"`
	LastCommit time.Time `json:"last_commit,omitzero"`
	LastPull   time.Time `json:"last_pull,omitzero"`
	LastPush   time.Time `json:"last_push,omitzero"`
	LastSync   time.Time `json:"last_sync,omitzero"`
	Updated    time.Time `json:"updated"`
}

// StatusFile is what the daemon writes to the state file.
type StatusFile struct {
	PID     int          `json:"pid"`
	Started time.Time    `json:"started"`
	Updated time.Time    `json:"updated"`
	Repos   []RepoStatus `json:"repos"`
}

// StatusWriter collects repository states and writes them atomically.
type StatusWriter struct {
	path    string
	updates chan RepoStatus
	file    StatusFile
}

func NewStatusWriter(path string, names []string) *StatusWriter {
	w := &StatusWriter{
		path:    path,
		updates: make(chan RepoStatus, 64),
		file:    StatusFile{PID: os.Getpid(), Started: time.Now()},
	}
	for _, n := range names {
		w.file.Repos = append(w.file.Repos, RepoStatus{Name: n, State: StateOK})
	}
	return w
}

func (w *StatusWriter) Publish(s RepoStatus) { w.updates <- s }

// Run writes the state file on every update until updates is closed.
func (w *StatusWriter) Run() error {
	if err := w.write(); err != nil {
		return err
	}
	for s := range w.updates {
		for i := range w.file.Repos {
			if w.file.Repos[i].Name == s.Name {
				w.file.Repos[i] = s
			}
		}
		if err := w.write(); err != nil {
			return err
		}
	}
	return nil
}

func (w *StatusWriter) write() error {
	w.file.Updated = time.Now()
	data, err := json.MarshalIndent(w.file, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
		return err
	}
	tmp := w.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, w.path)
}

func ReadStatusFile(path string) (*StatusFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f StatusFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &f, nil
}

// Running reports whether the daemon that wrote f is still alive.
func (f *StatusFile) Running() bool {
	return f.PID > 0 && syscall.Kill(f.PID, 0) == nil
}

// Problems counts repositories that are paused or failing.
func Problems(repos []RepoStatus) int {
	n := 0
	for _, r := range repos {
		if r.State == StatePaused || r.State == StateError {
			n++
		}
	}
	return n
}

func PrintStatus(out io.Writer, repos []RepoStatus) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REPO\tSTATE\tBRANCH\tUPSTREAM\t↑\t↓\tSYNCED\tREASON")
	for _, r := range repos {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\n",
			r.Name, r.State, dash(r.Branch), dash(r.Upstream),
			r.Ahead, r.Behind, ago(r.LastSync), r.Reason)
	}
	tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("2006-01-02")
	}
}
