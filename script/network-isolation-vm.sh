#!/bin/bash
# Measure the default network with two real guests (#369). A failed request
# counts only while its listener, both guests and an outbound control work.
# Exit 0: expected observation; 1: opposite observation; 2: setup/probe error.
# --expect reachable reproduces the old default; --expect isolated guards it.
set -uo pipefail

usage() {
	cat <<'EOF'
Usage: BRIG=/path/to/brig script/network-isolation-vm.sh [options]
  --expect reachable|isolated   expected default (default: isolated)
  --skip-shared-control        omit the explicit shared-network positive control
  --image REF                  image with bash, curl, ip and python3
  --publish-port PORT          host loopback control port (default: 18369)
  --outbound-url URL           reachable HTTP(S) control (default: https://example.com)
  --self-test                  check probe/verdict guards without a VM

Uses hull/hvi on macOS or nerdctl/io.containerd.urunc.v2 on Linux.
The default image is ghcr.io/brig-sh/claude-code-stock:root. No credential
names are declared. Scratch profiles, homes, state and sandboxes are removed.
BRIG_RUNTIME_BIN and boot-asset/image-verification settings are honored.
EOF
}

problem() { echo "network-isolation-vm: ERROR: $*" >&2; exit 2; }
quote() { printf '%q ' "$@"; }
run_brig() { "$BRIG" "$@"; }
exec_guest() {
	local name="brig-${1/@/-}"
	case "$BRIG_RUNTIME" in
	hull) "$runtime" exec "$name" -- bash -lc "$2" ;;
	nerdctl) "$runtime" exec "$name" bash -lc "$2" ;;
	esac
}

# Probe the guests Brig booted through native exec. Unlike brig sh, it cannot
# recreate a vanished/stale sandbox. A boot-ID check still rejects a reboot
# so it cannot turn a failed connection into evidence about the original pair.
guest() {
	local ref="$1" script="$2" expected="$3" out last boot status
	out="$(exec_guest "$ref" "$script; rc=\$?; printf '\\nNI_DONE boot=%s status=%s\\n' \"\$(cat /proc/sys/kernel/random/boot_id)\" \"\$rc\"" 2>&1 | tr -d '\r')" || { echo "guest $ref exec failed: $out" >&2; return 2; }
	last="$(printf '%s\n' "$out" | tail -1)"
	if [[ ! "$last" =~ ^NI_DONE\ boot=([0-9a-f-]+)\ status=([0-9]+)$ ]]; then
		echo "guest $ref gave no complete probe: $out" >&2
		return 2
	fi
	boot="${BASH_REMATCH[1]}"; status="${BASH_REMATCH[2]}"
	if [ -n "$expected" ] && [ "$boot" != "$expected" ]; then
		echo "guest $ref rebooted during the measurement: $out" >&2
		return 2
	fi
	[ "$status" = 0 ] || { echo "guest $ref probe exited $status: $out" >&2; return 2; }
	printf '%s\n' "$out" | sed '$d'
}

# Curl reports its peer only after connecting. A timeout after connect, an
# HTTP error, a missing curl and a wrong body are all inconclusive. A refused
# connection can come from a reachable guest with a closed port, so only a
# pre-connect timeout counts as isolation. The marker belongs only to A, so
# reaching another listener (including B itself) cannot pass as reachable.
classify() {
	local out="$1" marker="$2" last code peer body
	last="$(printf '%s\n' "$out" | tail -1)"
	[[ "$last" =~ ^NI_CURL\ code=([0-9]+)\ peer=([0-9.]*)$ ]] || return 2
	code="${BASH_REMATCH[1]}"; peer="${BASH_REMATCH[2]}"
	body="$(printf '%s\n' "$out" | sed '$d')"
	if [ "$code" = 0 ] && [ -n "$peer" ] && [ "$body" = "$marker" ]; then
		echo reachable
	elif [ "$code" = 28 ] && [ -z "$peer" ]; then
		echo isolated
	else
		return 2
	fi
}

self_test() {
	local failed=0 got rc name expected input
	while IFS='|' read -r name expected input; do
		got="$(classify "$(printf '%b' "$input")" only-in-a)"; rc=$?
		if [ "$expected" = error ]; then
			[ "$rc" = 2 ] || { echo "FAIL: $name"; failed=1; }
		else
			[ "$rc:$got" = "0:$expected" ] || { echo "FAIL: $name"; failed=1; }
		fi
	done <<'EOF'
marker proves A|reachable|only-in-a\nNI_CURL code=0 peer=10.1.0.2
timeout before connect|isolated|NI_CURL code=28 peer=
refused connection|error|NI_CURL code=7 peer=
timeout after connect|error|NI_CURL code=28 peer=10.1.0.2
missing curl|error|NI_CURL code=127 peer=
failed DNS|error|NI_CURL code=6 peer=
wrong listener|error|only-in-b\nNI_CURL code=0 peer=10.1.0.2
empty body|error|NI_CURL code=0 peer=10.1.0.2
HTTP failure|error|NI_CURL code=22 peer=10.1.0.2
missing answer|error|
EOF
	# Exercise the actual guest-envelope parser, including a dead/rebooted
	# guest, rather than merely checking the network verdict in isolation.
	exec_guest() { printf '%b' "$fake_answer"; }
	local fake_answer
	for fake_answer in '' 'NI_DONE boot=def status=0\n' 'NI_DONE boot=abc status=127\n'; do
		guest a true abc >/dev/null 2>&1 && { echo 'FAIL: invalid guest accepted'; failed=1; }
	done
	fake_answer='answer\n\nNI_DONE boot=abc status=0\n'
	got="$(guest a true abc)" || failed=1
	[ "$got" = answer ] || failed=1
	[ "$failed" = 0 ] && echo 'network-isolation-vm: self-test PASS'
	return "$failed"
}

expect=isolated
shared_control=1
image=ghcr.io/brig-sh/claude-code-stock:root
publish_port=18369
outbound_url=https://example.com
while [ "$#" -gt 0 ]; do
	case "$1" in
	--expect|--image|--publish-port|--outbound-url)
		[ "$#" -ge 2 ] || problem "$1 needs a value"
		case "$1" in
		--expect) expect="$2" ;;
		--image) image="$2" ;;
		--publish-port) publish_port="$2" ;;
		--outbound-url) outbound_url="$2" ;;
		esac
		shift 2 ;;
	--skip-shared-control) shared_control=0; shift ;;
	--self-test) self_test; exit $? ;;
	--help|-h) usage; exit 0 ;;
	*) problem "unknown option $1" ;;
	esac
done
case "$expect" in reachable|isolated) ;; *) problem 'invalid --expect' ;; esac
[[ "$image" =~ ^[a-zA-Z0-9./_:@-]+$ ]] || problem 'invalid image reference'
if [[ ! "$publish_port" =~ ^[1-9][0-9]{0,4}$ ]] || [ "$publish_port" -gt 65535 ]; then
	problem 'invalid publish port'
fi
case "$outbound_url" in http://*|https://*) ;; *) problem 'outbound control must use HTTP(S)' ;; esac
BRIG="$(command -v "${BRIG:-brig}")" || problem 'brig is not installed; set BRIG to the binary under test'
command -v curl >/dev/null || problem 'host curl is required'

# Keep only runtime location and verification inputs. In particular, a
# caller's BRIG_NETWORK, policy or skills flag must not change this test.
for setting in ${!BRIG_@}; do
	case "$setting" in
	BRIG_RUNTIME_BIN|BRIG_BOOT_ASSETS|BRIG_BOOT_ASSETS_REF|BRIG_VERIFY|BRIG_VERIFY_REGISTRY|BRIG_VERIFY_IDENTITY|BRIG_VERIFY_ISSUER|BRIG_COSIGN_BIN|BRIG_PULL) ;;
	*) unset "$setting" ;;
	esac
done
case "$(uname -s)" in
Darwin) export BRIG_RUNTIME=hull BRIG_HYPERVISOR=hvi ;;
Linux) export BRIG_RUNTIME=nerdctl BRIG_CONTAINERD_RUNTIME=io.containerd.urunc.v2 ;;
*) problem 'requires macOS or Linux' ;;
esac
runtime="${BRIG_RUNTIME_BIN:-$BRIG_RUNTIME}"
command -v "$runtime" >/dev/null || problem "runtime $runtime is not installed"

# A short physical path fits macOS's Unix-socket limit and avoids /tmp's
# symlink being rejected as a guest-home ancestor.
work="$(mktemp -d /tmp/bni.XXXXXX)" || problem 'cannot create scratch directory'
work="$(cd "$work" && pwd -P)" || exit 2
refs=()
server_pid=''
cleanup_pair() {
	local ref code failed=0
	# The alternate expansion is Bash 3.2's empty-array form under set -u.
	for ref in ${refs[@]+"${refs[@]}"}; do
		run_brig -q rm "$ref" >/dev/null 2>&1; code=$?
		# A boot may fail before creating B; a cleanup retry may already
		# have removed A. Not-found (3) is already clean in either case.
		case "$code" in 0|3) ;; *) echo "could not remove $ref" >&2; failed=1 ;; esac
	done
	[ "$failed" = 0 ] && refs=()
	if [ -n "$server_pid" ]; then
		kill "$server_pid" >/dev/null 2>&1 || true
		wait "$server_pid" 2>/dev/null || true
		server_pid=''
	fi
	return "$failed"
}
cleanup_gateways() {
	[ "$BRIG_RUNTIME" = hull ] || return 0
	local table pid argv current socket i
	table="$(ps -A -ww -o pid=,command=)" || return 1
	# Shared gateways outlive individual sandboxes. This run has its own
	# socket directory; inspect the command again before signaling so cleanup
	# never uses a stale PID or the user's ordinary shared gateway.
	while read -r pid argv; do
		case "$argv" in *network-gateway*"--socket $BRIG_GATEWAY_DIR/"*) ;; *) continue ;; esac
		socket="${argv#*--socket }"; socket="${socket%% *}"
		[ "${socket%/*}" = "$BRIG_GATEWAY_DIR" ] || continue
		[[ "$pid" =~ ^[0-9]+$ ]] && [ "$pid" -gt 1 ] || return 1
		current="$(ps -ww -o command= -p "$pid")" || continue
		[ "$current" = "$argv" ] || continue
		kill "$pid" || return 1
		for ((i=0; i<30; i++)); do
			kill -0 "$pid" 2>/dev/null || break
			sleep 0.1
		done
		kill -0 "$pid" 2>/dev/null && return 1
	done <<<"$table"
	return 0
}
cleanup() {
	local result=$?
	if cleanup_pair && cleanup_gateways; then
		rm -rf "$work" || result=2
	else
		echo "retained fixture for cleanup: $work" >&2
		result=2
	fi
	trap - EXIT
	exit "$result"
}
trap cleanup EXIT
trap 'exit 2' HUP INT TERM
export BRIG_PROFILE_DIR="$work/profiles" BRIG_POLICY_DIR="$work/policies"
export BRIG_STATE_DIR="$work/state" BRIG_GATEWAY_DIR="$work/gw" BRIG_WORKSPACE="$work/home"
mkdir -p "$BRIG_PROFILE_DIR" "$BRIG_POLICY_DIR" "$BRIG_STATE_DIR" || problem 'cannot create fixture'
profile="ni-$$-$RANDOM"
cat >"$BRIG_PROFILE_DIR/$profile.yaml" <<EOF
name: $profile
desc: two-guest default-network check
binary: sh
image: $image
genericBoot: true
guestHome: /root
mem: 1024
cpus: 2
EOF

echo "network-isolation-vm: $(run_brig version)"
"$runtime" --version || problem 'runtime version failed'
echo "network-isolation-vm: runtime=$BRIG_RUNTIME image=$image"

# -q is curl's first argument: neither the guest nor host curlrc should
# introduce a proxy. Numeric addresses avoid mistaking a DNS failure for
# isolation. HTTP errors remain errors even when the transport succeeded.
fetch() {
	guest "$1" "curl -q --noproxy '*' -fsS --connect-timeout 3 --max-time 5 $(quote "$2")" "$3"
}

pair() {
	local mode="$1" wanted="$2" a b boot_a boot_b ip marker out observation i ready
	local flags=()
	[ "$mode" = shared ] && flags=(--network shared)
	a="$profile@$mode-a"; b="$profile@$mode-b"
	refs=("$a" "$b")
	# Only the explicit positive control supplies a --network argument.
	run_brig -q run -d --no-project --home "$work/home" --publish "$publish_port:8080" ${flags[@]+"${flags[@]}"} "$a" || problem 'A did not boot'
	run_brig -q run -d --no-project --home "$work/home" ${flags[@]+"${flags[@]}"} "$b" || problem 'B did not boot'
	boot_a="$(guest "$a" 'cat /proc/sys/kernel/random/boot_id' '')" || problem 'A did not answer'
	boot_b="$(guest "$b" 'cat /proc/sys/kernel/random/boot_id' '')" || problem 'B did not answer'
	guest "$a" 'command -v curl >/dev/null && command -v python3 >/dev/null && command -v ip >/dev/null' "$boot_a" >/dev/null || problem 'A needs curl, python3 and ip'
	guest "$b" 'command -v curl >/dev/null' "$boot_b" >/dev/null || problem 'B needs curl'
	ip="$(guest "$a" "ip -4 -o addr show scope global | awk 'NR == 1 {split(\$4, a, \"/\"); print a[1]}'" "$boot_a")" || problem 'cannot read A address'
	[[ "$ip" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || problem "A has no usable IPv4 address: $ip"
	marker="only-in-$a-${work##*/}"
	guest "$a" "mkdir -p /tmp/ni-server && printf '%s\\n' $(quote "$marker") > /tmp/ni-server/who.txt" "$boot_a" >/dev/null || problem 'cannot prepare listener'
	exec_guest "$a" 'cd /tmp/ni-server && exec python3 -u -m http.server 8080 --bind 0.0.0.0' >"$work/server.log" 2>&1 &
	server_pid=$!
	ready=0
	for ((i=0; i<15; i++)); do
		out="$(fetch "$a" "http://$ip:8080/who.txt" "$boot_a" 2>/dev/null)" && [ "$out" = "$marker" ] && { ready=1; break; }
		sleep 1
	done
	[ "$ready" = 1 ] || { cat "$work/server.log" >&2; problem 'listener did not become ready'; }

	# Bracket the negative probe with controls. A dead listener, a reboot or
	# an entirely disconnected B leaves no evidence about guest separation.
	controls() {
		local control_out
		if ! control_out="$(fetch "$a" "http://$ip:8080/who.txt" "$boot_a")" || [ "$control_out" != "$marker" ]; then
			problem 'A self-connect control failed'
		fi
		if ! control_out="$(curl -q --noproxy '*' -fsS --connect-timeout 3 --max-time 5 "http://127.0.0.1:$publish_port/who.txt")" || [ "$control_out" != "$marker" ]; then
			problem 'host published-port control failed'
		fi
		guest "$b" "curl -q --noproxy '*' -fsS --connect-timeout 5 --max-time 15 -o /dev/null $(quote "$outbound_url")" "$boot_b" >/dev/null || problem 'B outbound control failed'
	}
	controls
	out="$(guest "$b" "peer=\$(curl -q --noproxy '*' -sS --connect-timeout 5 --max-time 10 -o /tmp/ni-body -w '%{remote_ip}' $(quote "http://$ip:8080/who.txt")); code=\$?; if [ \"\$code\" = 0 ]; then cat /tmp/ni-body; fi; printf 'NI_CURL code=%s peer=%s\\n' \"\$code\" \"\$peer\"" "$boot_b")" || problem 'B cross-guest probe failed'
	controls
	observation="$(classify "$out" "$marker")" || problem "inconclusive cross-guest response: $out"
	echo "network-isolation-vm: $mode B -> A ($ip:8080): $observation ($(printf '%s\n' "$out" | tail -1)); listener, host publish and outbound controls passed before and after"
	cleanup_pair || problem 'sandbox cleanup failed'
	# Each pair starts with a fresh gateway too: reusing one after removing
	# its guests can leave old forwarding state in the runtime under test.
	cleanup_gateways || problem 'gateway cleanup failed'
	[ "$observation" = "$wanted" ] || { echo "network-isolation-vm: FAIL: expected $wanted" >&2; return 1; }
}

if [ "$shared_control" = 1 ]; then pair shared reachable || problem 'explicit shared positive control failed'; fi
pair default "$expect" || exit 1
echo 'network-isolation-vm: PASS'
