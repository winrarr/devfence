# Architecture

Devfence resolves a workspace and profile before selecting a backend. The CLI records a non-secret session manifest, then invokes the relevant host tools. It does not run a daemon and does not use a privileged host-side broker.

```text
CLI
 ├─ configuration + path policy ──> effective profile
 ├─ workspace policy ────────────> live directory or filtered private copy
 ├─ credential provider ─────────> per-session SSH agent / selected GitHub token
 └─ backend
     ├─ Bubblewrap: foreground process in user, mount, PID, and network namespaces
     ├─ rootless Docker: named container, no host socket, attach via docker exec
     └─ libvirt/KVM: guest with its own kernel, SSH attach, workspace via virtiofs
```

Container and VM sessions have stable IDs and can be inspected, stopped, attached, and explicitly deleted. Bubblewrap sessions are foreground-only because their namespaces live with the launching process. Copy-mode workspaces stay under Devfence state and are exported back only through a conflict-checked command.

Backend-specific limits are part of the policy contract. Bubblewrap shares the host kernel and user identity. Rootless Docker depends on the configured rootless daemon. KVM uses a separate guest kernel and receives only the selected workspace and credentials. See [the security model](security-model.md).
