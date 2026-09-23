package devfence

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	ubuntuCloudImageURL = "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img"
	ubuntuChecksumsURL  = "https://cloud-images.ubuntu.com/noble/current/SHA256SUMS"
	libvirtPoolName     = "devfence"
	workspaceShareTag   = "devfence-workspace"
)

var safeVersion = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+([-.+][a-zA-Z0-9.-]+)?$`)

func runVM(session *Session, resolved Resolved, command []string, create, revokeGitHub bool) error {
	address, err := prepareVM(session, resolved, create, revokeGitHub)
	if err != nil {
		return err
	}
	if len(command) == 0 {
		command = []string{"/bin/bash", "-l"}
	}
	return execInVM(session, vmGuestUser(resolved.Profile.VM), address, command)
}

func prepareVM(session *Session, resolved Resolved, create, revokeGitHub bool) (string, error) {
	if session.Network == "none" {
		return "", errors.New("the VM backend needs network access for SSH management; network: none is unsupported")
	}
	if create {
		if err := createVM(session, resolved); err != nil {
			return "", err
		}
	} else if err := startVM(session); err != nil {
		return "", err
	}
	address, err := waitForVM(session, resolved.Profile.VM)
	if err != nil {
		return "", err
	}
	session.VMAddress = address
	userName := vmGuestUser(resolved.Profile.VM)
	if err := writeSSHConfig(resolved.StateDir, session, userName); err != nil {
		return "", err
	}
	if err := syncForwardedToolsToVM(session, resolved.Profile, userName, address); err != nil {
		return "", err
	}
	if session.GitHubEnabled {
		if err := loginGitHubInVM(session, userName, address); err != nil {
			return "", err
		}
		if err := saveSession(resolved.StateDir, session); err != nil {
			return "", err
		}
	} else if revokeGitHub {
		logout := sshCommand(session, userName, address, false, "gh", "auth", "logout", "--hostname", "github.com")
		logout.Stdout, logout.Stderr = io.Discard, io.Discard
		if err := logout.Run(); err != nil {
			return "", errors.New("could not revoke GitHub credentials inside VM")
		}
	}
	return address, nil
}

func validateVMTools(needsVirtioFS bool) error {
	if runtime.GOARCH != "amd64" {
		return fmt.Errorf("VM backend currently supports x86-64 hosts, not %s", runtime.GOARCH)
	}
	for _, name := range []string{"virsh", "virt-install", "qemu-img", "ssh", "ssh-keygen"} {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("VM backend requires %s", name)
		}
	}
	if needsVirtioFS {
		found := false
		for _, path := range []string{"/usr/libexec/virtiofsd", "/usr/lib/qemu/virtiofsd"} {
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
				found = true
				break
			}
		}
		if !found {
			return errors.New("VM workspaces and live project shares require virtiofsd")
		}
	}
	if err := execCommand("virsh", "--connect", "qemu:///system", "list", "--all").Run(); err != nil {
		return fmt.Errorf("cannot access system libvirt: %w", err)
	}
	return nil
}

func createVM(session *Session, resolved Resolved) error {
	if strings.Contains(session.PresentedWorkspace, ",") {
		return errors.New("VM virtiofs workspace paths cannot contain commas")
	}
	for _, share := range resolved.Profile.ProjectShares {
		if share.Mode == "mount" && strings.Contains(share.Source, ",") {
			return errors.New("VM virtiofs share paths cannot contain commas")
		}
	}
	if err := validateVMTools(session.WorkspaceMode == "live" || session.WorkspaceMode == "copy" || hasProjectMountShares(resolved.Profile.ProjectShares)); err != nil {
		return err
	}
	if err := ensureVMLoginKey(session); err != nil {
		return err
	}
	baseImage, err := ensureUbuntuImage(resolved.StateDir, resolved.Profile.VM)
	if err != nil {
		return err
	}
	userData, err := guestUserData(session, resolved.Profile.VM, resolved.Profile)
	if err != nil {
		return err
	}
	cloudConfigPath := filepath.Join(session.SessionDir, "cloud-init.yaml")
	if err := os.WriteFile(cloudConfigPath, userData, 0600); err != nil {
		return err
	}
	metaData, err := guestMetaData(session)
	if err != nil {
		return fmt.Errorf("encode cloud-init metadata: %w", err)
	}
	metaDataPath := filepath.Join(session.SessionDir, "cloud-init-meta-data.yaml")
	if err := os.WriteFile(metaDataPath, metaData, 0600); err != nil {
		return err
	}
	return withLibvirtLock(resolved.StateDir, func() error {
		if err := ensureLibvirtPool(); err != nil {
			return err
		}
		baseVolumePath, err := importBaseImage(baseImage)
		if err != nil {
			return err
		}
		diskName := session.VMName + ".qcow2"
		if err := createVMDisk(diskName, baseVolumePath, resolved.Profile); err != nil {
			return err
		}
		args := virtInstallArgs(session, resolved.Profile, cloudConfigPath, metaDataPath, diskName)
		cmd := execCommand("virt-install", args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("create KVM guest: %w", err)
		}
		session.Status = "running"
		return nil
	})
}

func withLibvirtLock(stateDir string, operation func() error) error {
	file, err := os.OpenFile(filepath.Join(stateDir, "libvirt.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return operation()
}

func ensureVMLoginKey(session *Session) error {
	privatePath := filepath.Join(session.SessionDir, "vm-login")
	if _, err := os.Stat(privatePath); errors.Is(err, os.ErrNotExist) {
		cmd := execCommand("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "devfence-"+session.ID, "-f", privatePath)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("create guest login key: %s: %w", strings.TrimSpace(string(output)), err)
		}
	} else if err != nil {
		return err
	}
	if err := os.Chmod(privatePath, 0600); err != nil {
		return err
	}
	session.VMLoginKey = privatePath
	return nil
}

func ensureUbuntuImage(stateDir string, settings VMConfig) (string, error) {
	cache := os.Getenv("XDG_CACHE_HOME")
	if cache == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		cache = filepath.Join(home, ".cache")
	}
	cache = filepath.Join(cache, "devfence")
	if err := os.MkdirAll(cache, 0700); err != nil {
		return "", err
	}
	imageURL := settings.ImageURL
	if imageURL == "" {
		imageURL = ubuntuCloudImageURL
	}
	checksumsURL := settings.ChecksumsURL
	if checksumsURL == "" {
		checksumsURL = ubuntuChecksumsURL
	}
	parsedImageURL, err := url.Parse(imageURL)
	if err != nil {
		return "", fmt.Errorf("invalid VM image URL: %w", err)
	}
	imageName := filepath.Base(parsedImageURL.Path)
	if imageName == "." || imageName == "/" || strings.ContainsAny(imageName, "\\\x00") {
		return "", errors.New("invalid Ubuntu image URL filename")
	}
	if !strings.HasPrefix(imageURL, "https://") || !strings.HasPrefix(checksumsURL, "https://") {
		return "", errors.New("VM image and checksum URLs must use HTTPS")
	}
	client := &http.Client{Timeout: 20 * time.Minute}
	checksumData, err := httpGet(client, checksumsURL)
	if err != nil {
		return "", fmt.Errorf("download VM image checksums: %w", err)
	}
	expected := checksumFor(checksumData, imageName)
	if expected == "" {
		return "", fmt.Errorf("checksums file does not contain %s", imageName)
	}
	imagePath := filepath.Join(cache, imageName)
	if hash, err := fileSHA256(imagePath); err == nil && hash == expected {
		return imagePath, nil
	} else if err == nil {
		if err := os.Remove(imagePath); err != nil {
			return "", fmt.Errorf("remove invalid cached VM image: %w", err)
		}
	}
	temporaryFile, err := os.CreateTemp(cache, filepath.Base(imageName)+"-*.part")
	if err != nil {
		return "", err
	}
	temporary := temporaryFile.Name()
	if err := temporaryFile.Close(); err != nil {
		_ = os.Remove(temporary)
		return "", err
	}
	if err := downloadAndCheck(client, imageURL, temporary, expected); err != nil {
		_ = os.Remove(temporary)
		return "", err
	}
	if err := os.Rename(temporary, imagePath); err != nil {
		return "", err
	}
	return imagePath, nil
}

func httpGet(client *http.Client, url string) ([]byte, error) {
	response, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %s", response.Status)
	}
	if response.Request.URL.Scheme != "https" {
		return nil, errors.New("VM checksum download redirected away from HTTPS")
	}
	return io.ReadAll(io.LimitReader(response.Body, 2<<20))
}

func checksumFor(data []byte, fileName string) string {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if filepath.Base(name) == fileName && len(fields[0]) == 64 {
			return strings.ToLower(fields[0])
		}
	}
	return ""
}

func downloadAndCheck(client *http.Client, url, path, expected string) error {
	response, err := client.Get(url)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("download VM image: HTTP %s", response.Status)
	}
	if response.Request.URL.Scheme != "https" {
		return errors.New("VM image download redirected away from HTTPS")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(file, hash), response.Body)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return errors.New("VM image SHA256 checksum mismatch")
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func ensureLibvirtPool() error {
	poolInfo := func() ([]byte, error) {
		return execCommand("virsh", "--connect", "qemu:///system", "pool-info", libvirtPoolName).CombinedOutput()
	}
	info, err := poolInfo()
	if err != nil {
		if err := execCommand("virsh", "--connect", "qemu:///system", "pool-define-as", libvirtPoolName, "dir", "--target", "/var/lib/libvirt/images/devfence").Run(); err != nil {
			info, err = poolInfo()
			if err != nil {
				return fmt.Errorf("define libvirt storage pool: %w", err)
			}
		} else if err := execCommand("virsh", "--connect", "qemu:///system", "pool-build", libvirtPoolName).Run(); err != nil {
			return fmt.Errorf("build libvirt storage pool: %w", err)
		}
	}
	if !libvirtPoolIsRunning(info) {
		if err := execCommand("virsh", "--connect", "qemu:///system", "pool-start", libvirtPoolName).Run(); err != nil {
			info, err = poolInfo()
			if err != nil {
				return fmt.Errorf("start libvirt storage pool: %w", err)
			}
			if !libvirtPoolIsRunning(info) {
				return errors.New("libvirt storage pool did not reach the running state")
			}
		}
	}
	if err := execCommand("virsh", "--connect", "qemu:///system", "pool-autostart", libvirtPoolName).Run(); err != nil {
		return fmt.Errorf("enable libvirt storage pool: %w", err)
	}
	return nil
}

func libvirtPoolIsRunning(info []byte) bool {
	for _, line := range strings.Split(string(info), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.EqualFold(fields[0], "State:") {
			return strings.EqualFold(fields[1], "running")
		}
	}
	return false
}

func importBaseImage(imagePath string) (string, error) {
	const volumeName = "ubuntu-noble-base.qcow2"
	output, err := execCommand("virsh", "--connect", "qemu:///system", "vol-info", "--pool", libvirtPoolName, volumeName).CombinedOutput()
	if err == nil {
		return libvirtVolumePath(volumeName)
	}
	if !missingLibvirtVolume(output) {
		return "", fmt.Errorf("inspect Ubuntu base image volume: %w", err)
	}
	info, err := execCommand("qemu-img", "info", "--output=json", imagePath).Output()
	if err != nil {
		return "", fmt.Errorf("inspect cloud image: %w", err)
	}
	var metadata struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	if err := json.Unmarshal(info, &metadata); err != nil || metadata.VirtualSize <= 0 {
		return "", errors.New("cloud image has invalid virtual size")
	}
	if err := execCommand("virsh", "--connect", "qemu:///system", "vol-create-as", "--pool", libvirtPoolName, volumeName, strconv.FormatInt(metadata.VirtualSize, 10), "--format", "qcow2").Run(); err != nil {
		return "", fmt.Errorf("create base image volume: %w", err)
	}
	if err := execCommand("virsh", "--connect", "qemu:///system", "vol-upload", "--pool", libvirtPoolName, volumeName, imagePath).Run(); err != nil {
		return "", fmt.Errorf("import base image volume: %w", err)
	}
	return libvirtVolumePath(volumeName)
}

func libvirtVolumePath(name string) (string, error) {
	output, err := execCommand("virsh", "--connect", "qemu:///system", "vol-path", "--pool", libvirtPoolName, name).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func createVMDisk(name, backingPath string, profile Profile) error {
	diskGiB := profile.Resources.DiskGiB
	if diskGiB <= 0 {
		diskGiB = 40
	}
	capacity := strconv.Itoa(diskGiB) + "G"
	if err := execCommand("virsh", "--connect", "qemu:///system", "vol-create-as", "--pool", libvirtPoolName, name, capacity, "--format", "qcow2", "--backing-vol", backingPath, "--backing-vol-format", "qcow2").Run(); err != nil {
		return fmt.Errorf("create guest disk: %w", err)
	}
	return nil
}

func guestUserData(session *Session, settings VMConfig, profile Profile) ([]byte, error) {
	tools := profile.Tools
	userName := vmGuestUser(settings)
	if !regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`).MatchString(userName) {
		return nil, fmt.Errorf("invalid VM guest user name %q", userName)
	}
	users, err := vmGuestUsers(userName, os.Getuid())
	if err != nil {
		return nil, err
	}
	pubKey, err := os.ReadFile(session.VMLoginKey + ".pub")
	if err != nil {
		return nil, err
	}
	kubectlVersion := settings.KubectlVersion
	if kubectlVersion == "" {
		kubectlVersion = "stable"
	}
	kindVersion := settings.KindVersion
	if kindVersion == "" {
		kindVersion = "latest"
	}
	ciliumVersion := settings.CiliumVersion
	if ciliumVersion == "" {
		ciliumVersion = "stable"
	}
	for label, version := range map[string]string{"kubectl": kubectlVersion, "Kind": kindVersion, "Cilium": ciliumVersion} {
		if version != "stable" && version != "latest" && !safeVersion.MatchString(version) {
			return nil, fmt.Errorf("invalid %s version %q", label, version)
		}
	}
	var mounts [][]string
	var bootcmd [][]string
	if session.WorkspaceMode == "live" || session.WorkspaceMode == "copy" {
		mounts = [][]string{{workspaceShareTag, "/workspace", "virtiofs", "rw,nosuid,nodev", "0", "0"}}
		bootcmd = append(bootcmd, []string{"mkdir", "-p", "/workspace"})
	}
	for index, share := range profile.ProjectShares {
		if share.Mode != "mount" {
			continue
		}
		guestPath := projectShareGuestPath(userName, share.Target)
		if share.Access == "read-write" {
			mounts = append(mounts, []string{projectShareTag(index), guestPath, "virtiofs", "rw,nosuid,nodev", "0", "0"})
		} else {
			mounts = append(mounts, []string{projectShareTag(index), guestPath, "virtiofs", "ro,nosuid,nodev", "0", "0"})
		}
		bootcmd = append(bootcmd, []string{"mkdir", "-p", guestPath})
	}
	userdata := map[string]any{
		"hostname":            session.VMName,
		"manage_etc_hosts":    true,
		"ssh_pwauth":          false,
		"disable_root":        true,
		"ssh_authorized_keys": []string{strings.TrimSpace(string(pubKey))},
		"packages":            []string{"ca-certificates", "curl", "docker.io", "git", "openssh-server", "jq", "make", "build-essential", "nodejs", "npm", "python3", "iptables", "iproute2", "conntrack"},
		"mounts":              mounts,
		"bootcmd":             bootcmd,
		"write_files": []map[string]any{
			{"path": "/etc/modules-load.d/devfence.conf", "permissions": "0644", "content": "overlay\nbr_netfilter\nvxlan\nip_tables\nip6_tables\n"},
			{"path": "/etc/sysctl.d/99-devfence.conf", "permissions": "0644", "content": "net.ipv4.ip_forward=1\nnet.bridge.bridge-nf-call-iptables=1\nnet.bridge-nf-call-ip6tables=1\n"},
			{"path": "/usr/local/bin/devfence-exec", "permissions": "0755", "content": guestExecHelper},
		},
		"runcmd": [][]string{
			{"chown", userName + ":" + userName, filepath.Join("/home", userName)},
			{"usermod", "-aG", "docker", userName},
			{"systemctl", "enable", "--now", "docker"},
			{"sysctl", "--system"},
			{"bash", "-lc", bootstrapGuestTools(kubectlVersion, kindVersion, ciliumVersion, tools)},
			{"touch", "/var/lib/devfence-ready"},
		},
	}
	if users != nil {
		userdata["users"] = users
	}
	if session.WorkspaceMode != "live" && session.WorkspaceMode != "copy" {
		delete(userdata, "mounts")
	}
	data, err := yaml.Marshal(userdata)
	if err != nil {
		return nil, err
	}
	return append([]byte("#cloud-config\n"), data...), nil
}

func vmGuestUsers(userName string, uid int) ([]any, error) {
	if userName == "ubuntu" {
		if uid != 1000 {
			return nil, errors.New("VM guest user ubuntu requires host UID 1000; choose another guestUser for this host")
		}
		return nil, nil
	}
	if uid == 1000 {
		return nil, errors.New("VM guestUser must be ubuntu for host UID 1000; Ubuntu already reserves that UID")
	}
	return []any{map[string]any{
		"name":   userName,
		"uid":    uid,
		"groups": []string{"adm", "sudo"},
		"sudo":   "ALL=(ALL) NOPASSWD:ALL",
		"shell":  "/bin/bash",
	}}, nil
}

func guestMetaData(session *Session) ([]byte, error) {
	return yaml.Marshal(map[string]string{
		"instance-id":    session.ID,
		"local-hostname": session.VMName,
	})
}

func bootstrapGuestTools(kubectlVersion, kindVersion, ciliumVersion string, tools ToolPolicy) string {
	kubectlSetup := "KUBECTL_VERSION=$(curl -fsSL https://dl.k8s.io/release/stable.txt)"
	if kubectlVersion != "stable" {
		kubectlSetup = "KUBECTL_VERSION=" + strconv.Quote(versionTag(kubectlVersion))
	}
	kindURL := "https://kind.sigs.k8s.io/dl/latest/kind-linux-amd64"
	if kindVersion != "latest" {
		kindURL = "https://kind.sigs.k8s.io/dl/" + versionTag(kindVersion) + "/kind-linux-amd64"
	}
	ciliumSetup := "CILIUM_VERSION=$(curl -fsSL https://raw.githubusercontent.com/cilium/cilium-cli/main/stable.txt)"
	if ciliumVersion != "stable" {
		ciliumSetup = "CILIUM_VERSION=" + strconv.Quote(versionTag(ciliumVersion))
	}
	var installTools []string
	if tools.GH.Enabled {
		installTools = append(installTools, "apt-get update && apt-get install -y --no-install-recommends gh")
	}
	if tools.Codex.Enabled {
		installTools = append(installTools, "npm install --global @openai/codex")
	}
	if tools.Claude.Enabled {
		installTools = append(installTools, "npm install --global @anthropic-ai/claude-code")
	}
	return fmt.Sprintf(`set -eu
%s
curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" -o /usr/local/bin/kubectl
chmod 0755 /usr/local/bin/kubectl
curl -fsSL %q -o /usr/local/bin/kind
chmod 0755 /usr/local/bin/kind
%s
curl -fsSL "https://github.com/cilium/cilium-cli/releases/download/${CILIUM_VERSION}/cilium-linux-amd64.tar.gz" -o /tmp/cilium.tar.gz
tar -xzf /tmp/cilium.tar.gz -C /usr/local/bin cilium
chmod 0755 /usr/local/bin/cilium
rm -f /tmp/cilium.tar.gz
npm install --global n
n 22
hash -r

%s`,
		kubectlSetup, kindURL, ciliumSetup, strings.Join(installTools, "\n"))
}

func versionTag(version string) string {
	if strings.HasPrefix(version, "v") {
		return version
	}
	return "v" + version
}

const guestExecHelper = `#!/usr/bin/python3
import json
import os
import sys

argv = json.loads(bytes.fromhex(sys.argv[1]))
if not argv or not all(isinstance(arg, str) for arg in argv):
    raise SystemExit("invalid Devfence command")
os.execvp(argv[0], argv)
`

func virtInstallArgs(session *Session, profile Profile, cloudConfig, metaData, diskName string) []string {
	memory := profile.Resources.MemoryMiB
	if memory <= 0 {
		memory = 6144
	}
	vcpus := profile.Resources.VCPUs
	if vcpus <= 0 {
		vcpus = 4
	}
	args := []string{
		"--connect", "qemu:///system",
		"--name", session.VMName,
		"--memory", strconv.Itoa(memory),
		"--vcpus", strconv.Itoa(vcpus),
		"--cpu", "host-passthrough",
		"--osinfo", "ubuntu24.04",
		"--import",
		"--disk", "vol=" + libvirtPoolName + "/" + diskName + ",bus=virtio",
		"--cloud-init", "user-data=" + cloudConfig + ",meta-data=" + metaData,
		"--network", "network=default,model=virtio",
		"--graphics", "none",
		"--console", "pty,target.type=serial",
		"--noautoconsole",
	}
	if profile.Workspace.Mode == "live" || profile.Workspace.Mode == "copy" {
		args = append(args, "--memorybacking", "source.type=memfd,access.mode=shared")
		args = append(args, "--filesystem", session.PresentedWorkspace+","+workspaceShareTag+",driver.type=virtiofs,binary.sandbox.mode=namespace")
	}
	for index, share := range profile.ProjectShares {
		if share.Mode != "mount" {
			continue
		}
		filesystem := share.Source + "," + projectShareTag(index) + ",driver.type=virtiofs,binary.sandbox.mode=namespace"
		if share.Access != "read-write" {
			filesystem += ",readonly=on"
		}
		args = append(args, "--filesystem", filesystem)
	}
	return args
}

func hasProjectMountShares(shares []ProjectShare) bool {
	for _, share := range shares {
		if share.Mode == "mount" {
			return true
		}
	}
	return false
}

func projectShareTag(index int) string {
	return "devfence-share-" + strconv.Itoa(index)
}

func projectShareGuestPath(userName, target string) string {
	return filepath.Join("/home", userName, filepath.FromSlash(target))
}

func startVM(session *Session) error {
	state, err := vmDomainState(session.VMName)
	if err != nil {
		return err
	}
	if state == "running" {
		return nil
	}
	if state == "missing" {
		return fmt.Errorf("VM %s no longer exists", session.VMName)
	}
	if err := execCommand("virsh", "--connect", "qemu:///system", "start", session.VMName).Run(); err != nil {
		return fmt.Errorf("start VM: %w", err)
	}
	return nil
}

func vmDomainState(name string) (string, error) {
	cmd := execCommand("virsh", "--connect", "qemu:///system", "domstate", name)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.ToLower(string(output))
		if strings.Contains(message, "no domain with matching name") || strings.Contains(message, "domain not found") || strings.Contains(message, "failed to get domain") {
			return "missing", nil
		}
		return "", fmt.Errorf("query VM %s state: %w", name, err)
	}
	return strings.ToLower(strings.TrimSpace(string(output))), nil
}

func waitForVM(session *Session, settings VMConfig) (string, error) {
	userName := vmGuestUser(settings)
	deadline := time.Now().Add(20 * time.Minute)
	lastLog := time.Now()
	for time.Now().Before(deadline) {
		output, err := execCommand("virsh", "--connect", "qemu:///system", "domifaddr", session.VMName, "--source", "lease").Output()
		if err == nil {
			for _, line := range strings.Split(string(output), "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 4 && strings.Contains(fields[2], "ipv4") {
					address := strings.Split(fields[3], "/")[0]
					if address != "" {
						if net.ParseIP(address).To4() == nil {
							return "", fmt.Errorf("libvirt returned an invalid guest IPv4 address %q", address)
						}
						if err := waitForGuestReady(session, userName, address); err != nil {
							return "", err
						}
						return address, nil
					}
				}
			}
		}
		if time.Since(lastLog) >= 15*time.Second {
			fmt.Fprintf(os.Stderr, "devfence: waiting for guest %s to finish setup\n", session.VMName)
			lastLog = time.Now()
		}
		time.Sleep(2 * time.Second)
	}
	return "", fmt.Errorf("guest %s did not become ready before timeout", session.VMName)
}

func waitForGuestReady(session *Session, userName, address string) error {
	deadline := time.Now().Add(20 * time.Minute)
	lastLog := time.Now()
	for time.Now().Before(deadline) {
		if err := sshCommand(session, userName, address, false, "test", "-f", "/var/lib/devfence-ready").Run(); err == nil {
			return nil
		}
		if time.Since(lastLog) >= 15*time.Second {
			fmt.Fprintf(os.Stderr, "devfence: waiting for guest %s SSH/tool setup\n", session.VMName)
			lastLog = time.Now()
		}
		time.Sleep(3 * time.Second)
	}
	return errors.New("guest SSH or development-tool provisioning did not become ready")
}

func vmGuestUser(settings VMConfig) string {
	return vmGuestUserForUID(settings, os.Getuid())
}

func vmGuestUserForUID(settings VMConfig, uid int) string {
	if settings.GuestUser != "" {
		return settings.GuestUser
	}
	if uid == 1000 {
		return "ubuntu"
	}
	return "sandbox"
}

func sshCommand(session *Session, userName, address string, tty bool, command ...string) *exec.Cmd {
	args := []string{
		"-i", session.VMLoginKey,
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + filepath.Join(session.SessionDir, "known_hosts"),
		"-o", "HostKeyAlias=" + session.ID,
		"-o", "ConnectTimeout=5",
	}
	if session.SSHKeyFingerprint != "" {
		args = append(args, "-A")
	}
	if tty && hasTerminal() {
		args = append(args, "-t")
	} else {
		args = append(args, "-T")
	}
	args = append(args, userName+"@"+address)
	if len(command) > 0 {
		args = append(args, safeRemoteCommand(command)...)
	}
	cmd := execCommand("ssh", args...)
	cmd.Env = os.Environ()
	if session.SSHAgentDir != "" && session.SSHKeyFingerprint != "" {
		cmd.Env = replaceEnv(cmd.Env, "SSH_AUTH_SOCK", filepath.Join(session.SSHAgentDir, "agent.sock"))
	}
	return cmd
}

func safeRemoteCommand(command []string) []string {
	data, _ := json.Marshal(command)
	encoded := hex.EncodeToString(data)
	return []string{"/usr/local/bin/devfence-exec", encoded}
}

func execInVM(session *Session, userName, address string, command []string) error {
	cmd := sshCommand(session, userName, address, true, command...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("VM command: %w", err)
	}
	return nil
}

const vmForwardedFilesReceiver = `import io, json, os, shutil, stat, sys, tarfile

home = os.path.realpath(os.path.expanduser("~"))
readonly = json.loads(sys.argv[1])

def safe_parts(value):
    if not isinstance(value, str) or value.startswith("/"):
        raise ValueError("invalid forwarded path")
    parts = value.split("/")
    if not parts or any(part in ("", ".", "..") for part in parts):
        raise ValueError("invalid forwarded path")
    return parts

def directory(parts):
    current = home
    for part in parts:
        current = os.path.join(current, part)
        try:
            os.mkdir(current, 0o700)
        except FileExistsError:
            pass
        if os.path.islink(current) or not os.path.isdir(current):
            raise ValueError("forwarded target parent is not a real directory")
    return current

with tarfile.open(fileobj=sys.stdin.buffer, mode="r|") as archive:
    for item in archive:
        parts = safe_parts(item.name.rstrip("/"))
        if item.isdir():
            directory(parts)
            continue
        if not item.isfile():
            raise ValueError("forwarded archive contains a non-file entry")
        parent = directory(parts[:-1])
        target = os.path.join(parent, parts[-1])
        mode = stat.S_IMODE(item.mode) & 0o777
        source = archive.extractfile(item)
        if source is None:
            raise ValueError("forwarded archive file has no contents")
        try:
            descriptor = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode)
        except FileExistsError:
            while source.read(1024 * 1024):
                pass
            continue
        try:
            with os.fdopen(descriptor, "wb") as output:
                shutil.copyfileobj(source, output)
            os.chmod(target, mode)
        except Exception:
            try:
                os.unlink(target)
            except FileNotFoundError:
                pass
            raise

for value in readonly:
    parts = safe_parts(value)
    parent = directory(parts[:-1])
    root = os.path.join(parent, parts[-1])
    if os.path.islink(root):
        raise ValueError("read-only forwarded target is a symlink")
    if os.path.isfile(root):
        mode = stat.S_IMODE(os.stat(root, follow_symlinks=False).st_mode) & ~0o222
        os.chmod(root, mode, follow_symlinks=False)
        continue
    if not os.path.isdir(root):
        raise ValueError("read-only forwarded target is missing or unsupported")
    for current, dirs, files in os.walk(root, topdown=False, followlinks=False):
        for name in dirs + files:
            child = os.path.join(current, name)
            if os.path.islink(child):
                raise ValueError("shared directory contains a symlink")
            mode = stat.S_IMODE(os.stat(child, follow_symlinks=False).st_mode) & ~0o222
            os.chmod(child, mode, follow_symlinks=False)
        mode = stat.S_IMODE(os.stat(current, follow_symlinks=False).st_mode) & ~0o222
        os.chmod(current, mode, follow_symlinks=False)
`

func syncForwardedToolsToVM(session *Session, profile Profile, userName, address string) error {
	entries, err := forwardedSyncEntries(session.HomeDir, profile)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	var readOnlyTargets []string
	for _, policy := range profile.Tools.SharedDirectories {
		path, err := safeForwardTarget(session.HomeDir, policy.Target)
		if err != nil {
			return err
		}
		if info, err := os.Lstat(path); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			readOnlyTargets = append(readOnlyTargets, policy.Target)
		}
	}
	for _, share := range profile.ProjectShares {
		if share.Mode == "copy" {
			readOnlyTargets = append(readOnlyTargets, share.Target)
		}
	}
	readonlyJSON, _ := json.Marshal(readOnlyTargets)
	cmd := sshCommand(session, userName, address, false, "python3", "-c", vmForwardedFilesReceiver, string(readonlyJSON))
	var remoteError bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &remoteError
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("prepare forwarded tool files for VM: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start VM tool forwarding: %w", err)
	}
	archive := tar.NewWriter(stdin)
	var archiveErr error
	for _, entry := range entries {
		info, err := os.Lstat(entry.source)
		if err != nil {
			archiveErr = err
			break
		}
		mode := info.Mode().Perm()
		if entry.readOnly {
			mode &^= 0222
		}
		header := &tar.Header{Name: entry.target, Mode: int64(mode), ModTime: info.ModTime(), Format: tar.FormatPAX}
		if entry.directory {
			header.Typeflag = tar.TypeDir
			header.Name = strings.TrimSuffix(entry.target, "/") + "/"
			if err := archive.WriteHeader(header); err != nil {
				archiveErr = err
				break
			}
			continue
		}
		header.Typeflag = tar.TypeReg
		header.Size = info.Size()
		if err := archive.WriteHeader(header); err != nil {
			archiveErr = err
			break
		}
		file, err := openRegularNoFollow(entry.source)
		if err != nil {
			archiveErr = err
			break
		}
		_, copyErr := io.CopyN(archive, file, info.Size())
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			archiveErr = errors.Join(copyErr, closeErr)
			break
		}
	}
	if closeErr := archive.Close(); archiveErr == nil {
		archiveErr = closeErr
	}
	if closeErr := stdin.Close(); archiveErr == nil {
		archiveErr = closeErr
	}
	waitErr := cmd.Wait()
	if archiveErr != nil {
		return fmt.Errorf("stream forwarded files to VM: %w", archiveErr)
	}
	if waitErr != nil {
		if message := strings.TrimSpace(remoteError.String()); message != "" {
			return fmt.Errorf("could not install forwarded files inside VM: %s", message)
		}
		return fmt.Errorf("could not install forwarded tool files inside VM: %w", waitErr)
	}
	return nil
}

func loginGitHubInVM(session *Session, userName, address string) error {
	token, err := os.ReadFile(filepath.Join(session.SessionDir, "credentials", "github-token"))
	if err != nil {
		return err
	}
	tokenHash := sha256.Sum256(token)
	newHash := hex.EncodeToString(tokenHash[:])
	status := sshCommand(session, userName, address, false, "gh", "auth", "status", "--hostname", "github.com")
	status.Stdout, status.Stderr = io.Discard, io.Discard
	if newHash == session.GitHubTokenHash && status.Run() == nil {
		return nil
	}
	activeUser := sshCommand(session, userName, address, false, "gh", "auth", "status", "--hostname", "github.com", "--json", "hosts", "--jq", `.hosts["github.com"][] | select(.active) | .login`)
	userOutput, _ := activeUser.Output()
	if existingUser := strings.TrimSpace(string(userOutput)); existingUser != "" {
		logout := sshCommand(session, userName, address, false, "gh", "auth", "logout", "--hostname", "github.com", "--user", existingUser)
		logout.Stdout, logout.Stderr = io.Discard, io.Discard
		if err := logout.Run(); err != nil {
			return errors.New("could not replace existing GitHub credentials inside VM")
		}
	}
	cmd := sshCommand(session, userName, address, false, "gh", "auth", "login", "--hostname", "github.com", "--git-protocol", "ssh", "--skip-ssh-key", "--with-token")
	cmd.Stdin = bytes.NewReader(token)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &stderr
	if err := cmd.Run(); err != nil {
		return errors.New("could not authenticate GitHub CLI inside VM")
	}
	setup := sshCommand(session, userName, address, false, "gh", "auth", "setup-git", "--hostname", "github.com")
	setup.Stdout, setup.Stderr = io.Discard, io.Discard
	if err := setup.Run(); err != nil {
		return errors.New("could not configure GitHub Git credentials inside VM")
	}
	session.GitHubTokenHash = newHash
	return nil
}

func writeSSHConfig(stateDir string, session *Session, userName string) error {
	if userName == "" {
		userName = vmGuestUser(VMConfig{})
	}
	configPath := filepath.Join(session.SessionDir, "ssh_config")
	var b strings.Builder
	fmt.Fprintf(&b, "Host %s\n", session.VMName)
	fmt.Fprintf(&b, "  HostName %s\n", session.VMAddress)
	fmt.Fprintf(&b, "  User %s\n", userName)
	fmt.Fprintf(&b, "  IdentityFile %s\n", sshConfigValue(session.VMLoginKey))
	fmt.Fprintf(&b, "  IdentitiesOnly yes\n")
	fmt.Fprintf(&b, "  HostKeyAlias %s\n", session.ID)
	fmt.Fprintf(&b, "  UserKnownHostsFile %s\n", sshConfigValue(filepath.Join(session.SessionDir, "known_hosts")))
	fmt.Fprintf(&b, "  StrictHostKeyChecking accept-new\n")
	if session.SSHKeyFingerprint != "" {
		fmt.Fprintf(&b, "  IdentityAgent %s\n  ForwardAgent yes\n", sshConfigValue(filepath.Join(session.SSHAgentDir, "agent.sock")))
	}
	if err := os.WriteFile(configPath, []byte(b.String()), 0600); err != nil {
		return err
	}
	return ensureSSHConfigInclude(stateDir)
}

func sshConfigValue(value string) string { return strconv.Quote(value) }

func ensureSSHConfigInclude(stateDir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configPath := filepath.Join(home, ".ssh", "config")
	sshDir := filepath.Dir(configPath)
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return err
	}
	sshDirInfo, err := os.Lstat(sshDir)
	if err != nil || sshDirInfo.Mode()&os.ModeSymlink != 0 || !sshDirInfo.IsDir() {
		return errors.New("refusing to modify SSH configuration through a symlink or non-directory")
	}
	info, err := os.Lstat(configPath)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to modify a symlinked ~/.ssh/config")
	}
	data, err := os.ReadFile(configPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	include := "Include " + sshConfigValue(filepath.Join(stateDir, "sessions", "*", "ssh_config"))
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == include {
			return nil
		}
	}
	updated := []byte(include + "\n" + string(data))
	mode := os.FileMode(0600)
	if info != nil {
		mode = info.Mode().Perm()
	}
	temporary, err := os.CreateTemp(sshDir, ".devfence-config-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(updated); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, configPath)
}

func stopVM(session *Session, force bool) error {
	state, err := vmDomainState(session.VMName)
	if err != nil || state == "missing" || state != "running" {
		return err
	}
	if err := execCommand("virsh", "--connect", "qemu:///system", "shutdown", session.VMName).Run(); err != nil {
		return fmt.Errorf("request guest shutdown: %w", err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		state, _ = vmDomainState(session.VMName)
		if state != "running" {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	if !force {
		return errors.New("VM did not shut down cleanly; it remains running; use --force to power it off")
	}
	return execCommand("virsh", "--connect", "qemu:///system", "destroy", session.VMName).Run()
}

func deleteVM(session *Session, force bool) error {
	state, err := vmDomainState(session.VMName)
	if err != nil {
		return err
	}
	if state == "missing" {
		return deleteVMVolume(session.VMName + ".qcow2")
	}
	if err := stopVM(session, force); err != nil {
		return err
	}
	if err := execCommand("virsh", "--connect", "qemu:///system", "undefine", session.VMName, "--remove-all-storage").Run(); err != nil {
		return fmt.Errorf("delete VM domain and disk: %w", err)
	}
	return deleteVMVolume(session.VMName + ".qcow2")
}

func deleteVMVolume(name string) error {
	output, err := execCommand("virsh", "--connect", "qemu:///system", "vol-info", "--pool", libvirtPoolName, name).CombinedOutput()
	if err != nil {
		if missingLibvirtVolume(output) {
			return nil
		}
		return fmt.Errorf("inspect Devfence VM disk %s: %w", name, err)
	}
	cmd := execCommand("virsh", "--connect", "qemu:///system", "vol-delete", "--pool", libvirtPoolName, name)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("delete Devfence VM disk %s: %w", name, err)
	}
	return nil
}

func missingLibvirtVolume(output []byte) bool {
	message := strings.ToLower(string(output))
	return strings.Contains(message, "storage volume not found") || strings.Contains(message, "no storage vol") || strings.Contains(message, "storage pool not found") || strings.Contains(message, "no storage pool")
}
