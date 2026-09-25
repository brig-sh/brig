#!/bin/bash
# The claims in docs/claims.md that only a booted sandbox can prove.
#
# script/smoke.sh drives brig against a stub runtime, and the stub runs on the
# host. It shows what brig asked the runtime for. It cannot show what the
# guest reaches, so these checks boot a real sandbox with hull or nerdctl and
# ask from inside it.
#
# Each check is one vm_check line at the bottom. A vm row in docs/claims.md
# names the word after vm_check, and script/check-claims.sh resolves the row
# against that line. A name that resolves is then always a check that runs.
#
# With no runtime on PATH this exits 0 and says it skipped, which is what a
# laptop without one wants. BRIG_CLAIMS_VM=require turns that into a failure,
# for a runner that exists to boot sandboxes.
#
# BRIG names the binary under test. make claims-vm sets it to the brig the
# checkout builds. Run by hand, it defaults to the brig on PATH.
#
# Usage:
#   script/claims-vm.sh               boot a sandbox and run every check
#   script/claims-vm.sh --self-test   run every check against a fake guest
#
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

BRIG="${BRIG:-brig}"
# A label of its own keeps this sandbox and its index entry apart from any
# claude session the caller already has.
REF=claude@claims-vm

# The host's own agent socket, read before the fixture below replaces the
# variable. A guest that sees this path reaches the agent you log in with.
host_agent_sock="${SSH_AUTH_SOCK:-}"
HOST_HOME="$HOME"

# fixture WORK lays out the host side: a guest home, a project, and a file
# beside both but in neither, holding a string the guest must never print.
fixture() {
	WORK="$1"
	# A labelled session appends its label to --home, so a run given
	# $WORK/home mounts $WORK/home-claims-vm.
	GUEST_DIR="$WORK/home-claims-vm"
	PROJECT="$WORK/claimsproj"
	OUTSIDE="$WORK/outside"
	mkdir -p "$GUEST_DIR" "$PROJECT" "$OUTSIDE" || return 1
	outside_marker="claims-vm outside $$"
	printf '%s\n' "$outside_marker" >"$OUTSIDE/secret.txt"
	printf 'claims-vm home\n' >"$GUEST_DIR/from-host.txt"
	printf 'claims-vm project\n' >"$PROJECT/from-host.txt"
	# An agent socket and a session bus of the kind a host announces. brig
	# forwards a variable only when a profile names it, so the guest sees
	# neither. The socket path exists, so the by-name probe has a file to
	# find if the guest can reach it.
	: >"$OUTSIDE/agent.sock"
	export SSH_AUTH_SOCK="$OUTSIDE/agent.sock"
	export DBUS_SESSION_BUS_ADDRESS="unix:path=$OUTSIDE/bus"
}

# Every probe ends by printing this line. A sandbox that died prints nothing,
# and a negative check that reads nothing passes on it. So a probe without
# the line fails its check.
SENTINEL=claims-vm-probe-done

# The line also carries the guest kernel's boot_id. brig sh boots a new
# sandbox when the one under test is gone, and that one answers from a
# default home with no project. Once boot_id is set, an answer from any
# other boot fails its check.
BOOT_ID_FILE=/proc/sys/kernel/random/boot_id
boot_id=""

# guest SCRIPT runs one bash script in the sandbox. brig sh passes each of its
# words to the guest as one argument, so the script goes to bash -c as one
# word. The command runs on a pty, so its lines end in CR LF.
#
# A probe with no answer says so on stderr, with the last line it got. A
# real run once failed a check with nothing to say why. This line tells a
# guest that did not answer apart from a guest that leaked.
guest() {
	local out last id
	out="$("$BRIG" -q sh "$REF" bash -c "$1; echo $SENTINEL boot=\$(cat $(quote "$BOOT_ID_FILE") 2>/dev/null)" 2>&1 | tr -d '\r')"
	last="$(printf '%s\n' "$out" | tail -1)"
	case "$last" in
	"$SENTINEL boot="*) id="${last#"$SENTINEL boot="}" ;;
	*)
		printf '  no answer from the guest, last line: %s\n' "$last" >&2
		return 1
		;;
	esac
	if [ -n "$boot_id" ] && [ "$id" != "$boot_id" ]; then
		printf '  the answer came from boot %s, not the sandbox under test (boot %s)\n' "$id" "$boot_id" >&2
		return 1
	fi
	printf '%s\n' "$out" | sed '$d'
}

# quote WORD... prints each word escaped for the guest's bash. The probes
# carry host paths, from mktemp, HOME and SSH_AUTH_SOCK, and a path with a
# space split in two names two paths that do not exist. A negative check
# then passes with the real path reachable.
quote() { printf '%q ' "$@"; }

# probe NAME asks the guest one question. The checks below judge the answer.
# The self-test answers the same names from a fake guest, so a check that
# stopped failing on a leak shows up without a sandbox.
#
# The probes print paths and variable names, never an environment dump. The
# guest environment carries the credentials the profile forwards.
probe() {
	case "$1" in
	home)
		guest "cat $(quote "$guest_home/from-host.txt"); touch $(quote "$guest_home/from-guest.txt"); printenv HOME"
		;;
	# The listing of /work is the control for outside-list below. It has to
	# name the project, so a guest ls that rejects the flags fails here and
	# not in silence there.
	project)
		guest "cat /work/claimsproj/from-host.txt; touch /work/claimsproj/from-guest.txt; ls -1A /work"
		;;
	outside-read) guest "cat $(quote "$OUTSIDE/secret.txt") 2>/dev/null" ;;
	# ls on a pty prints columns, so -1 keeps one name per line.
	outside-list) guest "ls -1A $(quote "$WORK") 2>/dev/null" ;;
	mounts) guest "cat /proc/mounts" ;;
	ssh-env) guest "printenv SSH_AUTH_SOCK" ;;
	# Searched as the guest user, which is the user the agent runs as. find
	# lists only directories that user can read. A socket in a directory the
	# user can enter but not read still takes a connection by its full path,
	# and the scan misses it. So the two agent paths are tried by name. The
	# scan hides find's errors, so a guest without find says so.
	sockets)
		guest "command -v find >/dev/null || echo 'no find in the guest'; find / \\( -path /proc -o -path /sys \\) -prune -o -type s -print 2>/dev/null; for p in $(quote "$SSH_AUTH_SOCK" "$host_agent_sock"); do [ -e \"\$p\" ] && echo \"\$p\"; done"
		;;
	# The keychain is files under ~/Library/Keychains and /Library/Keychains.
	# None of them has a path in the guest unless something mounted it.
	# /Users holds every home on macOS. A Linux guest has none, so a /Users
	# there is the host's tree of homes mounted in.
	keychain)
		guest "for p in $(quote "$HOST_HOME/Library/Keychains") /Library/Keychains /Users; do [ -e \"\$p\" ] && echo \"\$p\"; done"
		;;
	# Secret Service is the Linux keychain. It is reached over the session
	# bus, and gnome-keyring keeps its files under ~/.local/share/keyrings.
	secret-service)
		guest "printenv DBUS_SESSION_BUS_ADDRESS; for p in /run/user/*/bus /run/user/*/keyring \"\$HOME/.local/share/keyrings\" $(quote "$HOST_HOME/.local/share/keyrings"); do [ -e \"\$p\" ] && echo \"\$p\"; done"
		;;
	*)
		echo "claims-vm: no probe named $1" >&2
		return 1
		;;
	esac
}

# The guest home the profile names, its HOME, and a write that lands in the
# host directory behind it.
check_guest_home() {
	local out
	out="$(probe home)" || return 1
	case "$out" in
	*"claims-vm home"*) ;;
	*) return 1 ;;
	esac
	[ "$(printf '%s\n' "$out" | tail -1)" = "$guest_home" ] || return 1
	[ -f "$GUEST_DIR/from-guest.txt" ]
}

check_project() {
	local out
	out="$(probe project)" || return 1
	case "$out" in
	*"claims-vm project"*) ;;
	*) return 1 ;;
	esac
	if ! printf '%s\n' "$out" | grep -qxF claimsproj; then
		echo "  ls -1A /work did not name claimsproj"
		return 1
	fi
	[ -f "$PROJECT/from-guest.txt" ]
}

# share_at MOUNTS PATH prints the tag of the virtiofs or 9p share at PATH.
share_at() {
	printf '%s\n' "$1" | awk -v m="$2" '$2 == m && ($3 == "virtiofs" || $3 == "9p") { print $1; exit }'
}

# Two reads and the mount table. The reads are negative: the fixture by its
# host path, and the directory that holds the guest home and the project. The
# mount table is the stronger half. A host directory reaches the guest as a
# virtiofs or 9p share, and the guest home and the project each come from
# one. A share mounted twice keeps its tag. So every other mount has to come
# from one of those two shares, or be a kernel filesystem that no host
# directory backs. The root is the image, which the runtime serves from its
# own store and not from a directory of yours.
check_other_host_directory() {
	local out mounts home_src project_src extra
	out="$(probe outside-read)" || return 1
	case "$out" in
	*"$outside_marker"*)
		echo "  the guest read $OUTSIDE/secret.txt"
		return 1
		;;
	esac
	# Split on blanks too. A probe that lost its -1 prints the names in
	# columns, and the listing still has to fail.
	out="$(probe outside-list)" || return 1
	if printf '%s\n' "$out" | tr -s ' \t' '\n' | grep -qxF outside; then
		echo "  the guest listed $WORK"
		return 1
	fi
	mounts="$(probe mounts)" || return 1
	home_src="$(share_at "$mounts" "$guest_home")"
	project_src="$(share_at "$mounts" /work/claimsproj)"
	if [ -z "$home_src" ] || [ -z "$project_src" ]; then
		echo "  no virtiofs or 9p share at $guest_home and /work/claimsproj"
		return 1
	fi
	extra="$(printf '%s\n' "$mounts" | awk -v h="$home_src" -v p="$project_src" '
		$2 == "/" { next }
		($3 == "virtiofs" || $3 == "9p") && ($1 == h || $1 == p) { next }
		$3 ~ /^(proc|sysfs|tmpfs|devtmpfs|devpts|mqueue|cgroup|cgroup2|securityfs|debugfs|tracefs|pstore|bpf|hugetlbfs|configfs)$/ { next }
		NF { print }')"
	if [ -n "$extra" ]; then
		printf '  a mount from neither the guest home nor the project: %s\n' "$extra"
		return 1
	fi
	return 0
}

check_ssh_agent() {
	local out
	out="$(probe ssh-env)" || return 1
	[ -z "$out" ]
}

# listed PROBE fails when the probe names any path, and prints what it named.
listed() {
	local out
	out="$(probe "$1")" || return 1
	if [ -n "$out" ]; then
		printf '%s\n' "$out" | sed 's/^/  found /'
		return 1
	fi
}

# No socket anywhere the guest user can reach, and neither the announced
# agent path nor the host's real one. An agent is reached through its socket,
# so no socket means no agent.
check_no_agent_socket() { listed sockets; }
check_keychain() { listed keychain; }
check_secret_service() { listed secret-service; }

vm_check() {
	local name="$1"
	shift
	if "$@"; then
		ok "$name"
	else
		bad "$name"
	fi
}

fail=0
ok() { printf '  ok   %s\n' "$1"; }
bad() {
	printf '  FAIL %s\n' "$1"
	fail=1
}

# Each probe leaks on its own in the fake guest, and the check that reads it
# has to fail while every other check passes. One leak at a time keeps a dead
# check from hiding behind a live one. A guest that answers nothing fails
# every check, and a clean guest passes every check.
LEAKS="guest-home-read-write:home
project-at-work:project
other-host-directory:outside-read
other-host-directory:outside-list
other-host-directory:mounts
other-host-directory:mounts/9p
other-host-directory:mounts/nfs
ssh-agent-not-forwarded:ssh-env
no-agent-socket:sockets
keychain-not-reachable:keychain
secret-service-not-reachable:secret-service"

# fake_answer NAME answers a probe as a clean guest does, or as a leaking one
# when FAKE is leak:NAME or leak:NAME/KIND. A leak in home or project is the
# share gone missing. The listing leak is in columns, the way ls prints on a
# pty without -1. FAKE=clean-9p is a clean guest whose shares are 9p.
fake_answer() {
	local leak=""
	[ "$FAKE" = dead ] && return 1
	case "$FAKE" in
	"leak:$1") leak=1 ;;
	"leak:$1/"*) leak="${FAKE#"leak:$1/"}" ;;
	esac
	case "$1" in
	home)
		[ -n "$leak" ] && return 0
		touch "$GUEST_DIR/from-guest.txt"
		printf 'claims-vm home\n%s\n' "$guest_home"
		;;
	project)
		[ -n "$leak" ] && return 0
		touch "$PROJECT/from-guest.txt"
		printf 'claims-vm project\nclaimsproj\n'
		;;
	outside-read) [ -n "$leak" ] && printf '%s\n' "$outside_marker" ;;
	outside-list) [ -n "$leak" ] && printf 'claimsproj\thome-claims-vm\toutside\n' ;;
	mounts)
		if [ "$FAKE" = clean-9p ]; then
			printf '/dev/vda / ext4 rw 0 0\nproc /proc proc rw 0 0\n'
			printf 'home %s 9p rw 0 0\n' "$guest_home"
			printf 'proj /work/claimsproj 9p rw 0 0\n'
		else
			printf 'rootfs / virtiofs rw 0 0\nproc /proc proc rw 0 0\n'
			printf 'share0 %s virtiofs rw 0 0\n' "$guest_home"
			printf 'share1 /work/claimsproj virtiofs rw 0 0\n'
			printf 'share0 /run/brig/persist/x virtiofs rw 0 0\n'
		fi
		printf 'tmpfs %s/.claude tmpfs rw 0 0\n' "$guest_home"
		case "$leak" in
		1) printf 'share2 /mnt/host virtiofs rw 0 0\n' ;;
		9p) printf 'hostshare /mnt/host 9p rw 0 0\n' ;;
		nfs) printf 'host:/export /mnt/host nfs4 rw 0 0\n' ;;
		esac
		;;
	ssh-env) [ -n "$leak" ] && printf '%s\n' "$SSH_AUTH_SOCK" ;;
	sockets) [ -n "$leak" ] && printf '/tmp/ssh-XXXX/agent.1\n' ;;
	keychain) [ -n "$leak" ] && printf '/Users\n' ;;
	secret-service) [ -n "$leak" ] && printf '/run/user/501/bus\n' ;;
	*) return 1 ;;
	esac
	return 0
}

self_test() {
	local tmp checks name fn want got line bad=0 status out
	tmp="$(mktemp -d)" && [ -n "$tmp" ] || return 1
	fixture "$tmp/work" || return 1
	guest_home=/home/claude

	# The fake guest below stands in for probe, so guest and the probe
	# commands never run under it. These cases run them through a stub brig
	# that runs each probe on the host and ends every line in CR LF, as brig
	# sh does on a pty. The host reaches what a guest must not, so a negative
	# check through it has to fail, and a check with nothing to find passes
	# only once the CR and the sentinel line are gone. The stub hands its words
	# on as brig sh does, each one argument, so a script passed as one word
	# fails here as it does in a guest.
	cat >"$tmp/brig" <<'STUB'
#!/bin/bash
[ "$1 $2" = "-q sh" ] || exit 2
shift 3
case "${CLAIMS_VM_STUB:-}" in
dead)
	printf 'sandbox gone\r\n'
	exit 1
	;;
silent) exit 1 ;;
nofind)
	# A guest with bash and nothing else on its PATH.
	mkdir -p "${0%/*}/nofind" && ln -sf /bin/bash "${0%/*}/nofind/bash"
	PATH="${0%/*}/nofind" /bin/bash -c '"$@"' bash "$@" | sed 's/$/\r/'
	exit
	;;
esac
/bin/bash -c '"$@"' bash "$@" | sed 's/$/\r/'
STUB
	chmod +x "$tmp/brig"
	local real_brig="$BRIG"
	BRIG="$tmp/brig"
	# stub_case DESC WANT CMD runs CMD, where WANT is pass or fail.
	stub_case() {
		local desc="$1" want="$2"
		shift 2
		if out="$("$@" 2>&1)"; then got=pass; else got=fail; fi
		if [ "$got" = "$want" ]; then
			echo "self-test: ok   stub brig: $desc $got"
		else
			echo "self-test: FAIL stub brig: $desc got $got, want $want: $out"
			bad=1
		fi
	}
	stub_case "the host has the variable, ssh-agent-not-forwarded" fail check_ssh_agent
	stub_case "the host reads outside, other-host-directory" fail check_other_host_directory
	no_variable() { (unset SSH_AUTH_SOCK && check_ssh_agent); }
	dead_sandbox() { (export CLAIMS_VM_STUB=dead && check_ssh_agent); }
	stub_case "no variable to find, ssh-agent-not-forwarded" pass no_variable
	stub_case "a dead sandbox, ssh-agent-not-forwarded" fail dead_sandbox
	case "$out" in
	*"no answer from the guest, last line: sandbox gone"*) echo "self-test: ok   stub brig: a dead sandbox says no answer" ;;
	*)
		echo "self-test: FAIL stub brig: a dead sandbox did not say no answer: $out"
		bad=1
		;;
	esac
	# A sandbox that died without a word is the case the sentinel is for.
	# The dead stub above leaves text behind, which fails the check even
	# with no sentinel. Here the answer is empty, and only the missing
	# sentinel line keeps a negative check from passing.
	silent_sandbox() { CLAIMS_VM_STUB=silent "$@"; }
	stub_case "a silent sandbox, ssh-agent-not-forwarded" fail silent_sandbox check_ssh_agent
	stub_case "a silent sandbox, keychain-not-reachable" fail silent_sandbox check_keychain
	# brig sh boots a new sandbox when the one under test is gone, and that
	# one answers with another boot_id.
	printf 'boot-1\n' >"$tmp/boot_id"
	from_boot() { (unset SSH_AUTH_SOCK && BOOT_ID_FILE="$tmp/boot_id" boot_id="$1" check_ssh_agent); }
	stub_case "the boot under test, ssh-agent-not-forwarded" pass from_boot boot-1
	stub_case "another boot, ssh-agent-not-forwarded" fail from_boot boot-0
	case "$out" in
	*"the answer came from boot boot-1, not the sandbox under test (boot boot-0)"*)
		echo "self-test: ok   stub brig: another boot says so"
		;;
	*)
		echo "self-test: FAIL stub brig: another boot did not say so: $out"
		bad=1
		;;
	esac
	stub_case "the host has the session bus variable, secret-service-not-reachable" fail check_secret_service
	# The socket scan hides find's errors. A guest without find has to fail
	# the check, and no socket path exists here to fail it another way.
	no_find() {
		(
			SSH_AUTH_SOCK="$tmp/none/agent.sock" host_agent_sock=""
			CLAIMS_VM_STUB=nofind check_no_agent_socket
		)
	}
	stub_case "a guest with no find, no-agent-socket" fail no_find
	case "$out" in
	*"found no find in the guest"*) echo "self-test: ok   stub brig: a guest with no find says so" ;;
	*)
		echo "self-test: FAIL stub brig: a guest with no find did not say so: $out"
		bad=1
		;;
	esac
	# A host path with a space has to reach the guest as one word. Split in
	# two, the by-name probes test halves that do not exist and pass on a
	# leak. find does nothing here, so the socket probe skips its host scan.
	mkdir -p "$tmp/agent dir" "$tmp/host home/Library/Keychains" \
		"$tmp/host home/.local/share/keyrings"
	: >"$tmp/agent dir/agent.sock"
	names_path() {
		(
			host_agent_sock="$tmp/agent dir/agent.sock"
			HOST_HOME="$tmp/host home"
			find() { :; }
			export -f find
			probe "$1"
		) | grep -qxF "$2"
	}
	stub_case "an agent socket path with a space, no-agent-socket names it" pass \
		names_path sockets "$tmp/agent dir/agent.sock"
	stub_case "the fixture's agent socket, no-agent-socket names it" pass \
		names_path sockets "$OUTSIDE/agent.sock"
	stub_case "the host lists the work directory, other-host-directory names outside" pass \
		names_path outside-list outside
	stub_case "a keychain path with a space, keychain-not-reachable names it" pass \
		names_path keychain "$tmp/host home/Library/Keychains"
	stub_case "a keyring path with a space, secret-service-not-reachable names it" pass \
		names_path secret-service "$tmp/host home/.local/share/keyrings"
	BRIG="$real_brig"

	probe() { fake_answer "$1"; }

	# The checks the real run makes, read from the vm_check lines below.
	checks="$(awk '/^vm_check / { print $2 " " $3 }' script/claims-vm.sh)"
	if [ -z "$checks" ]; then
		echo "self-test: FAIL no vm_check line in script/claims-vm.sh"
		bad=1
	fi
	while read -r name _; do
		[ -z "$name" ] && continue
		if ! printf '%s\n' "$LEAKS" | grep -q "^$name:"; then
			echo "self-test: FAIL $name has no leak in the fake guest"
			bad=1
		fi
	done <<<"$checks"
	while IFS=: read -r name _; do
		if ! printf '%s\n' "$checks" | grep -q "^$name "; then
			echo "self-test: FAIL no vm_check line for $name"
			bad=1
		fi
	done <<<"$LEAKS"

	# run_case FAKE EXPECT, where EXPECT is pass, fail, or the one check
	# that fails while the rest pass.
	run_case() {
		FAKE="$1"
		while read -r name fn; do
			[ -z "$name" ] && continue
			rm -f "$GUEST_DIR/from-guest.txt" "$PROJECT/from-guest.txt"
			want=pass
			[ "$2" = fail ] || [ "$2" = "$name" ] && want=fail
			if "$fn" >/dev/null; then got=pass; else got=fail; fi
			if [ "$got" = "$want" ]; then
				echo "self-test: ok   $FAKE: $name $got"
			else
				echo "self-test: FAIL $FAKE: $name got $got, want $want"
				bad=1
			fi
		done <<<"$checks"
	}
	run_case clean pass
	run_case clean-9p pass
	run_case dead fail
	while IFS=: read -r name line; do
		run_case "leak:$line" "$name"
	done <<<"$LEAKS"

	# The skip and the require refusal, from a PATH with no runtime on it.
	# dirname is the one command the script runs before it looks.
	mkdir -p "$tmp/bin"
	ln -s "$(command -v dirname)" "$tmp/bin/dirname"
	out="$(env -u BRIG_CLAIMS_VM PATH="$tmp/bin" "$BASH" script/claims-vm.sh 2>&1)"
	status=$?
	if [ "$status" = 0 ] && [ "$out" = "claims-vm: skipped, no runtime (hull or nerdctl) on PATH" ]; then
		echo "self-test: ok   no runtime skips with exit 0"
	else
		echo "self-test: FAIL no runtime exited $status: $out"
		bad=1
	fi
	out="$(BRIG_CLAIMS_VM=require PATH="$tmp/bin" "$BASH" script/claims-vm.sh 2>&1)"
	status=$?
	case "$status:$out" in
	1:*BRIG_CLAIMS_VM=require*) echo "self-test: ok   no runtime under require fails" ;;
	*)
		echo "self-test: FAIL no runtime under require exited $status: $out"
		bad=1
		;;
	esac
	rm -rf "$tmp"
	return "$bad"
}

if [ "${1:-}" = --self-test ]; then
	if self_test; then
		echo "self-test: PASS"
		exit 0
	fi
	echo "self-test: FAIL"
	exit 1
fi

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

# The trap below removes $WORK, so an empty one has to stop the script first:
# cd "" stays in the checkout, and pwd then names the checkout.
WORK="$(mktemp -d)" && [ -n "$WORK" ] || exit 1
# macOS hands out /var/folders, which is a link to /private/var/folders.
# brig refuses a link on the way to the guest home, so name the real path.
WORK="$(cd "$WORK" && pwd -P)" || exit 1
trap '"$BRIG" rm "$REF" >/dev/null 2>&1; rm -rf "$WORK"' EXIT
fixture "$WORK" || exit 1

# The guest home comes from the profile, which a file of yours can override.
# The home check then holds the guest to what the profile says.
guest_home="$("$BRIG" agent show "${REF%@*}" --json 2>/dev/null |
	sed -n 's/^ *"guestHome": *"\([^"]*\)".*/\1/p' | head -1)"
case "$guest_home" in
/*) ;;
*)
	echo "claims-vm: no guestHome in brig agent show ${REF%@*} --json"
	exit 1
	;;
esac

echo "claims-vm: booting $REF on $runtime with $(command -v "$BRIG"), guest home $guest_home"
if ! "$BRIG" -q run -d --home "$WORK/home" "$REF" "$PROJECT" >"$WORK/boot.out" 2>&1; then
	echo "claims-vm: the boot failed:"
	cat "$WORK/boot.out"
	exit 1
fi
boot_id="$(guest "cat $(quote "$BOOT_ID_FILE")")"
case "$boot_id" in
"" | *[!0-9a-f-]*)
	echo "claims-vm: could not read the guest boot_id, got: $boot_id"
	exit 1
	;;
esac

vm_check guest-home-read-write check_guest_home
vm_check project-at-work check_project
vm_check other-host-directory check_other_host_directory
vm_check ssh-agent-not-forwarded check_ssh_agent
vm_check no-agent-socket check_no_agent_socket
vm_check keychain-not-reachable check_keychain
vm_check secret-service-not-reachable check_secret_service

if [ "$fail" = 0 ]; then
	echo "claims-vm: PASS"
else
	echo "claims-vm: FAIL"
fi
exit "$fail"
