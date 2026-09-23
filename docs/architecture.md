# Architecture

Devfence resolves host defaults and the current project's runtime and path-share requests before selecting a backend. The host user approves each exact project share request once; the approval record stays in Devfence state. The CLI records a non-secret session manifest, then invokes the relevant host tools. It does not run a daemon and does not use a privileged host-side broker.

```text
CLI
 ├─ host defaults + project config ─> effective runtime and approved path-share policy
 ├─ workspace policy ────────────> live directory or filtered private copy
 ├─ host tool policy ─────────────> Bubblewrap host CLIs or packaged Docker/VM CLIs; per-session tool files
 ├─ credential provider ─────────> per-session SSH agent / path-scoped GitHub token / allowlisted tool auth
 └─ backend
     ├─ Bubblewrap: foreground process in user, mount, PID, and network namespaces, with a private PTY for interactive use
     ├─ rootless Docker: named container, no host socket, approved shares as bind mounts, attach via docker exec
     └─ libvirt/KVM: guest with its own kernel, SSH attach, workspace and approved live shares via virtiofs, copied files synced into guest home
```

Container and VM sessions have stable IDs and can be inspected, stopped, attached, and explicitly deleted. Bubblewrap sessions are foreground-only because their namespaces live with the launching process. Copy-mode workspaces stay under Devfence state and are exported back only through a conflict-checked command.

Backend-specific limits are part of the policy contract. Bubblewrap shares the host kernel and user identity. Rootless Docker depends on the configured rootless daemon. KVM uses a separate guest kernel and receives only the selected workspace and credentials. See [the security model](security-model.md).
