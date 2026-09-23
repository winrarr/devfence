package devfence

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type hostToolInstall struct {
	name         string
	source       string
	entryPoint   string
	isDirectory  bool
	nodeRuntime  string
	dependencies []nodePackageDependency
}

type nodePackageDependency struct {
	source string
	target string
}

func hostToolInstallation(command string, enabled bool) (*hostToolInstall, error) {
	if !enabled {
		return nil, nil
	}
	path, err := exec.LookPath(command)
	if errors.Is(err, exec.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find host %s executable: %w", command, err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("resolve host %s executable: %w", command, err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect host %s executable: %w", command, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return nil, fmt.Errorf("host %s path is not an executable regular file", command)
	}

	if filepath.Base(path) == command && filepath.Base(filepath.Dir(path)) == "bin" {
		release := filepath.Dir(filepath.Dir(path))
		if isDirectory(filepath.Join(release, command+"-resources")) {
			if _, err := os.Stat(filepath.Join(release, command+"-package.json")); err == nil {
				return &hostToolInstall{name: command, source: release, entryPoint: filepath.Join("bin", command), isDirectory: true}, nil
			}
		}
	}

	if filepath.Ext(path) == ".js" {
		if packageRoot, err := findNodePackageRoot(path); err != nil {
			return nil, err
		} else if packageRoot != "" {
			relative, err := filepath.Rel(packageRoot, path)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("host %s entry point is outside its Node package", command)
			}
			nodeRuntime, err := hostNodeRuntime(command)
			if err != nil {
				return nil, err
			}
			dependencies, err := nodePackageDependencyClosure(packageRoot)
			if err != nil {
				return nil, err
			}
			return &hostToolInstall{name: command, source: packageRoot, entryPoint: relative, isDirectory: true, nodeRuntime: nodeRuntime, dependencies: dependencies}, nil
		}
	}

	return &hostToolInstall{name: command, source: path, entryPoint: filepath.Base(path)}, nil
}

func nodePackageDependencyClosure(packageRoot string) ([]nodePackageDependency, error) {
	type manifest struct {
		Dependencies         map[string]string `json:"dependencies"`
		OptionalDependencies map[string]string `json:"optionalDependencies"`
	}
	readDependencies := func(root string) (map[string]string, map[string]bool, error) {
		data, err := os.ReadFile(filepath.Join(root, "package.json"))
		if err != nil {
			return nil, nil, fmt.Errorf("read host Node package metadata: %w", err)
		}
		var details manifest
		if err := json.Unmarshal(data, &details); err != nil {
			return nil, nil, fmt.Errorf("parse host Node package metadata: %w", err)
		}
		dependencies := make(map[string]string, len(details.Dependencies)+len(details.OptionalDependencies))
		optional := make(map[string]bool, len(details.OptionalDependencies))
		for name, version := range details.Dependencies {
			dependencies[name] = version
		}
		for name, version := range details.OptionalDependencies {
			dependencies[name] = version
			optional[name] = true
		}
		return dependencies, optional, nil
	}

	queue := []string{packageRoot}
	visited := make(map[string]bool)
	mountsByTarget := make(map[string]string)
	var mounts []nodePackageDependency
	for len(queue) > 0 {
		root := queue[0]
		queue = queue[1:]
		canonicalRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, fmt.Errorf("resolve host Node package dependency: %w", err)
		}
		if visited[canonicalRoot] {
			continue
		}
		visited[canonicalRoot] = true
		dependencies, optional, err := readDependencies(canonicalRoot)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(dependencies))
		for name := range dependencies {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if !validNodePackageName(name) {
				return nil, fmt.Errorf("host Node package has invalid dependency name %q", name)
			}
			dependencyRoot, err := resolveNodeDependency(canonicalRoot, name)
			if errors.Is(err, os.ErrNotExist) && optional[name] {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("resolve host Node dependency %q: %w", name, err)
			}
			if within(canonicalRoot, dependencyRoot) {
				queue = append(queue, dependencyRoot)
				continue
			}
			target := filepath.Join("/opt/devfence/node_modules", filepath.FromSlash(name))
			if existing, ok := mountsByTarget[target]; ok {
				if existing != dependencyRoot {
					return nil, fmt.Errorf("host Node dependency %q has conflicting installed versions", name)
				}
				continue
			}
			mountsByTarget[target] = dependencyRoot
			mounts = append(mounts, nodePackageDependency{source: dependencyRoot, target: target})
			queue = append(queue, dependencyRoot)
		}
	}
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].target < mounts[j].target })
	return mounts, nil
}

func validNodePackageName(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return false
	}
	parts := strings.Split(name, "/")
	if len(parts) == 0 || len(parts) > 2 || strings.HasPrefix(name, "@") && len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return len(parts) == 1 || strings.HasPrefix(parts[0], "@")
}

func resolveNodeDependency(packageRoot, name string) (string, error) {
	for dir := packageRoot; ; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, "node_modules", filepath.FromSlash(name))
		info, err := os.Stat(candidate)
		if err == nil && info.IsDir() {
			resolved, err := filepath.EvalSymlinks(candidate)
			if err != nil {
				return "", err
			}
			return resolved, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
	}
}

func hostNodeRuntime(tool string) (string, error) {
	path, err := exec.LookPath("node")
	if errors.Is(err, exec.ErrNotFound) {
		return "", fmt.Errorf("host %s is a Node package, but Node.js is not installed on the host", tool)
	}
	if err != nil {
		return "", fmt.Errorf("find Node.js runtime for host %s: %w", tool, err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve Node.js runtime for host %s: %w", tool, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("inspect Node.js runtime for host %s: %w", tool, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("Node.js runtime for host %s is not an executable regular file", tool)
	}
	return path, nil
}

func findNodePackageRoot(entryPoint string) (string, error) {
	for dir := filepath.Dir(entryPoint); ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
			return dir, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect host Node package: %w", err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
	}
}

func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
