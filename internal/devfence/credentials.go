package devfence

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var sshAgentPIDPattern = regexp.MustCompile(`SSH_AGENT_PID=([0-9]+)`)

func prepareCredentials(session *Session, profile Profile) error {
	if session.Status == "starting" {
		if err := prepareForwardedTools(session, profile); err != nil {
			return err
		}
	}
	policy := profile.Credentials
	if policy.SSHKey != "" {
		if err := ensureSSHAgent(session, policy.SSHKey); err != nil {
			return err
		}
	} else if session.SSHKeyFingerprint != "" {
		_ = sshAdd(filepath.Join(session.SSHAgentDir, "agent.sock"), "-D")
		stopSSHAgent(session)
		session.SSHKeyName = ""
		session.SSHKeyFingerprint = ""
	}
	tokenCommand := profile.Tools.GH.Authentication.TokenCommand
	if len(tokenCommand) > 0 {
		if err := retrieveGitHubToken(session, tokenCommand); err != nil {
			return err
		}
		session.GitHubEnabled = true
	} else {
		_ = os.Remove(filepath.Join(session.SessionDir, "credentials", "github-token"))
		session.GitHubEnabled = false
	}
	return saveSession(filepath.Dir(filepath.Dir(session.SessionDir)), session)
}

func retrieveGitHubToken(session *Session, command []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := execCommandContext(ctx, command[0], command[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("GitHub credential command failed: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" || len(token) > 8192 || strings.ContainsAny(token, "\r\n\x00") {
		return errors.New("GitHub credential command returned an empty or invalid token")
	}
	credentialDir := filepath.Join(session.SessionDir, "credentials")
	if err := os.MkdirAll(credentialDir, 0700); err != nil {
		return err
	}
	path := filepath.Join(credentialDir, "github-token")
	if err := os.WriteFile(path, []byte(token), 0600); err != nil {
		return err
	}
	return nil
}

func ensureSSHAgent(session *Session, keyPath string) error {
	keyPath, err := expandPath(keyPath)
	if err != nil {
		return err
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		return fmt.Errorf("SSH key %s: %w", filepath.Base(keyPath), err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("configured SSH key is not a regular file")
	}
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("configured SSH private key must not be accessible by group or others")
	}
	if err := os.MkdirAll(filepath.Join(session.SessionDir, "ssh-agent"), 0700); err != nil {
		return err
	}
	session.SSHAgentDir = filepath.Join(session.SessionDir, "ssh-agent")
	socket := filepath.Join(session.SSHAgentDir, "agent.sock")
	if len(socket) >= 100 {
		return errors.New("session state path is too long for a Unix SSH agent socket; set a shorter XDG_STATE_HOME")
	}
	fingerprint, err := sshFingerprint(keyPath)
	if err != nil {
		return err
	}
	if session.SSHKeyFingerprint != "" && session.SSHKeyFingerprint != fingerprint {
		return errors.New("the selected SSH key changed since this session started; create a new session to change identities")
	}
	loaded := currentAgentKeys(socket)
	if len(loaded) == 1 && loaded[0] == fingerprint {
		session.SSHKeyName = filepath.Base(keyPath)
		session.SSHKeyFingerprint = fingerprint
		return nil
	}
	if len(loaded) > 0 {
		_ = sshAdd(socket, "-D")
	}
	if len(loaded) == 0 {
		_ = os.Remove(socket)
		output, err := execCommand("ssh-agent", "-a", socket, "-s").Output()
		if err != nil {
			return fmt.Errorf("start isolated SSH agent: %w", err)
		}
		pidMatch := sshAgentPIDPattern.FindSubmatch(output)
		if len(pidMatch) != 2 {
			return errors.New("could not determine isolated SSH agent process ID")
		}
		session.SSHAgentPID, _ = strconv.Atoi(string(pidMatch[1]))
	}
	if err := sshAddWithInput(socket, keyPath); err != nil {
		return fmt.Errorf("load selected SSH key: %w", err)
	}
	loaded = currentAgentKeys(socket)
	if len(loaded) != 1 || loaded[0] != fingerprint {
		return errors.New("isolated SSH agent does not contain exactly the selected key")
	}
	session.SSHKeyName = filepath.Base(keyPath)
	session.SSHKeyFingerprint = fingerprint
	return nil
}

func sshFingerprint(keyPath string) (string, error) {
	publicPath := keyPath + ".pub"
	if _, err := os.Stat(publicPath); err != nil {
		publicPath = keyPath
	}
	output, err := execCommand("ssh-keygen", "-lf", publicPath).Output()
	if err != nil {
		return "", fmt.Errorf("read selected SSH key fingerprint: %w", err)
	}
	fields := strings.Fields(string(output))
	if len(fields) < 2 {
		return "", errors.New("ssh-keygen returned an invalid fingerprint")
	}
	return fields[1], nil
}

func currentAgentKeys(socket string) []string {
	cmd := execCommand("ssh-add", "-l")
	cmd.Env = replaceEnv(os.Environ(), "SSH_AUTH_SOCK", socket)
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	var keys []string
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 {
			keys = append(keys, fields[1])
		}
	}
	return keys
}

func sshAdd(socket string, args ...string) error {
	cmd := execCommand("ssh-add", args...)
	cmd.Env = replaceEnv(os.Environ(), "SSH_AUTH_SOCK", socket)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func sshAddWithInput(socket, keyPath string) error {
	return sshAdd(socket, keyPath)
}

func stopSSHAgent(session *Session) {
	if session.SSHAgentPID == 0 || session.SSHAgentDir == "" {
		return
	}
	cmd := execCommand("ssh-agent", "-k")
	cmd.Env = replaceEnv(os.Environ(), "SSH_AUTH_SOCK", filepath.Join(session.SSHAgentDir, "agent.sock"))
	cmd.Env = replaceEnv(cmd.Env, "SSH_AGENT_PID", fmt.Sprintf("%d", session.SSHAgentPID))
	_ = cmd.Run()
	session.SSHAgentPID = 0
}
