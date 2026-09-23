package devfence

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func approvalTestResolved(state, workspace string) Resolved {
	return Resolved{
		StateDir:  state,
		Workspace: workspace,
		Profile: Profile{
			Backend:       BackendBubblewrap,
			ProjectShares: []ProjectShare{{Source: "/home/test/agents", Target: "agents", Mode: "copy", Exclude: []string{".env"}}},
		},
	}
}

func TestProjectShareApprovalPersistsOnlyPrivateRequestHash(t *testing.T) {
	root := t.TempDir()
	resolved := approvalTestResolved(filepath.Join(root, "state"), filepath.Join(root, "project"))
	var output bytes.Buffer
	if err := authorizeProjectShares(resolved, strings.NewReader("yes\n"), &output, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"/home/test/agents" -> "agents"`) || !strings.Contains(output.String(), "[y/N]") {
		t.Fatalf("approval prompt did not show the reviewed paths: %q", output.String())
	}
	path := projectShareApprovalPath(resolved.StateDir, resolved.Workspace)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("approval file permissions = %o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "/home/test/agents") {
		t.Fatalf("approval record persisted a host path instead of a request hash: %s", data)
	}
	var record shareApproval
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.Version != shareApprovalVersion || record.RequestHash != projectShareApprovalHash(resolved) {
		t.Fatalf("unexpected approval record: %#v", record)
	}
	approved, err := projectSharesApproved(resolved)
	if err != nil || !approved {
		t.Fatalf("saved request approval = %t, err=%v", approved, err)
	}
	directoryInfo, err := os.Stat(filepath.Dir(path))
	if err != nil || directoryInfo.Mode().Perm() != 0700 {
		t.Fatalf("approval directory permissions = %v, err=%v", directoryInfo, err)
	}
}

func TestProjectShareApprovalIsBoundToWorkspaceBackendAndExactRequest(t *testing.T) {
	root := t.TempDir()
	resolved := approvalTestResolved(filepath.Join(root, "state"), filepath.Join(root, "project"))
	if err := saveProjectShareApproval(resolved); err != nil {
		t.Fatal(err)
	}
	newVariant := func() Resolved {
		copy := resolved
		copy.Profile.ProjectShares = append([]ProjectShare(nil), resolved.Profile.ProjectShares...)
		return copy
	}
	variants := []Resolved{newVariant(), newVariant(), newVariant(), newVariant(), newVariant()}
	variants[1].Workspace = filepath.Join(root, "another-project")
	variants[2].Profile.ProjectShares[0].Source = "/home/test/other-agents"
	variants[3].Profile.ProjectShares[0].Mode = "mount"
	variants[3].Profile.ProjectShares[0].Access = "read-write"
	variants[4].Profile.Backend = BackendDocker
	for index, variant := range variants {
		approved, err := projectSharesApproved(variant)
		if err != nil {
			t.Fatalf("variant %d approval check: %v", index, err)
		}
		want := index == 0 || index == 4
		if approved != want {
			t.Errorf("variant %d approved = %t, want %t", index, approved, want)
		}
	}
	otherWorkspace := approvalTestResolved(resolved.StateDir, filepath.Join(root, "another-project"))
	if err := saveProjectShareApproval(otherWorkspace); err != nil {
		t.Fatal(err)
	}
	firstPath := projectShareApprovalPath(resolved.StateDir, resolved.Workspace)
	secondPath := projectShareApprovalPath(resolved.StateDir, otherWorkspace.Workspace)
	if firstPath == secondPath {
		t.Fatal("different workspaces share the same approval record")
	}
}

func TestProjectShareApprovalPromptEscapesTerminalControlCharacters(t *testing.T) {
	root := t.TempDir()
	resolved := approvalTestResolved(filepath.Join(root, "state"), filepath.Join(root, "project"))
	resolved.Profile.ProjectShares[0].Source = "/tmp/\x1b[31msecret"
	var output bytes.Buffer
	err := authorizeProjectShares(resolved, strings.NewReader("no\n"), &output, true)
	if err == nil {
		t.Fatal("negative answer unexpectedly approved the request")
	}
	if strings.Contains(output.String(), "\x1b") || !strings.Contains(output.String(), `"/tmp/\x1b[31msecret"`) {
		t.Fatalf("terminal control character was emitted without escaping: %q", output.String())
	}
}

func TestProjectShareApprovalRefusalAndNoninteractiveLaunchFailClosed(t *testing.T) {
	root := t.TempDir()
	resolved := approvalTestResolved(filepath.Join(root, "state"), filepath.Join(root, "project"))
	for _, answer := range []string{"", "no\n", "yesterday\n"} {
		if err := authorizeProjectShares(resolved, strings.NewReader(answer), &bytes.Buffer{}, true); err == nil {
			t.Errorf("answer %q unexpectedly approved project shares", answer)
		}
	}
	if err := authorizeProjectShares(resolved, strings.NewReader("yes\n"), &bytes.Buffer{}, false); err == nil || !strings.Contains(err.Error(), "interactively") {
		t.Fatalf("noninteractive approval error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(resolved.StateDir, "approvals")); !os.IsNotExist(err) {
		t.Fatalf("refused request created an approval directory: %v", err)
	}
	if err := authorizeProjectShares(resolved, strings.NewReader("YES"), &bytes.Buffer{}, true); err != nil {
		t.Fatalf("case-insensitive yes at EOF should approve: %v", err)
	}
}

func TestProjectShareApprovalRejectsMalformedUnsafeAndStaleRecords(t *testing.T) {
	root := t.TempDir()
	resolved := approvalTestResolved(filepath.Join(root, "state"), filepath.Join(root, "project"))
	directory := filepath.Join(resolved.StateDir, "approvals")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := projectShareApprovalPath(resolved.StateDir, resolved.Workspace)
	if err := os.WriteFile(path, []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := projectSharesApproved(resolved); err == nil {
		t.Fatal("malformed approval record was accepted")
	}
	if err := os.WriteFile(path, []byte(`{"version":999,"requestHash":"old"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if approved, err := projectSharesApproved(resolved); err != nil || approved {
		t.Fatalf("stale approval record = %t, err=%v", approved, err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := projectSharesApproved(resolved); err == nil || !strings.Contains(err.Error(), "private file") {
		t.Fatalf("public approval record error = %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "elsewhere")
	if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := projectSharesApproved(resolved); err == nil {
		t.Fatal("symlink approval record was accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := projectSharesApproved(resolved); err == nil || !strings.Contains(err.Error(), "private, real directory") {
		t.Fatalf("public approval directory error = %v", err)
	}
}

func TestProjectShareApprovalFailsWhenApprovalDirectoryIsSymlink(t *testing.T) {
	root := t.TempDir()
	resolved := approvalTestResolved(filepath.Join(root, "state"), filepath.Join(root, "project"))
	if err := os.MkdirAll(resolved.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "other")
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(resolved.StateDir, "approvals")); err != nil {
		t.Fatal(err)
	}
	if _, err := projectSharesApproved(resolved); err == nil {
		t.Fatal("symlink approval directory was accepted")
	}
	if err := saveProjectShareApproval(resolved); err == nil {
		t.Fatal("approval was written through a symlink directory")
	}
}
