# 0005: Approved project host path shares

## Status

Accepted

## Decision

Allow `.devfence.yaml` to request host path shares with explicit `copy` or `mount` semantics. Before first use, Devfence shows the exact host source, sandbox target, mode, and access and requires approval from the host user. Store only a hash of that request in private Devfence state, keyed to its workspace. A changed request requires approval again.

Copies are staged snapshots. Directory copies may exclude relative paths. Live mounts require directories and default to read-only; `read-write` deliberately allows edits to reach the host immediately. Do not allow a share to overlap protected paths or Devfence state.

## Rationale

Projects sometimes need selected host files or directories to remain visible or editable from an isolated environment. Putting each request in the project directory makes the setup self-contained, while separate host consent prevents a checked-in file from silently granting access.

## Consequences

Users see and approve each new request once per workspace. The same approval applies across backends as long as the paths and access request are unchanged. Approval metadata is not committed with project files. Single-file live mounts and mount exclusions are unsupported; files can be copied, and directories can be mounted. Backend adapters must preserve the same copy/live and read-only/read-write meaning.
