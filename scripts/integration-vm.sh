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
mkdir -p "$test_root/agent-guidance/.env" "$test_root/shared-config" "$test_root/readonly-config"
sed "s|GLOBAL_INSTRUCTIONS_PATH|$test_root/global-instructions.md|" \
	"$project_root/scripts/fixtures/vm-integration.yaml" > "$test_root/config/devfence/config.yaml"
cat > "$test_root/global-instructions.md" <<'EOF'
When asked to report the global instruction marker, include DEVFENCE_VM_CODEX_INSTRUCTIONS_48a62d.
EOF
printf 'shared agent guidance\n' > "$test_root/agent-guidance/AGENTS.md"
printf 'host-only value\n' > "$test_root/agent-guidance/.env/secret"
printf 'initial host value\n' > "$test_root/shared-config/value.txt"
printf 'read-only host value\n' > "$test_root/readonly-config/value.txt"
printf 'host-only sentinel\n' > "$test_root/host-secret"
cat > "$test_root/project/.devfence.yaml" <<'EOF'
version: 3
shares:
  - source: ../agent-guidance
    target: agents
    mode: copy
    exclude: [.env]
  - source: ../shared-config
    target: shared-config
    mode: mount
    access: read-write
  - source: ../readonly-config
    target: readonly-config
    mode: mount
EOF

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
cat > "$test_root/start-session" <<EOF
#!/usr/bin/env bash
set -euo pipefail
export HOME="$test_root/home"
export XDG_CONFIG_HOME="$test_root/config"
export XDG_STATE_HOME="$test_root/state"
export XDG_CACHE_HOME="$test_root/cache"
exec "$binary" run --name "$session_id" -- /usr/bin/true
EOF
chmod 0700 "$test_root/start-session"
printf 'yes\n' | script -qefc "$test_root/start-session" /dev/null
devfence list

guest_secret_path="$(printf '%q' "$test_root/host-secret")"
guest_command="set -euxo pipefail
test ! -e $guest_secret_path
test \"\$(cat \"\$HOME/agents/AGENTS.md\")\" = 'shared agent guidance'
test ! -e \"\$HOME/agents/.env\"
test -r \"\$HOME/.codex/AGENTS.md\"
set +x
prompt_input=\$(codex debug prompt-input 'Report the global instruction marker.')
[[ "\$prompt_input" == *DEVFENCE_VM_CODEX_INSTRUCTIONS_48a62d* ]]
set -x
unset prompt_input
printf 'changed by VM\\n' > \"\$HOME/shared-config/from-vm\"
if touch \"\$HOME/readonly-config/forbidden\"; then exit 1; fi
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
test "$(cat "$test_root/shared-config/from-vm")" = 'changed by VM'
test ! -e "$test_root/readonly-config/forbidden"
devfence stop "$session_id"
devfence inspect "$session_id"
devfence attach "$session_id" -- /usr/bin/true
devfence delete "$session_id" --yes --force

printf 'VM, Docker, Kind, Cilium/eBPF, host-path isolation, attach, stop, restart, and deletion checks passed.\n'
