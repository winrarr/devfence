package devfence

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Backend string

const (
	BackendBubblewrap Backend = "bubblewrap"
	BackendDocker     Backend = "docker"
	BackendVM         Backend = "vm"
)

type Config struct {
	Version        int                `yaml:"version"`
	DefaultProfile string             `yaml:"defaultProfile"`
	Profiles       map[string]Profile `yaml:"profiles"`
	Rules          []Rule             `yaml:"rules"`
	ProtectedPaths []string           `yaml:"protectedPaths"`
}

type Rule struct {
	Path    string `yaml:"path"`
	Match   string `yaml:"match"`
	Profile string `yaml:"profile"`
}

type Profile struct {
	Backend     Backend          `yaml:"backend"`
	Network     string           `yaml:"network"`
	Workspace   WorkspacePolicy  `yaml:"workspace"`
	Credentials CredentialPolicy `yaml:"credentials"`
	Resources   Resources        `yaml:"resources"`
	VM          VMConfig         `yaml:"vm"`
}

type WorkspacePolicy struct {
	Mode    string   `yaml:"mode"`
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`
}

type CredentialPolicy struct {
	SSHKey             string   `yaml:"sshKey"`
	GitHubTokenCommand []string `yaml:"githubTokenCommand"`
}

type Resources struct {
	MemoryMiB int `yaml:"memoryMiB"`
	VCPUs     int `yaml:"vcpus"`
	DiskGiB   int `yaml:"diskGiB"`
}

type VMConfig struct {
	ImageURL       string `yaml:"imageURL"`
	ChecksumsURL   string `yaml:"checksumsURL"`
	GuestUser      string `yaml:"guestUser"`
	KubectlVersion string `yaml:"kubectlVersion"`
	KindVersion    string `yaml:"kindVersion"`
	CiliumVersion  string `yaml:"ciliumVersion"`
}

type Resolved struct {
	Config      Config
	ProfileName string
	Profile     Profile
	Workspace   string
	WorkingDir  string
	Protected   []string
	StateDir    string
}

func DefaultConfig() Config {
	return Config{
		Version:        1,
		DefaultProfile: "default",
		Profiles: map[string]Profile{
			"default": {
				Backend:   BackendBubblewrap,
				Network:   "full",
				Workspace: WorkspacePolicy{Mode: "live"},
			},
		},
	}
}

func ConfigPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "devfence", "config.yaml"), nil
}

func StatePath() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "devfence"), nil
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return DefaultConfig(), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (cfg Config) Validate() error {
	if cfg.Version != 1 {
		return fmt.Errorf("unsupported config version %d (expected 1)", cfg.Version)
	}
	if len(cfg.Profiles) == 0 {
		return errors.New("config must define at least one profile")
	}
	if _, ok := cfg.Profiles[cfg.DefaultProfile]; !ok {
		return fmt.Errorf("default profile %q is not defined", cfg.DefaultProfile)
	}
	for name, profile := range cfg.Profiles {
		if err := profile.Validate(); err != nil {
			return fmt.Errorf("profile %q: %w", name, err)
		}
	}
	for i, rule := range cfg.Rules {
		if rule.Path == "" || rule.Profile == "" {
			return fmt.Errorf("rule %d needs path and profile", i+1)
		}
		if _, ok := cfg.Profiles[rule.Profile]; !ok {
			return fmt.Errorf("rule %d selects undefined profile %q", i+1, rule.Profile)
		}
		if rule.Match != "" && rule.Match != "exact" && rule.Match != "subtree" {
			return fmt.Errorf("rule %d has unsupported match mode %q", i+1, rule.Match)
		}
	}
	return nil
}

func (p Profile) Validate() error {
	if p.Backend == "" {
		return errors.New("backend is required")
	}
	switch p.Backend {
	case BackendBubblewrap, BackendDocker, BackendVM:
	default:
		return fmt.Errorf("unsupported backend %q", p.Backend)
	}
	if p.Network == "" {
		return errors.New("network must be full or none")
	}
	if p.Network != "full" && p.Network != "none" {
		return fmt.Errorf("unsupported network mode %q", p.Network)
	}
	if p.Workspace.Mode == "" {
		return errors.New("workspace.mode is required")
	}
	if p.Workspace.Mode != "live" && p.Workspace.Mode != "copy" {
		return fmt.Errorf("unsupported workspace mode %q", p.Workspace.Mode)
	}
	for _, pattern := range append(append([]string{}, p.Workspace.Include...), p.Workspace.Exclude...) {
		if err := validatePattern(pattern); err != nil {
			return err
		}
	}
	if p.Backend == BackendVM && p.Network == "none" {
		return errors.New("the VM backend needs network access for SSH management; network: none is unsupported")
	}
	if p.Resources.MemoryMiB < 0 || p.Resources.VCPUs < 0 || p.Resources.DiskGiB < 0 {
		return errors.New("resource limits cannot be negative")
	}
	if len(p.Credentials.GitHubTokenCommand) > 0 && strings.TrimSpace(p.Credentials.GitHubTokenCommand[0]) == "" {
		return errors.New("githubTokenCommand must start with a command")
	}
	return nil
}

func validatePattern(pattern string) error {
	if pattern == "" || filepath.IsAbs(pattern) {
		return fmt.Errorf("workspace patterns must be non-empty relative paths: %q", pattern)
	}
	clean := filepath.Clean(filepath.FromSlash(pattern))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("workspace pattern escapes its root: %q", pattern)
	}
	return nil
}

func Resolve(cfg Config, cwd, backendOverride, workspaceOverride, networkOverride, profileOverride string) (Resolved, error) {
	if err := cfg.Validate(); err != nil {
		return Resolved{}, err
	}
	workingDir, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve current directory: %w", err)
	}
	workingDir, err = filepath.Abs(workingDir)
	if err != nil {
		return Resolved{}, err
	}
	if info, err := os.Stat(workingDir); err != nil || !info.IsDir() {
		return Resolved{}, fmt.Errorf("current path is not a directory: %s", workingDir)
	}
	workspace := workingDir
	if top, err := gitTopLevel(workingDir); err == nil {
		workspace = top
	}
	if err := validateWorkspaceRoot(workspace); err != nil {
		return Resolved{}, err
	}

	profileName := cfg.DefaultProfile
	if profileOverride != "" {
		if _, ok := cfg.Profiles[profileOverride]; !ok {
			return Resolved{}, fmt.Errorf("profile %q is not defined", profileOverride)
		}
		profileName = profileOverride
	} else {
		for _, rule := range cfg.Rules {
			base, err := expandPath(rule.Path)
			if err != nil {
				return Resolved{}, fmt.Errorf("rule path %q: %w", rule.Path, err)
			}
			base, err = canonicalExistingOrClean(base)
			if err != nil {
				return Resolved{}, err
			}
			match := rule.Match
			if match == "" {
				match = "subtree"
			}
			if match == "exact" && workspace == base || match == "subtree" && within(base, workspace) {
				profileName = rule.Profile
				break
			}
		}
	}

	profile := cfg.Profiles[profileName]
	if backendOverride != "" {
		profile.Backend = Backend(backendOverride)
	}
	if workspaceOverride != "" {
		profile.Workspace.Mode = workspaceOverride
	}
	if networkOverride != "" {
		profile.Network = networkOverride
	}
	if err := profile.Validate(); err != nil {
		return Resolved{}, err
	}

	protected := make([]string, 0, len(cfg.ProtectedPaths))
	for _, path := range cfg.ProtectedPaths {
		expanded, err := expandPath(path)
		if err != nil {
			return Resolved{}, fmt.Errorf("protected path %q: %w", path, err)
		}
		resolved, err := canonicalExistingOrClean(expanded)
		if err != nil {
			return Resolved{}, err
		}
		if workspace == resolved || within(resolved, workspace) {
			if profile.Workspace.Mode != "copy" || len(profile.Workspace.Include) == 0 {
				return Resolved{}, fmt.Errorf("workspace %s overlaps protected path %s; select copy mode with explicit include patterns", workspace, resolved)
			}
		}
		if within(workspace, resolved) && (profile.Workspace.Mode == "live" || len(profile.Workspace.Include) == 0) {
			return Resolved{}, fmt.Errorf("workspace %s contains protected path %s; use copy mode with explicit include patterns", workspace, resolved)
		}
		protected = append(protected, resolved)
	}
	stateDir, err := StatePath()
	if err != nil {
		return Resolved{}, err
	}
	if within(workspace, stateDir) || within(stateDir, workspace) {
		return Resolved{}, errors.New("workspace and Devfence state directory must not overlap; set XDG_STATE_HOME elsewhere")
	}
	return Resolved{Config: cfg, ProfileName: profileName, Profile: profile, Workspace: workspace, WorkingDir: workingDir, Protected: protected, StateDir: stateDir}, nil
}

func validateWorkspaceRoot(workspace string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	blocked := []string{"/", "/home", home, "/tmp", "/var/tmp", "/run", "/proc", "/sys", "/dev"}
	for _, path := range blocked {
		if filepath.Clean(workspace) == filepath.Clean(path) {
			return fmt.Errorf("refusing broad or special workspace root %s", workspace)
		}
	}
	return nil
}

func gitTopLevel(cwd string) (string, error) {
	cmd := execCommand("git", "-C", cwd, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return canonicalExistingOrClean(strings.TrimSpace(string(out)))
}

func expandPath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return filepath.Abs(filepath.Clean(path))
}

func canonicalExistingOrClean(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
