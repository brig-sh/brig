#!/usr/bin/env bash
# Check a guest image against the contract in docs/guest-image.md.
#
# NOT wired into CI, and should not be. It pulls and boots the image you name,
# and CI has no registry access and no container runtime to boot one with, so a
# job running this would either fail on every pull request or be skipped into
# meaninglessness. This is a tool you run yourself, against an image you are
# building.
#
#   script/check-guest-image.sh <image> [profile]
#
#   <image>    the image reference, as you would pass it to `brig --image`
#   [profile]  the profile to boot it under, default claude-code. Its
#              guestHome: is the guest home, and the guest user is that path's
#              last element, which is how brig derives it too. Its binary: is
#              the agent CLI, checked for when the profile names one
#
# The boot goes through brig: `BRIG_IMAGE=<image> brig create <profile>`, torn
# down with `brig rm`. That is the point of the script rather than an
# implementation detail. An image booted bare as a container -- `hull run
# <image>` with no profile behind it -- runs with the runtime's default
# capability set and no CAP_SYS_ADMIN, so `mount -t tmpfs` in it fails with
# EPERM for images brig mounts a tmpfs in every day. Going through brig gets
# the profile's hypervisor, rootfs type, generic-boot annotations and guest
# home, and root in that sandbox holds every capability. It is also the only
# boot mode brig has, so it is the one the contract describes.
#
# The checks run as root through the runtime's own exec against the sandbox
# brig created, one exec per requirement, so what they report is behaviour
# rather than a file being present. An image that will not boot falls back to
# listing the image filesystem, and says so: those checks are presence only.
#
# The boot is a real run of a real profile, so whatever credentials that
# profile delivers reach the image under test. Point this at an image you are
# building, not at one you have a reason to distrust.
#
# BRIG_VERIFY=off is set for the boot, so an image under test that nobody has
# signed is not refused. BRIG_DRY_RUN=1 prints what would be booted and stops
# before booting it, which is how the argument handling and the profile lookup
# can be checked on a machine with no runtime.
#
# Exit status: 0 every requirement met, 1 something is missing, 2 there was
# nothing to check with -- no brig, no runtime, or no such profile. It never
# reports a pass it did not perform.
#
# Not checked here, because neither is a property of the image: that the guest
# has no swap, and that a tmpfs brig mounted reads back as tmpfs. brig verifies
# both inside the running sandbox on every delivery.
set -uo pipefail

usage() {
	printf 'usage: %s <image> [profile]\n' "$0" >&2
	exit 2
}

IMAGE="${1:-}"
[ -n "$IMAGE" ] || usage
PROFILE="${2:-claude-code}"

dry() { [ -n "${BRIG_DRY_RUN:-}" ] && [ "${BRIG_DRY_RUN}" != 0 ]; }

# --- brig itself ---
# $BRIG first, then PATH, then the binary a `make build` leaves in the
# checkout. Which one answered is printed: running a checkout's brig against a
# profile you edited in your installed one is the confusion worth ruling out
# before reading any of the lines below.
BRIG_BIN="${BRIG:-}"
BRIG_FROM='$BRIG'
if [ -z "$BRIG_BIN" ]; then
	if BRIG_BIN="$(command -v brig 2>/dev/null)"; then
		BRIG_FROM='PATH'
	else
		BRIG_BIN="$(cd "$(dirname "$0")/.." && pwd)/brig"
		BRIG_FROM='this checkout'
		if [ ! -x "$BRIG_BIN" ]; then
			printf 'no brig found: put one on PATH, run `make build`, or set BRIG.\n' >&2
			printf 'Nothing was checked.\n' >&2
			exit 2
		fi
	fi
fi

# --- the profile ---
# Read from brig rather than from internal/profile/specs, so a profile of your
# own and a file that overrides a built-in are read the same way brig will read
# them at boot. The JSON export is the resolved profile, one field per line.
if ! SPEC="$("$BRIG_BIN" profile export "$PROFILE" --json 2>&1)"; then
	printf '%s\n' "$SPEC" >&2
	printf 'Nothing was checked.\n' >&2
	exit 2
fi
field() {
	printf '%s\n' "$SPEC" | sed -n 's/^  "'"$1"'": "\(.*\)",*$/\1/p' | head -1
}
GUEST_HOME="$(field guestHome)"
BINARY="$(field binary)"
PROFILE_RUNTIME_BIN="$(field runtimeBin)"
if [ -z "$GUEST_HOME" ]; then
	printf 'the %s profile declares no guestHome:, so there is no guest home to\n' "$PROFILE" >&2
	printf 'check against. Nothing was checked.\n' >&2
	exit 2
fi
# The last path element, which is how brig derives the account it chowns to.
# See GuestUser in internal/profile/profile.go.
GUEST_USER="${GUEST_HOME##*/}"
# Whether this profile delivers anything as a file or a mount. Everything from
# sh down to rm in the table is only run for a profile that does, so a missing
# guest user costs a profile with neither nothing today -- which the line that
# reports it should say rather than predicting a chown that never happens.
DELIVERS=no
if printf '%s\n' "$SPEC" | grep -q '^  "\(files\|volumes\)": \['; then
	DELIVERS=yes
fi

# --- the runtime ---
# The checks exec into the sandbox brig booted, which means finding the same
# runtime brig will find: BRIG_RUNTIME picks the backend, hull on macOS and
# nerdctl elsewhere by default, and BRIG_RUNTIME_BIN then the profile's
# runtimeBin pick the binary. See DetectFor in internal/runtime/runtime.go.
KIND="${BRIG_RUNTIME:-}"
if [ -z "$KIND" ]; then
	case "$(uname -s)" in
	Darwin) KIND=hull ;;
	*) KIND=nerdctl ;;
	esac
fi
case "$KIND" in
hull) CANDIDATES='hull' ;;
nerdctl) CANDIDATES='nerdctl docker' ;;
*)
	printf 'unknown BRIG_RUNTIME "%s" (want hull or nerdctl). Nothing was checked.\n' "$KIND" >&2
	exit 2
	;;
esac
RT="${BRIG_RUNTIME_BIN:-$PROFILE_RUNTIME_BIN}"
case "$RT" in
"~/"*) RT="$HOME/${RT#~/}" ;;
esac
if [ -z "$RT" ]; then
	for candidate in $CANDIDATES; do
		if RT="$(command -v "$candidate" 2>/dev/null)"; then break; fi
		RT=""
	done
fi

printf 'brig guest image contract: %s\n' "$IMAGE"
printf 'brig: %s (%s)\n' "$BRIG_BIN" "$BRIG_FROM"
printf 'profile: %s, guest home %s, guest user %s\n' "$PROFILE" "$GUEST_HOME" "$GUEST_USER"
printf 'runtime: %s (%s)\n' "$KIND" "${RT:-none found}"

if dry; then
	printf '\nBRIG_DRY_RUN is set, so nothing was booted and nothing was checked.\n'
	printf 'The boot would be:\n'
	printf '  BRIG_IMAGE=%s BRIG_VERIFY=off %s create %s --name image-check --workspace <scratch>\n' \
		"$IMAGE" "$BRIG_BIN" "$PROFILE"
	printf '  %s rm %s --name image-check --workspace <scratch>\n' "$BRIG_BIN" "$PROFILE"
	printf 'and each requirement would run as `%s exec -u root <sandbox>`.\n' \
		"$(basename "${RT:-$KIND}")"
	exit 0
fi

if [ -z "$RT" ]; then
	printf '\nno container runtime found: install %s, or set BRIG_RUNTIME_BIN.\n' \
		"${CANDIDATES// / or }" >&2
	printf 'Nothing was checked.\n' >&2
	exit 2
fi

FAILED=0
MODE=exec      # or "list", when the image would not boot
NAME=""        # the sandbox, once brig has named it
BOOT_FAILED="" # brig's create returned an error, whatever came up
LISTED=""      # the container the fallback listing was taken from

WORK="$(mktemp -d)"
# brig suffixes the session slug onto the workspace it is given, so the
# directory it actually uses is <WORKSPACE>-image-chec. Keeping the base inside
# a directory of our own means one rm -rf covers whatever it created.
WORKSPACE="$WORK/workspace"
LISTING="$WORK/listing"
BOOTLOG="$WORK/boot.log"

cleanup() {
	# Unconditionally, not only when the boot printed a name: a create that
	# failed after the sandbox was up leaves one behind holding the name, and
	# `brig rm` on a sandbox that was never created is a no-op.
	"$BRIG_BIN" rm "$PROFILE" --name image-check --workspace "$WORKSPACE" >/dev/null 2>&1
	if [ -n "$LISTED" ]; then
		"$RT" rm -f "$LISTED" >/dev/null 2>&1
	fi
	rm -rf "$WORK"
}
trap cleanup EXIT

# --- reporting ---
ok()   { printf '  ok   %-22s %s\n' "$1" "$2"; }
bad()  { printf '  FAIL %-22s %s\n' "$1" "$2"; FAILED=1; }
skip() { printf '  --   %-22s %s\n' "$1" "$2"; }

# --- booting ---
# BRIG_VERIFY=off because the image under test is one you are building, and an
# unsigned image is the normal case for that; the contract is about what is
# inside the image, not about who signed it.
#
# The sandbox name comes back on brig's stdout rather than being spelled out
# here. It is brig-<profile>-<slug>, and the slug is the session name cut to
# ten characters, so `--name image-check` is brig-<profile>-image-chec --
# taking what brig printed means this does not have to reimplement that.
export BRIG_IMAGE="$IMAGE"
export BRIG_VERIFY=off

printf '\n'
if BOOTED="$("$BRIG_BIN" create "$PROFILE" --name image-check --workspace "$WORKSPACE" 2>"$BOOTLOG")"; then
	NAME="$(printf '%s\n' "$BOOTED" | tail -1 | tr -d '[:space:]')"
else
	# A create that fails during credential delivery leaves the sandbox
	# running, and that is the failure this script exists to explain: the
	# checks can run in it and name the requirement brig tripped over, which
	# is more use than brig's one line about the step that hit it. brig names
	# the sandbox as it starts it, so the name is in what it printed.
	BOOT_FAILED=1
	NAME="$(sed -n 's/^brig: starting sandbox \(.*\)\.\.\.$/\1/p' "$BOOTLOG" | tail -1)"
fi

# guest runs one command inside the sandbox as root, the way every privileged
# brig exec runs. Output on stdout, everything else discarded.
guest() {
	if [ "$KIND" = hull ]; then
		"$RT" exec -u root "$NAME" -- "$@" 2>/dev/null
	else
		"$RT" exec -u root "$NAME" "$@" 2>/dev/null
	fi
}

# The fallback: create the container without starting it and list its
# filesystem. This is the only thing that works for an image that cannot run at
# all, which is exactly the scratch case worth reporting on. hull has no such
# verb, so on macOS there is nothing to fall back to.
build_listing() {
	if [ "$KIND" = hull ]; then
		return 1
	fi
	LISTED="brig-image-check-list-$$"
	"$RT" create --name "$LISTED" "$IMAGE" /bin/true >/dev/null 2>&1 || return 1
	"$RT" export "$LISTED" 2>/dev/null | tar -tf - >"$LISTING" 2>/dev/null || return 1
	[ -s "$LISTING" ]
}

brig_said() { sed -n '1,10p' "$BOOTLOG" | sed 's/^/  /'; }

if [ -n "$NAME" ] && guest /bin/true >/dev/null; then
	printf 'sandbox: %s\n' "$NAME"
	if [ -n "$BOOT_FAILED" ]; then
		printf '\nbrig did not finish the boot, and said:\n\n'
		brig_said
		printf '\nThe sandbox is up, so the checks below ran inside it.\n'
	fi
	printf '\n'
else
	printf 'brig could not bring this image up as a %s sandbox, so there is nothing\n' "$PROFILE"
	printf 'to exec into. What brig said:\n\n'
	brig_said
	printf '\n'
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

# chown is the one check whose failure has two quite different causes, and the
# useful one is the account rather than the binary. An image that has chown but
# not the profile's user fails every run of that profile at credential
# delivery, and the line that says so has to name the user it looked for.
chown_check() {
	local label='chown, guest user' note="hands directories to $GUEST_USER and back"
	if [ "$MODE" = list ]; then
		if have_binary chown; then ok "$label" "$note"; else bad "$label" "$note"; fi
		return
	fi
	if guest chown "$GUEST_USER" "$SCRATCH/d" >/dev/null; then
		ok "$label" "$note"
		return
	fi
	# `id root` first: without it, an image with no id at all would be reported
	# as an image with no guest user, which is a different repair.
	if guest id -u root >/dev/null && ! guest id -u "$GUEST_USER" >/dev/null; then
		local msg="the profile's guest home is $GUEST_HOME, so brig will chown to $GUEST_USER, and this image has no such user"
		if [ "$DELIVERS" = no ]; then
			msg="$msg (the $PROFILE profile declares no volumes: or files:, so nothing chowns in it today)"
		fi
		bad "$label" "$msg"
		return
	fi
	bad "$label" "$note"
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
chown_check
run 'rm' rm 'removes a planted symlink rather than following it' -- \
	rm -f "$SCRATCH/absent"
run 'mount, tmpfs' mount 'covers a directory so a credential stays off disk' -- \
	mount -t tmpfs -o size=1m,mode=0700,nodev,nosuid tmpfs "$SCRATCH/d"
run 'sleep' sleep 'the container command on the nerdctl path' -- sleep 0
run 'bash' bash 'brig shell' -- bash -lc 'exit 0'

if [ -n "$BINARY" ]; then
	run "$BINARY" "$BINARY" "the profile's binary:" -- "$BINARY" --version
else
	skip 'binary:' "the $PROFILE profile names none, so no agent CLI was checked"
fi

# The guest home is created by the workspace mount, so its absence is not a
# failure. Said out loud because an image that ships content there is a mistake
# worth knowing about: the mount hides all of it. Only visible in list mode --
# in a booted sandbox the workspace is already over it, which is the whole
# point.
if [ "$MODE" = list ] && have_path "$GUEST_HOME"; then
	printf '\nnote: %s exists in the image. The workspace is mounted over it, so\n' "$GUEST_HOME"
	printf '      anything the image ships there is invisible in the sandbox.\n'
fi

printf '\n'
if [ "$FAILED" -eq 0 ]; then
	if [ -n "$BOOT_FAILED" ] && [ "$MODE" = exec ]; then
		# Every requirement passed and brig still could not finish, so whatever
		# stopped it is outside this list. Not a pass: the run it was asked
		# about does not work.
		printf 'Every requirement on the list is met, and brig still could not finish the\n'
		printf 'boot. What stopped it is above and is not something this list covers.\n'
		exit 1
	fi
	if [ "$MODE" = list ]; then
		printf 'Every binary is present. Boot the image to check they work.\n'
	else
		printf 'This image satisfies the contract in docs/guest-image.md.\n'
	fi
	exit 0
fi
printf 'This image does not satisfy the contract. See docs/guest-image.md.\n'
exit 1
