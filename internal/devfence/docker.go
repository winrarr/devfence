package devfence

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

//go:embed runtime/Dockerfile runtime/devfence-exec
var dockerAssets embed.FS

func runDocker(session *Session, resolved Resolved, command []string, create bool) error {
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
	} else {
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
	}
	if len(command) == 0 {
		command = []string{"/bin/bash", "-l"}
	}
	return execInContainer(session, resolved, command)
}

func requireRootlessDocker() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("Docker CLI is not installed")
	}
	output, err := execCommand("docker", "info", "--format", "{{json .SecurityOptions}}").Output()
	if err != nil {
		return fmt.Errorf("Docker daemon is unavailable: %w", err)
	}
	if !strings.Contains(strings.ToLower(string(output)), "rootless") {
		return errors.New("container backend requires a rootless Docker daemon; the selected daemon does not report rootless mode")
	}
	return nil
}

func ensureDockerImage(sessionDir string) error {
	image, err := dockerImageReference()
	if err != nil {
		return err
	}
	if err := execCommand("docker", "image", "inspect", image).Run(); err == nil {
		return nil
	}
	contextDir := filepath.Join(sessionDir, "image-context")
	if err := os.MkdirAll(contextDir, 0700); err != nil {
		return err
	}
	for _, path := range []string{"runtime/Dockerfile", "runtime/devfence-exec"} {
		data, err := fs.ReadFile(dockerAssets, path)
		if err != nil {
			return err
		}
		name := filepath.Base(path)
		mode := os.FileMode(0600)
		if name == "devfence-exec" {
			mode = 0700
		}
		if err := os.WriteFile(filepath.Join(contextDir, name), data, mode); err != nil {
			return err
		}
	}
	cmd := execCommand("docker", "build", "--tag", image, contextDir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build Devfence container image: %w", err)
	}
	return nil
}

func dockerImageReference() (string, error) {
	hash := sha256.New()
	for _, path := range []string{"runtime/Dockerfile", "runtime/devfence-exec"} {
		data, err := fs.ReadFile(dockerAssets, path)
		if err != nil {
			return "", err
		}
		_, _ = hash.Write([]byte(path + "\x00"))
		_, _ = hash.Write(data)
	}
	return "devfence:ubuntu-24.04-" + hex.EncodeToString(hash.Sum(nil)[:6]), nil
}

func createContainer(session *Session, resolved Resolved) error {
	for _, value := range []string{session.PresentedWorkspace, session.SessionDir} {
		if strings.Contains(value, ",") {
			return errors.New("container bind mount paths cannot contain commas")
		}
	}
	uid, gid := os.Getuid(), os.Getgid()
	network := "bridge"
	if session.Network == "none" {
		network = "none"
	}
	args := []string{
		"run", "--detach", "--name", session.ContainerName,
		"--label", "devfence.managed=true",
		"--label", "devfence.session=" + session.ID,
		"--user", strconv.Itoa(uid) + ":" + strconv.Itoa(gid),
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true",
		"--read-only",
		"--tmpfs", "/tmp:rw,nosuid,nodev,size=1g",
		"--tmpfs", "/var/tmp:rw,nosuid,nodev,size=256m",
		"--pids-limit", "512",
		"--network", network,
		"--mount", "type=bind,src=" + session.PresentedWorkspace + ",dst=/workspace",
		"--mount", "type=bind,src=" + session.HomeDir + ",dst=/home/devfence",
		"--mount", "type=bind,src=" + filepath.Join(session.SessionDir, "credentials") + ",dst=/run/secrets,readonly",
		"--workdir", containerWorkingDirectory(session),
		"--env", "HOME=/home/devfence",
		"--env", "PATH=/home/devfence/.local/bin:/home/devfence/node_modules/.bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"--env", "GIT_CONFIG_NOSYSTEM=1",
		"--env", "GIT_CONFIG_GLOBAL=/home/devfence/.gitconfig",
	}
	if session.SSHKeyFingerprint != "" {
		args = append(args,
			"--mount", "type=bind,src="+session.SSHAgentDir+",dst=/run/devfence/ssh",
			"--env", "SSH_AUTH_SOCK=/run/devfence/ssh/agent.sock",
		)
	}
	if resolved.Profile.Resources.MemoryMiB > 0 {
		args = append(args, "--memory", fmt.Sprintf("%dm", resolved.Profile.Resources.MemoryMiB))
	}
	if resolved.Profile.Resources.VCPUs > 0 {
		args = append(args, "--cpus", strconv.Itoa(resolved.Profile.Resources.VCPUs))
	}
	if value := os.Getenv("TERM"); value != "" {
		args = append(args, "--env", "TERM="+value)
	}
	image, err := dockerImageReference()
	if err != nil {
		return err
	}
	args = append(args, image, "sleep", "infinity")
	cmd := execCommand("docker", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("create rootless container: %w", err)
	}
	return nil
}

func execInContainer(session *Session, resolved Resolved, command []string) error {
	args := []string{"exec"}
	if hasTerminal() {
		args = append(args, "-it")
	} else {
		args = append(args, "-i")
	}
	args = append(args,
		"--workdir", containerWorkingDirectory(session),
		"--env", "HOME=/home/devfence",
		"--env", "GIT_CONFIG_NOSYSTEM=1",
		"--env", "GIT_CONFIG_GLOBAL=/home/devfence/.gitconfig",
	)
	if session.SSHKeyFingerprint != "" {
		args = append(args, "--env", "SSH_AUTH_SOCK=/run/devfence/ssh/agent.sock")
	}
	if value := os.Getenv("TERM"); value != "" {
		args = append(args, "--env", "TERM="+value)
	}
	args = append(args, session.ContainerName, "/usr/local/bin/devfence-exec")
	args = append(args, command...)
	cmd := execCommand("docker", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("container command: %w", err)
	}
	return nil
}

func containerWorkingDirectory(session *Session) string {
	relative, err := filepath.Rel(session.SourceWorkspace, session.WorkingDir)
	if err != nil || relative == "." {
		return "/workspace"
	}
	return filepath.Join("/workspace", relative)
}

func hasTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func dockerAction(session *Session, action string) error {
	if err := requireRootlessDocker(); err != nil {
		return err
	}
	status, err := dockerStatus(session)
	if err != nil {
		return err
	}
	if status == "missing" {
		if action == "rm" {
			return nil
		}
		return fmt.Errorf("container %s no longer exists", session.ContainerName)
	}
	args := []string{"container", action, session.ContainerName}
	if action == "rm" {
		args = []string{"container", "rm", "--force", session.ContainerName}
	}
	cmd := execCommand("docker", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

func dockerStatus(session *Session) (string, error) {
	format := dockerInspectFormat()
	cmd := execCommand("docker", "inspect", "--format", format, session.ContainerName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.ToLower(string(output))
		if strings.Contains(message, "no such object") || strings.Contains(message, "no such container") {
			return "missing", nil
		}
		return "", fmt.Errorf("inspect container %s: %w", session.ContainerName, err)
	}
	fields := strings.SplitN(strings.TrimSpace(string(output)), "\t", 2)
	if len(fields) != 2 || fields[0] != session.ID {
		return "", fmt.Errorf("container %s is not owned by this Devfence session", session.ContainerName)
	}
	return fields[1], nil
}

func dockerInspectFormat() string {
	return "{{printf " + strconv.Quote("%s\t%s") + " (index .Config.Labels " + strconv.Quote("devfence.session") + ") .State.Status}}"
}

func dockerRunCommand(ctx context.Context, args ...string) ([]byte, error) {
	return commandOutput(ctx, "docker", args...)
}
