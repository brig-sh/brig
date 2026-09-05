#!/bin/bash
# Claims that only a real VM can prove, run against whatever runtime is present.
#
# The isolation boundary -- "the guest sees one directory and nothing else on
# the host is reachable from inside it" -- cannot be shown against the stub
# runtime that script/smoke.sh uses, because that stub is a shell script running
# on the host, not a guest kernel. A stub that answered "I cannot read
# /etc/shadow" would be asserting its own politeness, not the sandbox's. So this
# claim runs only where a real runtime exists, and skips cleanly everywhere else
# rather than pretending a stub proved it.
#
#   script/claims-vm.sh
#
# In CI there is no runtime, so this exits 0 after saying it skipped. On a Mac
# with hull, or a Linux host with nerdctl and the urunc shim, it boots a real
# sandbox and checks the boundary from inside it.
set -uo pipefail
cd "$(dirname "$0")/.."

runtime=""
for cand in hull nerdctl; do
	if command -v "$cand" >/dev/null 2>&1; then
		runtime="$cand"
		break
	fi
done

if [ -z "$runtime" ]; then
	echo "claims-vm: no runtime (hull or nerdctl) present, skipping"
	exit 0
fi

echo "claims-vm: found $runtime, booting a real sandbox"

WORK="$(mktemp -d)"
trap 'brig stop claude >/dev/null 2>&1; rm -rf "$WORK"' EXIT
WS="$WORK/ws"
mkdir -p "$WS"
# A scratch workspace of our own, never the caller's, so the fixture and the
# sandbox are self-contained.
export BRIG_WORKSPACE="$WS"

# The fixture is the thing the whole boundary is about: a file that lives on the
# host, outside the workspace, holding something the guest must never see. If
# the guest can read this, the one-directory promise is broken.
secret="$WORK/outside-the-workspace.txt"
printf 'this file is on the host and outside the workspace\n' >"$secret"

fail=0
ok() { printf '  ok   %s\n' "$1"; }
bad() {
	printf '  FAIL %s\n' "$1"
	fail=1
}

# Ask the guest to read the host fixture by its host path. The workspace is the
# guest's home and the only thing mounted, so this path does not exist inside
# the guest at all: the read must fail. A run that comes back with the fixture's
# contents is the regression this defends against.
out="$(brig shell claude cat "$secret" 2>&1)"
case "$out" in
*"this file is on the host"*)
	bad "the guest read a host file outside the workspace"
	;;
*)
	ok "a host file outside the workspace is unreadable from the guest"
	;;
esac

# The converse, so a boundary that refuses everything is not mistaken for one
# that isolates: the workspace itself IS reachable, because that is the home.
printf 'inside\n' >"$WS/inside.txt"
out="$(brig shell claude cat /home/claude/inside.txt 2>&1)"
case "$out" in
*inside*)
	ok "the workspace itself is reachable as the guest home"
	;;
*)
	bad "the workspace was not reachable as the guest home -- got: $out"
	;;
esac

[ "$fail" = 0 ] && echo "claims-vm: PASS" || echo "claims-vm: FAILURES"
exit "$fail"
