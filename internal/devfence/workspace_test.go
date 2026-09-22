package devfence

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilteredCopyOmitsGitSecretsAndProtectedPaths(t *testing.T) {
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "copy")
	protected := filepath.Join(source, "private")
	for path, content := range map[string]string{
		"go.mod":              "module sample\n",
		"src/main.go":         "package main\n",
		"src/.env":            "SECRET=never-copy\n",
		"private/credentials": "must not be copied\n",
		".git/config":         "remote = secret\n",
	} {
		target := filepath.Join(source, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	policy := WorkspacePolicy{Mode: "copy", Include: []string{"go.mod", "src/**/*.go"}}
	state := &CopyState{Baseline: map[string]string{}, BaselineModes: map[string]uint32{}}
	if err := copyTree(source, destination, policy, []string{protected}, state); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"go.mod", "src/main.go"} {
		if _, err := os.Stat(filepath.Join(destination, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("expected copied file %s: %v", rel, err)
		}
	}
	for _, rel := range []string{"src/.env", "private/credentials", ".git/config"} {
		if _, err := os.Stat(filepath.Join(destination, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("file %s leaked into filtered copy", rel)
		}
	}
	if len(state.Baseline) != 2 {
		t.Fatalf("baseline contains %d entries, want 2", len(state.Baseline))
	}
}

func TestCopyRejectsSymlinks(t *testing.T) {
	source := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "escape")); err != nil {
		t.Fatal(err)
	}
	err := copyTree(source, filepath.Join(t.TempDir(), "copy"), WorkspacePolicy{Mode: "copy"}, nil, &CopyState{Baseline: map[string]string{}, BaselineModes: map[string]uint32{}})
	if err == nil || !strings.Contains(err.Error(), "refuses symlink") {
		t.Fatalf("expected symlink refusal, got %v", err)
	}
}

func TestExportPreviewApplyAndConflictDetection(t *testing.T) {
	source := t.TempDir()
	presented := t.TempDir()
	original := []byte("before\n")
	if err := os.WriteFile(filepath.Join(source, "file.txt"), original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(presented, "file.txt"), []byte("after\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(presented, "new.txt"), []byte("new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	state := &CopyState{Source: source, Presented: presented, Baseline: map[string]string{"file.txt": hashHex(original)}}
	plan, err := planExport(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 2 || len(plan.Conflicts) != 0 {
		t.Fatalf("unexpected export plan: %+v", plan)
	}
	if err := applyExport(state, plan); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(source, "new.txt"))
	if err != nil || string(data) != "new\n" {
		t.Fatalf("new file export: %q, %v", data, err)
	}
	if err := os.WriteFile(filepath.Join(presented, "file.txt"), []byte("agent edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "file.txt"), []byte("host edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err = planExport(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Conflicts) != 1 || plan.Conflicts[0] != "file.txt" {
		t.Fatalf("expected one host conflict, got %+v", plan)
	}
	if err := applyExport(state, plan); err == nil {
		t.Fatal("conflicting export should fail")
	}
}

func TestGlobMatchSupportsRecursiveAndSegmentPatterns(t *testing.T) {
	for _, test := range []struct {
		pattern string
		value   string
		want    bool
	}{
		{"**/*.go", "main.go", true},
		{"**/*.go", "internal/app/main.go", true},
		{"src/**", "src/deep/file.txt", true},
		{"src/*.go", "src/deep/file.go", false},
		{"*.env", "nested/.env", false},
	} {
		if got := globMatch(test.pattern, test.value); got != test.want {
			t.Errorf("globMatch(%q, %q)=%v, want %v", test.pattern, test.value, got, test.want)
		}
	}
}

func hashHex(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}
