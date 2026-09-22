# Devfence plan

## Goal

Make it easy to start a useful, project-scoped development environment from any directory while knowing exactly which host files, credentials, services, and networks it can reach. The user should be able to choose a shell, coding tool, or editor without changing the environment's access boundary.

## Requirements

- **Policy-driven, not machine-specific.** Configuration can select a runtime and access policy by directory or project, with an explicit default and per-launch overrides that cannot silently weaken protected paths. No usernames, project paths, SSH identities, or coding tools are hard-coded.
- **Distinct workspace modes.** Trusted projects may use a live mount. Sensitive projects can instead receive an explicitly selected, filtered copy, with a reviewable path for bringing changes back. The effective workspace scope must be clear before launch. Non-Git directories, symlinks, nested mounts, and worktrees must not accidentally expand that scope.
- **Appropriate isolation for the workload.** Support a lightweight host sandbox, a container, and a full VM where available. Policies must account for escape routes such as host Docker sockets, privileged services, inherited environment variables, and editor integrations. Workloads needing their own privileged kernel features—such as Docker, Kind, Cilium, and eBPF—must be possible in a VM without granting access to the host daemon or protected host files.
- **Scoped credentials and connectivity.** A profile can provide only the selected SSH identity and optional GitHub or other credentials needed by that environment. Secrets must not be placed in the project workspace by default. Network access and host-service access must be configurable and inspectable; any host-side command bridge must be narrow and explicitly authorized.
- **Natural daily workflow.** One command from the current directory should open a shell or chosen tool. Multiple environments can run concurrently. Users can attach a terminal or editor to an existing environment, and child processes remain within the same boundary.
- **Safe lifecycle.** Users can list, inspect, reattach to, stop, and remove environments. Cleanup must not discard uncommitted work, unpushed commits, guest-only files, or active sessions; interrupted launches should be recoverable.
- **Observable policy.** Before launch and on reattach, show the effective runtime, workspace mode, mounts or copied paths, credential identities (never secret values), network permissions, and relevant resource limits. Refuse ambiguous or unsupported policy combinations rather than implying protection that is not present.

## How to judge success

- A personal project can start from a non-Git directory, open a shell or coding tool, use an editor, and run concurrently with another project under its configured policy.
- A sensitive project exposes only approved files and one selected credential; attempts to read excluded files or use another identity fail, including through symlinks, mounts, host services, and tool integrations.
- A VM-based project can run Docker, Kind, and Cilium/eBPF while remaining unable to reach protected host files or the host Docker daemon.
- Environment inspection matches what is actually available inside. Stopping or cleaning up never silently loses work.

## Delivery approach

Keep runtime-specific behavior behind small backend boundaries and make policy resolution visible and testable before launching anything. Prefer native host tools over reimplementing container or hypervisor behavior. Start with Linux and validate the host sandbox directly; exercise Docker and libvirt through both command-level tests and opt-in end-to-end checks.

The first release should cover the workflows above with explicit errors for unsupported combinations. Add policy or runtime features only when they close a stated user need and can be verified. Go is the initial implementation language; see [the decision record](docs/decisions/0001-implementation-language.md).
