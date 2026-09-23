package devfence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"gopkg.in/yaml.v3"
)

func TestBubblewrapClearsHostEnvironmentAndIsolatesHomeAndNetwork(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "project")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	resolved := Resolved{Profile: Profile{Network: "none"}}
	session := &Session{
		SourceWorkspace:    workspace,
		PresentedWorkspace: workspace,
		WorkingDir:         workspace,
		WorkspaceMode:      "live",
		HomeDir:            filepath.Join(root, "session-home"),
	}
	args, err := bubblewrapArgs(session, resolved, []string{"codex"}, nil, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--clearenv", "--tmpfs /home", "--tmpfs /run", "--tmpfs /tmp", "--setenv HOME /home/devfence"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Bubblewrap arguments do not include %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "--share-net") {
		t.Fatal("network:none must not share the host network namespace")
	}
	if !strings.Contains(joined, "-- codex") {
		t.Fatalf("command argv was not preserved: %s", joined)
	}
}

func TestBubblewrapFullNetworkIsExplicit(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "project")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	args, err := bubblewrapArgs(&Session{
		SourceWorkspace: workspace, PresentedWorkspace: workspace,
		WorkingDir: workspace, WorkspaceMode: "live", HomeDir: filepath.Join(root, "home"),
	}, Resolved{Profile: Profile{Network: "full"}}, []string{"sh"}, nil, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !containsArg(args, "--share-net") {
		t.Fatal("network:full must explicitly share the host network namespace")
	}
}

func TestBubblewrapCopiesSystemdResolverIntoPrivateRun(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "project")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	target := "/run/systemd/resolve/stub-resolv.conf"
	args, err := bubblewrapArgs(&Session{
		SourceWorkspace: workspace, PresentedWorkspace: workspace,
		WorkingDir: workspace, WorkspaceMode: "live", HomeDir: filepath.Join(root, "home"),
	}, Resolved{Profile: Profile{Network: "full"}}, []string{"sh"}, nil, target, 4)
	if err != nil {
		t.Fatal(err)
	}
	tmpfsIndex := -1
	fileIndex := -1
	for index := range args {
		if index+1 < len(args) && args[index] == "--tmpfs" && args[index+1] == "/run" {
			tmpfsIndex = index
		}
		if index+2 < len(args) && args[index] == "--file" && args[index+1] == "4" && args[index+2] == target {
			fileIndex = index
		}
	}
	if tmpfsIndex < 0 || fileIndex <= tmpfsIndex {
		t.Fatalf("resolver file must be copied into private /run after it is created: %v", args)
	}
	if hasTriple(args, "--ro-bind", target, target) {
		t.Fatalf("host resolver path must not be looked up after /run is hidden: %v", args)
	}
}

func TestBubblewrapMountsConfiguredHostCodexReadOnly(t *testing.T) {
	bin := t.TempDir()
	release := filepath.Join(bin, "releases", "codex-version")
	if err := os.MkdirAll(filepath.Join(release, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(release, "codex-resources"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "codex-package.json"), []byte(`{"name":"codex"}`), 0600); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(release, "bin", "codex"), "#!/bin/sh\nexit 0\n")
	codexPath := filepath.Join(bin, "codex")
	if err := os.Symlink(filepath.Join(release, "bin", "codex"), codexPath); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	root := t.TempDir()
	workspace := filepath.Join(root, "project")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	install, err := hostToolInstallation("codex", true)
	if err != nil {
		t.Fatal(err)
	}
	args, err := bubblewrapArgs(&Session{
		SourceWorkspace: workspace, PresentedWorkspace: workspace,
		WorkingDir: workspace, WorkspaceMode: "live", HomeDir: filepath.Join(root, "home"),
	}, Resolved{Profile: Profile{Network: "none", Tools: ToolPolicy{Codex: ForwardedToolPolicy{Enabled: true}}}}, []string{"codex"}, []*hostToolInstall{install}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTriple(args, "--ro-bind", release, "/opt/devfence/codex") {
		t.Fatalf("the full host Codex release was not mounted read-only: %v", args)
	}
	if !hasTriple(args, "--symlink", "/opt/devfence/codex/bin/codex", "/opt/devfence/bin/codex") {
		t.Fatalf("the Codex command was not linked into the sandbox tool path: %v", args)
	}
}

func TestBubblewrapMountsHostNodeCodexPackageAndRuntime(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	packageRoot := filepath.Join(root, "node_modules", "@openai", "codex")
	if err := os.MkdirAll(filepath.Join(packageRoot, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "package.json"), []byte(`{"name":"@openai/codex","dependencies":{"@openai/codex-linux-x64":"1.0.0"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(packageRoot, "bin", "codex.js"), "#!/usr/bin/env node\n")
	dependencyRoot := filepath.Join(root, "node_modules", "@openai", "codex-linux-x64")
	if err := os.MkdirAll(dependencyRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dependencyRoot, "package.json"), []byte(`{"name":"@openai/codex-linux-x64"}`), 0600); err != nil {
		t.Fatal(err)
	}
	unrelatedRoot := filepath.Join(root, "node_modules", "unrelated-tool")
	if err := os.MkdirAll(unrelatedRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unrelatedRoot, "package.json"), []byte(`{"name":"unrelated-tool"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	codexPath := filepath.Join(bin, "codex")
	if err := os.Symlink(filepath.Join(packageRoot, "bin", "codex.js"), codexPath); err != nil {
		t.Fatal(err)
	}
	nodePath := filepath.Join(bin, "node")
	writeExecutable(t, nodePath, "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", bin)

	install, err := hostToolInstallation("codex", true)
	if err != nil {
		t.Fatal(err)
	}
	if install == nil || install.source != packageRoot || install.nodeRuntime != nodePath || install.entryPoint != filepath.Join("bin", "codex.js") {
		t.Fatalf("resolved host Node package = %+v", install)
	}
	workspace := filepath.Join(root, "project")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	args, err := bubblewrapArgs(&Session{
		SourceWorkspace: workspace, PresentedWorkspace: workspace,
		WorkingDir: workspace, WorkspaceMode: "live", HomeDir: filepath.Join(root, "home"),
	}, Resolved{Profile: Profile{Network: "none", Tools: ToolPolicy{Codex: ForwardedToolPolicy{Enabled: true}}}}, []string{"codex"}, []*hostToolInstall{install}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTriple(args, "--ro-bind", packageRoot, "/opt/devfence/codex") {
		t.Fatalf("Node package directory was not mounted read-only: %v", args)
	}
	if !hasTriple(args, "--ro-bind", nodePath, "/opt/devfence/node/bin/node") {
		t.Fatalf("host Node runtime was not mounted read-only: %v", args)
	}
	if !hasTriple(args, "--ro-bind", dependencyRoot, "/opt/devfence/node_modules/@openai/codex-linux-x64") {
		t.Fatalf("declared Codex Node dependency was not mounted: %v", args)
	}
	if strings.Contains(strings.Join(args, "\x00"), unrelatedRoot) {
		t.Fatalf("unrelated host Node package was mounted: %v", args)
	}
	if !strings.Contains(strings.Join(args, "\x00"), "/opt/devfence/node/bin:/opt/devfence/bin:") {
		t.Fatalf("host Node runtime and Codex are not in PATH: %v", args)
	}
	if !hasTriple(args, "--setenv", "NODE_PATH", "/opt/devfence/node_modules") {
		t.Fatalf("Codex dependency directory is not in NODE_PATH: %v", args)
	}
}

func TestStartedCodexSeesGlobalInstructionsInsideBubblewrap(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("Bubblewrap is not installed")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("Codex CLI is not installed")
	}
	if err := execCommand("codex", "debug", "prompt-input", "Report the global marker.").Run(); err != nil {
		t.Skipf("installed Codex CLI does not support debug prompt rendering: %v", err)
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "project")
	agentSource := filepath.Join(root, "agents")
	if err := os.MkdirAll(filepath.Join(workspace), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(agentSource, ".env"), 0700); err != nil {
		t.Fatal(err)
	}
	instructions := filepath.Join(root, "global-instructions.md")
	marker := "DEVFENCE_GLOBAL_INSTRUCTIONS_83e14a6c"
	secretMarker := "DEVFENCE_EXCLUDED_SECRET_70f91d2b"
	if err := os.WriteFile(instructions, []byte("Always include this marker in your answer: "+marker), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentSource, ".env", "secret"), []byte(secretMarker), 0600); err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(root, "session")
	if err := os.Mkdir(sessionDir, 0700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(sessionDir, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(home, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			mode := os.FileMode(0600)
			if entry.IsDir() {
				mode = 0700
			}
			_ = os.Chmod(path, mode)
			return nil
		})
	})
	profile := Profile{Backend: BackendBubblewrap, Network: "none", Workspace: WorkspacePolicy{Mode: "live"}, Tools: ToolPolicy{
		Codex:             ForwardedToolPolicy{Enabled: true, Configuration: []ForwardedFile{{Source: instructions, Target: ".codex/AGENTS.md"}}},
		SharedDirectories: []ForwardedPath{{Source: agentSource, Target: "agents", Exclude: []string{".env"}}},
	}}
	if err := prepareForwardedTools(&Session{Backend: BackendBubblewrap, HomeDir: home}, profile); err != nil {
		t.Fatal(err)
	}
	install, err := hostToolInstallation("codex", true)
	if err != nil || install == nil {
		t.Fatalf("locate host Codex CLI: install=%#v err=%v", install, err)
	}
	args, err := bubblewrapArgs(&Session{
		SourceWorkspace: workspace, PresentedWorkspace: workspace, WorkingDir: workspace,
		WorkspaceMode: "live", HomeDir: home, SessionDir: sessionDir,
	}, Resolved{Workspace: workspace, Profile: profile}, []string{"codex", "debug", "prompt-input", "Report the global marker."}, []*hostToolInstall{install}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	command := execCommand("bwrap", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		if strings.Contains(strings.ToLower(string(output)), "operation not permitted") || strings.Contains(strings.ToLower(string(output)), "namespace") {
			t.Skipf("Bubblewrap namespaces are unavailable in this environment: %s", output)
		}
		t.Fatalf("run Codex prompt inspection in Bubblewrap: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte(marker)) {
		t.Fatalf("started Codex did not receive the global instructions marker:\n%s", output)
	}
	if bytes.Contains(output, []byte(secretMarker)) {
		t.Fatalf("excluded .env content reached Codex's prompt input:\n%s", output)
	}
}

func TestProjectShareSnapshotIsStagedForVMWithoutHostCLIInstallation(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "agent-guidance")
	if err := os.MkdirAll(filepath.Join(source, ".env"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "AGENTS.md"), []byte("follow global instructions"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".env", "secret"), []byte("hidden"), 0600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "session", "home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	profile := Profile{ProjectShares: []ProjectShare{{Source: source, Target: "agents", Mode: "copy", Exclude: []string{".env"}}}}
	if err := prepareForwardedTools(&Session{Backend: BackendVM, HomeDir: home}, profile); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(home, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if entry.IsDir() {
				_ = os.Chmod(path, 0700)
			} else {
				_ = os.Chmod(path, 0600)
			}
			return nil
		})
	})
	if _, err := os.Stat(filepath.Join(home, ".devfence")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("host CLI installation was copied into VM session: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "agents", ".env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("excluded .env directory was staged: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(home, "agents", "AGENTS.md")); err != nil || string(data) != "follow global instructions" {
		t.Fatalf("global instructions were not staged: %q, %v", data, err)
	}
	entries, err := forwardedSyncEntries(home, profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].target != "agents" || !entries[0].directory || !entries[0].readOnly || entries[1].target != "agents/AGENTS.md" || !entries[1].readOnly {
		t.Fatalf("VM sync entries do not include the read-only project copy: %#v", entries)
	}
}

func TestVMSyncIncludesCopiedProjectFileAsReadOnly(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "settings.json"), []byte("snapshot"), 0400); err != nil {
		t.Fatal(err)
	}
	profile := Profile{ProjectShares: []ProjectShare{{Source: "/host/settings.json", Target: "settings.json", Mode: "copy"}}}
	entries, err := forwardedSyncEntries(home, profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].target != "settings.json" || entries[0].directory || !entries[0].readOnly {
		t.Fatalf("copied project file sync entry = %#v", entries)
	}
}

func TestGlobalCodexInstructionsAndFilteredAgentDirectoryReachEveryBackendHome(t *testing.T) {
	root := t.TempDir()
	instructions := filepath.Join(root, "global-instructions.md")
	agents := filepath.Join(root, "agents")
	if err := os.MkdirAll(filepath.Join(agents, ".env"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(instructions, []byte("global instruction marker"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agents, "AGENTS.md"), []byte("shared agents"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agents, ".env", "secret"), []byte("host only"), 0600); err != nil {
		t.Fatal(err)
	}
	profile := Profile{Tools: ToolPolicy{
		Codex:             ForwardedToolPolicy{Enabled: true, Configuration: []ForwardedFile{{Source: instructions, Target: ".codex/AGENTS.md"}}},
		SharedDirectories: []ForwardedPath{{Source: agents, Target: "agents", Exclude: []string{".env"}}},
	}}
	for _, backend := range []Backend{BackendBubblewrap, BackendDocker, BackendVM} {
		t.Run(string(backend), func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "home")
			if err := os.Mkdir(home, 0700); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = filepath.WalkDir(home, func(path string, entry os.DirEntry, walkErr error) error {
					if walkErr != nil {
						return nil
					}
					mode := os.FileMode(0600)
					if entry.IsDir() {
						mode = 0700
					}
					_ = os.Chmod(path, mode)
					return nil
				})
			})
			if err := prepareForwardedTools(&Session{Backend: backend, HomeDir: home}, profile); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(filepath.Join(home, ".codex", "AGENTS.md")); err != nil || string(data) != "global instruction marker" {
				t.Fatalf("Codex discovery file missing from isolated HOME: %q, err=%v", data, err)
			}
			if _, err := os.Stat(filepath.Join(home, "agents", ".env")); !os.IsNotExist(err) {
				t.Fatalf("host-only .env directory reached isolated HOME: %v", err)
			}
			if backend == BackendVM {
				entries, err := forwardedSyncEntries(home, profile)
				if err != nil {
					t.Fatal(err)
				}
				foundInstructions, foundAgents := false, false
				for _, entry := range entries {
					foundInstructions = foundInstructions || entry.target == ".codex/AGENTS.md"
					foundAgents = foundAgents || entry.target == "agents/AGENTS.md"
					if strings.Contains(entry.target, ".env") {
						t.Fatalf("VM sync included excluded path %s", entry.target)
					}
				}
				if !foundInstructions || !foundAgents {
					t.Fatalf("VM sync omitted Codex/global files: %#v", entries)
				}
			}
		})
	}
}

func TestNodeCodexDependencyConflictsFailClosed(t *testing.T) {
	root := t.TempDir()
	packageRoot := filepath.Join(root, "node_modules", "@openai", "codex")
	alternateRoot := filepath.Join(root, "alternate", "node_modules")
	packages := map[string]string{
		packageRoot: `{"name":"@openai/codex","dependencies":{"bar":"1","foo":"1"}}`,
		filepath.Join(root, "node_modules", "bar"):    `{"name":"bar","dependencies":{"shared":"1"}}`,
		filepath.Join(root, "node_modules", "shared"): `{"name":"shared","version":"1"}`,
		filepath.Join(alternateRoot, "foo"):           `{"name":"foo","dependencies":{"shared":"2"}}`,
		filepath.Join(alternateRoot, "shared"):        `{"name":"shared","version":"2"}`,
	}
	for path, manifest := range packages {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "package.json"), []byte(manifest), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "node_modules"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(alternateRoot, "foo"), filepath.Join(root, "node_modules", "foo")); err != nil {
		t.Fatal(err)
	}
	if _, err := nodePackageDependencyClosure(packageRoot); err == nil || !strings.Contains(err.Error(), "conflicting installed versions") {
		t.Fatalf("dependency closure error = %v, want conflicting installed versions", err)
	}
}

func hasTriple(args []string, first, second, third string) bool {
	for index := 0; index+2 < len(args); index++ {
		if args[index] == first && args[index+1] == second && args[index+2] == third {
			return true
		}
	}
	return false
}

func TestForwardedCodexAuthenticationIsCopiedIntoSessionHome(t *testing.T) {
	root := t.TempDir()
	hostCodexHome := filepath.Join(root, "host-codex")
	if err := os.Mkdir(hostCodexHome, 0700); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(hostCodexHome, "auth.json")
	if err := os.WriteFile(authPath, []byte("secret-auth-value"), 0600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "session-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	policy := forwardedFilePolicy{auth: true, file: ForwardedFile{Source: authPath, Target: ".codex/auth.json"}}
	if err := stageForwardedFile(home, policy); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(home, ".codex", "auth.json")
	data, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "secret-auth-value" {
		t.Fatal("forwarded Codex authentication copy has different contents")
	}
	info, err := os.Stat(copyPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("forwarded Codex auth mode = %v, err=%v", info, err)
	}
}

func TestCodexAuthRejectsGroupReadableFile(t *testing.T) {
	root := t.TempDir()
	authPath := filepath.Join(root, "auth.json")
	if err := os.WriteFile(authPath, []byte("secret"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(authPath, 0640); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := stageForwardedFile(home, forwardedFilePolicy{auth: true, file: ForwardedFile{Source: authPath, Target: "auth.json"}}); err == nil {
		t.Fatal("group-readable Codex auth file was accepted")
	}
}

func TestForwardedDirectorySnapshotIsReadOnly(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "agents")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "reviewer.md"), []byte("agent"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("reviewer.md", filepath.Join(source, "reviewer-link.md")); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "session-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := stageForwardedDirectory(home, ForwardedPath{Source: source, Target: "agents"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(home, "agents"), 0700) })
	for _, name := range []string{"reviewer.md", "reviewer-link.md"} {
		path := filepath.Join(home, "agents", name)
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "agent" {
			t.Fatalf("forwarded %s content=%q err=%v", name, data, err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&0222 != 0 {
			t.Fatalf("forwarded %s should be read-only: info=%v err=%v", name, info, err)
		}
	}
	info, err := os.Stat(filepath.Join(home, "agents"))
	if err != nil || info.Mode().Perm()&0222 != 0 {
		t.Fatalf("forwarded agent directory should be read-only: info=%v err=%v", info, err)
	}
}

func TestForwardedDirectoryExcludesSensitiveSubtreeAndKeepsInstructions(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "agents")
	secretDir := filepath.Join(source, ".env")
	if err := os.MkdirAll(secretDir, 0700); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		filepath.Join(source, "AGENTS.md"):      "follow these instructions",
		filepath.Join(secretDir, "credentials"): "private value",
	} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	home := filepath.Join(root, "session-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	policy := ForwardedPath{Source: source, Target: "agents", Exclude: []string{".env"}}
	if err := stageForwardedDirectory(home, policy); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(home, "agents"), 0700) })
	if data, err := os.ReadFile(filepath.Join(home, "agents", "AGENTS.md")); err != nil || string(data) != "follow these instructions" {
		t.Fatalf("global instructions were not forwarded: content=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(home, "agents", ".env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("excluded .env directory was forwarded: err=%v", err)
	}
}

func TestForwardedDirectoryRejectsSymlinkEscapingSource(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "agents")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.md")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "outside.md")); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "session-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := stageForwardedDirectory(home, ForwardedPath{Source: source, Target: "agents"}); err == nil {
		t.Fatal("forwarding a symlink outside the source directory was accepted")
	}
}

func TestProjectCopySnapshotsFileAndDirectoryAndMountLeavesOnlyMountPoint(t *testing.T) {
	root := t.TempDir()
	fileSource := filepath.Join(root, "config.json")
	if err := os.WriteFile(fileSource, []byte("host value"), 0700); err != nil {
		t.Fatal(err)
	}
	directorySource := filepath.Join(root, "agents")
	if err := os.MkdirAll(filepath.Join(directorySource, ".env"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directorySource, "AGENTS.md"), []byte("instructions"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directorySource, ".env", "secret"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	mountSource := filepath.Join(root, "mount-source")
	if err := os.Mkdir(mountSource, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mountSource, "sentinel"), []byte("host"), 0600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "session-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	profile := Profile{ProjectShares: []ProjectShare{
		{Source: fileSource, Target: "config/settings.json", Mode: "copy"},
		{Source: directorySource, Target: "agents", Mode: "copy", Exclude: []string{".env"}},
		{Source: mountSource, Target: "live", Mode: "mount", Access: "read-write"},
	}}
	if err := prepareForwardedTools(&Session{Backend: BackendBubblewrap, HomeDir: home}, profile); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(home, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			mode := os.FileMode(0600)
			if entry.IsDir() {
				mode = 0700
			}
			_ = os.Chmod(path, mode)
			return nil
		})
	})
	copyFile := filepath.Join(home, "config", "settings.json")
	if data, err := os.ReadFile(copyFile); err != nil || string(data) != "host value" {
		t.Fatalf("copy share file contents = %q, err=%v", data, err)
	}
	if info, err := os.Stat(copyFile); err != nil || info.Mode().Perm()&0222 != 0 || info.Mode().Perm()&0111 == 0 {
		t.Fatalf("copied file mode = %v, err=%v", info, err)
	}
	if data, err := os.ReadFile(filepath.Join(home, "agents", "AGENTS.md")); err != nil || string(data) != "instructions" {
		t.Fatalf("copy share directory contents = %q, err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(home, "agents", ".env")); !os.IsNotExist(err) {
		t.Fatalf("excluded .env directory exists in copy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "live", "sentinel")); !os.IsNotExist(err) {
		t.Fatalf("live mount contents were copied into its mount point: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mountSource, "sentinel")); err != nil {
		t.Fatalf("live mount source was modified: %v", err)
	}
}

func TestBubblewrapProjectSharesUseRequestedReadAndWriteModes(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "project")
	home := filepath.Join(root, "home")
	copySource := filepath.Join(root, "copied")
	readOnlySource := filepath.Join(root, "readonly")
	readWriteSource := filepath.Join(root, "readwrite")
	for _, path := range []string{workspace, home, copySource, readOnlySource, readWriteSource} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []string{"snapshot", "live-ro", "live-rw"} {
		if err := os.Mkdir(filepath.Join(home, target), 0700); err != nil {
			t.Fatal(err)
		}
	}
	shares := []ProjectShare{
		{Source: copySource, Target: "snapshot", Mode: "copy"},
		{Source: readOnlySource, Target: "live-ro", Mode: "mount"},
		{Source: readWriteSource, Target: "live-rw", Mode: "mount", Access: "read-write"},
	}
	args, err := bubblewrapArgs(&Session{
		SourceWorkspace: workspace, PresentedWorkspace: workspace, WorkingDir: workspace,
		WorkspaceMode: "live", HomeDir: home,
	}, Resolved{Profile: Profile{Network: "none", ProjectShares: shares}}, []string{"sh"}, nil, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTriple(args, "--ro-bind", filepath.Join(home, "snapshot"), "/home/devfence/snapshot") {
		t.Fatalf("copied share is not read-only in Bubblewrap: %v", args)
	}
	if !hasTriple(args, "--ro-bind", readOnlySource, "/home/devfence/live-ro") {
		t.Fatalf("read-only live mount is not read-only in Bubblewrap: %v", args)
	}
	if !hasTriple(args, "--bind", readWriteSource, "/home/devfence/live-rw") {
		t.Fatalf("read-write live mount is not writable in Bubblewrap: %v", args)
	}
}

func TestBubblewrapMountsSharedAgentDirectoryReadOnlyAndMasksImConfigHook(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "project")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "session-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	agents := filepath.Join(home, "agents")
	if err := os.Mkdir(agents, 0500); err != nil {
		t.Fatal(err)
	}
	profile := Profile{Network: "none", Tools: ToolPolicy{SharedDirectories: []ForwardedPath{{Source: "~/agents", Target: "agents"}}}}
	args, err := bubblewrapArgs(&Session{
		SourceWorkspace: workspace, PresentedWorkspace: workspace,
		WorkingDir: workspace, WorkspaceMode: "live", HomeDir: home,
	}, Resolved{Profile: profile}, []string{"bash", "-lc", "true"}, nil, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTriple(args, "--ro-bind", agents, "/home/devfence/agents") {
		t.Fatalf("Bubblewrap did not mount agent files read-only: %v", args)
	}
	if _, err := os.Stat("/etc/profile.d/im-config_wayland.sh"); err == nil {
		if !hasTriple(args, "--ro-bind", "/dev/null", "/etc/profile.d/im-config_wayland.sh") {
			t.Fatalf("Bubblewrap did not mask the host journal profile hook: %v", args)
		}
	}
}

func TestPrivateTerminalRemovalKeepsCommandArguments(t *testing.T) {
	args := []string{"--new-session", "--", "command", "--new-session"}
	got := withoutOptionBeforeCommand(args, "--new-session")
	want := []string{"--", "command", "--new-session"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("filtered Bubblewrap args = %q, want %q", got, want)
	}
}

func TestReplaceEnvRemovesDuplicateValues(t *testing.T) {
	env := []string{"PATH=/bin", "SSH_AUTH_SOCK=/host/agent", "SSH_AUTH_SOCK=/other/agent"}
	got := replaceEnv(env, "SSH_AUTH_SOCK", "/session/agent")
	var sockets []string
	for _, item := range got {
		if strings.HasPrefix(item, "SSH_AUTH_SOCK=") {
			sockets = append(sockets, strings.TrimPrefix(item, "SSH_AUTH_SOCK="))
		}
	}
	if len(sockets) != 1 || sockets[0] != "/session/agent" {
		t.Fatalf("SSH_AUTH_SOCK values = %q, want only the session agent", sockets)
	}
}

func TestDockerLifecycleRequiresRootlessDaemonAndSessionOwnership(t *testing.T) {
	bin := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "docker-calls")
	writeExecutable(t, filepath.Join(bin, "docker"), `#!/bin/sh
case "$1" in
  info) printf '%s\n' "$DOCKER_SECURITY" ;;
  inspect) printf '%s\n' "$DOCKER_INSPECT" ;;
  container) printf '%s\n' "$*" >> "$DOCKER_CALLS" ;;
  *) exit 2 ;;
esac
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_CALLS", logPath)
	t.Setenv("DOCKER_SECURITY", `["name=rootless"]`)
	session := &Session{ID: "owned-session", ContainerName: "devfence-owned-session"}
	t.Setenv("DOCKER_INSPECT", `{"devfence.managed":"true","devfence.session":"owned-session"} running`)
	t.Setenv("DOCKER_INSPECT", dockerInspection("owned-session"))
	if err := dockerAction(session, "stop"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil || strings.TrimSpace(string(data)) != "container stop devfence-owned-session" {
		t.Fatalf("unexpected Docker action %q, %v", data, err)
	}

	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_SECURITY", `["name=seccomp"]`)
	if err := dockerAction(session, "stop"); err == nil || !strings.Contains(err.Error(), "rootless Docker") {
		t.Fatalf("rootful daemon should be refused, got %v", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatal("rootful daemon must not receive a container mutation")
	}

	t.Setenv("DOCKER_SECURITY", `["name=rootless"]`)
	t.Setenv("DOCKER_INSPECT", `{"devfence.managed":"true","devfence.session":"someone-else"} running`)
	t.Setenv("DOCKER_INSPECT", dockerInspection("someone-else"))
	if err := dockerAction(session, "stop"); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("foreign container should be refused, got %v", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatal("foreign container must not receive a mutation")
	}
}

func TestDockerInspectionTemplateReturnsOwnerAndState(t *testing.T) {
	parsed, err := template.New("inspect").Parse(dockerInspectFormat())
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	data := struct {
		Config struct{ Labels map[string]string }
		State  struct{ Status string }
	}{}
	data.Config.Labels = map[string]string{"devfence.session": "session-a"}
	data.State.Status = "running"
	if err := parsed.Execute(&output, data); err != nil {
		t.Fatal(err)
	}
	if output.String() != "session-a\trunning" {
		t.Fatalf("Docker inspection output %q, want owner and state", output.String())
	}
}

func TestDockerCreateDoesNotMountHostDaemonOrHome(t *testing.T) {
	bin := t.TempDir()
	argsPath := filepath.Join(t.TempDir(), "args")
	writeExecutable(t, filepath.Join(bin, "docker"), `#!/bin/sh
printf '%s\0' "$@" > "$DOCKER_ARGS"
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_ARGS", argsPath)
	root := t.TempDir()
	homeDir := filepath.Join(root, "session", "home")
	agentsDir := filepath.Join(homeDir, "agents")
	if err := os.MkdirAll(homeDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(agentsDir, 0500); err != nil {
		t.Fatal(err)
	}
	session := &Session{
		ID: "container-test", ContainerName: "devfence-container-test",
		PresentedWorkspace: filepath.Join(root, "workspace"), SessionDir: filepath.Join(root, "session"),
		HomeDir: homeDir, SSHAgentDir: filepath.Join(root, "session", "ssh-agent"),
		SourceWorkspace: filepath.Join(root, "source"), WorkingDir: filepath.Join(root, "source"),
		Network: "full",
	}
	profile := Profile{Tools: ToolPolicy{Codex: ForwardedToolPolicy{Enabled: true}, SharedDirectories: []ForwardedPath{{Source: "~/agents", Target: "agents"}}}}
	if err := createContainer(session, Resolved{Profile: profile}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	joined := strings.Join(args, " ")
	for _, forbidden := range []string{"/var/run/docker.sock", "/run/docker.sock"} {
		if forbidden != "" && strings.Contains(joined, forbidden) {
			t.Errorf("container mounts forbidden host path %q: %s", forbidden, joined)
		}
	}
	if home := os.Getenv("HOME"); home != "" && strings.Contains(joined, "type=bind,src="+home+",") {
		t.Fatalf("container mounted the host home: %s", joined)
	}
	for _, required := range []string{"--cap-drop ALL", "--security-opt no-new-privileges:true", "type=bind,src=" + session.PresentedWorkspace + ",dst=/workspace", "--network bridge"} {
		if !strings.Contains(joined, required) {
			t.Errorf("container arguments missing %q: %s", required, joined)
		}
	}
	if !strings.Contains(joined, "type=bind,src="+agentsDir+",dst=/home/devfence/agents,readonly") {
		t.Fatalf("container did not mount the shared agent directory read-only: %s", joined)
	}
	if containsArg(args, "--user") {
		t.Fatalf("rootless container must use container root to map to the host user, not pass host numeric IDs: %s", joined)
	}
	if strings.Contains(joined, ".devfence/bin") || strings.Contains(joined, "NODE_PATH=") {
		t.Fatalf("container runtime still refers to copied host CLI installations: %s", joined)
	}
	if containsArg(args, "--privileged") {
		t.Fatal("rootless container must not be privileged")
	}
}

func TestDockerProjectSharesEnforceCopyReadOnlyAndLiveMountAccess(t *testing.T) {
	bin := t.TempDir()
	argsPath := filepath.Join(t.TempDir(), "args")
	writeExecutable(t, filepath.Join(bin, "docker"), "#!/bin/sh\nprintf '%s\\0' \"$@\" > \"$DOCKER_ARGS\"\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_ARGS", argsPath)
	root := t.TempDir()
	sessionDir := filepath.Join(root, "session")
	home := filepath.Join(sessionDir, "home")
	copyTarget := filepath.Join(home, "snapshot", "settings.json")
	readOnlyMount := filepath.Join(root, "host-ro")
	readWriteMount := filepath.Join(root, "host-rw")
	for _, path := range []string{filepath.Dir(copyTarget), home, filepath.Join(home, "live-ro"), filepath.Join(home, "live-rw"), filepath.Join(sessionDir, "credentials"), readOnlyMount, readWriteMount} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(copyTarget, []byte("snapshot"), 0400); err != nil {
		t.Fatal(err)
	}
	session := &Session{
		ID: "container-share-test", ContainerName: "devfence-container-share-test",
		PresentedWorkspace: filepath.Join(root, "workspace"), SessionDir: sessionDir,
		HomeDir: home, SourceWorkspace: filepath.Join(root, "source"), WorkingDir: filepath.Join(root, "source"), Network: "full",
	}
	profile := Profile{ProjectShares: []ProjectShare{
		{Source: filepath.Join(root, "unused-copy-source"), Target: "snapshot/settings.json", Mode: "copy"},
		{Source: readOnlyMount, Target: "live-ro", Mode: "mount"},
		{Source: readWriteMount, Target: "live-rw", Mode: "mount", Access: "read-write"},
	}}
	if err := createContainer(session, Resolved{Profile: profile}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.ReplaceAll(string(data), "\x00", " ")
	wants := []string{
		"type=bind,src=" + copyTarget + ",dst=/home/devfence/snapshot/settings.json,readonly",
		"type=bind,src=" + readOnlyMount + ",dst=/home/devfence/live-ro,readonly",
		"type=bind,src=" + readWriteMount + ",dst=/home/devfence/live-rw",
	}
	for _, want := range wants {
		if !strings.Contains(joined, want) {
			t.Errorf("Docker project share mount missing %q: %s", want, joined)
		}
	}
}

func TestTokenIsStoredOutsideManifestAndWithPrivateMode(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	sessionDir := filepath.Join(stateDir, "sessions", "token-session")
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		t.Fatal(err)
	}
	session := &Session{ID: "token-session", SessionDir: sessionDir}
	profile := Profile{Tools: ToolPolicy{GH: GitHubToolPolicy{Enabled: true, Authentication: GitHubAuthenticationPolicy{TokenCommand: []string{"sh", "-c", "printf '%s' 'secret-token'"}}}}}
	if err := prepareCredentials(session, profile); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(sessionDir, "credentials", "github-token")
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("token mode is %o, want 600", info.Mode().Perm())
	}
	manifest, err := os.ReadFile(filepath.Join(sessionDir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(manifest), "secret-token") {
		t.Fatal("token value was written into the session manifest")
	}
	if !session.GitHubEnabled {
		t.Fatal("session should record that GitHub credentials were enabled")
	}
}

func TestSessionDeletionRefusesUnverifiedGitAndPrivateHomeData(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", repo, "init", "--initial-branch=main")
	runGit(t, "-C", repo, "config", "user.email", "test@example.invalid")
	runGit(t, "-C", repo, "config", "user.name", "Devfence Test")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", repo, "add", "file.txt")
	runGit(t, "-C", repo, "commit", "-m", "initial")
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	session := &Session{WorkspaceMode: "live", SourceWorkspace: repo, HomeDir: home}
	if err := checkSessionHasNoUnpreservedWork(session); err == nil || !strings.Contains(err.Error(), "upstream") {
		t.Fatalf("branch without upstream should be unverified, got %v", err)
	}

	remote := filepath.Join(root, "remote.git")
	runGit(t, "init", "--bare", "--initial-branch=main", remote)
	runGit(t, "-C", repo, "remote", "add", "origin", remote)
	runGit(t, "-C", repo, "push", "--set-upstream", "origin", "main")
	if err := checkSessionHasNoUnpreservedWork(session); err != nil {
		t.Fatalf("clean worktree at upstream should be safe: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkSessionHasNoUnpreservedWork(session); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty worktree should be preserved, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".agent-state"), []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkSessionHasNoUnpreservedWork(session); err == nil || !strings.Contains(err.Error(), "session home directory") {
		t.Fatalf("private home data should block deletion, got %v", err)
	}
}

func TestSessionDeletionRefusesUnexportedCopyChanges(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	presented := filepath.Join(root, "presented")
	for _, path := range []string{source, presented} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(presented, "new.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "copy-state.json")
	copyState := &CopyState{Source: source, Presented: presented, Baseline: map[string]string{}}
	if err := writeCopyState(statePath, copyState); err != nil {
		t.Fatal(err)
	}
	session := &Session{WorkspaceMode: "copy", SourceWorkspace: source, CopyStatePath: statePath, HomeDir: filepath.Join(root, "empty-home")}
	if err := os.Mkdir(session.HomeDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := checkSessionHasNoUnpreservedWork(session); err == nil || !strings.Contains(err.Error(), "unexported") {
		t.Fatalf("unexported copy changes should block deletion, got %v", err)
	}
}

func TestForwardedToolFilesDoNotBlockSessionDeletion(t *testing.T) {
	home := t.TempDir()
	for _, file := range []string{".codex/auth.json", ".codex/config.toml", ".claude/settings.json"} {
		path := filepath.Join(home, file)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("session copy"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	agents := filepath.Join(home, "agents")
	if err := os.MkdirAll(agents, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agents, "reviewer.md"), []byte("host snapshot"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(agents, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(agents, 0700) })
	tools := ToolPolicy{
		Codex: ForwardedToolPolicy{Enabled: true,
			Authentication: []ForwardedFile{{Source: "~/.codex/auth.json", Target: ".codex/auth.json"}},
			Configuration:  []ForwardedFile{{Source: "~/.codex/config.toml", Target: ".codex/config.toml"}}},
		Claude:            ForwardedToolPolicy{Enabled: true, Configuration: []ForwardedFile{{Source: "~/.claude/settings.json", Target: ".claude/settings.json"}}},
		SharedDirectories: []ForwardedPath{{Source: "~/agents", Target: "agents"}},
	}
	files, directories := managedHomeTargets(Profile{Backend: BackendBubblewrap, Tools: tools})
	if found, err := homeHasUnmanagedFiles(home, files, directories); err != nil || found {
		t.Fatalf("forwarded session files should be managed: found=%t err=%v", found, err)
	}
	history := filepath.Join(home, ".codex", "session-history.jsonl")
	if err := os.WriteFile(history, []byte("user session data"), 0600); err != nil {
		t.Fatal(err)
	}
	if found, err := homeHasUnmanagedFiles(home, files, directories); err != nil || !found {
		t.Fatalf("unforwarded tool data should block deletion: found=%t err=%v", found, err)
	}
}

func TestManagedReadOnlyHomeSnapshotsBecomeRemovableWithoutFollowingSymlinks(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "session")
	home := filepath.Join(sessionDir, "home")
	readonlyDirectory := filepath.Join(home, "agents", "nested")
	if err := os.MkdirAll(readonlyDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(readonlyDirectory, "AGENTS.md"), []byte("snapshot"), 0400); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(sessionDir, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr == nil && entry.IsDir() {
				_ = os.Chmod(path, 0700)
			}
			return nil
		})
		_ = os.Chmod(outside, 0700)
	})
	if err := os.Symlink(outside, filepath.Join(home, "agents", "outside-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readonlyDirectory, 0500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(home, "agents"), 0500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0000); err != nil {
		t.Fatal(err)
	}
	session := &Session{SessionDir: sessionDir, HomeDir: home}
	if err := makeSessionHomeRemovable(session); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{home, filepath.Join(home, "agents"), readonlyDirectory} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Errorf("managed directory %s mode = %v, err=%v", path, info, err)
		}
	}
	outsideInfo, err := os.Stat(outside)
	if err != nil || outsideInfo.Mode().Perm() != 0500 {
		t.Fatalf("cleanup chmod followed a symlink outside session home: info=%v err=%v", outsideInfo, err)
	}
	if err := os.RemoveAll(sessionDir); err != nil {
		t.Fatalf("remove session after preparing readonly data: %v", err)
	}
}

func TestMakeSessionHomeRemovableRejectsPathOutsideSession(t *testing.T) {
	root := t.TempDir()
	outsideHome := filepath.Join(root, "outside-home")
	if err := os.Mkdir(outsideHome, 0700); err != nil {
		t.Fatal(err)
	}
	session := &Session{SessionDir: filepath.Join(root, "session"), HomeDir: outsideHome}
	if err := makeSessionHomeRemovable(session); err == nil {
		t.Fatal("session manifest home path outside session state was accepted")
	}
}

func TestGuestProvisioningAndRemoteArgumentEncoding(t *testing.T) {
	root := t.TempDir()
	key := filepath.Join(root, "vm-login")
	if err := os.WriteFile(key+".pub", []byte("ssh-ed25519 AAAATEST devfence-test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	profile := Profile{Tools: ToolPolicy{Codex: ForwardedToolPolicy{Enabled: true}, Claude: ForwardedToolPolicy{Enabled: true}, GH: GitHubToolPolicy{Enabled: true}}}
	data, err := guestUserData(&Session{ID: "guest-test", VMName: "devfence-guest-test", VMLoginKey: key, WorkspaceMode: "live"}, VMConfig{}, profile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "#cloud-config\n") {
		t.Fatal("cloud-init seed must be explicitly marked as cloud-config")
	}
	if strings.Contains(guestExecHelper, ".devfence") || strings.Contains(guestExecHelper, "NODE_PATH") {
		t.Fatal("VM command helper still prioritizes host-copied tool installations")
	}
	var cloud map[string]any
	if err := yaml.Unmarshal(data, &cloud); err != nil {
		t.Fatal(err)
	}
	users, hasUsers := cloud["users"].([]any)
	if os.Getuid() == 1000 {
		if hasUsers {
			t.Fatalf("Ubuntu host UID should use cloud-init's native default account: %#v", users)
		}
	} else {
		if !hasUsers || len(users) != 1 {
			t.Fatalf("unexpected cloud-init users: %#v", cloud["users"])
		}
		user, ok := users[0].(map[string]any)
		if !ok {
			t.Fatalf("non-default host UID needs a configured guest account: %#v", users[0])
		}
		uid, uidOK := user["uid"].(int)
		if user["name"] != "sandbox" || !uidOK || uid != os.Getuid() {
			t.Fatalf("guest user does not match workspace ownership: %#v", users[0])
		}
		if strings.Contains(strings.Join(toStringSlice(user["groups"]), ","), "docker") {
			t.Fatal("docker group must be added only after its package creates the group")
		}
	}
	keys, ok := cloud["ssh_authorized_keys"].([]any)
	if !ok || len(keys) != 1 || keys[0] != "ssh-ed25519 AAAATEST devfence-test" {
		t.Fatalf("cloud-init SSH key must target the configured guest user: %#v", cloud["ssh_authorized_keys"])
	}
	if !strings.Contains(string(data), "virtiofs") || !strings.Contains(string(data), "devfence-exec") {
		t.Fatal("cloud-init does not configure the workspace mount and safe command helper")
	}
	bootstrap := bootstrapGuestTools("stable", "latest", "stable", ToolPolicy{Codex: ForwardedToolPolicy{Enabled: true}, Claude: ForwardedToolPolicy{Enabled: true}, GH: GitHubToolPolicy{Enabled: true}})
	if !strings.Contains(bootstrap, "n 22") || !strings.Contains(bootstrap, "@openai/codex") || !strings.Contains(bootstrap, "@anthropic-ai/claude-code") {
		t.Fatalf("guest bootstrap is missing supported developer tools: %s", bootstrap)
	}

	command := []string{"sh", "-c", "printf '%s' \"$1\"", "arg0", "$(touch /tmp/not-executed); spaces"}
	encodedArgs := safeRemoteCommand(command)
	if len(encodedArgs) != 2 || encodedArgs[0] != "/usr/local/bin/devfence-exec" {
		t.Fatalf("unexpected remote helper argv: %#v", encodedArgs)
	}
	decoded, err := hex.DecodeString(encodedArgs[1])
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal(decoded, &got); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "\x00") != strings.Join(command, "\x00") {
		t.Fatalf("remote command argv changed: %#v", got)
	}
}

func TestVMProjectSharesCreateGuestMountpointsAndVirtiofsDevices(t *testing.T) {
	root := t.TempDir()
	key := filepath.Join(root, "vm-login")
	if err := os.WriteFile(key+".pub", []byte("ssh-ed25519 AAAATEST devfence-test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	readOnlySource := filepath.Join(root, "readonly")
	readWriteSource := filepath.Join(root, "readwrite")
	for _, path := range []string{readOnlySource, readWriteSource} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	profile := Profile{ProjectShares: []ProjectShare{
		{Source: filepath.Join(root, "snapshot-source"), Target: "agents/snapshot", Mode: "copy"},
		{Source: readOnlySource, Target: "agents/live-ro", Mode: "mount"},
		{Source: readWriteSource, Target: "agents/live-rw", Mode: "mount", Access: "read-write"},
	}}
	session := &Session{ID: "vm-shares", VMName: "devfence-vm-shares", VMLoginKey: key, WorkspaceMode: "live"}
	data, err := guestUserData(session, VMConfig{}, profile)
	if err != nil {
		t.Fatal(err)
	}
	var cloud map[string]any
	if err := yaml.Unmarshal(data, &cloud); err != nil {
		t.Fatal(err)
	}
	mounts, ok := cloud["mounts"].([]any)
	if !ok {
		t.Fatalf("cloud-init mounts = %#v", cloud["mounts"])
	}
	joinedMounts, _ := json.Marshal(mounts)
	for _, want := range []string{
		projectShareTag(1), projectShareGuestPath(vmGuestUser(VMConfig{}), "agents/live-ro"), "ro,nosuid,nodev",
		projectShareTag(2), projectShareGuestPath(vmGuestUser(VMConfig{}), "agents/live-rw"), "rw,nosuid,nodev",
	} {
		if !strings.Contains(string(joinedMounts), want) {
			t.Errorf("cloud-init mount configuration missing %q: %s", want, joinedMounts)
		}
	}
	bootcmd, _ := json.Marshal(cloud["bootcmd"])
	if !strings.Contains(string(bootcmd), projectShareGuestPath(vmGuestUser(VMConfig{}), "agents/live-ro")) || !strings.Contains(string(bootcmd), projectShareGuestPath(vmGuestUser(VMConfig{}), "agents/live-rw")) {
		t.Fatalf("cloud-init does not create mountpoint parents before mounting: %s", bootcmd)
	}
	runcmd, _ := json.Marshal(cloud["runcmd"])
	if !strings.Contains(string(runcmd), "chown") || !strings.Contains(string(runcmd), projectShareGuestPath(vmGuestUser(VMConfig{}), "")) {
		t.Fatalf("cloud-init does not restore guest HOME ownership after preparing mountpoints: %s", runcmd)
	}
	args := virtInstallArgs(session, profile, "/private/user-data", "/private/meta-data", "disk.qcow2")
	if !containsArg(args, readOnlySource+","+projectShareTag(1)+",driver.type=virtiofs,binary.sandbox.mode=namespace,readonly=on") {
		t.Fatalf("read-only virtiofs share device is missing: %#v", args)
	}
	if !containsArg(args, readWriteSource+","+projectShareTag(2)+",driver.type=virtiofs,binary.sandbox.mode=namespace") {
		t.Fatalf("read-write virtiofs share device is missing: %#v", args)
	}
	if hasProjectMountShares([]ProjectShare{{Source: "/tmp/copy", Target: "copy", Mode: "copy"}}) {
		t.Fatal("copy-only shares incorrectly require a live virtiofs device")
	}
}

func TestVMGuestUserMatchesUbuntuCloudImageUID(t *testing.T) {
	for _, test := range []struct {
		name     string
		settings VMConfig
		uid      int
		want     string
	}{
		{name: "ubuntu image default account", uid: 1000, want: "ubuntu"},
		{name: "other host uid", uid: 1001, want: "sandbox"},
		{name: "explicit account", settings: VMConfig{GuestUser: "dev"}, uid: 1001, want: "dev"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := vmGuestUserForUID(test.settings, test.uid); got != test.want {
				t.Fatalf("vmGuestUserForUID() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestVMGuestUsersAvoidsUbuntuUIDCollision(t *testing.T) {
	if _, err := vmGuestUsers("sandbox", 1000); err == nil {
		t.Fatal("custom guest user with Ubuntu's reserved UID should fail before VM creation")
	}
	if _, err := vmGuestUsers("ubuntu", 1001); err == nil {
		t.Fatal("Ubuntu default account with a mismatched host UID should fail")
	}
}

func TestVirtInstallSeedIncludesUserAndInstanceMetadata(t *testing.T) {
	args := virtInstallArgs(&Session{VMName: "devfence-test"}, Profile{}, "/private/user-data", "/private/meta-data", "devfence-test.qcow2")
	if !containsArg(args, "user-data=/private/user-data,meta-data=/private/meta-data") {
		t.Fatalf("virt-install cloud-init seed is missing user or instance metadata: %#v", args)
	}
	data, err := guestMetaData(&Session{ID: "session-test", VMName: "devfence-session-test"})
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]string
	if err := yaml.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["instance-id"] != "session-test" || metadata["local-hostname"] != "devfence-session-test" {
		t.Fatalf("cloud-init metadata does not identify this VM: %#v", metadata)
	}
}

func TestChecksumParsingAndImageDigest(t *testing.T) {
	image := []byte("verified image")
	hash := sha256.Sum256(image)
	digest := hex.EncodeToString(hash[:])
	checksums := []byte(digest + "  noble-server-cloudimg-amd64.img\n" + strings.Repeat("0", 64) + " *other.img\n")
	if got := checksumFor(checksums, "noble-server-cloudimg-amd64.img"); got != digest {
		t.Fatalf("checksumFor=%q, want %q", got, digest)
	}
	if got := checksumFor(checksums, "missing.img"); got != "" {
		t.Fatalf("missing file checksum = %q, want empty", got)
	}
}

func TestLibvirtStateParsingAndMissingVMDetection(t *testing.T) {
	if !libvirtPoolIsRunning([]byte("Name: devfence\nState:          running\n")) {
		t.Fatal("libvirt pool parser did not accept the normal padded state field")
	}
	if libvirtPoolIsRunning([]byte("Name: devfence\nState:          inactive\n")) {
		t.Fatal("inactive libvirt pool must not be treated as running")
	}

	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "virsh"), `#!/bin/sh
printf '%s\n' "$VIRSH_RESPONSE"
exit "${VIRSH_EXIT:-0}"
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("VIRSH_RESPONSE", "error: failed to get domain 'devfence-absent'")
	t.Setenv("VIRSH_EXIT", "1")
	state, err := vmDomainState("devfence-absent")
	if err != nil || state != "missing" {
		t.Fatalf("absent domain = %q, %v; want missing", state, err)
	}
	t.Setenv("VIRSH_RESPONSE", "error: failed to connect to the hypervisor")
	if state, err := vmDomainState("devfence-absent"); err == nil || state == "missing" {
		t.Fatalf("libvirt outage must not be mistaken for a missing VM: state=%q err=%v", state, err)
	}
}

func TestCLIBootstrapAndPlanFromNonGitDirectory(t *testing.T) {
	root := t.TempDir()
	configHome, stateHome := filepath.Join(root, "config"), filepath.Join(root, "state")
	for _, path := range []string{configHome, stateHome} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_STATE_HOME", stateHome)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "not-a-repository")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	code, output := captureMain(t, []string{"config", "init"})
	if code != 0 || !strings.Contains(output, "Wrote ") {
		t.Fatalf("config init failed: code=%d output=%q", code, output)
	}
	code, output = captureMain(t, []string{"config", "init", "--project"})
	if code != 0 || !strings.Contains(output, ProjectConfigPath(project)) {
		t.Fatalf("project config init failed: code=%d output=%q", code, output)
	}
	code, output = captureMain(t, []string{"plan"})
	if code != 0 {
		t.Fatalf("plan from non-Git directory failed: %s", output)
	}
	if !strings.Contains(output, "Project config: "+ProjectConfigPath(project)) || !strings.Contains(output, "Backend: bubblewrap") || !strings.Contains(output, "Workspace mode: live") || !strings.Contains(output, project) {
		t.Fatalf("plan omitted effective workspace policy: %s", output)
	}
}

func captureMain(t *testing.T, args []string) (int, string) {
	t.Helper()
	stdout, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	code := Main(args, stdout, stderr)
	if _, err := stdout.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stderr.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	errData, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	return code, string(data) + string(errData)
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
}

func dockerInspection(owner string) string {
	return owner + "\trunning"
}

func runGit(t *testing.T, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s: %v", args, output, err)
	}
}

func containsArg(args []string, value string) bool {
	for _, arg := range args {
		if arg == value {
			return true
		}
	}
	return false
}

func toStringSlice(value any) []string {
	values, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			result = append(result, text)
		}
	}
	return result
}
