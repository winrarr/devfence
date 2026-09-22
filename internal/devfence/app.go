package devfence

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

type runOptions struct {
	Backend   string
	Workspace string
	Network   string
	Profile   string
	Name      string
	VSCode    bool
	Command   []string
}

func Main(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		return report(runSession(args, stdout), stderr)
	}
	switch args[0] {
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	case "run":
		return report(runSession(args[1:], stdout), stderr)
	case "plan":
		return report(showPlan(args[1:], stdout), stderr)
	case "config":
		return report(configCommand(args[1:], stdout), stderr)
	case "list", "ls":
		return report(listCommand(stdout), stderr)
	case "inspect":
		return report(inspectCommand(args[1:], stdout), stderr)
	case "attach":
		return report(attachCommand(args[1:], stdout), stderr)
	case "vscode", "code":
		if len(args) < 2 {
			return report(errors.New("usage: devfence vscode SESSION_ID"), stderr)
		}
		return report(attachCommand(append([]string{args[1], "--vscode"}, args[2:]...), stdout), stderr)
	case "stop":
		return report(stopCommand(args[1:]), stderr)
	case "delete", "rm":
		return report(deleteCommand(args[1:]), stderr)
	case "export":
		return report(exportCommand(args[1:], stdout), stderr)
	default:
		return report(runSession(args, stdout), stderr)
	}
}

func report(err error, stderr *os.File) int {
	if err == nil {
		return 0
	}
	fmt.Fprintf(stderr, "devfence: %v\n", err)
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	return 1
}

func printUsage(w *os.File) {
	fmt.Fprintln(w, `Usage:
  devfence [RUN_OPTIONS] [-- COMMAND [ARGS...]]
  devfence run [RUN_OPTIONS] [-- COMMAND [ARGS...]]
  devfence plan [RUN_OPTIONS]
  devfence config init [--force]
  devfence list | inspect ID | attach ID [--vscode] [-- COMMAND [ARGS...]]
  devfence stop ID | delete ID --yes [--force] | export ID [--apply]

Run options:
  --backend bubblewrap|docker|vm   Runtime (aliases: --host, --container, --vm)
  --workspace live|copy            Workspace presentation
  --network full|none              Agent network access
  --profile NAME                   Configuration profile
  --name ID                        Explicit session ID
  --vscode                         Open an editor attached to a persistent runtime

With no command, Devfence opens a shell. A bare command is accepted, for example:
  devfence codex
  devfence --backend vm -- codex`)
}

func parseRunOptions(args []string) (runOptions, error) {
	var options runOptions
	for index := 0; index < len(args); {
		arg := args[index]
		if arg == "--" {
			options.Command = append([]string(nil), args[index+1:]...)
			return options, nil
		}
		if arg == "--host" {
			options.Backend = string(BackendBubblewrap)
			index++
			continue
		}
		if arg == "--container" {
			options.Backend = string(BackendDocker)
			index++
			continue
		}
		if arg == "--vm" {
			options.Backend = string(BackendVM)
			index++
			continue
		}
		if arg == "--vscode" {
			options.VSCode = true
			index++
			continue
		}
		key, inlineValue, hasInline := strings.Cut(arg, "=")
		var destination *string
		switch key {
		case "--backend":
			destination = &options.Backend
		case "--workspace":
			destination = &options.Workspace
		case "--network":
			destination = &options.Network
		case "--profile":
			destination = &options.Profile
		case "--name":
			destination = &options.Name
		case "-h", "--help":
			return options, errors.New("use `devfence help` to show usage")
		default:
			if strings.HasPrefix(arg, "-") {
				return options, fmt.Errorf("unknown run option %q", arg)
			}
			options.Command = append([]string(nil), args[index:]...)
			return options, nil
		}
		value := inlineValue
		if !hasInline {
			index++
			if index >= len(args) {
				return options, fmt.Errorf("%s requires a value", key)
			}
			value = args[index]
		}
		*destination = value
		index++
	}
	return options, nil
}

func runSession(args []string, stdout *os.File) error {
	options, err := parseRunOptions(args)
	if err != nil {
		return err
	}
	cfg, _, err := userConfig()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	resolved, err := Resolve(cfg, cwd, options.Backend, options.Workspace, options.Network, options.Profile)
	if err != nil {
		return err
	}
	if options.VSCode && resolved.Profile.Backend == BackendBubblewrap {
		return errors.New("VS Code attachment is supported for Docker and VM sessions; use VS Code locally for Bubblewrap sessions")
	}
	session, err := newSession(resolved, options.Name)
	if err != nil {
		return err
	}
	if err := prepareCredentials(session, resolved.Profile.Credentials); err != nil {
		cleanupFailedSession(session)
		return err
	}
	if session.Backend == BackendBubblewrap {
		session.PID = os.Getpid()
	}
	if err := saveSession(resolved.StateDir, session); err != nil {
		cleanupFailedSession(session)
		return err
	}
	printSessionSummary(stdout, session, resolved.Profile)

	var runErr error
	switch session.Backend {
	case BackendBubblewrap:
		session.Status = "running"
		_ = saveSession(resolved.StateDir, session)
		runErr = runBubblewrap(session, resolved, options.Command)
		stopSSHAgent(session)
		session.PID = 0
		session.Status = "stopped"
	case BackendDocker:
		if options.VSCode {
			runErr = prepareAndOpenDocker(session, resolved, true)
		} else {
			runErr = runDocker(session, resolved, options.Command, true)
		}
		status, statusErr := dockerStatus(session)
		if statusErr != nil && runErr == nil {
			runErr = statusErr
		}
		if statusErr == nil {
			session.Status = status
		}
		if statusErr == nil && session.Status == "missing" {
			cleanupFailedSession(session)
			return runErr
		}
	case BackendVM:
		if options.VSCode {
			if err := createVM(session, resolved); err != nil {
				runErr = err
			} else {
				_, runErr = prepareVM(session, resolved, false, false)
				if runErr == nil {
					runErr = openVMEditor(session)
				}
			}
		} else {
			runErr = runVM(session, resolved, options.Command, true, false)
		}
		state, _ := vmDomainState(session.VMName)
		if state == "missing" {
			cleanupFailedSession(session)
			return runErr
		}
		session.Status = state
	}
	if err := saveSession(resolved.StateDir, session); err != nil && runErr == nil {
		runErr = err
	}
	return runErr
}

func showPlan(args []string, stdout *os.File) error {
	options, err := parseRunOptions(args)
	if err != nil {
		return err
	}
	cfg, _, err := userConfig()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	resolved, err := Resolve(cfg, cwd, options.Backend, options.Workspace, options.Network, options.Profile)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Profile: %s\nBackend: %s\nWorkspace: %s\nWorkspace mode: %s\nNetwork: %s\n",
		resolved.ProfileName, resolved.Profile.Backend, resolved.Workspace, resolved.Profile.Workspace.Mode, resolved.Profile.Network)
	if resolved.Profile.Workspace.Mode == "live" {
		fmt.Fprintln(stdout, "Presented path: live read-write mount")
	} else {
		fmt.Fprintf(stdout, "Presented path: filtered private copy under %s/sessions/<new-id>/workspace\n", filepath.Join(resolved.StateDir, "sessions"))
		if len(resolved.Profile.Workspace.Include) > 0 {
			fmt.Fprintf(stdout, "Include: %s\n", strings.Join(resolved.Profile.Workspace.Include, ", "))
		}
		if len(resolved.Profile.Workspace.Exclude) > 0 {
			fmt.Fprintf(stdout, "Exclude: %s\n", strings.Join(resolved.Profile.Workspace.Exclude, ", "))
		}
	}
	if len(resolved.Protected) > 0 {
		fmt.Fprintf(stdout, "Protected paths: %s\n", strings.Join(resolved.Protected, ", "))
	}
	if resolved.Profile.Credentials.SSHKey == "" {
		fmt.Fprintln(stdout, "SSH identity: none")
	} else {
		fmt.Fprintf(stdout, "SSH identity: %s\n", filepath.Base(resolved.Profile.Credentials.SSHKey))
	}
	if len(resolved.Profile.Credentials.GitHubTokenCommand) > 0 {
		fmt.Fprintln(stdout, "GitHub token: provided by configured command (value hidden)")
	} else {
		fmt.Fprintln(stdout, "GitHub token: none")
	}
	if resolved.Profile.Backend == BackendVM {
		memory, cpus, disk := vmResourceValues(resolved.Profile)
		fmt.Fprintf(stdout, "VM resources: %d MiB memory, %d vCPU, %d GiB disk\n", memory, cpus, disk)
		fmt.Fprintln(stdout, "Guest: private kernel; Docker, Kind, kubectl, Cilium CLI will be installed")
	}
	if resolved.Profile.Backend == BackendDocker {
		fmt.Fprintln(stdout, "Container: rootless Docker required; host sockets and host home are not mounted")
	}
	if options.VSCode {
		fmt.Fprintln(stdout, "Editor: VS Code remote attachment")
	}
	return nil
}

func configCommand(args []string, stdout *os.File) error {
	if len(args) == 0 {
		return errors.New("usage: devfence config init|path|validate")
	}
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	switch args[0] {
	case "path":
		fmt.Fprintln(stdout, path)
		return nil
	case "init":
		force := len(args) == 2 && args[1] == "--force"
		if len(args) > 2 || len(args) == 2 && !force {
			return errors.New("usage: devfence config init [--force]")
		}
		if _, err := os.Stat(path); err == nil && !force {
			return fmt.Errorf("config already exists at %s; use --force to replace it", path)
		}
		data, err := yaml.Marshal(DefaultConfig())
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Wrote %s\n", path)
		return nil
	case "validate":
		cfg, err := LoadConfig(path)
		if err != nil {
			return err
		}
		if err := cfg.Validate(); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Configuration is valid (%d profiles)\n", len(cfg.Profiles))
		return nil
	default:
		return fmt.Errorf("unknown config command %q", args[0])
	}
}

func userConfig() (Config, string, error) {
	path, err := ConfigPath()
	if err != nil {
		return Config{}, "", err
	}
	cfg, err := LoadConfig(path)
	return cfg, path, err
}

func printSessionSummary(stdout *os.File, session *Session, profile Profile) {
	fmt.Fprintf(stdout, "devfence: session %s\n", session.ID)
	fmt.Fprintf(stdout, "  backend: %s\n  workspace: %s (%s)\n  network: %s\n", session.Backend, session.SourceWorkspace, session.WorkspaceMode, session.Network)
	if session.SSHKeyName != "" {
		fmt.Fprintf(stdout, "  SSH identity: %s (%s)\n", session.SSHKeyName, session.SSHKeyFingerprint)
	} else {
		fmt.Fprintln(stdout, "  SSH identity: none")
	}
	if session.GitHubEnabled {
		fmt.Fprintln(stdout, "  GitHub token: enabled (value hidden)")
	} else {
		fmt.Fprintln(stdout, "  GitHub token: none")
	}
	if session.Backend == BackendVM {
		memory, cpus, disk := vmResourceValues(profile)
		fmt.Fprintf(stdout, "  VM: %d MiB, %d vCPU, %d GiB disk\n", memory, cpus, disk)
	}
}

func vmResourceValues(profile Profile) (memory, cpus, disk int) {
	memory, cpus, disk = profile.Resources.MemoryMiB, profile.Resources.VCPUs, profile.Resources.DiskGiB
	if memory <= 0 {
		memory = 6144
	}
	if cpus <= 0 {
		cpus = 4
	}
	if disk <= 0 {
		disk = 40
	}
	return
}

func listCommand(stdout *os.File) error {
	stateDir, err := StatePath()
	if err != nil {
		return err
	}
	sessions, err := listSessions(stateDir)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Fprintln(stdout, "No sessions")
		return nil
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].CreatedAt.Before(sessions[j].CreatedAt) })
	fmt.Fprintln(stdout, "ID\tBACKEND\tSTATUS\tWORKSPACE")
	for _, session := range sessions {
		status, _ := sessionStatus(&session)
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", session.ID, session.Backend, status, session.SourceWorkspace)
	}
	return nil
}

func inspectCommand(args []string, stdout *os.File) error {
	if len(args) != 1 {
		return errors.New("usage: devfence inspect SESSION_ID")
	}
	stateDir, err := StatePath()
	if err != nil {
		return err
	}
	session, err := loadSession(stateDir, args[0])
	if err != nil {
		return err
	}
	status, err := sessionStatus(session)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "ID: %s\nBackend: %s\nProfile: %s\nStatus: %s\nWorkspace: %s\nPresented workspace: %s\nWorkspace mode: %s\nNetwork: %s\nCreated: %s\n",
		session.ID, session.Backend, session.ProfileName, status, session.SourceWorkspace, session.PresentedWorkspace, session.WorkspaceMode, session.Network, session.CreatedAt.Format(time.RFC3339))
	if len(session.ProtectedPaths) > 0 {
		fmt.Fprintf(stdout, "Protected paths at creation: %s\n", strings.Join(session.ProtectedPaths, ", "))
	}
	if session.SSHKeyName != "" {
		fmt.Fprintf(stdout, "SSH identity: %s (%s)\n", session.SSHKeyName, session.SSHKeyFingerprint)
	} else {
		fmt.Fprintln(stdout, "SSH identity: none")
	}
	fmt.Fprintf(stdout, "GitHub token: %t (value hidden)\n", session.GitHubEnabled)
	if session.VMAddress != "" {
		fmt.Fprintf(stdout, "VM address: %s\n", session.VMAddress)
	}
	return nil
}

func sessionStatus(session *Session) (string, error) {
	switch session.Backend {
	case BackendBubblewrap:
		if processAlive(session.PID) {
			return "running", nil
		}
		return "stopped", nil
	case BackendDocker:
		return dockerStatus(session)
	case BackendVM:
		return vmDomainState(session.VMName)
	default:
		return "unknown", fmt.Errorf("unsupported backend %q", session.Backend)
	}
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func attachCommand(args []string, stdout *os.File) error {
	if len(args) == 0 {
		return errors.New("usage: devfence attach SESSION_ID [--vscode] [-- COMMAND [ARGS...]]")
	}
	id := args[0]
	options, err := parseRunOptions(args[1:])
	if err != nil {
		return err
	}
	stateDir, err := StatePath()
	if err != nil {
		return err
	}
	session, err := loadSession(stateDir, id)
	if err != nil {
		return err
	}
	if session.Backend == BackendBubblewrap {
		return errors.New("Bubblewrap sessions are foreground-only and cannot be reattached")
	}
	cfg, _, err := userConfig()
	if err != nil {
		return err
	}
	resolved, err := resolveExistingSession(cfg, session)
	if err != nil {
		return err
	}
	wasGitHubEnabled := session.GitHubEnabled
	if err := prepareCredentials(session, resolved.Profile.Credentials); err != nil {
		return err
	}
	if options.VSCode {
		return openEditor(session, resolved, wasGitHubEnabled && !session.GitHubEnabled)
	}
	printSessionSummary(stdout, session, resolved.Profile)
	var runErr error
	switch session.Backend {
	case BackendDocker:
		if err := runDocker(session, resolved, options.Command, false); err != nil {
			runErr = err
		}
		status, statusErr := dockerStatus(session)
		if statusErr != nil && runErr == nil {
			runErr = statusErr
		} else if statusErr == nil {
			session.Status = status
		}
	case BackendVM:
		runErr = runVM(session, resolved, options.Command, false, wasGitHubEnabled && !session.GitHubEnabled)
		session.Status, _ = vmDomainState(session.VMName)
	}
	if err := saveSession(stateDir, session); err != nil && runErr == nil {
		runErr = err
	}
	return runErr
}

func resolveExistingSession(cfg Config, session *Session) (Resolved, error) {
	resolved, err := Resolve(cfg, session.WorkingDir, "", "", "", session.ProfileName)
	if err != nil {
		return Resolved{}, err
	}
	if resolved.Workspace != session.SourceWorkspace || resolved.Profile.Backend != session.Backend || resolved.Profile.Workspace.Mode != session.WorkspaceMode || resolved.Profile.Network != session.Network || credentialPolicyHash(resolved.Profile.Credentials) != session.CredentialPolicy {
		return Resolved{}, errors.New("the selected profile's workspace or runtime policy changed; create a new session instead of attaching")
	}
	return resolved, nil
}

func openEditor(session *Session, resolved Resolved, revokeGitHub bool) error {
	if session.Backend == BackendDocker {
		if err := requireRootlessDocker(); err != nil {
			return err
		}
		if err := ensureContainerRunning(session, resolved); err != nil {
			return err
		}
		if err := ensureVSCodeExtension("ms-vscode-remote.remote-containers"); err != nil {
			return err
		}
		encoded, _ := json.Marshal(map[string]string{"containerName": session.ContainerName})
		uri := "vscode-remote://attached-container+" + hex.EncodeToString(encoded) + "/workspace"
		return execCommand("code", "--folder-uri", uri).Run()
	}
	if session.Backend == BackendVM {
		address, err := prepareVM(session, resolved, false, revokeGitHub)
		if err != nil {
			return err
		}
		session.VMAddress = address
		if err := ensureVSCodeExtension("ms-vscode-remote.remote-ssh"); err != nil {
			return err
		}
		return execCommand("code", "--remote", "ssh-remote+"+session.VMName, "/workspace").Run()
	}
	return errors.New("VS Code attachment is not supported for this backend")
}

func prepareAndOpenDocker(session *Session, resolved Resolved, create bool) error {
	if err := requireRootlessDocker(); err != nil {
		return err
	}
	if create {
		if err := ensureDockerImage(session.SessionDir); err != nil {
			return err
		}
		if err := createContainer(session, resolved); err != nil {
			return err
		}
	} else if err := ensureContainerRunning(session, resolved); err != nil {
		return err
	}
	if err := ensureVSCodeExtension("ms-vscode-remote.remote-containers"); err != nil {
		return err
	}
	encoded, _ := json.Marshal(map[string]string{"containerName": session.ContainerName})
	uri := "vscode-remote://attached-container+" + hex.EncodeToString(encoded) + "/workspace"
	return execCommand("code", "--folder-uri", uri).Run()
}

func ensureContainerRunning(session *Session, resolved Resolved) error {
	status, err := dockerStatus(session)
	if err != nil {
		return err
	}
	if status == "missing" {
		return fmt.Errorf("container %s no longer exists", session.ContainerName)
	}
	if status != "running" {
		if err := dockerAction(session, "start"); err != nil {
			return fmt.Errorf("start container: %w", err)
		}
	}
	return nil
}

func ensureVSCodeExtension(extension string) error {
	if _, err := exec.LookPath("code"); err != nil {
		return errors.New("VS Code command line is not installed")
	}
	output, err := execCommand("code", "--list-extensions").Output()
	if err == nil && strings.Contains(strings.ToLower(string(output)), strings.ToLower(extension)) {
		return nil
	}
	cmd := execCommand("code", "--install-extension", extension)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("install VS Code extension %s: %w", extension, err)
	}
	return nil
}

func openVMEditor(session *Session) error {
	if err := ensureVSCodeExtension("ms-vscode-remote.remote-ssh"); err != nil {
		return err
	}
	return execCommand("code", "--remote", "ssh-remote+"+session.VMName, "/workspace").Run()
}

func stopCommand(args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: devfence stop SESSION_ID [--force]")
	}
	force := len(args) == 2 && args[1] == "--force"
	if len(args) == 2 && !force {
		return fmt.Errorf("unknown stop option %q", args[1])
	}
	stateDir, err := StatePath()
	if err != nil {
		return err
	}
	session, err := loadSession(stateDir, args[0])
	if err != nil {
		return err
	}
	switch session.Backend {
	case BackendBubblewrap:
		return errors.New("Bubblewrap sessions are foreground-only; interrupt the running command in its terminal")
	case BackendDocker:
		if err := dockerAction(session, "stop"); err != nil {
			return err
		}
	case BackendVM:
		if err := stopVM(session, force); err != nil {
			return err
		}
	}
	stopSSHAgent(session)
	session.Status = "stopped"
	return saveSession(stateDir, session)
}

func deleteCommand(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: devfence delete SESSION_ID --yes [--force]")
	}
	yes, force := false, false
	for _, arg := range args[1:] {
		switch arg {
		case "--yes":
			yes = true
		case "--force":
			force = true
		default:
			return fmt.Errorf("unknown delete option %q", arg)
		}
	}
	if !yes {
		return errors.New("deleting a persistent environment requires --yes")
	}
	stateDir, err := StatePath()
	if err != nil {
		return err
	}
	session, err := loadSession(stateDir, args[0])
	if err != nil {
		return err
	}
	if err := checkSessionHasNoUnpreservedWork(session); err != nil && !force {
		return fmt.Errorf("%w; use --force only after reviewing the work", err)
	}
	if session.Backend == BackendVM && !force {
		return errors.New("deleting a VM also removes its private disk and guest-only files; pass --force after reviewing the data")
	}
	if session.Backend == BackendDocker {
		if status, err := dockerStatus(session); err != nil {
			return err
		} else if status != "missing" {
			if err := dockerAction(session, "stop"); err != nil {
				return err
			}
		}
		if err := dockerAction(session, "rm"); err != nil {
			return err
		}
	}
	if session.Backend == BackendVM {
		if err := deleteVM(session, force); err != nil {
			return err
		}
	}
	stopSSHAgent(session)
	return os.RemoveAll(session.SessionDir)
}

func checkSessionHasNoUnpreservedWork(session *Session) error {
	if session.CopyStatePath != "" {
		state, err := readCopyState(session.CopyStatePath)
		if err != nil {
			return err
		}
		plan, err := planExport(state)
		if err != nil {
			return err
		}
		if len(plan.Changes)+len(plan.Deleted)+len(plan.Conflicts) > 0 {
			return errors.New("copied workspace contains unexported changes or deletions")
		}
	}
	if session.WorkspaceMode == "live" {
		if _, err := gitTopLevel(session.SourceWorkspace); err == nil {
			dirty, err := gitDirty(session.SourceWorkspace)
			if err != nil {
				return fmt.Errorf("could not verify live worktree: %w", err)
			}
			if dirty {
				return errors.New("live Git worktree has uncommitted changes")
			}
			ahead, err := gitAhead(session.SourceWorkspace)
			if err != nil {
				return fmt.Errorf("could not verify commits against an upstream branch: %w", err)
			}
			if ahead > 0 {
				return errors.New("live Git worktree has commits ahead of its upstream")
			}
		} else if _, err := os.Stat(filepath.Join(session.SourceWorkspace, ".git")); err == nil {
			return errors.New("could not verify live Git worktree; use --force only after reviewing the work")
		}
	}
	hasHomeFiles, err := homeHasFiles(session.HomeDir)
	if err != nil {
		return fmt.Errorf("inspect session home directory: %w", err)
	}
	if hasHomeFiles {
		return fmt.Errorf("session home directory contains files that deletion would remove: %s", session.HomeDir)
	}
	return nil
}

func homeHasFiles(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

func gitDirty(path string) (bool, error) {
	output, err := execCommand("git", "-C", path, "status", "--porcelain=v1", "--untracked-files=all").Output()
	if err != nil {
		return false, err
	}
	return len(strings.TrimSpace(string(output))) > 0, nil
}

func gitAhead(path string) (int, error) {
	upstream, err := execCommand("git", "-C", path, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}").Output()
	if err != nil {
		return 0, err
	}
	output, err := execCommand("git", "-C", path, "rev-list", "--count", strings.TrimSpace(string(upstream))+"..HEAD").Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(output)))
}

func exportCommand(args []string, stdout *os.File) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: devfence export SESSION_ID [--apply]")
	}
	apply := len(args) == 2 && args[1] == "--apply"
	if len(args) == 2 && !apply {
		return fmt.Errorf("unknown export option %q", args[1])
	}
	stateDir, err := StatePath()
	if err != nil {
		return err
	}
	session, err := loadSession(stateDir, args[0])
	if err != nil {
		return err
	}
	if session.CopyStatePath == "" {
		return errors.New("this session does not use a copied workspace")
	}
	status, _ := sessionStatus(session)
	if status == "running" {
		return errors.New("stop the session before exporting to avoid concurrent changes")
	}
	copyState, err := readCopyState(session.CopyStatePath)
	if err != nil {
		return err
	}
	plan, err := planExport(copyState)
	if err != nil {
		return err
	}
	for _, change := range plan.Changes {
		fmt.Fprintf(stdout, "%s %s\n", change.Action, change.Path)
	}
	for _, path := range plan.Deleted {
		fmt.Fprintf(stdout, "deleted-in-copy (host unchanged) %s\n", path)
	}
	for _, path := range plan.Conflicts {
		fmt.Fprintf(stdout, "conflict %s\n", path)
	}
	if len(plan.Conflicts) > 0 {
		return errors.New("export has conflicts; resolve them before applying")
	}
	if !apply {
		fmt.Fprintln(stdout, "Preview only; use --apply to copy eligible changes back")
		return nil
	}
	if err := applyExport(copyState, plan); err != nil {
		return err
	}
	if err := writeCopyState(session.CopyStatePath, copyState); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Export applied; deleted files were left unchanged on the host")
	return nil
}

func cleanupFailedSession(session *Session) {
	stopSSHAgent(session)
	if session.Backend == BackendDocker {
		_ = dockerAction(session, "rm")
	}
	if session.Backend == BackendVM {
		_ = deleteVM(session, true)
	}
	_ = os.RemoveAll(session.SessionDir)
}
