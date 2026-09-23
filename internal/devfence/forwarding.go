package devfence

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

const maxForwardedFileSize = 8 << 20
const maxForwardedDirectorySize = 256 << 20
const maxForwardedDirectoryEntries = 100000

type forwardedFilePolicy struct {
	file ForwardedFile
	auth bool
}

func prepareForwardedTools(session *Session, profile Profile) error {
	tools := profile.Tools
	for name, tool := range map[string]ForwardedToolPolicy{"codex": tools.Codex, "claude": tools.Claude} {
		if !tool.Enabled {
			continue
		}
		files := make([]forwardedFilePolicy, 0, len(tool.Authentication)+len(tool.Configuration))
		for _, file := range tool.Authentication {
			files = append(files, forwardedFilePolicy{file: file, auth: true})
		}
		for _, file := range tool.Configuration {
			files = append(files, forwardedFilePolicy{file: file})
		}
		for _, item := range files {
			if err := stageForwardedFile(session.HomeDir, item); err != nil {
				return fmt.Errorf("forward %s file to %s: %w", name, item.file.Target, err)
			}
		}
	}
	for _, directory := range tools.SharedDirectories {
		if err := stageForwardedDirectory(session.HomeDir, directory); err != nil {
			return fmt.Errorf("forward directory to %s: %w", directory.Target, err)
		}
	}
	for _, share := range profile.ProjectShares {
		if err := stageProjectShare(session.HomeDir, share); err != nil {
			return fmt.Errorf("prepare project share %s: %w", share.Target, err)
		}
	}
	return nil
}

func stageProjectShare(home string, share ProjectShare) error {
	target, err := safeForwardTarget(home, share.Target)
	if err != nil {
		return err
	}
	relative, _ := filepath.Rel(home, target)
	if err := ensureSafeDirectories(home, filepath.Dir(relative), 0700); err != nil {
		return err
	}
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("share target %s already exists", share.Target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if share.Mode == "mount" {
		return os.Mkdir(target, 0700)
	}
	info, err := os.Stat(share.Source)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return stageForwardedDirectory(home, ForwardedPath{Source: share.Source, Target: share.Target, Exclude: share.Exclude})
	}
	return stageProjectShareFile(target, share.Source)
}

func stageProjectShareFile(target, source string) error {
	input, err := openRegularNoFollow(source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if info.Size() < 0 || info.Size() > maxForwardedDirectorySize {
		return errors.New("source file exceeds the 256 MiB forwarding limit")
	}
	mode := os.FileMode(0444)
	if info.Mode().Perm()&0111 != 0 {
		mode |= 0111
	}
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(output, io.LimitReader(input, maxForwardedDirectorySize+1))
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(target)
		return errors.Join(copyErr, closeErr)
	}
	if written > maxForwardedDirectorySize {
		_ = os.Remove(target)
		return errors.New("source file exceeds the 256 MiB forwarding limit")
	}
	return os.Chmod(target, mode)
}

func stageForwardedFile(home string, policy forwardedFilePolicy) error {
	source, err := expandPath(policy.file.Source)
	if err != nil {
		return err
	}
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) && policy.file.Optional {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("source must be a regular file, not a symlink or special file")
	}
	if info.Size() > maxForwardedFileSize {
		return fmt.Errorf("source exceeds the %d MiB forwarding limit", maxForwardedFileSize>>20)
	}
	if policy.auth {
		if info.Mode().Perm()&0077 != 0 {
			return errors.New("authentication file must not be accessible by group or others")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) {
			return errors.New("authentication file must be owned by the current user")
		}
	}
	target, err := safeForwardTarget(home, policy.file.Target)
	if err != nil {
		return err
	}
	relativeTarget, _ := filepath.Rel(home, target)
	if err := ensureSafeDirectories(home, filepath.Dir(relativeTarget), 0700); err != nil {
		return err
	}
	if existing, err := os.Lstat(target); err == nil {
		if !existing.Mode().IsRegular() {
			return errors.New("existing target is not a regular file")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := readRegularNoFollow(source)
	if err != nil {
		return fmt.Errorf("read source: %w", err)
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		_ = os.Remove(target)
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Chmod(target, 0600)
}

func stageForwardedDirectory(home string, policy ForwardedPath) error {
	source, err := expandPath(policy.Source)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) && policy.Optional {
		return nil
	} else if err != nil {
		return err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return fmt.Errorf("resolve source directory: %w", err)
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("source must resolve to a directory")
	}
	target, err := safeForwardTarget(home, policy.Target)
	if err != nil {
		return err
	}
	relativeTarget, _ := filepath.Rel(home, target)
	if err := ensureSafeDirectories(home, filepath.Dir(relativeTarget), 0700); err != nil {
		return err
	}
	if existing, err := os.Lstat(target); err == nil {
		if !existing.IsDir() || existing.Mode()&os.ModeSymlink != 0 {
			return errors.New("existing target is not a directory")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Mkdir(target, 0700); err != nil {
		return err
	}
	budget := &directoryCopyBudget{}
	excluded := make(map[string]bool, len(policy.Exclude))
	for _, path := range policy.Exclude {
		excluded[filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))] = true
	}
	if err := copyForwardedNode(source, source, target, map[string]bool{}, budget, excluded); err != nil {
		_ = os.RemoveAll(target)
		return err
	}
	return nil
}

type directoryCopyBudget struct {
	bytes   int64
	entries int
}

func copyForwardedNode(root, source, target string, ancestors map[string]bool, budget *directoryCopyBudget, excluded map[string]bool) error {
	if forwardedPathExcluded(root, source, excluded) {
		return nil
	}
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(source)
		if err != nil {
			return err
		}
		if !within(root, resolved) {
			return fmt.Errorf("source symlink %s escapes the forwarded directory", filepath.Base(source))
		}
		source = resolved
		if forwardedPathExcluded(root, source, excluded) {
			return nil
		}
		info, err = os.Stat(source)
		if err != nil {
			return err
		}
	}
	budget.entries++
	if budget.entries > maxForwardedDirectoryEntries {
		return errors.New("source directory exceeds the forwarding entry limit")
	}
	if info.IsDir() {
		resolved, err := filepath.EvalSymlinks(source)
		if err != nil {
			return err
		}
		if ancestors[resolved] {
			return errors.New("source directory contains a symlink cycle")
		}
		ancestors[resolved] = true
		defer delete(ancestors, resolved)
		if err := os.Mkdir(target, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		targetInfo, err := os.Lstat(target)
		if err != nil || targetInfo.Mode()&os.ModeSymlink != 0 || !targetInfo.IsDir() {
			return errors.New("forwarded directory target is not a real directory")
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyForwardedNode(root, filepath.Join(source, entry.Name()), filepath.Join(target, entry.Name()), ancestors, budget, excluded); err != nil {
				return fmt.Errorf("copy %s: %w", entry.Name(), err)
			}
		}
		return os.Chmod(target, 0555)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source contains unsupported file %s", filepath.Base(source))
	}
	if info.Size() < 0 || budget.bytes+info.Size() > maxForwardedDirectorySize {
		return errors.New("source directory exceeds the 256 MiB forwarding limit")
	}
	input, err := openRegularNoFollow(source)
	if err != nil {
		return err
	}
	outputMode := os.FileMode(0444)
	if info.Mode().Perm()&0111 != 0 {
		outputMode |= 0111
	}
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, outputMode)
	if err != nil {
		input.Close()
		return err
	}
	written, copyErr := io.Copy(output, io.LimitReader(input, maxForwardedDirectorySize-budget.bytes+1))
	inputErr := input.Close()
	outputErr := output.Close()
	if copyErr != nil || inputErr != nil || outputErr != nil {
		_ = os.Remove(target)
		return errors.Join(copyErr, inputErr, outputErr)
	}
	budget.bytes += written
	if budget.bytes > maxForwardedDirectorySize {
		_ = os.Remove(target)
		return errors.New("source directory exceeds the 256 MiB forwarding limit")
	}
	return os.Chmod(target, outputMode)
}

func forwardedPathExcluded(root, source string, exclusions map[string]bool) bool {
	if len(exclusions) == 0 {
		return false
	}
	relative, err := filepath.Rel(root, source)
	if err != nil {
		return false
	}
	relative = filepath.ToSlash(relative)
	for excluded := range exclusions {
		if relative == excluded || strings.HasPrefix(relative, excluded+"/") {
			return true
		}
	}
	return false
}

func openRegularNoFollow(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("source changed to a non-regular file while opening it")
	}
	return file, nil
}

func safeForwardTarget(home, relative string) (string, error) {
	if err := validateForwardedFileTarget(relative); err != nil {
		return "", err
	}
	target := filepath.Join(home, filepath.FromSlash(relative))
	if !within(home, target) {
		return "", errors.New("forwarded target escapes the session home")
	}
	return target, nil
}

func ensureSafeDirectories(root, relative string, mode os.FileMode) error {
	if relative == "." || relative == "" {
		return nil
	}
	current := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return errors.New("invalid forwarded target parent")
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, mode); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("forwarded target parent %s is not a real directory", part)
		}
	}
	return nil
}

func forwardedDirectoryTargets(tools ToolPolicy, home string) ([]string, error) {
	var targets []string
	for _, policy := range tools.SharedDirectories {
		if _, err := expandPath(policy.Source); err != nil {
			return nil, err
		}
		target, err := safeForwardTarget(home, policy.Target)
		if err != nil {
			return nil, err
		}
		if info, err := os.Lstat(target); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return nil, fmt.Errorf("forwarded target %s is not a real directory", policy.Target)
			}
			targets = append(targets, target)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return targets, nil
}

type forwardedSyncEntry struct {
	source    string
	target    string
	directory bool
	readOnly  bool
}

func forwardedSyncEntries(home string, profile Profile) ([]forwardedSyncEntry, error) {
	tools := profile.Tools
	entriesByTarget := make(map[string]forwardedSyncEntry)
	add := func(entry forwardedSyncEntry) error {
		if existing, ok := entriesByTarget[entry.target]; ok {
			if existing.directory != entry.directory || existing.source != entry.source {
				return fmt.Errorf("multiple forwarded paths target %s", entry.target)
			}
			return nil
		}
		entriesByTarget[entry.target] = entry
		return nil
	}
	for _, tool := range []ForwardedToolPolicy{tools.Codex, tools.Claude} {
		if !tool.Enabled {
			continue
		}
		for _, file := range append(append([]ForwardedFile(nil), tool.Authentication...), tool.Configuration...) {
			source, err := safeForwardTarget(home, file.Target)
			if err != nil {
				return nil, err
			}
			info, err := os.Lstat(source)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return nil, fmt.Errorf("staged tool file %s is not a regular file", file.Target)
			}
			if err := add(forwardedSyncEntry{source: source, target: file.Target}); err != nil {
				return nil, err
			}
		}
	}
	for _, policy := range tools.SharedDirectories {
		root, err := safeForwardTarget(home, policy.Target)
		if err != nil {
			return nil, err
		}
		rootInfo, err := os.Lstat(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
			return nil, fmt.Errorf("staged shared directory %s is not a real directory", policy.Target)
		}
		if err := filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("staged shared directory contains symlink %s", entry.Name())
			}
			relative, err := filepath.Rel(home, current)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			if entry.IsDir() {
				return add(forwardedSyncEntry{source: current, target: relative, directory: true, readOnly: true})
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("staged shared directory contains unsupported file %s", entry.Name())
			}
			return add(forwardedSyncEntry{source: current, target: relative, readOnly: true})
		}); err != nil {
			return nil, err
		}
	}
	for _, share := range profile.ProjectShares {
		if share.Mode != "copy" {
			continue
		}
		root, err := safeForwardTarget(home, share.Target)
		if err != nil {
			return nil, err
		}
		rootInfo, err := os.Lstat(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if rootInfo.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("staged project share %s is a symlink", share.Target)
		}
		if rootInfo.IsDir() {
			if err := filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.Type()&os.ModeSymlink != 0 {
					return fmt.Errorf("staged project share contains symlink %s", entry.Name())
				}
				relative, err := filepath.Rel(home, current)
				if err != nil {
					return err
				}
				relative = filepath.ToSlash(relative)
				if entry.IsDir() {
					return add(forwardedSyncEntry{source: current, target: relative, directory: true, readOnly: true})
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				if !info.Mode().IsRegular() {
					return fmt.Errorf("staged project share contains unsupported file %s", entry.Name())
				}
				return add(forwardedSyncEntry{source: current, target: relative, readOnly: true})
			}); err != nil {
				return nil, err
			}
			continue
		}
		if !rootInfo.Mode().IsRegular() {
			return nil, fmt.Errorf("copied project share %s is not a regular file or directory", share.Target)
		}
		if err := add(forwardedSyncEntry{source: root, target: share.Target, readOnly: true}); err != nil {
			return nil, err
		}
	}
	entries := make([]forwardedSyncEntry, 0, len(entriesByTarget))
	for _, entry := range entriesByTarget {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].directory != entries[j].directory {
			return entries[i].directory
		}
		if strings.Count(entries[i].target, "/") != strings.Count(entries[j].target, "/") {
			return strings.Count(entries[i].target, "/") < strings.Count(entries[j].target, "/")
		}
		return entries[i].target < entries[j].target
	})
	return entries, nil
}
