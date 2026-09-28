#!/bin/bash
# The claims in docs/claims.md that only a booted sandbox can prove.
#
# script/smoke.sh drives brig against a stub runtime, and the stub runs on the
# host. It shows what brig asked the runtime for. It cannot show what the
# guest reaches, so these checks boot a real sandbox with hull or nerdctl and
# ask from inside it.
#
# Each check is one vm_check line. A vm row in docs/claims.md names the word
# after vm_check, and script/check-claims.sh resolves the row against that
# line. A name that resolves is then always a check that runs.
#
# With no runtime on PATH this exits 0 and says it skipped, which is what a
# laptop without one wants. BRIG_CLAIMS_VM=require turns that into a failure,
# for a runner that exists to boot sandboxes.
#
# BRIG names the binary under test. make claims-vm sets it to the brig the
# checkout builds. Run by hand, it defaults to the brig on PATH.
#
# Usage: script/claims-vm.sh
# The check functions run through vm_check, which shellcheck cannot follow.
# shellcheck disable=SC2329
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1

case "${BRIG_CLAIMS_VM:-}" in
"" | require) ;;
*)
	echo "claims-vm: BRIG_CLAIMS_VM is require or unset, not \"$BRIG_CLAIMS_VM\"" >&2
	exit 2
	;;
esac

runtime=""
for cand in hull nerdctl; do
	if command -v "$cand" >/dev/null 2>&1; then
		runtime="$cand"
		break
	fi
done
if [ -z "$runtime" ]; then
	if [ "${BRIG_CLAIMS_VM:-}" = require ]; then
		echo "claims-vm: no runtime (hull or nerdctl) on PATH, and BRIG_CLAIMS_VM=require" >&2
		exit 1
	fi
	echo "claims-vm: skipped, no runtime (hull or nerdctl) on PATH"
	exit 0
fi

BRIG="${BRIG:-brig}"
# A label of its own keeps this sandbox and its index entry apart from any
# claude session the caller already has.
REF=claude@claims-vm
# The trap below removes $WORK, so an empty one has to stop the script first:
# cd "" stays in the checkout, and pwd then names the checkout.
WORK="$(mktemp -d)" && [ -n "$WORK" ] || exit 1
# macOS hands out /var/folders, which is a link to /private/var/folders.
# brig refuses a link on the way to the guest home, so name the real path.
WORK="$(cd "$WORK" && pwd -P)" || exit 1
# A labelled session appends its label to --home, so a run given
# $WORK/home mounts $WORK/home-claims-vm.
GUEST_DIR="$WORK/home-claims-vm"
PROJECT="$WORK/claimsproj"
OUTSIDE="$WORK/outside"
mkdir -p "$GUEST_DIR" "$PROJECT" "$OUTSIDE"
trap '"$BRIG" rm "$REF" >/dev/null 2>&1; rm -rf "$WORK"' EXIT

# The fixture the boundary is about: a file on the host, beside the guest
# home and the project but in neither, holding a string the guest must never
# print.
outside_marker="claims-vm outside $$"
printf '%s\n' "$outside_marker" >"$OUTSIDE/secret.txt"
printf 'claims-vm home\n' >"$GUEST_DIR/from-host.txt"
printf 'claims-vm project\n' >"$PROJECT/from-host.txt"

# A socket path of the kind a host SSH agent announces. brig forwards a
# variable only when a profile names it, so the guest never sees this one.
export SSH_AUTH_SOCK="$OUTSIDE/agent.sock"

fail=0
ok() { printf '  ok   %s\n' "$1"; }
bad() {
	printf '  FAIL %s\n' "$1"
	fail=1
}

# The guest command runs on a pty, so its lines end in CR LF. Every word is
# plain, with no space or quote, so the command reads the same whether brig
# sh joins its words into a script or passes them as arguments.
guest() { "$BRIG" -q sh "$REF" "$@" 2>&1 | tr -d '\r'; }

echo "claims-vm: booting $REF on $runtime"
if ! "$BRIG" -q run -d --home "$WORK/home" "$REF" "$PROJECT" >"$WORK/boot.out" 2>&1; then
	echo "claims-vm: the boot failed:"
	cat "$WORK/boot.out"
	exit 1
fi
guest_home="$(guest printenv HOME | tail -1)"
case "$guest_home" in
/*) ;;
*)
	echo "claims-vm: could not read the guest HOME, got: $guest_home"
	exit 1
	;;
esac

check_guest_home() {
	case "$(guest cat "$guest_home/from-host.txt")" in
	*"claims-vm home"*) ;;
	*) return 1 ;;
	esac
	guest touch "$guest_home/from-guest.txt" >/dev/null
	[ -f "$GUEST_DIR/from-guest.txt" ]
}

check_project() {
	case "$(guest cat /work/claimsproj/from-host.txt)" in
	*"claims-vm project"*) ;;
	*) return 1 ;;
	esac
	guest touch /work/claimsproj/from-guest.txt >/dev/null
	[ -f "$PROJECT/from-guest.txt" ]
}

# Two reads and the mount table. The reads are negative: the fixture by its
# host path, and the directory that holds the guest home and the project. A
# sandbox that died fails both reads too, so the first read proves this guest
# still reads its home. The mount table is the stronger half. Every virtiofs
# share in the guest has to come from the share behind the guest home or the
# one behind the project. The root is the image, which the runtime serves from
# its own store and not from a directory of yours.
check_other_host_directory() {
	local mounts home_src project_src extra
	case "$(guest cat "$guest_home/from-host.txt")" in
	*"claims-vm home"*) ;;
	*) return 1 ;;
	esac
	case "$(guest cat "$OUTSIDE/secret.txt")" in
	*"$outside_marker"*) return 1 ;;
	esac
	case "$(guest ls "$WORK")" in
	*outside*) return 1 ;;
	esac
	mounts="$(guest cat /proc/mounts)"
	home_src="$(printf '%s\n' "$mounts" | awk -v m="$guest_home" '$2 == m && $3 == "virtiofs" { print $1 }')"
	project_src="$(printf '%s\n' "$mounts" | awk '$2 == "/work/claimsproj" && $3 == "virtiofs" { print $1 }')"
	[ -n "$home_src" ] && [ -n "$project_src" ] || return 1
	extra="$(printf '%s\n' "$mounts" | awk -v h="$home_src" -v p="$project_src" \
		'$3 == "virtiofs" && $2 != "/" && $1 != h && $1 != p')"
	if [ -n "$extra" ]; then
		printf '  a share from neither the guest home nor the project: %s\n' "$extra"
		return 1
	fi
	return 0
}

check_ssh_agent() {
	[ -z "$(guest printenv SSH_AUTH_SOCK)" ]
}

vm_check() {
	local name="$1"
	shift
	if "$@"; then
		ok "$name"
	else
		bad "$name"
	fi
}

vm_check guest-home-read-write check_guest_home
vm_check project-at-work check_project
vm_check other-host-directory check_other_host_directory
vm_check ssh-agent-not-forwarded check_ssh_agent

if [ "$fail" = 0 ]; then
	echo "claims-vm: PASS"
else
	echo "claims-vm: FAIL"
fi
exit "$fail"
