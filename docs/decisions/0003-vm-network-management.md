# VM networking requires a management path

- Status: accepted
- Date: 2026-09-22

## Decision

The initial VM backend supports only `network: full`; it rejects `network: none` rather than disabling the SSH channel it currently depends on or implying that guest egress is blocked.

## Rationale

VM creation, guest provisioning, terminal/editor attachment, and optional GitHub access currently use the guest's libvirt network interface. No separately enforced host-only management channel exists yet.

## Consequences

Networked VMs can reach services exposed by the host or LAN through libvirt NAT. Users needing that boundary must choose another supported backend or not run the VM until an independently enforced management/egress design is available. See the related [tech debt](../tech-debt.md) and [backlog item](../backlog.md).
