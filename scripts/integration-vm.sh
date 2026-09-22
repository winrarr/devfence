#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
binary="$project_root/devfence"
if [[ ! -x "$binary" ]]; then
	printf 'Build Devfence first with `make build`.\n' >&2
	exit 1
fi
for tool in virsh virt-install qemu-img ssh ssh-keygen; do
	command -v "$tool" >/dev/null || { printf 'Missing required tool: %s\n' "$tool" >&2; exit 1; }
done
virsh --connect qemu:///system net-info default >/dev/null

test_root="$(mktemp -d "${TMPDIR:-/tmp}/devfence-vm-test.XXXXXX")"
session_id="vm-smoke-$(date +%s)-$$"
mkdir -p "$test_root/home" "$test_root/config/devfence" "$test_root/state" "$test_root/cache" "$test_root/project"
cp "$project_root/scripts/fixtures/vm-integration.yaml" "$test_root/config/devfence/config.yaml"
printf 'host-only sentinel\n' > "$test_root/host-secret"

devfence() {
	env HOME="$test_root/home" \
		XDG_CONFIG_HOME="$test_root/config" \
		XDG_STATE_HOME="$test_root/state" \
		XDG_CACHE_HOME="$test_root/cache" \
		"$binary" "$@"
}

cleanup() {
	if [[ -f "$test_root/state/devfence/sessions/$session_id/session.json" ]]; then
		if ! devfence delete "$session_id" --yes --force; then
			printf 'Warning: cleanup failed; preserve state and clean VM %s manually from %s.\n' "$session_id" "$test_root" >&2
			return
		fi
	fi
	rm -rf -- "$test_root"
}
trap cleanup EXIT

cd "$test_root/project"
devfence config validate
devfence plan
devfence run --name "$session_id" -- /usr/bin/true
devfence list

guest_secret_path="$(printf '%q' "$test_root/host-secret")"
guest_command="set -euxo pipefail
test ! -e $guest_secret_path
test -S /var/run/docker.sock
test -w /workspace
touch /workspace/.devfence-vm-smoke
rm /workspace/.devfence-vm-smoke
sudo -n true
docker info >/dev/null
kind version
kubectl version --client
cilium version --client
kind create cluster --name devfence-smoke --wait 5m
cilium install --wait --wait-duration 5m
cilium status --wait --wait-duration 5m
kubectl get nodes -o wide
kind delete cluster --name devfence-smoke"
devfence attach "$session_id" -- /bin/bash -lc "$guest_command"
devfence stop "$session_id"
devfence inspect "$session_id"
devfence attach "$session_id" -- /usr/bin/true
devfence delete "$session_id" --yes --force

printf 'VM, Docker, Kind, Cilium/eBPF, host-path isolation, attach, stop, restart, and deletion checks passed.\n'
