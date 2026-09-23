package devfence

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testConfig() Config {
	return DefaultConfig()
}

func TestDefaultConfigContainsNoPersonalToolForwarding(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Defaults.Tools.Codex.Enabled || cfg.Defaults.Tools.Claude.Enabled || cfg.Defaults.Tools.GH.Enabled {
		t.Fatal("generic defaults must not enable personal tools")
	}
	if len(cfg.Defaults.Tools.Codex.Authentication) != 0 || len(cfg.Defaults.Tools.Codex.Configuration) != 0 || len(cfg.Defaults.Tools.Claude.Authentication) != 0 || len(cfg.Defaults.Tools.Claude.Configuration) != 0 || len(cfg.Defaults.Tools.GH.Authentication.TokenCommand) != 0 || len(cfg.Defaults.Tools.SharedDirectories) != 0 || len(cfg.GitHub.Paths) != 0 {
		t.Fatal("generic defaults must not contain personal tool paths or credentials")
	}
}

func TestDefaultAndExampleConfigurationValidate(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("generated default configuration: %v", err)
	}
	if _, err := LoadConfig(filepath.Join("..", "..", "config.example.yaml")); err != nil {
		t.Fatalf("example configuration: %v", err)
	}
}

func TestResolveDoesNotImplicitlyEnableOmittedTools(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	resolved, err := Resolve(testConfig(), root, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Profile.Tools.Codex.Enabled || resolved.Profile.Tools.Claude.Enabled {
		t.Fatal("omitted tool policies must remain disabled")
	}
}

func TestForwardedFilesRequireEnabledToolAndSafeTarget(t *testing.T) {
	profile := Profile{Backend: BackendBubblewrap, Network: "full", Workspace: WorkspacePolicy{Mode: "live"}}
	profile.Tools.Codex.Authentication = []ForwardedFile{{Source: "~/.codex/auth.json", Target: ".codex/auth.json"}}
	if err := profile.Validate(); err == nil {
		t.Fatal("authentication files for a disabled tool were accepted")
	}
	profile.Tools.Codex.Enabled = true
	profile.Tools.Codex.Authentication[0].Target = "../outside.json"
	if err := profile.Validate(); err == nil {
		t.Fatal("forwarded target escaping HOME was accepted")
	}
}

func TestForwardedTargetsCannotUseDevfenceRuntimeDirectory(t *testing.T) {
	profile := Profile{
		Backend: BackendDocker, Network: "full", Workspace: WorkspacePolicy{Mode: "live"},
		Tools: ToolPolicy{SharedDirectories: []ForwardedPath{{Source: "~/agents", Target: ".devfence/tools"}}},
	}
	if err := profile.Validate(); err == nil {
		t.Fatal("forwarded directory overlapping Devfence's host tool runtime was accepted")
	}
}

func TestForwardedDirectoryExclusionsMustStayRelative(t *testing.T) {
	profile := Profile{
		Backend: BackendDocker, Network: "full", Workspace: WorkspacePolicy{Mode: "live"},
		Tools: ToolPolicy{SharedDirectories: []ForwardedPath{{Source: "~/agents", Target: "agents", Exclude: []string{"../.env"}}}},
	}
	if err := profile.Validate(); err == nil {
		t.Fatal("forwarded directory exclusion escaping its source was accepted")
	}
}

func TestLoadConfigRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 3\ndefaults: {}\nunknown: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "field unknown not found") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func TestProjectConfigOverlaysRuntimeDefaults(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(project, "agent-guidance", ".env"), 0700); err != nil {
		t.Fatal(err)
	}
	projectPath := ProjectConfigPath(project)
	contents := "version: 3\nbackend: vm\nworkspace:\n  mode: copy\n  include: [src/**, go.mod]\nshares:\n  - source: agent-guidance\n    target: agents\n    mode: copy\n    exclude: [.env]\n"
	if err := os.WriteFile(projectPath, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	resolved, err := Resolve(testConfig(), project, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ProjectConfigPath != projectPath || resolved.Profile.Backend != BackendVM || resolved.Profile.Workspace.Mode != "copy" || len(resolved.Profile.Workspace.Include) != 2 || len(resolved.Profile.ProjectShares) != 1 {
		t.Fatalf("project config was not applied: %#v", resolved)
	}
	if resolved.Profile.ProjectShares[0].Source != filepath.Join(project, "agent-guidance") || resolved.Profile.ProjectShares[0].Target != "agents" || resolved.Profile.ProjectShares[0].Mode != "copy" || len(resolved.Profile.ProjectShares[0].Exclude) != 1 || resolved.Profile.ProjectShares[0].Exclude[0] != ".env" {
		t.Fatalf("project share was not resolved from the project directory: %#v", resolved.Profile.ProjectShares)
	}
}

func TestProjectShareValidationRejectsUnsafeModeAccessAndTargets(t *testing.T) {
	for _, share := range []ProjectShare{
		{Source: "settings", Target: "settings", Mode: "link"},
		{Source: "settings", Target: "settings", Mode: "copy", Access: "read-write"},
		{Source: "settings", Target: "settings", Mode: "copy", Access: "read-only"},
		{Source: "settings", Target: "settings", Mode: "mount", Access: "write"},
		{Source: "settings", Target: "../outside", Mode: "copy"},
		{Source: "settings", Target: "agents", Mode: "mount", Exclude: []string{".env"}},
		{Source: "settings", Target: "agents", Mode: "copy", Exclude: []string{"../.env"}},
	} {
		if err := validateProjectShares([]ProjectShare{share}); err == nil {
			t.Errorf("unsafe project share was accepted: %#v", share)
		}
	}
}

func TestProjectShareMountRequiresDirectoryAndCopiesAllowFileSources(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(project, "settings.json")
	if err := os.WriteFile(file, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	if err := os.WriteFile(ProjectConfigPath(project), []byte("version: 3\nshares:\n  - source: settings.json\n    target: settings.json\n    mode: mount\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(testConfig(), project, "", "", ""); err == nil || !strings.Contains(err.Error(), "live mounts must refer to directories") {
		t.Fatalf("file mount was accepted: %v", err)
	}
	if err := os.WriteFile(ProjectConfigPath(project), []byte("version: 3\nshares:\n  - source: settings.json\n    target: settings.json\n    mode: copy\n"), 0600); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(testConfig(), project, "", "", "")
	if err != nil || len(resolved.Profile.ProjectShares) != 1 || resolved.Profile.ProjectShares[0].Source != file {
		t.Fatalf("copy of a specific file was not accepted: share=%#v err=%v", resolved.Profile.ProjectShares, err)
	}
}

func TestProjectShareTargetsCannotOverlapOtherForwardedTargets(t *testing.T) {
	profile := Profile{
		Backend: BackendDocker, Network: "full", Workspace: WorkspacePolicy{Mode: "live"},
		Tools:         ToolPolicy{SharedDirectories: []ForwardedPath{{Source: "~/agents", Target: "agents"}}},
		ProjectShares: []ProjectShare{{Source: "/tmp/project-settings", Target: "agents/config", Mode: "copy"}},
	}
	if err := profile.Validate(); err == nil {
		t.Fatal("project share target nested under a shared directory was accepted")
	}
	profile.Tools.SharedDirectories = nil
	profile.ProjectShares[0].Target = ".devfence/tools"
	if err := profile.Validate(); err == nil {
		t.Fatal("project share target overlapping Devfence's reserved directory was accepted")
	}
}

func TestProjectSharePathsRejectBackendIncompatibleNames(t *testing.T) {
	profile := Profile{
		Backend: BackendDocker, Network: "full", Workspace: WorkspacePolicy{Mode: "live"},
		ProjectShares: []ProjectShare{{Source: "/tmp/share,part", Target: "shared", Mode: "mount"}},
	}
	if err := profile.Validate(); err == nil || !strings.Contains(err.Error(), "cannot contain commas") {
		t.Fatalf("Docker share path containing comma was accepted: %v", err)
	}
	profile.Backend = BackendVM
	profile.ProjectShares[0].Source = "/tmp/share"
	profile.ProjectShares[0].Target = "shared files"
	if err := profile.Validate(); err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Fatalf("VM mount target with whitespace was accepted: %v", err)
	}
	profile.ProjectShares[0].Target = "shared"
	profile.ProjectShares[0].Source = "/tmp/share,part"
	if err := profile.Validate(); err == nil || !strings.Contains(err.Error(), "source paths cannot contain commas") {
		t.Fatalf("VM mount source with comma was accepted: %v", err)
	}
}

func TestProjectShareCannotExposeStateOrBroadSystemDirectories(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	stateHome := filepath.Join(root, "state")
	state := filepath.Join(stateHome, "devfence")
	source := filepath.Join(state, "settings")
	for _, path := range []string{project, source} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(ProjectConfigPath(project), []byte("version: 3\nshares:\n  - source: "+source+"\n    target: settings\n    mode: copy\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateHome)
	if _, err := Resolve(testConfig(), project, "", "", ""); err == nil || !strings.Contains(err.Error(), "overlaps Devfence state directory") {
		t.Fatalf("project share overlapping Devfence state was accepted: %v", err)
	}
	if err := os.WriteFile(ProjectConfigPath(project), []byte("version: 3\nshares:\n  - source: /\n    target: root-copy\n    mode: copy\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateProjectShareSource("/"); err == nil || !strings.Contains(err.Error(), "too broad") {
		t.Fatalf("root directory share was accepted: %v", err)
	}
}

func TestProjectConfigCannotSetHostToolsOrCredentials(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	for _, contents := range []string{
		"version: 3\ntools: {}\n",
		"version: 3\ngithub:\n  authentication:\n    tokenCommand: [gh, auth, token]\n",
	} {
		if err := os.WriteFile(ProjectConfigPath(project), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadProjectConfig(project); err == nil {
			t.Fatalf("project config containing host-only policy was accepted: %s", contents)
		}
	}
}

func TestGitHubAuthenticationIsScopedToConfiguredWorkspaceTree(t *testing.T) {
	root := t.TempDir()
	trusted := filepath.Join(root, "Documents", "dev")
	inside := filepath.Join(trusted, "nested", "repo")
	outside := filepath.Join(root, "Documents", "personal")
	for _, path := range []string{inside, outside} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := DefaultConfig()
	cfg.GitHub = GitHubAccessPolicy{
		Paths:          []string{trusted},
		Authentication: GitHubAuthenticationPolicy{TokenCommand: []string{"gh", "auth", "token"}},
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	for _, tc := range []struct {
		path string
		want bool
	}{{inside, true}, {outside, false}} {
		resolved, err := Resolve(cfg, tc.path, "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		got := len(resolved.Profile.Tools.GH.Authentication.TokenCommand) > 0
		if got != tc.want {
			t.Errorf("GitHub token available at %s = %t, want %t", tc.path, got, tc.want)
		}
	}
}

func TestResolveDirectoryOutsideGitUsesCurrentDirectory(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "sub dir")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	resolved, err := Resolve(testConfig(), child, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Workspace != child || resolved.WorkingDir != child {
		t.Fatalf("resolved workspace=%q cwd=%q; want %q", resolved.Workspace, resolved.WorkingDir, child)
	}
}

func TestProtectedWorkspaceRequiresFilteredCopy(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "repo")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.ProtectedPaths = []string{root}
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	if _, err := Resolve(cfg, project, "", "", ""); err == nil {
		t.Fatal("live defaults should not expose a protected workspace")
	}
	cfg.Defaults.Workspace = WorkspacePolicy{Mode: "copy", Include: []string{"src/**"}}
	resolved, err := Resolve(cfg, project, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Profile.Workspace.Mode != "copy" {
		t.Fatalf("workspace mode is %q, want copy", resolved.Profile.Workspace.Mode)
	}
}

func TestProtectedChildRequiresCopyAllowlist(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "repo")
	protected := filepath.Join(project, "private")
	if err := os.MkdirAll(protected, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.ProtectedPaths = []string{protected}
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	if _, err := Resolve(cfg, project, "", "", ""); err == nil {
		t.Fatal("live workspace containing protected child should fail")
	}
	cfg.Defaults.Workspace = WorkspacePolicy{Mode: "copy"}
	if _, err := Resolve(cfg, project, "", "", ""); err == nil {
		t.Fatal("protected source requires explicit include patterns")
	}
}

func TestResolveRejectsUnsupportedVMNetwork(t *testing.T) {
	profile := Profile{Backend: BackendVM, Network: "none", Workspace: WorkspacePolicy{Mode: "live"}}
	if err := profile.Validate(); err == nil || !strings.Contains(err.Error(), "VM backend needs network") {
		t.Fatalf("expected VM network error, got %v", err)
	}
}
