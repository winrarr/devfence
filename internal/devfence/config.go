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
	Defaults       Profile            `yaml:"defaults"`
	GitHub         GitHubAccessPolicy `yaml:"github"`
	ProtectedPaths []string           `yaml:"protectedPaths"`
}

type GitHubAccessPolicy struct {
	Paths          []string                   `yaml:"paths"`
	Authentication GitHubAuthenticationPolicy `yaml:"authentication"`
}

type ProjectConfig struct {
	Version   int              `yaml:"version"`
	Backend   Backend          `yaml:"backend,omitempty"`
	Network   string           `yaml:"network,omitempty"`
	Workspace *WorkspacePolicy `yaml:"workspace,omitempty"`
	Shares    []ProjectShare   `yaml:"shares,omitempty"`
}

type Profile struct {
	Backend       Backend          `yaml:"backend"`
	Network       string           `yaml:"network"`
	Workspace     WorkspacePolicy  `yaml:"workspace"`
	Tools         ToolPolicy       `yaml:"tools"`
	Credentials   CredentialPolicy `yaml:"credentials"`
	Resources     Resources        `yaml:"resources"`
	VM            VMConfig         `yaml:"vm"`
	ProjectShares []ProjectShare   `yaml:"-" json:"projectShares,omitempty"`
}

type ProjectShare struct {
	Source  string   `yaml:"source" json:"source"`
	Target  string   `yaml:"target" json:"target"`
	Mode    string   `yaml:"mode" json:"mode"`
	Access  string   `yaml:"access,omitempty" json:"access,omitempty"`
	Exclude []string `yaml:"exclude,omitempty" json:"exclude,omitempty"`
}

type ToolPolicy struct {
	Codex             ForwardedToolPolicy `yaml:"codex"`
	Claude            ForwardedToolPolicy `yaml:"claude"`
	GH                GitHubToolPolicy    `yaml:"gh"`
	SharedDirectories []ForwardedPath     `yaml:"sharedDirectories"`
}

type ForwardedToolPolicy struct {
	Enabled        bool            `yaml:"enabled"`
	Authentication []ForwardedFile `yaml:"authentication"`
	Configuration  []ForwardedFile `yaml:"configuration"`
}

type ForwardedFile struct {
	Source   string `yaml:"source"`
	Target   string `yaml:"target"`
	Optional bool   `yaml:"optional,omitempty"`
}

type ForwardedPath struct {
	Source   string   `yaml:"source"`
	Target   string   `yaml:"target"`
	Exclude  []string `yaml:"exclude,omitempty"`
	Optional bool     `yaml:"optional,omitempty"`
}

type GitHubToolPolicy struct {
	Enabled        bool                       `yaml:"enabled"`
	Authentication GitHubAuthenticationPolicy `yaml:"authentication"`
}

type GitHubAuthenticationPolicy struct {
	TokenCommand []string `yaml:"tokenCommand"`
}

type WorkspacePolicy struct {
	Mode    string   `yaml:"mode"`
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`
}

type CredentialPolicy struct {
	SSHKey string `yaml:"sshKey"`
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
	Config            Config
	ProfileName       string
	Profile           Profile
	ProjectConfigPath string
	Workspace         string
	WorkingDir        string
	Protected         []string
	StateDir          string
}

func DefaultConfig() Config {
	return Config{
		Version: 3,
		Defaults: Profile{
			Backend:   BackendBubblewrap,
			Network:   "full",
			Workspace: WorkspacePolicy{Mode: "live"},
		},
	}
}

func DefaultProjectConfig() ProjectConfig {
	return ProjectConfig{Version: 3}
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
	if cfg.Version != 3 {
		return fmt.Errorf("unsupported config version %d (expected 3)", cfg.Version)
	}
	if err := cfg.Defaults.Validate(); err != nil {
		return fmt.Errorf("defaults: %w", err)
	}
	if len(cfg.Defaults.Tools.GH.Authentication.TokenCommand) > 0 {
		return errors.New("GitHub token authentication must use the top-level github path policy")
	}
	if len(cfg.GitHub.Paths) != 0 && len(cfg.GitHub.Authentication.TokenCommand) == 0 {
		return errors.New("github.paths requires github.authentication.tokenCommand")
	}
	if len(cfg.GitHub.Authentication.TokenCommand) != 0 && len(cfg.GitHub.Paths) == 0 {
		return errors.New("GitHub authentication needs at least one trusted path")
	}
	if len(cfg.GitHub.Authentication.TokenCommand) > 0 && strings.TrimSpace(cfg.GitHub.Authentication.TokenCommand[0]) == "" {
		return errors.New("github.authentication.tokenCommand must start with a command")
	}
	for _, path := range cfg.GitHub.Paths {
		if strings.TrimSpace(path) == "" || strings.ContainsAny(path, "*?[]") || (!filepath.IsAbs(path) && path != "~" && !strings.HasPrefix(path, "~/")) {
			return fmt.Errorf("github.paths entries must be absolute directory paths without glob patterns: %q", path)
		}
	}
	return nil
}

func ProjectConfigPath(workspace string) string {
	return filepath.Join(workspace, ".devfence.yaml")
}

func LoadProjectConfig(workspace string) (ProjectConfig, string, error) {
	path := ProjectConfigPath(workspace)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return DefaultProjectConfig(), "", nil
	}
	if err != nil {
		return ProjectConfig{}, "", fmt.Errorf("read project config: %w", err)
	}
	var project ProjectConfig
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&project); err != nil {
		return ProjectConfig{}, "", fmt.Errorf("parse project config: %w", err)
	}
	if err := project.Validate(); err != nil {
		return ProjectConfig{}, "", err
	}
	return project, path, nil
}

func (project ProjectConfig) Validate() error {
	if project.Version != 3 {
		return fmt.Errorf("unsupported project config version %d (expected 3)", project.Version)
	}
	if project.Backend != "" {
		switch project.Backend {
		case BackendBubblewrap, BackendDocker, BackendVM:
		default:
			return fmt.Errorf("unsupported backend %q", project.Backend)
		}
	}
	if project.Network != "" && project.Network != "full" && project.Network != "none" {
		return fmt.Errorf("unsupported network mode %q", project.Network)
	}
	if project.Workspace != nil {
		if project.Workspace.Mode != "" && project.Workspace.Mode != "live" && project.Workspace.Mode != "copy" {
			return fmt.Errorf("unsupported workspace mode %q", project.Workspace.Mode)
		}
		for _, pattern := range append(append([]string{}, project.Workspace.Include...), project.Workspace.Exclude...) {
			if err := validatePattern(pattern); err != nil {
				return err
			}
		}
	}
	if err := validateProjectShares(project.Shares); err != nil {
		return err
	}
	return nil
}

func validateProjectShares(shares []ProjectShare) error {
	for index, share := range shares {
		label := fmt.Sprintf("shares[%d]", index)
		if strings.TrimSpace(share.Source) == "" {
			return fmt.Errorf("%s.source is required", label)
		}
		if err := validateForwardedFileTarget(share.Target); err != nil {
			return fmt.Errorf("%s.target: %w", label, err)
		}
		switch share.Mode {
		case "copy":
			if share.Access != "" {
				return fmt.Errorf("%s.access applies only to live mounts", label)
			}
		case "mount":
			if share.Access != "" && share.Access != "read-only" && share.Access != "read-write" {
				return fmt.Errorf("%s.access must be read-only or read-write", label)
			}
			if len(share.Exclude) != 0 {
				return fmt.Errorf("%s.exclude is only supported for copied shares", label)
			}
		default:
			return fmt.Errorf("%s.mode must be copy or mount", label)
		}
		for _, excluded := range share.Exclude {
			if err := validateForwardedExclusion(excluded); err != nil {
				return fmt.Errorf("%s.exclude: %w", label, err)
			}
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
	var forwardedTargets []string
	for name, tool := range map[string]ForwardedToolPolicy{"codex": p.Tools.Codex, "claude": p.Tools.Claude} {
		files := append(append([]ForwardedFile(nil), tool.Authentication...), tool.Configuration...)
		if !tool.Enabled && len(files) > 0 {
			return fmt.Errorf("tools.%s must be enabled to forward authentication or configuration files", name)
		}
		for _, file := range files {
			if strings.TrimSpace(file.Source) == "" {
				return fmt.Errorf("tools.%s forwarded files need a source path", name)
			}
			if err := validateForwardedFileTarget(file.Target); err != nil {
				return fmt.Errorf("tools.%s: %w", name, err)
			}
			forwardedTargets = append(forwardedTargets, file.Target)
		}
	}
	for _, path := range p.Tools.SharedDirectories {
		if strings.TrimSpace(path.Source) == "" {
			return errors.New("tools.sharedDirectories entries need a source path")
		}
		if err := validateForwardedFileTarget(path.Target); err != nil {
			return fmt.Errorf("tools.sharedDirectories: %w", err)
		}
		for _, excluded := range path.Exclude {
			if err := validateForwardedExclusion(excluded); err != nil {
				return fmt.Errorf("tools.sharedDirectories exclusion: %w", err)
			}
		}
		forwardedTargets = append(forwardedTargets, path.Target)
	}
	if err := validateProjectShares(p.ProjectShares); err != nil {
		return err
	}
	for _, share := range p.ProjectShares {
		if p.Backend == BackendDocker && (strings.Contains(share.Source, ",") || strings.Contains(share.Target, ",")) {
			return fmt.Errorf("Docker project share paths cannot contain commas: %q -> %q", share.Source, share.Target)
		}
		if p.Backend == BackendVM && share.Mode == "mount" {
			if strings.Contains(share.Source, ",") {
				return fmt.Errorf("VM virtiofs share source paths cannot contain commas: %q", share.Source)
			}
			if strings.ContainsAny(share.Target, ",:# \t\r\n") {
				return fmt.Errorf("VM live mount targets cannot contain commas, colons, whitespace, or #: %q", share.Target)
			}
		}
		forwardedTargets = append(forwardedTargets, share.Target)
	}
	for _, target := range forwardedTargets {
		clean := filepath.Clean(filepath.FromSlash(target))
		if clean == ".devfence" || within(".devfence", clean) {
			return fmt.Errorf("forwarded tool target %q uses Devfence's reserved .devfence directory", target)
		}
	}
	for i, first := range forwardedTargets {
		first = filepath.Clean(filepath.FromSlash(first))
		for _, second := range forwardedTargets[i+1:] {
			second = filepath.Clean(filepath.FromSlash(second))
			if within(first, second) || within(second, first) {
				return fmt.Errorf("forwarded tool targets overlap: %q and %q", first, second)
			}
		}
	}
	if len(p.Tools.GH.Authentication.TokenCommand) > 0 && strings.TrimSpace(p.Tools.GH.Authentication.TokenCommand[0]) == "" {
		return errors.New("tools.gh.authentication.tokenCommand must start with a command")
	}
	if len(p.Tools.GH.Authentication.TokenCommand) > 0 && !p.Tools.GH.Enabled {
		return errors.New("tools.gh must be enabled to forward GitHub authentication")
	}
	if p.Resources.MemoryMiB < 0 || p.Resources.VCPUs < 0 || p.Resources.DiskGiB < 0 {
		return errors.New("resource limits cannot be negative")
	}
	return nil
}

func validateForwardedFileTarget(target string) error {
	if target == "" || filepath.IsAbs(target) {
		return fmt.Errorf("forwarded file target must be a non-empty relative path: %q", target)
	}
	clean := filepath.Clean(filepath.FromSlash(target))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("forwarded file target escapes the tool's home directory: %q", target)
	}
	return nil
}

func validateForwardedExclusion(excluded string) error {
	if excluded == "" || filepath.IsAbs(excluded) || strings.ContainsAny(excluded, "*?[]") {
		return fmt.Errorf("must be a relative path without glob patterns: %q", excluded)
	}
	clean := filepath.Clean(filepath.FromSlash(excluded))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("must stay inside the forwarded directory: %q", excluded)
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

func Resolve(cfg Config, cwd, backendOverride, workspaceOverride, networkOverride string) (Resolved, error) {
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

	projectConfig, projectConfigPath, err := LoadProjectConfig(workspace)
	if err != nil {
		return Resolved{}, err
	}
	profile := applyProjectConfig(cfg.Defaults, projectConfig)
	profile.ProjectShares, err = resolveProjectShares(projectConfig.Shares, workspace)
	if err != nil {
		return Resolved{}, err
	}
	profileName := "default"
	if projectConfigPath != "" {
		profileName = filepath.Base(workspace)
	}
	profile.Tools.GH, err = githubPolicyForWorkspace(cfg.GitHub, workspace, profile.Tools.GH.Enabled)
	if err != nil {
		return Resolved{}, err
	}
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
	for _, share := range profile.ProjectShares {
		for _, protectedPath := range protected {
			if within(protectedPath, share.Source) || within(share.Source, protectedPath) {
				return Resolved{}, fmt.Errorf("project share source %s overlaps protected path %s", share.Source, protectedPath)
			}
		}
	}
	stateDir, err := StatePath()
	if err != nil {
		return Resolved{}, err
	}
	stateDir, err = canonicalExistingOrClean(stateDir)
	if err != nil {
		return Resolved{}, err
	}
	if within(workspace, stateDir) || within(stateDir, workspace) {
		return Resolved{}, errors.New("workspace and Devfence state directory must not overlap; set XDG_STATE_HOME elsewhere")
	}
	for _, share := range profile.ProjectShares {
		if within(stateDir, share.Source) || within(share.Source, stateDir) {
			return Resolved{}, fmt.Errorf("project share source %s overlaps Devfence state directory %s", share.Source, stateDir)
		}
		if err := validateProjectShareSource(share.Source); err != nil {
			return Resolved{}, err
		}
	}
	return Resolved{
		Config: cfg, ProfileName: profileName, Profile: profile,
		ProjectConfigPath: projectConfigPath, Workspace: workspace,
		WorkingDir: workingDir, Protected: protected, StateDir: stateDir,
	}, nil
}

func applyProjectConfig(profile Profile, project ProjectConfig) Profile {
	if project.Backend != "" {
		profile.Backend = project.Backend
	}
	if project.Network != "" {
		profile.Network = project.Network
	}
	if project.Workspace != nil {
		if project.Workspace.Mode != "" {
			profile.Workspace.Mode = project.Workspace.Mode
		}
		if project.Workspace.Include != nil {
			profile.Workspace.Include = project.Workspace.Include
		}
		if project.Workspace.Exclude != nil {
			profile.Workspace.Exclude = project.Workspace.Exclude
		}
	}
	profile.ProjectShares = append([]ProjectShare(nil), project.Shares...)
	return profile
}

func resolveProjectShares(shares []ProjectShare, workspace string) ([]ProjectShare, error) {
	resolved := append([]ProjectShare(nil), shares...)
	for index := range resolved {
		share := &resolved[index]
		source := share.Source
		if !filepath.IsAbs(source) && source != "~" && !strings.HasPrefix(source, "~/") {
			source = filepath.Join(workspace, source)
		}
		expanded, err := expandPath(source)
		if err != nil {
			return nil, fmt.Errorf("shares[%d].source: %w", index, err)
		}
		canonical, err := filepath.EvalSymlinks(expanded)
		if err != nil {
			return nil, fmt.Errorf("shares[%d].source: %w", index, err)
		}
		info, err := os.Stat(canonical)
		if err != nil {
			return nil, fmt.Errorf("shares[%d].source: %w", index, err)
		}
		if info.IsDir() {
			share.Source = filepath.Clean(canonical)
		} else if info.Mode().IsRegular() {
			if share.Mode == "mount" {
				return nil, fmt.Errorf("shares[%d]: live mounts must refer to directories", index)
			}
			if len(share.Exclude) > 0 {
				return nil, fmt.Errorf("shares[%d].exclude requires a directory source", index)
			}
			share.Source = filepath.Clean(canonical)
		} else {
			return nil, fmt.Errorf("shares[%d].source must be a regular file or directory", index)
		}
		if share.Mode == "mount" && share.Access == "" {
			share.Access = "read-only"
		}
	}
	return resolved, nil
}

func validateProjectShareSource(source string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	blockedRoots := []string{"/", "/home", "/tmp", "/var/tmp", "/run", "/proc", "/sys", "/dev"}
	for _, blocked := range blockedRoots {
		if source == blocked || within(source, blocked) {
			return fmt.Errorf("project share source %s is too broad; choose a specific file or directory", source)
		}
		if blocked == "/run" || blocked == "/proc" || blocked == "/sys" || blocked == "/dev" {
			if within(blocked, source) {
				return fmt.Errorf("project share source %s is under protected system path %s", source, blocked)
			}
		}
	}
	if source == home || within(source, home) {
		return fmt.Errorf("project share source %s is too broad; choose a specific file or directory under the home directory", source)
	}
	return nil
}

func githubPolicyForWorkspace(policy GitHubAccessPolicy, workspace string, cliEnabled bool) (GitHubToolPolicy, error) {
	if len(policy.Authentication.TokenCommand) == 0 {
		return GitHubToolPolicy{Enabled: cliEnabled}, nil
	}
	for _, path := range policy.Paths {
		root, err := expandPath(path)
		if err != nil {
			return GitHubToolPolicy{}, fmt.Errorf("GitHub path %q: %w", path, err)
		}
		root, err = canonicalExistingOrClean(root)
		if err != nil {
			return GitHubToolPolicy{}, fmt.Errorf("GitHub path %q: %w", path, err)
		}
		if within(root, workspace) {
			return GitHubToolPolicy{Enabled: true, Authentication: policy.Authentication}, nil
		}
	}
	return GitHubToolPolicy{Enabled: cliEnabled}, nil
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
