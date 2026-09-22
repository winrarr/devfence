# Devfence project guide

Devfence is a Linux CLI for launching project-scoped shells and development tools through Bubblewrap, rootless Docker, or libvirt/KVM. User goals and acceptance criteria are in [PLAN.md](PLAN.md); runtime boundaries are in [docs/architecture.md](docs/architecture.md) and [docs/security-model.md](docs/security-model.md).

## Source of truth

- `internal/devfence` owns CLI behavior, configuration resolution, workspace handling, and runtime adapters.
- `docs/configuration.md` documents the accepted user configuration. Keep it synchronized with config types and validation.
- Tests define observable policy and lifecycle behavior. Do not weaken a boundary just to make a test pass.

## Work safely

- Treat workspace paths, credential paths, and runtime command arguments as untrusted input. Use argument arrays; never interpolate them into shell commands.
- Never log credential values or persist them in session manifests. Pass credentials only through the selected profile and the narrowest supported mechanism.
- Never mount a host Docker socket into a sandbox. Require a rootless Docker daemon for the container backend. Use a VM for workloads that need privileged Docker, Kind, Cilium, or eBPF.
- Refuse unsupported workspace/network combinations instead of claiming a policy the backend cannot enforce.
- Lifecycle commands must preserve dirty work and copied workspace state unless the user explicitly requests destructive removal.
- Changes to mount construction, path matching, credentials, or VM provisioning have a wide impact; add tests for allowed and denied cases.

## Canonical commands

- `make check` runs format validation, `go vet`, all tests, and a build.
- `go test ./...` runs focused Go tests.
- `scripts/integration-vm.sh` provisions a disposable VM and runs an end-to-end guest check; use only when KVM/libvirt resources are available.

Keep this guide short. Update product goals in `PLAN.md`, accepted trade-offs in `docs/decisions/`, concrete implementation debt in `docs/tech-debt.md`, and actionable future work in `docs/backlog.md`. Keep configuration details in `docs/configuration.md` and operational procedures beside their relevant docs. Add ADRs, debt, and backlog entries when a real decision, limitation, or deferred user need is encountered; do not add speculative filler.
