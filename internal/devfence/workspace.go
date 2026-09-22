package devfence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

type CopyState struct {
	Source         string            `json:"source"`
	Presented      string            `json:"presented"`
	Include        []string          `json:"include,omitempty"`
	Exclude        []string          `json:"exclude,omitempty"`
	ProtectedPaths []string          `json:"protectedPaths,omitempty"`
	Baseline       map[string]string `json:"baseline"`
	BaselineModes  map[string]uint32 `json:"baselineModes,omitempty"`
}

type ExportChange struct {
	Path   string `json:"path"`
	Action string `json:"action"`
}

type ExportPlan struct {
	Changes   []ExportChange `json:"changes"`
	Conflicts []string       `json:"conflicts"`
	Deleted   []string       `json:"deleted"`
}

func prepareWorkspace(source, destination string, policy WorkspacePolicy, protected []string) (string, *CopyState, error) {
	if err := rejectNestedMounts(source); err != nil {
		return "", nil, err
	}
	if policy.Mode == "live" {
		return source, nil, nil
	}
	if within(source, destination) || within(destination, source) {
		return "", nil, errors.New("workspace and Devfence state directory must not overlap")
	}
	if err := os.MkdirAll(destination, 0700); err != nil {
		return "", nil, fmt.Errorf("create copied workspace: %w", err)
	}
	baseline := make(map[string]string)
	state := &CopyState{Source: source, Presented: destination, Include: policy.Include, Exclude: policy.Exclude, Baseline: baseline, BaselineModes: make(map[string]uint32)}
	for _, path := range protected {
		if within(source, path) && path != source {
			if relative, err := filepath.Rel(source, path); err == nil {
				state.ProtectedPaths = append(state.ProtectedPaths, filepath.ToSlash(relative))
			}
		}
	}
	if err := copyTree(source, destination, policy, protected, state); err != nil {
		return "", nil, err
	}
	return destination, state, nil
}

func copyTree(source, destination string, policy WorkspacePolicy, protected []string, state *CopyState) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if excludedPath(rel, policy.Exclude) || hasGitComponent(rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		for _, protectedPath := range protected {
			if within(source, protectedPath) && within(protectedPath, path) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("copy mode refuses symlink %s; exclude it or make the target an ordinary file", rel)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("copy mode refuses special file %s", rel)
		}
		if len(policy.Include) > 0 && !matchesAny(rel, policy.Include) {
			return nil
		}
		data, err := readRegularNoFollow(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", rel, err)
		}
		target := filepath.Join(destination, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		mode := info.Mode().Perm()&0755 | 0200
		if err := os.WriteFile(target, data, mode); err != nil {
			return err
		}
		hash := sha256.Sum256(data)
		state.Baseline[rel] = hex.EncodeToString(hash[:])
		state.BaselineModes[rel] = uint32(mode & 0755)
		return nil
	})
}

func readRegularNoFollow(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	return io.ReadAll(file)
}

func writeCopyState(path string, state *CopyState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func readCopyState(path string) (*CopyState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state CopyState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	if state.Source == "" || state.Presented == "" {
		return nil, errors.New("copy state is incomplete")
	}
	return &state, nil
}

func planExport(state *CopyState) (ExportPlan, error) {
	plan := ExportPlan{}
	current := make(map[string]string)
	err := filepath.WalkDir(state.Presented, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(state.Presented, path)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if hasGitComponent(rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if excludedPath(rel, state.Exclude) || !allowedByPolicy(rel, state.Include) || excludedPath(rel, state.ProtectedPaths) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("export refuses symlink %s", rel)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("export refuses special file %s", rel)
		}
		data, err := readRegularNoFollow(path)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(data)
		current[rel] = hex.EncodeToString(hash[:])
		return nil
	})
	if err != nil {
		return plan, err
	}
	for rel, currentHash := range current {
		baselineHash, existed := state.Baseline[rel]
		if existed && baselineHash == currentHash {
			continue
		}
		action := "add"
		if existed {
			action = "update"
		}
		sourcePath := filepath.Join(state.Source, filepath.FromSlash(rel))
		if existed {
			if err := checkNoSymlinkParents(state.Source, rel); err != nil {
				plan.Conflicts = append(plan.Conflicts, rel)
				continue
			}
			data, err := readRegularNoFollow(sourcePath)
			if err != nil {
				plan.Conflicts = append(plan.Conflicts, rel)
				continue
			}
			hash := sha256.Sum256(data)
			if hex.EncodeToString(hash[:]) != baselineHash {
				plan.Conflicts = append(plan.Conflicts, rel)
				continue
			}
			if baselineMode, ok := state.BaselineModes[rel]; ok {
				info, err := os.Stat(sourcePath)
				if err != nil || uint32(info.Mode().Perm()&0755) != baselineMode {
					plan.Conflicts = append(plan.Conflicts, rel)
					continue
				}
			}
		} else if err := checkNoSymlinkParents(state.Source, rel); err != nil {
			plan.Conflicts = append(plan.Conflicts, rel)
			continue
		} else if _, err := os.Lstat(sourcePath); !errors.Is(err, os.ErrNotExist) {
			plan.Conflicts = append(plan.Conflicts, rel)
			continue
		}
		plan.Changes = append(plan.Changes, ExportChange{Path: rel, Action: action})
	}
	for rel := range state.Baseline {
		if _, ok := current[rel]; !ok {
			plan.Deleted = append(plan.Deleted, rel)
		}
	}
	sort.Slice(plan.Changes, func(i, j int) bool { return plan.Changes[i].Path < plan.Changes[j].Path })
	sort.Strings(plan.Conflicts)
	sort.Strings(plan.Deleted)
	return plan, nil
}

func applyExport(state *CopyState, plan ExportPlan) error {
	if state.Baseline == nil {
		state.Baseline = make(map[string]string)
	}
	if state.BaselineModes == nil {
		state.BaselineModes = make(map[string]uint32)
	}
	if len(plan.Conflicts) > 0 {
		return fmt.Errorf("export has conflicts: %s", strings.Join(plan.Conflicts, ", "))
	}
	for _, change := range plan.Changes {
		rel := filepath.Clean(filepath.FromSlash(change.Path))
		if rel == "." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
			return fmt.Errorf("invalid export path %q", change.Path)
		}
		source := filepath.Join(state.Presented, rel)
		target := filepath.Join(state.Source, rel)
		data, err := readRegularNoFollow(source)
		if err != nil {
			return err
		}
		info, err := os.Stat(source)
		if err != nil {
			return err
		}
		mode := info.Mode().Perm() & 0755
		hash := sha256.Sum256(data)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, mode); err != nil {
			return err
		}
		state.Baseline[change.Path] = hex.EncodeToString(hash[:])
		state.BaselineModes[change.Path] = uint32(mode)
	}
	return nil
}

func excludedPath(rel string, patterns []string) bool {
	for _, pattern := range patterns {
		if globMatch(pattern, rel) || strings.HasPrefix(rel, strings.TrimSuffix(pattern, "/")+"/") {
			return true
		}
	}
	return false
}

func hasGitComponent(rel string) bool {
	for _, component := range strings.Split(rel, "/") {
		if component == ".git" {
			return true
		}
	}
	return false
}

func matchesAny(rel string, patterns []string) bool {
	for _, pattern := range patterns {
		if globMatch(pattern, rel) {
			return true
		}
	}
	return false
}

func allowedByPolicy(rel string, includes []string) bool {
	return len(includes) == 0 || matchesAny(rel, includes)
}

func checkNoSymlinkParents(root, rel string) error {
	current := root
	parts := strings.Split(filepath.FromSlash(rel), string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("export path has a missing or symlinked parent")
		}
	}
	return nil
}

func globMatch(pattern, value string) bool {
	patterns := strings.Split(filepath.ToSlash(pattern), "/")
	values := strings.Split(filepath.ToSlash(value), "/")
	var match func(int, int) bool
	match = func(pi, vi int) bool {
		if pi == len(patterns) {
			return vi == len(values)
		}
		if patterns[pi] == "**" {
			if match(pi+1, vi) {
				return true
			}
			return vi < len(values) && match(pi, vi+1)
		}
		if vi >= len(values) {
			return false
		}
		ok, err := filepath.Match(patterns[pi], values[vi])
		return err == nil && ok && match(pi+1, vi+1)
	}
	return match(0, 0)
}

func rejectNestedMounts(workspace string) error {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("inspect mount table: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mountPoint := decodeMountField(fields[4])
		if mountPoint != workspace && within(workspace, mountPoint) {
			return fmt.Errorf("workspace contains nested mount %s; refusing to expose it", mountPoint)
		}
	}
	return nil
}

func decodeMountField(value string) string {
	for _, replacement := range []struct{ from, to string }{{"\\040", " "}, {"\\011", "\t"}, {"\\012", "\n"}, {"\\134", "\\"}} {
		value = strings.ReplaceAll(value, replacement.from, replacement.to)
	}
	return value
}
