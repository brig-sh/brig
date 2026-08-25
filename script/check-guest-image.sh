#!/usr/bin/env bash
# Check a guest image against the contract in docs/guest-image.md.
#
# NOT wired into CI, and should not be. It pulls and boots the image you name,
# and CI has no registry access and no container runtime to boot one with, so a
# job running this would either fail on every pull request or be skipped into
# meaninglessness. This is a tool you run yourself, against an image you are
# building.
#
#   script/check-guest-image.sh <image> [guest-home] [binary]
#
#   <image>       the image reference, as you would pass it to `brig --image`
#   [guest-home]  the profile's guestHome:, default /home/agent. The guest user
#                 is its last path element, which is how brig derives it too
#   [binary]      the profile's binary:, checked for if given
#
# It boots the image the way brig does and runs each requirement as its own
# exec, so what it reports is behaviour rather than a file being present. An
# image that will not stay up (no `sleep`, most often) falls back to listing the
# image filesystem, and says so: those checks are presence only.
#
# Exit status: 0 every requirement met, 1 something is missing, 2 there was no
# runtime to check with. It never reports a pass it did not perform.
#
# Not checked here, because neither is a property of the image: that the guest
# has no swap, and that a tmpfs brig mounted reads back as tmpfs. brig verifies
# both inside the running sandbox on every delivery.
set -uo pipefail

usage() {
	printf 'usage: %s <image> [guest-home] [binary]\n' "$0" >&2
	exit 2
}

IMAGE="${1:-}"
[ -n "$IMAGE" ] || usage
GUEST_HOME="${2:-/home/agent}"
BINARY="${3:-}"
GUEST_USER="${GUEST_HOME##*/}"

# --- the runtime ---
# The same order brig itself resolves in, plus podman, which takes the same
# flags and is a reasonable thing to have when you are building an image.
# BRIG_RUNTIME_BIN wins, as it does for brig.
RT="${BRIG_RUNTIME_BIN:-}"
if [ -z "$RT" ]; then
	for candidate in nerdctl docker podman hull; do
		if RT="$(command -v "$candidate" 2>/dev/null)"; then break; fi
		RT=""
	done
fi
if [ -z "$RT" ]; then
	printf 'no container runtime found: install nerdctl, docker or podman, or set\n' >&2
	printf 'BRIG_RUNTIME_BIN. Nothing was checked.\n' >&2
	exit 2
fi
KIND="$(basename "$RT")"

NAME="brig-image-check-$$"
FAILED=0
MODE=exec # or "list", when the image would not stay up

cleanup() {
	if [ "$KIND" = hull ]; then
		"$RT" stop "$NAME" >/dev/null 2>&1
		"$RT" rm "$NAME" >/dev/null 2>&1
	else
		"$RT" rm -f "$NAME" >/dev/null 2>&1
	fi
	rm -f "$LISTING"
}
LISTING="$(mktemp)"
trap cleanup EXIT

# --- reporting ---
ok()   { printf '  ok   %-22s %s\n' "$1" "$2"; }
bad()  { printf '  FAIL %-22s %s\n' "$1" "$2"; FAILED=1; }
skip() { printf '  --   %-22s %s\n' "$1" "$2"; }

printf 'brig guest image contract: %s\n' "$IMAGE"
printf 'runtime: %s (%s)\n' "$KIND" "$RT"
printf 'guest home: %s, guest user: %s\n\n' "$GUEST_HOME" "$GUEST_USER"

# --- booting ---
# Exactly what brig does: no entrypoint override, and `sleep infinity` as the
# command on a container runtime, because that is the command brig parks the
# sandbox on. hull takes no command and lets the image's own entrypoint run,
# which is why `sleep` is not a hull requirement.
boot() {
	if [ "$KIND" = hull ]; then
		"$RT" run --detach --name "$NAME" "$IMAGE" >/dev/null 2>&1
	else
		"$RT" run -d --name "$NAME" "$IMAGE" sleep infinity >/dev/null 2>&1
	fi
}

running() {
	if [ "$KIND" = hull ]; then
		"$RT" ps 2>/dev/null | grep -q "^$NAME[[:space:]]\+running"
	else
		"$RT" ps --filter "name=^${NAME}$" --format '{{.Names}}' 2>/dev/null |
			grep -qx "$NAME"
	fi
}

# guest runs one command inside the sandbox as root, the way every brig setup
# exec runs. Output on stdout, everything else discarded.
guest() {
	if [ "$KIND" = hull ]; then
		"$RT" exec -u root "$NAME" -- "$@" 2>/dev/null
	else
		"$RT" exec -u root "$NAME" "$@" 2>/dev/null
	fi
}

# The fallback: create the container without starting it and list its
# filesystem. This is the only thing that works for an image that cannot run at
# all, which is exactly the scratch case worth reporting on.
build_listing() {
	if [ "$KIND" = hull ]; then
		return 1
	fi
	"$RT" create --name "$NAME" "$IMAGE" /bin/true >/dev/null 2>&1 || return 1
	"$RT" export "$NAME" 2>/dev/null | tar -tf - >"$LISTING" 2>/dev/null || return 1
	[ -s "$LISTING" ]
}

if boot && running; then
	MODE=exec
else
	printf 'The image would not stay up, so there is nothing to exec into.\n'
	if [ "$KIND" != hull ]; then
		"$RT" rm -f "$NAME" >/dev/null 2>&1
	fi
	if build_listing; then
		MODE=list
		printf 'Falling back to the image filesystem: the checks below are presence\n'
		printf 'only, and cannot tell you whether a binary actually works.\n\n'
	else
		printf 'The image filesystem could not be listed either, so nothing was checked.\n' >&2
		exit 2
	fi
fi

# --- the checks ---
# In exec mode each one runs in the guest; in list mode each one looks for the
# binary on the usual paths. run takes the label, the note, and the argv.
# tar lists paths without a leading slash, and with or without a ./ prefix
# depending on how the layer was built. Accept either, and a trailing slash for
# a directory entry.
have_path() {
	local p="${1#/}"
	grep -qx "\(\./\)\{0,1\}${p}/\{0,1\}" "$LISTING" 2>/dev/null
}

have_binary() {
	local b="$1" d
	case "$b" in
	/*)
		have_path "$b"
		return
		;;
	esac
	for d in /bin /usr/bin /sbin /usr/sbin /usr/local/bin /usr/local/sbin; do
		have_path "$d/$b" && return 0
	done
	return 1
}

# run <label> <binary-for-list-mode> <note> -- <argv...>
run() {
	local label="$1" bin="$2" note="$3"
	shift 4 # label, bin, note, --
	if [ "$MODE" = list ]; then
		if have_binary "$bin"; then ok "$label" "$note"; else bad "$label" "$note"; fi
		return
	fi
	if guest "$@" >/dev/null; then ok "$label" "$note"; else bad "$label" "$note"; fi
}

# expect <label> <binary-for-list-mode> <note> <wanted> -- <argv...>
# For the two checks where succeeding is not enough: stat has to answer in the
# format brig parses, not merely exit 0.
expect() {
	local label="$1" bin="$2" note="$3" want="$4"
	shift 5
	if [ "$MODE" = list ]; then
		if have_binary "$bin"; then ok "$label" "$note"; else bad "$label" "$note"; fi
		return
	fi
	local got
	got="$(guest "$@")"
	got="${got%%$'\n'*}"
	if [ "$got" = "$want" ]; then
		ok "$label" "$note"
	else
		bad "$label" "$note (said \"$got\", wanted \"$want\")"
	fi
}

SCRATCH=/run/brig-image-check

run '/bin/true' /bin/true 'readiness probe' -- /bin/true
run 'cat' cat 'workspace marker, mount table' -- cat /proc/self/mountinfo
run '/proc/swaps' cat 'the swap tripwire' -- cat /proc/swaps
run 'sh' sh 'the credential write scripts' -- sh -c 'exit 0'
expect 'stat -c' stat 'file kind, owner and mode' 'directory' -- stat -c %F /
run 'stat -f -c' stat 'which filesystem a path is on' -- stat -f -c %T /
run 'mkdir, /run writable' mkdir 'mount targets, pin directory' -- mkdir -p "$SCRATCH/d"
run 'dirname' dirname 'inside createGuestTarget' -- dirname /a/b
run 'chmod' chmod 'the mode a files: binding declares' -- chmod 0700 "$SCRATCH/d"
run 'chown, guest user' chown "hands directories to $GUEST_USER and back" -- \
	chown "$GUEST_USER" "$SCRATCH/d"
run 'rm' rm 'removes a planted symlink rather than following it' -- \
	rm -f "$SCRATCH/absent"
run 'mount, tmpfs' mount 'covers a directory so a credential stays off disk' -- \
	mount -t tmpfs -o size=1m,mode=0700,nodev,nosuid tmpfs "$SCRATCH/d"
run 'sleep' sleep 'the container command on the nerdctl path' -- sleep 0
run 'bash' bash 'brig shell' -- bash -lc 'exit 0'

if [ -n "$BINARY" ]; then
	run "$BINARY" "$BINARY" "the profile's binary:" -- "$BINARY" --version
else
	skip 'binary:' 'not given, so the agent CLI was not checked'
fi

# The guest home is created by the workspace mount, so its absence is not a
# failure. Said out loud because an image that ships content there is a mistake
# worth knowing about: the mount hides all of it.
if [ "$MODE" = exec ] && guest test -d "$GUEST_HOME"; then
	printf '\nnote: %s exists in the image. The workspace is mounted over it, so\n' "$GUEST_HOME"
	printf '      anything the image ships there is invisible in the sandbox.\n'
fi

printf '\n'
if [ "$FAILED" -eq 0 ]; then
	if [ "$MODE" = list ]; then
		printf 'Every binary is present. Boot the image to check they work.\n'
	else
		printf 'This image satisfies the contract in docs/guest-image.md.\n'
	fi
	exit 0
fi
printf 'This image does not satisfy the contract. See docs/guest-image.md.\n'
exit 1
