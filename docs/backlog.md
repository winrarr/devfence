# Backlog

Items are goals with testable outcomes; implementation choices remain open to evidence gathered during work.

## Add VM management without guest egress

- Goal: support a VM profile with no public or host-service access while still supporting lifecycle operations and optional editor attachment.
- Rationale: the current VM backend requires `network: full` for SSH; this prevents applying the same network restriction available to Bubblewrap and Docker.
- Constraints: do not claim isolation unless host-side enforcement is verified; do not forward an unrestricted host SSH agent or mount host paths beyond the selected workspace.
- Acceptance: test against a public endpoint and a host-only test service; the VM cannot reach either in the denied mode while Devfence can still inspect, attach, and stop it.

## Pin and report guest tool versions

- Goal: make VM environments repeatable without removing the convenient default setup.
- Rationale: fresh guests currently install moving stable/latest tool releases.
- Constraints: retain configurable version pins and fail clearly for unavailable or incompatible versions.
- Acceptance: two fresh guests from one pinned profile report identical Codex, Claude Code, Docker, Kind, kubectl, and Cilium CLI versions.

## Safely collect abandoned sessions

- Goal: help users reclaim stopped environments without losing source changes, unpushed commits, guest-only files, or active editor/agent work.
- Rationale: explicit session deletion is safe but easy to forget when multiple projects are open concurrently.
- Constraints: default to preview; never infer that a VM disk or session home is disposable from Git cleanliness alone; account for active runtime and editor connections.
- Acceptance: a dry run explains every retain/delete decision; automatic deletion is limited to sessions whose workspace, Git upstream state, home, runtime, and attachments are all verifiably clean, and unknown state always retains the session.
