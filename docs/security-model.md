# Security model

Devfence limits which host paths and credentials a process can reach; it is not a proof against all kernel, hypervisor, or hardware vulnerabilities. Each backend has a different boundary, and the resolved policy must be inspected before use.

## Backends

- **Bubblewrap** runs as the current user in new mount, PID, IPC, UTS, and optionally network namespaces. It hides the host home, runtime directory, temporary directories, and configured protected paths, then exposes only the selected workspace and policy-selected tools or credentials. When the host resolver config points into `/run`, that one file is copied into the private runtime directory. Interactive runs use a separate PTY so the sandbox does not control the host terminal. It shares the host kernel and user ID, so use a VM when a separate kernel is required.
- **Docker** is accepted only when `docker info` identifies a rootless daemon. The container receives no Docker socket or host home. It gets the selected workspace, host-configured agent snapshots, and only the project shares the host user approved. Docker does not provide a separate kernel; privileged kernel workloads belong in a VM.
- **KVM/libvirt** provisions a guest with a private kernel and disk. It receives the workspace, a temporary guest login key, selected tool files and agent snapshots copied into the guest home, approved live project directory mounts, and optionally the selected GitHub token. It is not given the host Docker/libvirt sockets or the host home.

## Workspace and credentials

Live mode exposes the selected workspace read-write. If the workspace overlaps a configured protected path, live mode is refused. Copy mode uses include/exclude rules, omits Git metadata, and rejects symlinks or special files rather than following them outside the selected tree. Export is previewed first and refuses host-side conflicts.

Credential providers and automatic tool-auth forwarding are controlled by host configuration; a project cannot configure or silently enable them. A project may request copies or live mounts of host paths, but the host user must approve the exact source, target, mode, access, and backend before the request is used. The approval hash is stored privately in Devfence state, outside the repository; a changed request requires another approval. Project shares cannot overlap configured protected paths or Devfence state. Copy shares are snapshots staged in session state and exposed read-only; a live mount is read-only by default, while `read-write` lets the sandbox change the host directory immediately. Live mounts require directories and cannot use exclusions. Host-configured shared directories can exclude relative paths such as `.env`, which are omitted before copying. Bubblewrap uses enabled host-installed CLIs read-only. Docker images and VM guests install enabled CLIs inside their runtime; host CLI installations are not copied into those backends.

GitHub authentication is configured separately from the `gh` CLI. A host token command applies only when the resolved workspace is inside a configured trusted path. The path policy is based on the local workspace path, not the Git remote; every project below an authorized directory receives the token. The command runs on the host, and the resulting token has the selected host account's GitHub permissions. It is kept out of logs and session manifests, but sandbox processes can use it while the policy applies. VM GitHub auth persists on the VM disk until the VM is deleted. An SSH key is loaded into a new per-session agent; the private key itself is never mounted.

## Network

The initial policy supports `full` (normal outbound network access) or `none`. Bubblewrap can isolate the network namespace; Docker uses its `none` network mode. The VM backend currently uses libvirt's default NAT network for SSH management and guest provisioning, so `none` is rejected there. VM sessions with `full` networking may reach host services that listen on reachable interfaces; this is not a host-network isolation policy. Per-domain allowlists and a VM management channel that works without guest networking are not implemented.

Host path access can still occur through files deliberately included in the workspace, exposed credentials, or explicitly configured network access. In particular, `network: full` on a VM is broader than a filesystem-only boundary: reachable services on the host or LAN may be accessed. Keep project files and credentials separate from work data and review the plan output before launch.
