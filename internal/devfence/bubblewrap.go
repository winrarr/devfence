package devfence

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

func runBubblewrap(session *Session, resolved Resolved, command []string) error {
	bwrapPath, err := exec.LookPath("bwrap")
	if err != nil {
		return errors.New("Bubblewrap is not installed")
	}
	if len(command) == 0 {
		command = []string{"/bin/bash"}
	}
	var hostTools []*hostToolInstall
	for _, configured := range []struct {
		name    string
		enabled bool
	}{{"codex", resolved.Profile.Tools.Codex.Enabled}, {"claude", resolved.Profile.Tools.Claude.Enabled}} {
		install, err := hostToolInstallation(configured.name, configured.enabled)
		if err != nil {
			return err
		}
		if install != nil {
			hostTools = append(hostTools, install)
		}
	}
	extraFiles := make([]*os.File, 0, 3)
	resolverFile, resolverTarget, err := openHostResolverFile()
	if err != nil {
		return err
	}
	if resolverFile != nil {
		defer resolverFile.Close()
	}
	resolverFD := 0
	if resolverFile != nil {
		resolverFD = 4
		extraFiles = append(extraFiles, resolverFile)
	}
	args, err := bubblewrapArgs(session, resolved, command, hostTools, resolverTarget, resolverFD)
	if err != nil {
		return err
	}
	interactive := isTerminal(os.Stdin) && isTerminal(os.Stdout)
	if interactive {
		args = withoutOptionBeforeCommand(args, "--new-session")
	}
	commandSeparator := -1
	for index, arg := range args {
		if arg == "--" {
			commandSeparator = index
			break
		}
	}
	if commandSeparator < 0 || commandSeparator == len(args)-1 {
		return errors.New("Bubblewrap arguments are missing the command")
	}
	commandArgs := args[commandSeparator+1:]
	argsFile, err := nulSeparatedArgsFile(session.SessionDir, args[:commandSeparator])
	if err != nil {
		return fmt.Errorf("prepare Bubblewrap arguments: %w", err)
	}
	defer argsFile.Close()
	extraFiles = append([]*os.File{argsFile}, extraFiles...)

	var cmd *exec.Cmd
	if interactive {
		scriptPath, err := exec.LookPath("script")
		if err != nil {
			return errors.New("interactive Bubblewrap sessions need util-linux `script` to provide a private terminal")
		}
		commandFile, err := nulSeparatedArgsFile(session.SessionDir, commandArgs)
		if err != nil {
			return fmt.Errorf("prepare Bubblewrap command: %w", err)
		}
		defer commandFile.Close()
		// util-linux script closes inherited descriptors before running its command.
		// Reopen the unlinked argument files through this process, then let xargs pass
		// the command as normal argv entries; Bubblewrap's --args file carries options.
		scriptCommand := `exec 3< "$DEVFENCE_BWRAP_ARGS_PATH" && exec xargs -0 -a "$DEVFENCE_BWRAP_COMMAND_PATH" "$DEVFENCE_BWRAP_PATH" --args 3 --`
		env := []string{
			"PATH=/usr/bin:/bin",
			"DEVFENCE_BWRAP_PATH=" + bwrapPath,
			fmt.Sprintf("DEVFENCE_BWRAP_ARGS_PATH=/proc/%d/fd/%d", os.Getpid(), argsFile.Fd()),
			fmt.Sprintf("DEVFENCE_BWRAP_COMMAND_PATH=/proc/%d/fd/%d", os.Getpid(), commandFile.Fd()),
		}
		if resolverFile != nil {
			scriptCommand = `exec 3< "$DEVFENCE_BWRAP_ARGS_PATH" && exec 4< "$DEVFENCE_BWRAP_RESOLVER_PATH" && exec xargs -0 -a "$DEVFENCE_BWRAP_COMMAND_PATH" "$DEVFENCE_BWRAP_PATH" --args 3 --`
			env = append(env, fmt.Sprintf("DEVFENCE_BWRAP_RESOLVER_PATH=/proc/%d/fd/%d", os.Getpid(), resolverFile.Fd()))
		}
		cmd = execCommand(scriptPath, "-qefc", scriptCommand, "/dev/null")
		cmd.Env = env
		cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	} else {
		cmdArgs := append([]string{"--args", "3", "--"}, commandArgs...)
		cmd = execCommand(bwrapPath, cmdArgs...)
		cmd.Env = []string{}
		cmd.ExtraFiles = extraFiles
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Bubblewrap session: %w", err)
	}
	return nil
}

func bubblewrapArgs(session *Session, resolved Resolved, command []string, hostTools []*hostToolInstall, resolverTarget string, resolverFD int) ([]string, error) {
	args := []string{
		"--die-with-parent", "--new-session", "--unshare-all", "--clearenv",
		"--ro-bind", "/", "/",
	}
	if info, err := os.Stat("/etc/profile.d/im-config_wayland.sh"); err == nil && info.Mode().IsRegular() {
		// This GUI profile hook writes to the host journal, which is deliberately
		// absent from Bubblewrap's private /run.
		args = append(args, "--ro-bind", "/dev/null", "/etc/profile.d/im-config_wayland.sh")
	}
	var nodeRuntime string
	dependenciesByTarget := make(map[string]string)
	for _, install := range hostTools {
		if install.name != "codex" && install.name != "claude" {
			return nil, fmt.Errorf("unsupported forwarded host tool %q", install.name)
		}
		if install.nodeRuntime != "" {
			if nodeRuntime != "" && nodeRuntime != install.nodeRuntime {
				return nil, errors.New("forwarded host tools require different Node.js runtimes")
			}
			nodeRuntime = install.nodeRuntime
		}
		for _, dependency := range install.dependencies {
			if existing, ok := dependenciesByTarget[dependency.target]; ok && existing != dependency.source {
				return nil, fmt.Errorf("forwarded host tools have conflicting Node.js dependency at %s", dependency.target)
			}
			dependenciesByTarget[dependency.target] = dependency.source
		}
	}
	if len(hostTools) > 0 {
		args = append(args, "--tmpfs", "/opt", "--dir", "/opt/devfence", "--dir", "/opt/devfence/bin")
		if nodeRuntime != "" {
			args = append(args, "--dir", "/opt/devfence/node", "--dir", "/opt/devfence/node/bin")
			args = append(args, "--ro-bind", nodeRuntime, "/opt/devfence/node/bin/node")
		}
		for _, install := range hostTools {
			toolRoot := filepath.Join("/opt/devfence", install.name)
			args = append(args, "--dir", toolRoot)
			if install.isDirectory {
				args = append(args, "--ro-bind", install.source, toolRoot)
			} else {
				entryPath := filepath.Join(toolRoot, install.entryPoint)
				args = append(args, "--ro-bind", install.source, entryPath)
			}
			args = append(args, "--symlink", filepath.Join(toolRoot, install.entryPoint), filepath.Join("/opt/devfence/bin", install.name))
		}
		if len(dependenciesByTarget) > 0 {
			moduleRoot := "/opt/devfence/node_modules"
			args = append(args, "--dir", moduleRoot)
			createdDirs := map[string]bool{moduleRoot: true}
			dependencyTargets := make([]string, 0, len(dependenciesByTarget))
			for target := range dependenciesByTarget {
				dependencyTargets = append(dependencyTargets, target)
			}
			sort.Strings(dependencyTargets)
			for _, target := range dependencyTargets {
				parent := filepath.Dir(target)
				if parent != moduleRoot && !createdDirs[parent] {
					createdDirs[parent] = true
					args = append(args, "--dir", parent)
				}
				args = append(args, "--ro-bind", dependenciesByTarget[target], target)
			}
		}
	}
	args = append(args,
		"--tmpfs", "/home",
		"--tmpfs", "/run",
	)
	if resolverTarget != "" {
		if !within("/run", resolverTarget) || resolverTarget == "/run" {
			return nil, fmt.Errorf("host resolver target %q is outside the private runtime directory", resolverTarget)
		}
		parent := filepath.Dir(resolverTarget)
		if parent != "/run" {
			for current := "/run"; current != parent; {
				relative, err := filepath.Rel(current, parent)
				if err != nil || relative == "." || strings.HasPrefix(relative, "..") {
					return nil, fmt.Errorf("invalid host resolver target %q", resolverTarget)
				}
				component := strings.Split(relative, string(filepath.Separator))[0]
				current = filepath.Join(current, component)
				args = append(args, "--dir", current)
			}
		}
		args = append(args, "--file", strconv.Itoa(resolverFD), resolverTarget)
	}
	args = append(args,
		"--tmpfs", "/tmp",
		"--tmpfs", "/var/tmp",
		"--tmpfs", "/mnt",
		"--tmpfs", "/media",
		"--tmpfs", "/root",
		"--proc", "/proc",
		"--dev", "/dev",
	)
	if resolved.Profile.Network == "full" {
		args = append(args, "--share-net")
	}
	if err := addHomeParents(&args, session.PresentedWorkspace); err != nil {
		return nil, err
	}
	args = append(args, "--dir", "/home/devfence", "--bind", session.HomeDir, "/home/devfence")
	forwardedDirectories, err := forwardedDirectoryTargets(resolved.Profile.Tools, session.HomeDir)
	if err != nil {
		return nil, err
	}
	for _, target := range forwardedDirectories {
		relative, _ := filepath.Rel(session.HomeDir, target)
		args = append(args, "--ro-bind", target, filepath.Join("/home/devfence", relative))
	}
	for _, share := range resolved.Profile.ProjectShares {
		source, err := safeForwardTarget(session.HomeDir, share.Target)
		if err != nil {
			return nil, err
		}
		flag := "--ro-bind"
		if share.Mode == "mount" {
			source = share.Source
			if share.Access == "read-write" {
				flag = "--bind"
			}
		}
		destination := filepath.Join("/home/devfence", filepath.FromSlash(share.Target))
		args = append(args, flag, source, destination)
	}
	args = append(args, "--dir", "/run/devfence")
	if session.SSHKeyFingerprint != "" {
		args = append(args, "--dir", "/run/devfence/ssh")
		args = append(args, "--bind", session.SSHAgentDir, "/run/devfence/ssh")
	}
	if session.GitHubEnabled {
		tokenPath := filepath.Join(session.SessionDir, "credentials", "github-token")
		args = append(args, "--ro-bind", tokenPath, "/run/devfence/github-token")
	}
	if session.WorkspaceMode == "live" {
		home, _ := os.UserHomeDir()
		for _, path := range resolved.Protected {
			if within(session.SourceWorkspace, path) || path == session.SourceWorkspace {
				continue
			}
			if within(home, path) {
				continue
			}
			if _, err := os.Stat(path); err == nil {
				args = append(args, "--tmpfs", path)
			}
		}
	}
	args = append(args, "--bind", session.PresentedWorkspace, session.PresentedWorkspace)

	workingDir := session.PresentedWorkspace
	if relative, err := filepath.Rel(session.SourceWorkspace, session.WorkingDir); err == nil {
		workingDir = filepath.Join(session.PresentedWorkspace, relative)
	}
	if session.GitHubEnabled {
		command = append([]string{"/bin/sh", "-c", `GH_TOKEN="$(cat /run/devfence/github-token)"; export GH_TOKEN; exec "$@"`, "devfence"}, command...)
	}
	path := "/home/devfence/.local/bin:/home/devfence/node_modules/.bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	if len(hostTools) > 0 {
		path = "/opt/devfence/bin:" + path
		if nodeRuntime != "" {
			path = "/opt/devfence/node/bin:" + path
		}
	}
	env := []struct{ key, value string }{
		{"HOME", "/home/devfence"},
		{"USER", "devfence"},
		{"LOGNAME", "devfence"},
		{"PATH", path},
		{"TMPDIR", "/tmp"},
		{"GIT_CONFIG_NOSYSTEM", "1"},
		{"GIT_CONFIG_GLOBAL", "/home/devfence/.gitconfig"},
	}
	for _, item := range env {
		args = append(args, "--setenv", item.key, item.value)
	}
	if len(dependenciesByTarget) > 0 {
		args = append(args, "--setenv", "NODE_PATH", "/opt/devfence/node_modules")
	}
	for _, key := range []string{"TERM", "LANG", "LC_ALL", "COLORTERM"} {
		if value := os.Getenv(key); value != "" {
			args = append(args, "--setenv", key, value)
		}
	}
	if session.SSHKeyFingerprint != "" {
		args = append(args, "--setenv", "SSH_AUTH_SOCK", "/run/devfence/ssh/agent.sock")
	}
	args = append(args, "--chdir", workingDir)
	args = append(args, "--")
	args = append(args, command...)
	return args, nil
}

func openHostResolverFile() (*os.File, string, error) {
	link, err := os.Readlink("/etc/resolv.conf")
	if err != nil {
		return nil, "", nil
	}
	target := link
	if !filepath.IsAbs(target) {
		target = filepath.Join("/etc", target)
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		return nil, "", fmt.Errorf("resolve host DNS configuration: %w", err)
	}
	if !within("/run", target) {
		return nil, "", nil
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, "", fmt.Errorf("inspect host DNS configuration: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return nil, "", errors.New("host DNS configuration must be a regular file no larger than 64 KiB")
	}
	file, err := os.Open(target)
	if err != nil {
		return nil, "", fmt.Errorf("open host DNS configuration: %w", err)
	}
	return file, filepath.Clean(target), nil
}

func nulSeparatedArgsFile(dir string, args []string) (*os.File, error) {
	file, err := os.CreateTemp(dir, ".bwrap-args-*")
	if err != nil {
		return nil, err
	}
	name := file.Name()
	remove := func() {
		file.Close()
		os.Remove(name)
	}
	for _, arg := range args {
		if strings.ContainsRune(arg, '\x00') {
			remove()
			return nil, errors.New("Bubblewrap argument contains a NUL byte")
		}
		if _, err := io.WriteString(file, arg+"\x00"); err != nil {
			remove()
			return nil, err
		}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		remove()
		return nil, err
	}
	if err := os.Remove(name); err != nil {
		remove()
		return nil, err
	}
	return file, nil
}

func withoutOptionBeforeCommand(args []string, unwanted string) []string {
	for index, arg := range args {
		if arg == "--" {
			break
		}
		if arg == unwanted {
			return append(args[:index], args[index+1:]...)
		}
	}
	return args
}

func isTerminal(file *os.File) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&termios)))
	return errno == 0
}

func addHomeParents(args *[]string, workspace string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if !within(home, workspace) {
		return nil
	}
	parent := filepath.Dir(workspace)
	relative, err := filepath.Rel("/home", parent)
	if err != nil {
		return err
	}
	current := "/home"
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "." || component == "" {
			continue
		}
		current = filepath.Join(current, component)
		*args = append(*args, "--dir", current)
	}
	return nil
}
