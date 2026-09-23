# Devfence

Devfence starts a shell or development tool with an explicit project workspace and runtime policy. It supports lightweight Bubblewrap sessions, rootless Docker containers, and KVM guests managed through libvirt. The VM backend is for projects that need a separate kernel, including Docker, Kind, Cilium, and eBPF.

Devfence is Linux-first. It invokes host tools instead of replacing them, and reports the selected backend, workspace mode, network setting, and configured tool forwarding before launch. SSH identities and GitHub tokens are passed according to host-side policy.

Bubblewrap sessions need `bwrap`; interactive sessions also need util-linux `script` for a private terminal.

The generated configuration is generic and forwards no host tools, directories, or credentials. Configure Codex, Claude, `gh`, global agent instructions, and trusted GitHub workspace paths in the host user's Devfence config. Put project runtime settings and optional host-path share requests in that project's `.devfence.yaml`; each new share request requires explicit host approval once. The same host defaults work with Bubblewrap, rootless Docker, and VM sessions.

## Quick start

Build and install the CLI:

```sh
make check
go install ./cmd/devfence
devfence config init
# Run from a project to create its optional .devfence.yaml:
devfence config init --project
```

From any directory, start a shell; use a command after `--` to launch another tool:

```sh
cd path/to/project
devfence
devfence -- codex
devfence --backend vm -- codex
```

Inspect the resolved policy before starting a session with `devfence plan`. Host configuration lives at `${XDG_CONFIG_HOME:-~/.config}/devfence/config.yaml`; project settings live in `.devfence.yaml` at each workspace root; session state lives at `${XDG_STATE_HOME:-~/.local/state}/devfence`. See [the configuration reference](docs/configuration.md) and [the security model](docs/security-model.md).

## Session management

`devfence list`, `devfence inspect ID`, and `devfence attach ID` show and reconnect to persistent container or VM sessions. `devfence stop ID` stops one. `devfence delete ID` requires an explicit confirmation flag for persistent resources and checks for unexported or dirty workspace changes. A host Bubblewrap session exists only for the lifetime of the foreground command.

The VM backend needs libvirt/KVM, `virt-install`, `virsh`, `qemu-img`, and a usable libvirt network. Its first launch downloads and checksum-verifies an Ubuntu cloud image, then provisions a guest with Docker, Kind, kubectl, the Cilium CLI, and an isolated kernel. VM networking currently uses libvirt's default NAT network; VM project settings cannot select `network: none` yet, and networked guests may reach services exposed by the host or LAN. See [the security model](docs/security-model.md) and [VM setup and verification](docs/vm-setup.md).

## Development

Run `make check` for formatting, vetting, tests, and a build. Tests that require Bubblewrap run when the host supports unprivileged namespaces. Container and VM command behavior is covered with fake executables; `scripts/integration-vm.sh` is an explicit end-to-end check and creates a disposable VM.

Project operating guidance for contributors and coding tools is in [AGENTS.md](AGENTS.md).
