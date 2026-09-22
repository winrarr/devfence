# Security model

Devfence limits which host paths and credentials a process can reach; it is not a proof against all kernel, hypervisor, or hardware vulnerabilities. Each backend has a different boundary, and the resolved policy must be inspected before use.

## Backends

- **Bubblewrap** runs as the current user in new mount, PID, IPC, UTS, and optionally network namespaces. It hides the host home, runtime directory, temporary directories, and configured protected paths, then exposes only the selected workspace and an optional per-session SSH agent. It shares the host kernel and user ID, so use a VM when a separate kernel is required.
- **Docker** is accepted only when `docker info` identifies a rootless daemon. The container receives no Docker socket, host home, or unrelated host mounts. The selected workspace is the only project bind mount. Docker does not provide a separate kernel; privileged kernel workloads belong in a VM.
- **KVM/libvirt** provisions a guest with a private kernel and disk. It receives one workspace mount, a temporary guest login key, and optionally the selected GitHub token. It is not given the host Docker/libvirt sockets or the host home.

## Workspace and credentials

Live mode exposes the selected workspace read-write. If the workspace overlaps a configured protected path, live mode is refused. Copy mode uses include/exclude rules, omits Git metadata, and rejects symlinks or special files rather than following them outside the selected tree. Export is previewed first and refuses host-side conflicts.

No credentials are forwarded by default. An SSH key is loaded into a new per-session agent; the private key itself is never mounted. A GitHub token provider is an explicit argv configured in the selected profile; its output is kept out of logs and manifests. Only one configured SSH key is loaded into that agent.

## Network

The initial policy supports `full` (normal outbound network access) or `none`. Bubblewrap can isolate the network namespace; Docker uses its `none` network mode. The VM backend currently uses libvirt's default NAT network for SSH management and guest provisioning, so `none` is rejected there. VM sessions with `full` networking may reach host services that listen on reachable interfaces; this is not a host-network isolation policy. Per-domain allowlists and a VM management channel that works without guest networking are not implemented.

Host path access can still occur through files deliberately included in the workspace, exposed credentials, or explicitly configured network access. In particular, `network: full` on a VM is broader than a filesystem-only boundary: reachable services on the host or LAN may be accessed. Keep project files and credentials separate from work data and review the plan output before launch.
