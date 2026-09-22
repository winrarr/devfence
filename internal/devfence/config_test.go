package devfence

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testConfig() Config {
	return Config{
		Version:        1,
		DefaultProfile: "personal",
		Profiles: map[string]Profile{
			"personal":  {Backend: BackendBubblewrap, Network: "full", Workspace: WorkspacePolicy{Mode: "live"}},
			"sensitive": {Backend: BackendVM, Network: "full", Workspace: WorkspacePolicy{Mode: "copy", Include: []string{"src/**", "go.mod"}}},
		},
	}
}

func TestLoadConfigRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\ndefaultProfile: default\nunknown: true\nprofiles: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "field unknown not found") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func TestResolveDirectoryOutsideGitUsesCurrentDirectory(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "sub dir")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	resolved, err := Resolve(testConfig(), child, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Workspace != child || resolved.WorkingDir != child {
		t.Fatalf("resolved workspace=%q cwd=%q; want %q", resolved.Workspace, resolved.WorkingDir, child)
	}
}

func TestResolveUsesFirstMatchingPathRule(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.Rules = []Rule{{Path: root, Match: "subtree", Profile: "sensitive"}}
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	resolved, err := Resolve(cfg, project, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ProfileName != "sensitive" || resolved.Profile.Backend != BackendVM {
		t.Fatalf("selected profile %q (%s), want sensitive (vm)", resolved.ProfileName, resolved.Profile.Backend)
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
	if _, err := Resolve(cfg, project, "", "", "", "personal"); err == nil {
		t.Fatal("live profile should not expose a protected workspace")
	}
	resolved, err := Resolve(cfg, project, "", "", "", "sensitive")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Profile.Workspace.Mode != "copy" {
		t.Fatalf("workspace mode is %q, want copy", resolved.Profile.Workspace.Mode)
	}
}

func TestProtectedChildRejectsLiveAndRequiresCopyAllowlist(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "repo")
	protected := filepath.Join(project, "private")
	if err := os.MkdirAll(protected, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.ProtectedPaths = []string{protected}
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	if _, err := Resolve(cfg, project, "", "", "", "personal"); err == nil {
		t.Fatal("live workspace containing protected child should fail")
	}
	cfg.Profiles["sensitive"] = Profile{Backend: BackendVM, Network: "full", Workspace: WorkspacePolicy{Mode: "copy"}}
	if _, err := Resolve(cfg, project, "", "", "", "sensitive"); err == nil {
		t.Fatal("protected source requires explicit include patterns")
	}
}

func TestResolveRejectsUnsupportedVMNetwork(t *testing.T) {
	cfg := testConfig()
	if err := cfg.Profiles["sensitive"].Validate(); err != nil {
		t.Fatal(err)
	}
	profile := Profile{Backend: BackendVM, Network: "none", Workspace: WorkspacePolicy{Mode: "live"}}
	if err := profile.Validate(); err == nil || !strings.Contains(err.Error(), "VM backend needs network") {
		t.Fatalf("expected VM network error, got %v", err)
	}
}
