# Configuration

The optional configuration file is `${XDG_CONFIG_HOME:-~/.config}/devfence/config.yaml`. `devfence config init` writes a minimal default. The full example is in [config.example.yaml](../config.example.yaml); paths and credentials there are illustrative and must be replaced for a real machine.

## Profiles and rules

Each profile selects a `backend` (`bubblewrap`, `docker`, or `vm`), a `network` mode (`full` or `none`), and a workspace `mode` (`live` or `copy`). `defaultProfile` selects the fallback profile. Rules are checked in order, and the first matching directory rule wins. `match: subtree` includes the named path and its descendants; `match: exact` matches only that directory. Paths may use `~`.

Launch options can override backend, network, workspace mode, and profile. Validation still rejects combinations that would expose a configured protected path through a live workspace. VM profiles currently require `network: full` for guest SSH management.

## Workspace policies

`live` presents the selected directory read-write. It is convenient and appropriate only when every file in that tree may be exposed to the process.

`copy` creates a private copy under Devfence state, omits `.git`, follows no symlinks, rejects special files, and includes only files matching `include` patterns when that list is non-empty. `exclude` patterns take precedence over includes. Patterns are relative to the workspace and use `*` for one path segment and `**` for zero or more segments. Export is a separate preview/apply operation; additions and edits can be copied back, host conflicts are refused, and deletions in the copy never delete host files.

An allowlist is a policy boundary: include only files that have been reviewed as non-confidential. A broad pattern such as `**` defeats that boundary. Copy mode is not a Git clone; review and push from the host after exporting.

Protected paths are canonicalized before matching. A workspace that contains a protected path, or lies inside one, cannot be live-mounted. A copied workspace under a protected ancestor requires explicit include patterns. Nested protected paths are never copied. Keep the protected path list current; it does not scan file contents for secrets.

## Credentials

No credentials are forwarded by default. `credentials.sshKey` names one private key, which Devfence loads into a per-session SSH agent; it does not mount that private key into the environment. An optional `githubTokenCommand` is an argv list, not a shell string; its trimmed stdout is treated as a GitHub token. For example, `['gh', 'auth', 'token']` uses whichever account the host `gh` CLI currently selects. Devfence does not verify that the returned token belongs to a particular account or limit its repository scope.

The selected SSH agent can sign with its one loaded identity. A token is available to processes inside the environment while that profile is active. VM GitHub authentication persists on the VM disk until that VM is deleted; Docker and Bubblewrap credentials are stored only in that session's private state directory.

## Resources and VM tools

VM profiles may set `resources.memoryMiB`, `resources.vcpus`, and `resources.diskGiB`; defaults are 6144 MiB, 4 vCPUs, and a 40 GiB virtual disk. The `vm` section supports an Ubuntu cloud `imageURL`, matching `checksumsURL`, `guestUser`, and optional `kubectlVersion`, `kindVersion`, and `ciliumVersion`. The default guest account follows the host UID: UID 1000 uses Ubuntu's existing `ubuntu` account; other UIDs use `sandbox`. A custom `guestUser` must not collide with that UID mapping. Image and checksum URLs must use HTTPS, and the selected image must appear in its checksum file. Versions accept `stable`/`latest` or a semantic version with an optional `v` prefix.

The initial VM image and guest binaries target x86-64 Linux. Tool versions default to upstream stable/latest, so a new VM may install newer versions than an existing one. Pin versions in configuration when repeatability matters.

## Inspecting effective policy

Run `devfence plan` from the target directory before launch. It reports the selected profile, resolved workspace, live/copy mode, network policy, protected paths, selected SSH key basename, whether a token provider is configured, and VM resources. Secret values are never shown. A `full` network setting grants ordinary guest access to reachable network endpoints; it is not a host-service firewall.
