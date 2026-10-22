package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"
)

// Settings holds the per-repository options. Every field is optional so
// that a repository entry can override single values of the defaults.
type Settings struct {
	AutoCommit        *bool          `yaml:"auto_commit"`
	AutoPush          *bool          `yaml:"auto_push"`
	AutoPull          *bool          `yaml:"auto_pull"`
	PushRemotes       []string       `yaml:"push_remotes"`
	CommitMessage     *string        `yaml:"commit_message"`
	Sign              *bool          `yaml:"sign"`
	Debounce          *time.Duration `yaml:"debounce"`
	MinCommitInterval *time.Duration `yaml:"min_commit_interval"`
	PullInterval      *time.Duration `yaml:"pull_interval"`
	Ignore            []string       `yaml:"ignore"`
	OnPause           *string        `yaml:"on_pause"`
}

type RepoEntry struct {
	Name     string `yaml:"name"`
	Path     string `yaml:"path"`
	Settings `yaml:",inline"`
}

type Config struct {
	StateFile  string        `yaml:"state_file"`
	GitTimeout time.Duration `yaml:"git_timeout"`
	Defaults   Settings      `yaml:"defaults"`
	Repos      []RepoEntry   `yaml:"repos"`
}

// RepoConfig is the effective configuration of one repository after
// applying the defaults.
type RepoConfig struct {
	Name              string
	Path              string
	AutoCommit        bool
	AutoPush          bool
	AutoPull          bool
	PushRemotes       []string
	CommitMessage     *template.Template
	Sign              bool
	Debounce          time.Duration
	MinCommitInterval time.Duration
	PullInterval      time.Duration
	Ignore            []string
	OnPause           string
	GitTimeout        time.Duration
}

var builtinDefaults = Settings{
	AutoCommit:        ptr(true),
	AutoPush:          ptr(true),
	AutoPull:          ptr(true),
	PushRemotes:       []string{},
	CommitMessage:     ptr("autogit: {{.Files}} on {{.Host}}"),
	Sign:              ptr(false),
	Debounce:          ptr(5 * time.Second),
	MinCommitInterval: ptr(time.Minute),
	PullInterval:      ptr(2 * time.Minute),
	Ignore:            []string{".#*", "#*#", "*~", "*.swp", "*.swx", ".*.kate-swp"},
	OnPause:           ptr(""),
}

func ptr[T any](v T) *T { return &v }

func defaultConfigPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "autogit", "config.yaml")
	}
	return expandHome("~/.config/autogit/config.yaml")
}

func defaultStateFile() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "autogit", "status.json")
	}
	return expandHome("~/.local/state/autogit/status.json")
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// LoadConfig reads and validates the config file.
func LoadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	defer f.Close()
	cfg := &Config{}
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	// An empty file decodes to io.EOF; Resolve reports the missing repos.
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if cfg.StateFile == "" {
		cfg.StateFile = defaultStateFile()
	}
	cfg.StateFile = expandHome(cfg.StateFile)
	if cfg.GitTimeout == 0 {
		cfg.GitTimeout = 2 * time.Minute
	}
	return cfg, nil
}

// Resolve merges defaults into each repository entry and validates them.
func (c *Config) Resolve() ([]RepoConfig, error) {
	if len(c.Repos) == 0 {
		return nil, errors.New("no repos configured")
	}
	var out []RepoConfig
	seen := map[string]bool{}
	for i, e := range c.Repos {
		if e.Path == "" {
			return nil, fmt.Errorf("repos[%d]: path missing", i)
		}
		path := expandHome(e.Path)
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("repos[%d]: path %q must be absolute", i, e.Path)
		}
		name := e.Name
		if name == "" {
			name = filepath.Base(path)
		}
		if seen[name] {
			return nil, fmt.Errorf("repos[%d]: duplicate name %q (set name explicitly)", i, name)
		}
		seen[name] = true

		s := merge(builtinDefaults, c.Defaults, e.Settings)
		tmpl, err := template.New(name).Option("missingkey=error").Parse(*s.CommitMessage)
		if err != nil {
			return nil, fmt.Errorf("repo %s: commit_message: %w", name, err)
		}
		for _, g := range s.Ignore {
			if strings.Contains(g, "/") {
				return nil, fmt.Errorf("repo %s: ignore pattern %q: only file name patterns are supported", name, g)
			}
			if _, err := filepath.Match(g, ""); err != nil {
				return nil, fmt.Errorf("repo %s: ignore pattern %q: %w", name, g, err)
			}
		}
		for _, d := range []struct {
			name string
			v    time.Duration
		}{{"debounce", *s.Debounce}, {"pull_interval", *s.PullInterval}} {
			if d.v <= 0 {
				return nil, fmt.Errorf("repo %s: %s must be positive", name, d.name)
			}
		}
		out = append(out, RepoConfig{
			Name:              name,
			Path:              filepath.Clean(path),
			AutoCommit:        *s.AutoCommit,
			AutoPush:          *s.AutoPush,
			AutoPull:          *s.AutoPull,
			PushRemotes:       s.PushRemotes,
			CommitMessage:     tmpl,
			Sign:              *s.Sign,
			Debounce:          *s.Debounce,
			MinCommitInterval: *s.MinCommitInterval,
			PullInterval:      *s.PullInterval,
			Ignore:            s.Ignore,
			OnPause:           *s.OnPause,
			GitTimeout:        c.GitTimeout,
		})
	}
	return out, nil
}

// merge returns the settings with later layers overriding earlier ones.
// Lists replace, they are not appended.
func merge(layers ...Settings) Settings {
	var r Settings
	for _, l := range layers {
		if l.AutoCommit != nil {
			r.AutoCommit = l.AutoCommit
		}
		if l.AutoPush != nil {
			r.AutoPush = l.AutoPush
		}
		if l.AutoPull != nil {
			r.AutoPull = l.AutoPull
		}
		if l.PushRemotes != nil {
			r.PushRemotes = l.PushRemotes
		}
		if l.CommitMessage != nil {
			r.CommitMessage = l.CommitMessage
		}
		if l.Sign != nil {
			r.Sign = l.Sign
		}
		if l.Debounce != nil {
			r.Debounce = l.Debounce
		}
		if l.MinCommitInterval != nil {
			r.MinCommitInterval = l.MinCommitInterval
		}
		if l.PullInterval != nil {
			r.PullInterval = l.PullInterval
		}
		if l.Ignore != nil {
			r.Ignore = l.Ignore
		}
		if l.OnPause != nil {
			r.OnPause = l.OnPause
		}
	}
	return r
}
