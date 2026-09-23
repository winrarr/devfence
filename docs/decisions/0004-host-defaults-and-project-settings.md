# 0004: Host defaults and project settings

## Status

Accepted

## Decision

Keep machine-specific defaults, global agent instruction sources, host tool paths, credential sources, and GitHub token commands in the host configuration. Let each project's `.devfence.yaml` hold its backend, network mode, workspace presentation, and requested host path shares. Project files cannot configure credential providers or GitHub token commands.

Scope GitHub token commands to configured trusted workspace paths. This check uses the local workspace path rather than Git remote metadata. Copy global agent directories into each sandbox with configured exclusions for host-only data. Use the selected backend's packaged CLI installations for Docker and VM sessions; Bubblewrap may use enabled host-installed CLIs read-only.

The schema is version 3. This early project does not carry compatibility code for the previous schema.

## Rationale

Host settings describe the user's machine once, while each project file describes how Devfence should run in that directory. A user does not need to maintain a global list of projects. Sensitive host paths stay out of silent project-controlled grants, while the separate approval decision is made once on the host and is stored outside the repository.

## Consequences

Every project under a configured GitHub path receives that token, even when its remote is unrelated. Users must choose a path whose projects they trust with that account. Agent directory exclusions are applied while copying the source, so excluded data is not staged for any backend. Tool file copies persist in session state until session deletion; host files remain untouched. VM GitHub auth persists on the guest disk until VM deletion.
