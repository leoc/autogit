package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
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
	PushBranches      []string       `yaml:"push_branches"`
	PushOptions       PushOptions    `yaml:"push_options"`
	CommitMessage     *string        `yaml:"commit_message"`
	Sign              *bool          `yaml:"sign"`
	Debounce          *time.Duration `yaml:"debounce"`
	MinCommitInterval *time.Duration `yaml:"min_commit_interval"`
	PullInterval      *time.Duration `yaml:"pull_interval"`
	Ignore            []string       `yaml:"ignore"`
	OnPause           *string        `yaml:"on_pause"`
}

// RepoEntry is one entry of the repos list. Path may be a glob; it then
// stands for every git repository whose root it matches.
type RepoEntry struct {
	Name     string   `yaml:"name"`
	Path     string   `yaml:"path"`
	Exclude  []string `yaml:"exclude"`
	Settings `yaml:",inline"`
}

// PushOptions maps a remote name to the push options (git push -o) used
// for it. The key "*" applies to every remote without an own entry. In
// the config it is either a list (for all remotes) or such a map.
type PushOptions map[string][]string

const allRemotes = "*"

func (p *PushOptions) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.SequenceNode:
		var l []string
		if err := n.Decode(&l); err != nil {
			return err
		}
		*p = PushOptions{allRemotes: l}
		return nil
	case yaml.MappingNode:
		m := map[string][]string{}
		if err := n.Decode(&m); err != nil {
			return err
		}
		*p = m
		return nil
	}
	return fmt.Errorf("line %d: push_options must be a list or a map of remote to list", n.Line)
}

// For returns the push options for remote.
func (p PushOptions) For(remote string) []string {
	if o, ok := p[remote]; ok {
		return o
	}
	return p[allRemotes]
}

// SkippedRepo is a repository matched by a glob but left out.
type SkippedRepo struct {
	Path   string
	Reason string
}

type Config struct {
	StateFile  string        `yaml:"state_file"`
	GitTimeout time.Duration `yaml:"git_timeout"`
	Defaults   Settings      `yaml:"defaults"`
	Repos      []RepoEntry   `yaml:"repos"`

	// Skipped lists the glob matches that Resolve left out.
	Skipped []SkippedRepo `yaml:"-"`
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
	PushBranches      []string // nil: the current branch only
	PushOptions       PushOptions
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
	PushOptions:       PushOptions{},
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

// Resolve expands repository globs, merges defaults into each repository
// entry and validates them. Repositories a glob matched but that were left
// out are recorded in c.Skipped.
func (c *Config) Resolve() ([]RepoConfig, error) {
	if len(c.Repos) == 0 {
		return nil, errors.New("no repos configured")
	}
	c.Skipped = nil
	type target struct {
		idx  int
		path string
		name string
		glob bool
	}
	var targets []target
	explicit := map[string]bool{}
	for i, e := range c.Repos {
		if e.Path == "" {
			return nil, fmt.Errorf("repos[%d]: path missing", i)
		}
		path := expandHome(e.Path)
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("repos[%d]: path %q must be absolute", i, e.Path)
		}
		if !isGlob(path) {
			if len(e.Exclude) > 0 {
				return nil, fmt.Errorf("repos[%d]: exclude is only allowed with a glob path", i)
			}
			path = filepath.Clean(path)
			explicit[path] = true
			targets = append(targets, target{idx: i, path: path, name: e.Name})
			continue
		}
		if e.Name != "" {
			return nil, fmt.Errorf("repos[%d]: name is not allowed with a glob path", i)
		}
		for _, x := range append([]string{e.Path}, e.Exclude...) {
			if _, err := filepath.Match(expandHome(x), ""); err != nil {
				return nil, fmt.Errorf("repos[%d]: pattern %q: %w", i, x, err)
			}
		}
		matches, _ := filepath.Glob(path) // pattern validated above
		for _, m := range matches {
			if excluded(m, e.Exclude) || !isRepoRoot(m) {
				continue
			}
			targets = append(targets, target{idx: i, path: filepath.Clean(m), glob: true})
		}
	}

	var out []RepoConfig
	seenName := map[string]bool{}
	seenPath := map[string]bool{}
	for _, tg := range targets {
		e := c.Repos[tg.idx]
		if tg.glob && (explicit[tg.path] || seenPath[tg.path]) {
			continue // an explicit entry or an earlier glob wins
		}
		seenPath[tg.path] = true
		name := tg.name
		if name == "" {
			name = filepath.Base(tg.path)
			if tg.glob && seenName[name] {
				name = filepath.Join(filepath.Base(filepath.Dir(tg.path)), name)
				if seenName[name] {
					name = tg.path
				}
			}
		}
		if seenName[name] {
			return nil, fmt.Errorf("repos[%d]: duplicate name %q (set name explicitly)", tg.idx, name)
		}

		s := merge(builtinDefaults, c.Defaults, e.Settings)
		if tg.glob {
			if reason := ciBlocker(tg.path, s); reason != "" {
				c.Skipped = append(c.Skipped, SkippedRepo{Path: tg.path, Reason: reason})
				continue
			}
		}
		seenName[name] = true
		rc, err := c.repoConfig(name, tg.path, s)
		if err != nil {
			return nil, err
		}
		out = append(out, rc)
	}
	if len(out) == 0 {
		return nil, errors.New("no repos configured (globs matched no usable repository)")
	}
	return out, nil
}

func (c *Config) repoConfig(name, path string, s Settings) (RepoConfig, error) {
	tmpl, err := template.New(name).Option("missingkey=error").Parse(*s.CommitMessage)
	if err != nil {
		return RepoConfig{}, fmt.Errorf("repo %s: commit_message: %w", name, err)
	}
	for _, g := range s.Ignore {
		if strings.Contains(g, "/") {
			return RepoConfig{}, fmt.Errorf("repo %s: ignore pattern %q: only file name patterns are supported", name, g)
		}
		if _, err := filepath.Match(g, ""); err != nil {
			return RepoConfig{}, fmt.Errorf("repo %s: ignore pattern %q: %w", name, g, err)
		}
	}
	for _, g := range s.PushBranches {
		if _, err := pathpkg.Match(g, ""); err != nil {
			return RepoConfig{}, fmt.Errorf("repo %s: push_branches pattern %q: %w", name, g, err)
		}
	}
	for remote, opts := range s.PushOptions {
		for _, o := range opts {
			if o == "" || strings.ContainsAny(o, "\n\x00") {
				return RepoConfig{}, fmt.Errorf("repo %s: push_options for %s: invalid option %q", name, remote, o)
			}
		}
	}
	for _, d := range []struct {
		name string
		v    time.Duration
	}{{"debounce", *s.Debounce}, {"pull_interval", *s.PullInterval}} {
		if d.v <= 0 {
			return RepoConfig{}, fmt.Errorf("repo %s: %s must be positive", name, d.name)
		}
	}
	return RepoConfig{
		Name:              name,
		Path:              path,
		AutoCommit:        *s.AutoCommit,
		AutoPush:          *s.AutoPush,
		AutoPull:          *s.AutoPull,
		PushRemotes:       s.PushRemotes,
		PushBranches:      s.PushBranches,
		PushOptions:       s.PushOptions,
		CommitMessage:     tmpl,
		Sign:              *s.Sign,
		Debounce:          *s.Debounce,
		MinCommitInterval: *s.MinCommitInterval,
		PullInterval:      *s.PullInterval,
		Ignore:            s.Ignore,
		OnPause:           *s.OnPause,
		GitTimeout:        c.GitTimeout,
	}, nil
}

func isGlob(p string) bool { return strings.ContainsAny(p, "*?[") }

func excluded(path string, patterns []string) bool {
	for _, x := range patterns {
		x = expandHome(x)
		if ok, _ := filepath.Match(x, path); ok || filepath.Clean(x) == path {
			return true
		}
	}
	return false
}

// isRepoRoot reports whether dir is the root of a git worktree (not a bare
// repository).
func isRepoRoot(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// ciFiles are the paths (relative to the worktree root) whose presence
// means that a push may start a CI pipeline.
var ciFiles = []string{
	".gitlab-ci.yml",
	".github/workflows",
	".forgejo/workflows",
	".gitea/workflows",
	".woodpecker.yml",
	".woodpecker.yaml",
	".woodpecker",
	".drone.yml",
	".circleci",
	"bitbucket-pipelines.yml",
	"Jenkinsfile",
	".travis.yml",
}

// detectCI returns the first CI configuration found in the worktree.
func detectCI(dir string) string {
	for _, f := range ciFiles {
		if _, err := os.Lstat(filepath.Join(dir, f)); err == nil {
			return f
		}
	}
	return ""
}

// ciBlocker explains why a repository matched by a glob must be skipped:
// it has a CI configuration, autogit would push to it, and a push remote
// has no push options (such as ci.skip) to keep the pipeline from running.
func ciBlocker(dir string, s Settings) string {
	if !*s.AutoPush || len(s.PushRemotes) == 0 {
		return ""
	}
	ci := detectCI(dir)
	if ci == "" {
		return ""
	}
	var bare []string
	for _, r := range s.PushRemotes {
		if len(s.PushOptions.For(r)) == 0 {
			bare = append(bare, r)
		}
	}
	if len(bare) == 0 {
		return ""
	}
	return fmt.Sprintf("has CI configuration (%s) and no push_options for %s", ci, strings.Join(bare, ", "))
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
		if l.PushBranches != nil {
			r.PushBranches = l.PushBranches
		}
		if l.PushOptions != nil {
			r.PushOptions = l.PushOptions
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
