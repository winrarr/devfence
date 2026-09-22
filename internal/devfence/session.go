package devfence

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Session struct {
	ID                 string    `json:"id"`
	Backend            Backend   `json:"backend"`
	ProfileName        string    `json:"profile"`
	SourceWorkspace    string    `json:"sourceWorkspace"`
	PresentedWorkspace string    `json:"presentedWorkspace"`
	WorkingDir         string    `json:"workingDirectory"`
	WorkspaceMode      string    `json:"workspaceMode"`
	Network            string    `json:"network"`
	ProtectedPaths     []string  `json:"protectedPaths,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
	Status             string    `json:"status"`
	ContainerName      string    `json:"containerName,omitempty"`
	VMName             string    `json:"vmName,omitempty"`
	CopyStatePath      string    `json:"copyStatePath,omitempty"`
	SessionDir         string    `json:"-"`
	SSHAgentDir        string    `json:"sshAgentDirectory,omitempty"`
	SSHAgentPID        int       `json:"sshAgentPid,omitempty"`
	SSHKeyName         string    `json:"sshKeyName,omitempty"`
	SSHKeyFingerprint  string    `json:"sshKeyFingerprint,omitempty"`
	CredentialPolicy   string    `json:"credentialPolicyHash"`
	GitHubEnabled      bool      `json:"githubEnabled"`
	GitHubTokenHash    string    `json:"githubTokenHash,omitempty"`
	VMLoginKey         string    `json:"vmLoginKey,omitempty"`
	VMAddress          string    `json:"vmAddress,omitempty"`
	HomeDir            string    `json:"homeDirectory,omitempty"`
	PID                int       `json:"pid,omitempty"`
}

func vmNameForSession(id string) string {
	suffix := strings.ReplaceAll(id, "_", "-")
	name := "devfence-" + suffix
	if len(name) <= 63 {
		return name
	}
	hash := sha256.Sum256([]byte(id))
	return "devfence-" + suffix[:46] + "-" + hex.EncodeToString(hash[:4])
}

func credentialPolicyHash(policy CredentialPolicy) string {
	data, _ := json.Marshal(policy)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

var validSessionID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,95}$`)

func newSession(resolved Resolved, idOverride string) (*Session, error) {
	if err := os.MkdirAll(filepath.Join(resolved.StateDir, "sessions"), 0700); err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}
	id := idOverride
	if id == "" {
		base := strings.Trim(filepath.Base(resolved.Workspace), ".-_")
		base = regexp.MustCompile(`[^a-zA-Z0-9._-]+`).ReplaceAllString(base, "-")
		if base == "" {
			base = "workspace"
		}
		if len(base) > 32 {
			base = base[:32]
		}
		random := make([]byte, 4)
		if _, err := rand.Read(random); err != nil {
			return nil, err
		}
		id = base + "-" + hex.EncodeToString(random)
	}
	if !validSessionID.MatchString(id) {
		return nil, fmt.Errorf("invalid session ID %q", id)
	}
	sessionDir := filepath.Join(resolved.StateDir, "sessions", id)
	if _, err := os.Lstat(sessionDir); err == nil {
		return nil, fmt.Errorf("session %q already exists", id)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.Mkdir(sessionDir, 0700); err != nil {
		return nil, err
	}
	vmName := vmNameForSession(id)
	sshAgentDir := ""
	if resolved.Profile.Credentials.SSHKey != "" {
		sshAgentDir = filepath.Join(sessionDir, "ssh-agent")
	}
	var copyState *CopyState
	copyDir := filepath.Join(sessionDir, "workspace")
	presented, copyState, err := prepareWorkspace(resolved.Workspace, copyDir, resolved.Profile.Workspace, resolved.Protected)
	if err != nil {
		os.RemoveAll(sessionDir)
		return nil, err
	}
	session := &Session{
		ID:                 id,
		Backend:            resolved.Profile.Backend,
		ProfileName:        resolved.ProfileName,
		SourceWorkspace:    resolved.Workspace,
		PresentedWorkspace: presented,
		WorkingDir:         resolved.WorkingDir,
		WorkspaceMode:      resolved.Profile.Workspace.Mode,
		Network:            resolved.Profile.Network,
		ProtectedPaths:     resolved.Protected,
		CreatedAt:          time.Now().UTC(),
		Status:             "starting",
		SessionDir:         sessionDir,
		HomeDir:            filepath.Join(sessionDir, "home"),
		GitHubEnabled:      len(resolved.Profile.Credentials.GitHubTokenCommand) > 0,
		ContainerName:      "devfence-" + id,
		VMName:             vmName,
		SSHAgentDir:        sshAgentDir,
		CredentialPolicy:   credentialPolicyHash(resolved.Profile.Credentials),
	}
	if copyState != nil {
		session.CopyStatePath = filepath.Join(sessionDir, "copy-state.json")
		if err := writeCopyState(session.CopyStatePath, copyState); err != nil {
			os.RemoveAll(sessionDir)
			return nil, err
		}
	}
	for _, path := range []string{session.HomeDir, session.SSHAgentDir, filepath.Join(sessionDir, "credentials")} {
		if path == "" {
			continue
		}
		if err := os.MkdirAll(path, 0700); err != nil {
			os.RemoveAll(sessionDir)
			return nil, err
		}
	}
	if err := os.Chmod(sessionDir, 0700); err != nil {
		os.RemoveAll(sessionDir)
		return nil, err
	}
	if err := saveSession(resolved.StateDir, session); err != nil {
		os.RemoveAll(sessionDir)
		return nil, err
	}
	return session, nil
}

func sessionFile(stateDir, id string) (string, error) {
	if !validSessionID.MatchString(id) {
		return "", fmt.Errorf("invalid session ID %q", id)
	}
	return filepath.Join(stateDir, "sessions", id, "session.json"), nil
}

func saveSession(stateDir string, session *Session) error {
	path, err := sessionFile(stateDir, session.ID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func loadSession(stateDir, id string) (*Session, error) {
	path, err := sessionFile(stateDir, id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("parse session record: %w", err)
	}
	if session.ID != id {
		return nil, errors.New("session record ID does not match its directory")
	}
	if session.ContainerName != "devfence-"+id || session.VMName != vmNameForSession(id) {
		return nil, errors.New("session record runtime names do not match its ID")
	}
	session.SessionDir = filepath.Dir(path)
	return &session, nil
}

func listSessions(stateDir string) ([]Session, error) {
	root := filepath.Join(stateDir, "sessions")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sessions []Session
	for _, entry := range entries {
		if !entry.IsDir() || !validSessionID.MatchString(entry.Name()) {
			continue
		}
		session, err := loadSession(stateDir, entry.Name())
		if err != nil {
			continue
		}
		sessions = append(sessions, *session)
	}
	return sessions, nil
}
