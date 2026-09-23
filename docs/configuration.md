# Configuration

Devfence reads host-wide settings from `${XDG_CONFIG_HOME:-~/.config}/devfence/config.yaml`. It reads optional project settings from `.devfence.yaml` at the workspace root. The current schema version is 3; version 2 host configuration is not migrated. The generic host example is [config.example.yaml](../config.example.yaml).

Run `devfence config init` to create a generic host configuration, or `devfence config init --project` from a project to create its project file. Add `--force` to replace an existing file. Project files contain that directory's runtime and host-path requests. Host tool paths, credential sources, and GitHub token commands are rejected there, so a checked-in project file cannot grant itself access to host credentials.

## Global defaults and project settings

The host `defaults` section supplies defaults for a `backend` (`bubblewrap`, `docker`, or `vm`), `network` mode (`full` or `none`), `workspace` presentation (`live` or `copy`), tool forwarding, credentials, resource limits, and VM setup. A project's `.devfence.yaml` can override its backend, network, and workspace settings and describe that project's requested host path shares. Omitted runtime values inherit the host defaults. Command-line options `--backend`, `--network`, and `--workspace` override both.

Example project file:

```yaml
version: 3
backend: bubblewrap
network: full
workspace:
  mode: live
shares:
  - source: ../agent-setup
    target: agents
    mode: copy
    exclude: [.env, .git]
  - source: ../local-config
    target: .config/tool
    mode: mount
    access: read-write
```

Project configuration is resolved from the Git workspace root, or from the current directory when it is outside a Git repository. The path to the loaded project file appears in `devfence plan`.

## Workspace and protected paths

`live` presents the selected workspace read-write. `copy` creates a private copy under Devfence state, omits `.git`, rejects symlinks and special files, and includes only files matching `workspace.include` when that list is non-empty. `workspace.exclude` patterns take precedence over includes. Patterns are relative to the workspace and use `*` for one path segment and `**` for zero or more segments. Export is a separate preview/apply operation; additions and edits can be copied back, host conflicts are refused, and deletions in the copy never delete host files.

An include list is a policy boundary: include only files that have been reviewed as non-confidential. Protected host paths belong in the top-level host `protectedPaths` list, never in project configuration. A workspace overlapping a protected path cannot be exposed live, and a copied workspace under a protected ancestor requires explicit include patterns. Nested protected paths are never copied. This boundary does not scan workspace contents for secrets.

## Project host path shares

Each share has a host `source`, a sandbox-home-relative `target`, and a `mode`:

- `copy` takes a private snapshot when the session starts. The sandbox cannot edit the host source. A copied directory may use `exclude` entries for exact relative paths and their contents, such as `.env`. Copy mode accepts files or directories and is exposed read-only in the sandbox.
- `mount` exposes a directory live. `access` defaults to `read-only`; set it to `read-write` only when the agent should be able to change the host files directly. Exclusions are not supported for mounts.

Relative sources are resolved from the workspace root. Absolute paths and `~/...` are also accepted. Sources must exist, and broad system paths, the Devfence state directory, and configured protected paths are refused. Mounts currently require a directory; a single file can be copied.

Before the first launch, Devfence prints every requested source, target, mode, and access level and asks the host user to approve the exact request. The approval is stored in the user's Devfence state, outside the repository, and applies across backends while that request remains unchanged. A changed request requires approval again. Non-interactive launches cannot approve new requests. `devfence plan` shows the requests and whether they are approved.

Example:

```yaml
version: 3
backend: bubblewrap
network: full
workspace:
  mode: live
shares:
  - source: ../shared-agent-files
    target: agents
    mode: copy
    exclude: [.env, .git]
  - source: ../shared-config
    target: .config/my-tool
    mode: mount
    access: read-write
```

The approval gates host path access; a project file cannot silently grant itself that access. Project files cannot configure credential providers or GitHub token commands. A requested share can point to sensitive files, so review the exact host paths shown before approving.

## Forwarding host tools and global agent instructions

Codex and Claude tool configuration belongs in the host defaults. Each tool accepts `enabled`, `authentication`, and `configuration`. A forwarded file names a host `source`, a sandbox-home-relative `target`, and optional `optional: true`. Authentication files must be regular files owned by the current user with no group/other permissions. They are copied into session state with mode `0600`; the sandbox can use and modify its private copy, and the host source is not changed.

`tools.sharedDirectories` copies host directories under the sandbox home. Devfence rejects special files and symlinks that escape the source directory. Internal symlinks are copied as their target contents. `exclude` omits exact relative files or directories and their contents. For example:

```yaml
defaults:
  tools:
    codex:
      enabled: true
      configuration:
        - source: ~/agent-setup/global-instructions.md
          target: .codex/AGENTS.md
    sharedDirectories:
      - source: ~/agent-setup
        target: agents
        exclude: [.env, .git]
```

Configure the host's global agent instruction directory in `defaults.tools.sharedDirectories` to import those instructions into every sandbox. Omit `optional: true` when they must always be present. Exclude `.env` (and other host-only data) explicitly. These host-configured directories are copied when a session starts. Bubblewrap and Docker expose them read-only; VM guests receive a private copy with write bits removed. Start a new session to pick up host changes. Shared directories and project directory copies are limited to 256 MiB and 100,000 entries; tool files are limited to 8 MiB each.

Bubblewrap uses enabled host-installed Codex and Claude CLIs read-only. Docker images and VM guests install enabled CLIs inside their runtime; Devfence does not copy host CLI installations into those backends.

## GitHub and SSH credentials

GitHub authentication is configured at the host level and scoped to trusted workspace paths. Devfence runs the configured argv list on the host, uses trimmed stdout as the token, and never logs or stores the token in a session manifest. Example:

```yaml
github:
  paths:
    - ~/trusted-projects
  authentication:
    tokenCommand:
      - gh
      - auth
      - token
      - --hostname
      - github.com
      - --user
      - ACCOUNT
```

The token is available when the resolved workspace is inside one of the configured paths. It has the scopes and repository access of the selected host account. All projects below a configured path receive that access, regardless of their Git remote; keep this path limited to directories trusted with that token. Project configuration cannot change this policy. The GitHub CLI binary can be enabled separately under `defaults.tools.gh.enabled`.

`defaults.credentials.sshKey` names one private key, which Devfence loads into a per-session SSH agent; the private key itself is never mounted. The selected agent can sign with that one identity. VM GitHub authentication persists on the VM disk until the VM is deleted.

## Resources and VM tools

The host defaults may set `resources.memoryMiB`, `resources.vcpus`, and `resources.diskGiB`; defaults are 6144 MiB, 4 vCPUs, and a 40 GiB virtual disk. The `vm` section supports an Ubuntu cloud `imageURL`, matching `checksumsURL`, `guestUser`, and optional `kubectlVersion`, `kindVersion`, and `ciliumVersion`. The default guest account follows the host UID: UID 1000 uses Ubuntu's existing `ubuntu` account; other UIDs use `sandbox`. A custom `guestUser` must not collide with that UID mapping. Image and checksum URLs must use HTTPS, and the selected image must appear in its checksum file. Versions accept `stable`/`latest` or a semantic version with an optional `v` prefix.

The initial VM image and guest binaries target x86-64 Linux. Tool versions default to upstream stable/latest, so a new VM may install newer versions than an existing one. Pin versions in host defaults when repeatability matters.

## Inspecting effective policy

Run `devfence plan` from the target directory before launch. It reports the project config path, effective runtime settings, configured tool forwarding, whether a GitHub token command applies, VM resources, and protected paths. Secret values are never shown. A `full` network setting grants ordinary guest access to reachable network endpoints; it is not a host-service firewall.
