package devfence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	args, err := bubblewrapArgs(session, resolved, []string{"codex"})
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
	}, Resolved{Profile: Profile{Network: "full"}}, []string{"sh"})
	if err != nil {
		t.Fatal(err)
	}
	if !containsArg(args, "--share-net") {
		t.Fatal("network:full must explicitly share the host network namespace")
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
	session := &Session{
		ID: "container-test", ContainerName: "devfence-container-test",
		PresentedWorkspace: filepath.Join(root, "workspace"), SessionDir: filepath.Join(root, "session"),
		HomeDir: filepath.Join(root, "session", "home"), SSHAgentDir: filepath.Join(root, "session", "ssh-agent"),
		SourceWorkspace: filepath.Join(root, "source"), WorkingDir: filepath.Join(root, "source"),
		Network: "full",
	}
	if err := createContainer(session, Resolved{Profile: Profile{}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	joined := strings.Join(args, " ")
	for _, forbidden := range []string{"/var/run/docker.sock", "/run/docker.sock", os.Getenv("HOME")} {
		if forbidden != "" && strings.Contains(joined, forbidden) {
			t.Errorf("container mounts forbidden host path %q: %s", forbidden, joined)
		}
	}
	for _, required := range []string{"--cap-drop ALL", "--security-opt no-new-privileges:true", "type=bind,src=" + session.PresentedWorkspace + ",dst=/workspace", "--network bridge"} {
		if !strings.Contains(joined, required) {
			t.Errorf("container arguments missing %q: %s", required, joined)
		}
	}
	if containsArg(args, "--privileged") {
		t.Fatal("rootless container must not be privileged")
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
	if err := prepareCredentials(session, CredentialPolicy{GitHubTokenCommand: []string{"sh", "-c", "printf '%s' 'secret-token'"}}); err != nil {
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

func TestGuestProvisioningAndRemoteArgumentEncoding(t *testing.T) {
	root := t.TempDir()
	key := filepath.Join(root, "vm-login")
	if err := os.WriteFile(key+".pub", []byte("ssh-ed25519 AAAATEST devfence-test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := guestUserData(&Session{ID: "guest-test", VMName: "devfence-guest-test", VMLoginKey: key, WorkspaceMode: "live"}, VMConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "#cloud-config\n") {
		t.Fatal("cloud-init seed must be explicitly marked as cloud-config")
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
	bootstrap := bootstrapGuestTools("stable", "latest", "stable")
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
	code, output = captureMain(t, []string{"plan"})
	if code != 0 {
		t.Fatalf("plan from non-Git directory failed: %s", output)
	}
	if !strings.Contains(output, "Backend: bubblewrap") || !strings.Contains(output, "Workspace mode: live") || !strings.Contains(output, project) {
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
