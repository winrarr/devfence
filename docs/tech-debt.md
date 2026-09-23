# Tech debt

Each entry states a current limitation, its user impact, and an acceptance condition for addressing it. Remove an entry when the fix ships.

## VM network policy cannot deny guest egress

- Goal: let a VM retain a host-controlled management channel while denying or narrowing the guest's other network access.
- Rationale: the VM currently uses libvirt's default NAT network for SSH, provisioning, GitHub, and image pulls; `network: none` is rejected. A networked VM may reach host/LAN services.
- Constraints: do not expose a host agent, arbitrary host socket, or host filesystem to achieve management; policy must be enforced outside the guest and shown in `plan`/`inspect`.
- Acceptance: automated tests prove a VM can be managed while denied guest egress cannot reach a public endpoint or a host test service; `network: none` and any allowlist mode fail closed if enforcement is unavailable.

## VM provisioning uses mutable tool channels by default

- Goal: make fresh VMs reproducible while retaining a low-friction first launch.
- Rationale: enabled Codex/Claude packages and some Kubernetes tools install upstream `latest`/`stable`; two sessions created at different times may differ.
- Constraints: allow users to pin supported tool versions and do not embed additional machine-specific paths or account data beyond explicitly requested defaults.
- Acceptance: a pinned configuration provisions the same reported tool versions on two fresh guests; `plan` or `inspect` identifies the selected versions.

## VM bootstrap targets x86-64 only

- Goal: support the host architectures supported by the VM image, Kind, kubectl, and Cilium release artifacts.
- Rationale: the default Ubuntu image URL and guest bootstrap currently use amd64 binaries.
- Constraints: fail with a clear error on unsupported architectures; never silently install binaries for a mismatched architecture.
- Acceptance: architecture selection is covered by unit tests and a smoke test on each advertised architecture.

## Host Node tool dependency version conflicts are not forwarded

- Goal: support forwarded Codex or Claude Node installations whose dependency closure contains multiple installed versions of the same package name.
- Rationale: Devfence forwards only each selected package and its declared dependency closure. The current flat dependency view rejects conflicting versions rather than exposing a broad host package directory.
- Constraints: keep selected host files read-only and do not expose the host home or unrelated global package directories.
- Acceptance: conflicting versions resolve to the version selected for each package, with tests proving unrelated host packages remain hidden.
