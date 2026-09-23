# VM setup and end-to-end verification

The VM backend uses system libvirt with KVM/QEMU, `virt-install`, `virsh`, `qemu-img`, OpenSSH, and `virtiofsd` for workspaces. On Debian/Ubuntu systems, install the distribution's libvirt/QEMU packages, add the user to the `libvirt` and `kvm` groups, and start a new login session before testing. The VM live-mount path also requires `virtiofsd` and libvirt's default NAT network.

The first VM launch downloads an Ubuntu 24.04 cloud image and verifies it against Ubuntu's published SHA256SUMS. It then provisions a guest with a private disk and kernel, rootful Docker inside the guest, Kind, kubectl, and the Cilium CLI. Codex, Claude, and GitHub CLI packages are installed only when host defaults enable them. Guest setup needs outbound network access. The host Docker daemon and host home are not mounted in the guest. The guest account has passwordless sudo and Docker-group access so it can perform kernel-level development inside the VM.

VM sessions currently use libvirt's default NAT network. This is useful for package downloads, image pulls, SSH management, and GitHub access, but may also reach host/LAN services. `network: none` is rejected for VMs until management can be separated from guest egress. Do not use the VM backend as a network-isolation boundary for services reachable from that NAT network.

The first launch can take several minutes and download several hundred megabytes. Default resources are 6 GiB RAM, 4 vCPUs, and a 40 GiB virtual disk. Each session has a separate guest disk; the verified base image is shared.

## Automated VM smoke test

After `make build`, run `bash scripts/integration-vm.sh` (or `make integration-vm`). It creates a temporary project, isolated user configuration/state/cache directories, a disposable libvirt guest, a Kind cluster, and installs Cilium. The script verifies Docker, Kind, kubectl, Cilium, the guest workspace mount, and Cilium readiness, then removes the cluster and guest. It requires KVM/libvirt access, the default NAT network, outbound network access, and enough memory for the configured VM. It will not delete unrelated VMs.

To inspect or clean up a manually started environment, use `devfence list`, `devfence inspect ID`, `devfence stop ID`, and `devfence delete ID --yes --force`. VM deletion removes the guest disk and all guest-only files; review or export wanted changes first. Devfence adds an SSH `Include` line to the host's `~/.ssh/config` to support VS Code Remote-SSH and stores per-VM connection entries under its state directory.
