package devfence

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const shareApprovalVersion = 1

type shareApproval struct {
	Version     int    `json:"version"`
	RequestHash string `json:"requestHash"`
}

func projectShareApprovalHash(resolved Resolved) string {
	request := struct {
		Workspace string         `json:"workspace"`
		Shares    []ProjectShare `json:"shares"`
	}{resolved.Workspace, resolved.Profile.ProjectShares}
	data, _ := json.Marshal(request)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func projectShareApprovalPath(stateDir, workspace string) string {
	workspaceHash := sha256.Sum256([]byte(filepath.Clean(workspace)))
	return filepath.Join(stateDir, "approvals", hex.EncodeToString(workspaceHash[:])+".json")
}

func projectSharesApproved(resolved Resolved) (bool, error) {
	if len(resolved.Profile.ProjectShares) == 0 {
		return true, nil
	}
	directory := filepath.Join(resolved.StateDir, "approvals")
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ok || stat.Uid != uint32(os.Getuid()) {
		return false, errors.New("Devfence share approval directory must be a private, real directory")
	}
	path := projectShareApprovalPath(resolved.StateDir, resolved.Workspace)
	info, err = os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	fileStat, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ok || fileStat.Uid != uint32(os.Getuid()) {
		return false, errors.New("Devfence share approval record must be a private file owned by the current user")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return false, err
	}
	openedStat, ok := openedInfo.Sys().(*syscall.Stat_t)
	if !openedInfo.Mode().IsRegular() || openedInfo.Mode().Perm()&0077 != 0 || !ok || openedStat.Uid != uint32(os.Getuid()) {
		return false, errors.New("Devfence share approval record must be a private file owned by the current user")
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return false, err
	}
	var approval shareApproval
	if err := json.Unmarshal(data, &approval); err != nil {
		return false, fmt.Errorf("read Devfence share approval record: %w", err)
	}
	if approval.Version != shareApprovalVersion {
		return false, nil
	}
	return approval.RequestHash == projectShareApprovalHash(resolved), nil
}

func authorizeProjectShares(resolved Resolved, input io.Reader, output io.Writer, interactive bool) error {
	if len(resolved.Profile.ProjectShares) == 0 {
		return nil
	}
	approved, err := projectSharesApproved(resolved)
	if err != nil {
		return err
	}
	if approved {
		return nil
	}
	if !interactive {
		return errors.New("this project requests access to host paths; run Devfence interactively to review and approve them")
	}
	fmt.Fprintf(output, "This project requests access to host paths for %q:\n", resolved.Workspace)
	for _, share := range resolved.Profile.ProjectShares {
		mode := share.Mode
		if mode == "copy" {
			fmt.Fprintf(output, "  copy %q -> %q (sandbox copy)\n", share.Source, share.Target)
			continue
		}
		access := share.Access
		if access == "" {
			access = "read-only"
		}
		fmt.Fprintf(output, "  mount %q -> %q (%s)\n", share.Source, share.Target, access)
	}
	fmt.Fprint(output, "Approve these exact host path requests for this workspace? [y/N] ")
	answer, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer != "y" && answer != "yes" {
		return errors.New("host path access was not approved")
	}
	return saveProjectShareApproval(resolved)
}

func saveProjectShareApproval(resolved Resolved) error {
	directory := filepath.Join(resolved.StateDir, "approvals")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ok || stat.Uid != uint32(os.Getuid()) {
		return errors.New("Devfence share approval directory must be a private, real directory owned by the current user")
	}
	data, err := json.MarshalIndent(shareApproval{Version: shareApprovalVersion, RequestHash: projectShareApprovalHash(resolved)}, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".approval-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	removeTemporary := func() {
		_ = temporary.Close()
		_ = os.Remove(name)
	}
	if err := temporary.Chmod(0600); err != nil {
		removeTemporary()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		removeTemporary()
		return err
	}
	if err := temporary.Sync(); err != nil {
		removeTemporary()
		return err
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, projectShareApprovalPath(resolved.StateDir, resolved.Workspace)); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}
