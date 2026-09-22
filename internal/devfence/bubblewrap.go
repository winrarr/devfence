package devfence

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func runBubblewrap(session *Session, resolved Resolved, command []string) error {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return errors.New("Bubblewrap is not installed")
	}
	if len(command) == 0 {
		command = []string{"/bin/bash", "-l"}
	}
	args, err := bubblewrapArgs(session, resolved, command)
	if err != nil {
		return err
	}
	cmd := execCommand("bwrap", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Bubblewrap session: %w", err)
	}
	return nil
}

func bubblewrapArgs(session *Session, resolved Resolved, command []string) ([]string, error) {
	args := []string{
		"--die-with-parent", "--new-session", "--unshare-all", "--clearenv",
		"--ro-bind", "/", "/",
		"--tmpfs", "/home",
		"--tmpfs", "/run",
		"--tmpfs", "/tmp",
		"--tmpfs", "/var/tmp",
		"--tmpfs", "/mnt",
		"--tmpfs", "/media",
		"--tmpfs", "/root",
		"--proc", "/proc",
		"--dev", "/dev",
	}
	if resolved.Profile.Network == "full" {
		args = append(args, "--share-net")
	}
	if err := addHomeParents(&args, session.PresentedWorkspace); err != nil {
		return nil, err
	}
	args = append(args, "--dir", "/home/devfence", "--bind", session.HomeDir, "/home/devfence")
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
	env := []struct{ key, value string }{
		{"HOME", "/home/devfence"},
		{"USER", "devfence"},
		{"LOGNAME", "devfence"},
		{"PATH", "/home/devfence/.local/bin:/home/devfence/node_modules/.bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		{"TMPDIR", "/tmp"},
		{"GIT_CONFIG_NOSYSTEM", "1"},
		{"GIT_CONFIG_GLOBAL", "/home/devfence/.gitconfig"},
	}
	for _, item := range env {
		args = append(args, "--setenv", item.key, item.value)
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
