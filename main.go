// Command autogit keeps git repositories in sync with their remotes: it
// commits worktree changes, pushes new commits to allowed remotes and pulls
// upstream changes when that is possible without a human.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"
)

const usage = `usage: autogit [-config file] [-v] <command> [args]

commands:
  run             run the daemon (default)
  status [-json]  show the state of all repositories; exit 1 if any is
                  paused or failing
  check [repo…]   commit, pull and push once in the foreground, then exit
  sync            ask the running daemon to sync all repositories now
`

func main() {
	configPath := flag.String("config", "", "config file (default ~/.config/autogit/config.yaml)")
	verbose := flag.Bool("v", false, "debug logging")
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, usage+"\nflags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	path := *configPath
	if path == "" {
		path = defaultConfigPath()
	}

	cmd, args := "run", flag.Args()
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	code := 0
	switch cmd {
	case "run":
		err = runDaemon(path, log)
	case "status":
		code, err = statusCmd(path, args)
	case "check":
		code, err = checkCmd(path, args, log)
	case "sync":
		err = syncCmd(path)
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		log.Error(cmd, "err", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func loadRepos(path string) (*Config, []RepoConfig, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, nil, err
	}
	repos, err := cfg.Resolve()
	if err != nil {
		return nil, nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, repos, nil
}

// runDaemon runs one worker per repository. SIGHUP reloads the config,
// SIGUSR1 syncs all repositories, SIGINT/SIGTERM stop.
func runDaemon(path string, log *slog.Logger) error {
	cfg, repos, err := loadRepos(path)
	if err != nil {
		return err
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGINT, syscall.SIGTERM)

	for {
		d := startDaemon(cfg, repos, log)
		reload := false
		for !reload {
			switch <-sigs {
			case syscall.SIGUSR1:
				log.Info("sync requested")
				d.syncAll()
			case syscall.SIGHUP:
				newCfg, newRepos, err := loadRepos(path)
				if err != nil {
					log.Error("reload failed, keeping current config", "err", err)
					continue
				}
				log.Info("reloading config")
				cfg, repos, reload = newCfg, newRepos, true
			default:
				log.Info("stopping")
				d.stop()
				return nil
			}
		}
		d.stop()
	}
}

type daemon struct {
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	syncChs []chan struct{}
	status  *StatusWriter
	done    chan error
}

func startDaemon(cfg *Config, repos []RepoConfig, log *slog.Logger) *daemon {
	ctx, cancel := context.WithCancel(context.Background())
	names := make([]string, len(repos))
	for i, rc := range repos {
		names[i] = rc.Name
	}
	d := &daemon{cancel: cancel, status: NewStatusWriter(cfg.StateFile, names), done: make(chan error, 1)}
	go func() { d.done <- d.status.Run() }()
	log.Info("starting", "repos", len(repos), "skipped", len(cfg.Skipped), "state_file", cfg.StateFile)
	logSkipped(cfg, log)
	for _, rc := range repos {
		ch := make(chan struct{}, 1)
		d.syncChs = append(d.syncChs, ch)
		repo := NewRepo(rc, log, d.status.Publish)
		d.wg.Go(func() { repo.Run(ctx, ch) })
	}
	return d
}

func logSkipped(cfg *Config, log *slog.Logger) {
	for _, s := range cfg.Skipped {
		log.Info("skipping repository", "path", s.Path, "reason", s.Reason)
	}
}

func (d *daemon) syncAll() {
	for _, ch := range d.syncChs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (d *daemon) stop() {
	d.cancel()
	d.wg.Wait()
	close(d.status.updates)
	if err := <-d.done; err != nil {
		slog.Error("writing state file", "err", err)
	}
}

func statusCmd(path string, args []string) (int, error) {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the raw state as JSON")
	if err := fs.Parse(args); err != nil {
		return 2, nil
	}
	stateFile := defaultStateFile()
	if cfg, err := LoadConfig(path); err == nil {
		stateFile = cfg.StateFile
	}
	f, err := ReadStatusFile(stateFile)
	if errors.Is(err, os.ErrNotExist) {
		return 1, fmt.Errorf("no state file at %s: is the daemon running?", stateFile)
	}
	if err != nil {
		return 1, err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		out := struct {
			*StatusFile
			Running bool `json:"running"`
		}{f, f.Running()}
		return 0, enc.Encode(out)
	}
	if !f.Running() {
		fmt.Printf("autogit is not running (state from %s)\n\n", f.Updated.Format("2006-01-02 15:04:05"))
	}
	PrintStatus(os.Stdout, f.Repos)
	if Problems(f.Repos) > 0 || !f.Running() {
		return 1, nil
	}
	return 0, nil
}

// checkCmd runs one commit and sync per repository without watching.
func checkCmd(path string, names []string, log *slog.Logger) (int, error) {
	cfg, repos, err := loadRepos(path)
	if err != nil {
		return 1, err
	}
	logSkipped(cfg, log)
	var results []RepoStatus
	ctx := context.Background()
	for _, rc := range repos {
		if len(names) > 0 && !slices.Contains(names, rc.Name) {
			continue
		}
		var last RepoStatus
		r := NewRepo(rc, log, func(s RepoStatus) { last = s })
		if r.setup(ctx) == nil {
			if rc.AutoCommit {
				r.Commit(ctx)
			}
			r.Sync(ctx)
		}
		r.update()
		results = append(results, last)
	}
	if len(results) == 0 {
		return 1, fmt.Errorf("no repository matches %v", names)
	}
	PrintStatus(os.Stdout, results)
	if Problems(results) > 0 {
		return 1, nil
	}
	return 0, nil
}

func syncCmd(path string) error {
	stateFile := defaultStateFile()
	if cfg, err := LoadConfig(path); err == nil {
		stateFile = cfg.StateFile
	}
	f, err := ReadStatusFile(stateFile)
	if err != nil || !f.Running() {
		return errors.New("autogit daemon is not running")
	}
	return syscall.Kill(f.PID, syscall.SIGUSR1)
}
