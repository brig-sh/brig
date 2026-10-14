#!/bin/bash
# End-to-end smoke test for brig against a stub runtime.
#
# It drives the real binary through the real code path -- workspace
# preparation, credential resolution, boot, readiness probe, share check,
# exec -- with a fake `hull` standing in for the microVM. That covers
# everything except the VM itself, on any machine, in CI included.
#
#   script/smoke.sh
set -uo pipefail
cd "$(dirname "$0")/.."

# Nothing here is answered by a person. Several cases assert the outcome brig
# reaches when it has nobody to ask -- a bad signature refused, a profile rm
# that will not guess -- and brig decides that by looking at its own stdin.
# `out="$(brig ... 2>&1)"` redirects stdout and stderr and leaves stdin alone,
# so run from a terminal those same cases found one, printed a [y/N] into the
# captured stream where nobody could see it, and blocked on the answer. CI has
# no terminal and never hit it. Detach stdin once, here, so the script behaves
# the same either way; the two `profile import -` cases feed brig through a
# pipe and supply their own.
exec < /dev/null

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
# brig runs with the caller's HOME here, which one assertion below depends on.
# Telemetry would read and write that home's ~/.hull, so it is off.
export HULL_TELEMETRY_DISABLED=1
export STUB_LOG="$WORK/argv.log"
export STUB_STATE="$WORK/instance"
WS="$WORK/ws"

# The guest home the claude-code profile mounts the workspace at, spelled once
# because six assertions below check it. It follows that profile's guestHome:
# change it there and this is the single line to match.
GUEST_HOME=/root

# Assertions on brig's own output capture it first rather than piping into
# `grep -q`. grep -q exits at the first match, brig then dies of SIGPIPE
# writing the rest, and `set -o pipefail` reports the pipeline as failed --
# intermittently, depending on how much was left to write. Reading from a file
# is fine; reading from a process is not.
fail=0
ok()  { printf '  ok   %s\n' "$1"; }
bad() { printf '  FAIL %s\n' "$1"; fail=1; }

# --- the stub runtime ---
# It answers the questions brig asks (ps, run, exec, stop/rm, and the network
# gateway cases below) and logs every argument so the test can assert on argv.
# The words: line brackets each argument, for a check that has to see where
# one ends and the next begins, which the space-joined argv: line cannot show.
cat > "$WORK/hull" <<'STUB'
#!/bin/bash
# brig asks `run --help` whether this hull takes --profile before a run that
# names a machine. Answered before the argv log, under a line of its own, so
# the cases that read the run line find only the boot. STUB_HULL_NO_PROFILE
# is a hull from before machine profiles.
if [ "${1:-}" = run ] && [ "${2:-}" = --help ]; then
  printf 'probe: run --help\n' >> "$STUB_LOG"
  printf 'OPTIONS:\n'
  [ -n "${STUB_HULL_NO_PROFILE:-}" ] || \
    printf '   --profile hull assets pull --profile   hvi machine profile\n'
  printf '   --rootfs-type string                   rootfs sharing mode\n'
  exit 0
fi
{ printf 'argv:'; printf ' %s' "$@"; printf '\n'; } >> "$STUB_LOG"
{ printf 'words:'; printf '[%s]' "$@"; printf '\n'; } >> "$STUB_LOG"
verb="$1"; shift
case "$verb" in
  --version)
    # What the real one prints. rc23 is the first that resolves a digest
    # reference against its store; a case below drives an older one.
    printf 'hull version %s\n' "${STUB_HULL_VERSION:-0.1.0-rc23}"
    ;;
  ps)
    # `ps -a` also lists an instance that is merely holding its name.
    [ -f "$STUB_STATE" ] && printf '%s running\n' "$(cat "$STUB_STATE")"
    [ "${1:-}" = -a ] && [ -f "$STUB_STATE.stopped" ] && \
      printf '%s stopped\n' "$(cat "$STUB_STATE.stopped")"
    ;;
  run)
    # What a real runtime says on its way up: a pull, then a boot. brig holds
    # this rather than passing it through, so the cases below can ask where it
    # ended up. STUB_RUN_FAIL turns the boot into the one that has to stay
    # reportable however quiet brig is.
    printf 'pulling ghcr.io/brig-sh/claude-code-stock:latest\n' >&2
    # STUB_RUN_PULL prints what hull prints while it pulls an image that is not
    # in its store, in the format of its pkg/ociclient/progress.go.
    if [ -n "${STUB_RUN_PULL:-}" ]; then
      printf 'pull: layer 1/2, 12.0 MB extracted (image 480.2 MB compressed)\n' >&2
      printf 'pull: 2/2 layers, 1.1 GB extracted\n' >&2
    fi
    if [ -n "${STUB_RUN_FAIL:-}" ]; then
      printf 'FATAL: no space left on device\n' >&2
      exit 1
    fi
    # Remember the instance name and the shared directory the way a real
    # runtime binds them at boot.
    #
    # Every share is logged, and the FIRST one is the home: brig puts the
    # workspace first and a project after it, and it is the home the marker is
    # read back out of below.
    #
    # Each share is also a mount point in the guest's table, under the root a
    # real kernel always lists, so brig reading /proc/self/mountinfo sees the
    # home a share is -- and a teleportable boot, which shares nothing, sees a
    # home that is only a directory of its disk. The root type is kept too,
    # for the hvi argv inspect reports below.
    name=""; share=""
    : > "$STUB_STATE.shares"
    : > "$STUB_STATE.rootfs"
    : > "$STUB_STATE.profile"
    printf '1 0 254:1 / / rw - ext4\n' > "$STUB_STATE.mounts"
    while [ $# -gt 0 ]; do
      case "$1" in
        --name) name="$2"; shift 2 ;;
        --rootfs-type) printf '%s' "$2" > "$STUB_STATE.rootfs"; shift 2 ;;
        --profile) printf '%s' "$2" > "$STUB_STATE.profile"; shift 2 ;;
        --shared-dir)
          printf '%s\n' "$2" >> "$STUB_STATE.shares"
          [ -z "$share" ] && share="$2"
          guest="${2#*:}"; guest="${guest%%:*}"
          printf '1 1 0:3 / %s rw - virtiofs\n' "$guest" >> "$STUB_STATE.mounts"
          shift 2 ;;
        *) shift ;;
      esac
    done
    printf '%s' "$name" > "$STUB_STATE"
    printf '%s' "${share%%:*}" > "$STUB_STATE.share"
    # Record which credential values arrived through the environment rather
    # than through argv, and the HOME this process runs with.
    printf 'env-token:%s\n' "${CLAUDE_CODE_OAUTH_TOKEN:-<unset>}" >> "$STUB_LOG"
    printf 'env-gh:%s\n' "${GH_TOKEN:-<unset>}" >> "$STUB_LOG"
    printf 'env-home:%s\n' "${HOME:-<unset>}" >> "$STUB_LOG"
    ;;
  exec)
    # Everything after -- is the guest command. /bin/true is the readiness
    # probe; a cat of the marker is answered from the bound share.
    #
    # The mount cases below exist because brig's volume delivery is
    # fail-closed: it mounts, then reads the guest's own mount table back and
    # refuses the run if what it asked for is not there. A stub that accepts a
    # mount and then reports nothing mounted is indistinguishable from a mount
    # that silently failed, which is precisely what the check is for -- so the
    # stub has to remember what it was told to mount.
    while [ $# -gt 0 ] && [ "$1" != "--" ]; do shift; done
    shift
    # A teleportable session's guest-home walk takes the home and a path
    # relative to it. Only the seed carries content, kept under the joined
    # path like a fed credential; the rest makes nothing the cases read.
    if [ "$1" = sh ] && case "${3:-}" in *'kind=$3'*) true ;; *) false ;; esac; then
      if [ "${7:-}" = seed ]; then
        cat > "$STUB_STATE.fed.$(printf '%s/%s' "${5:-}" "${6:-}" | tr / _)"
      fi
      exit 0
    fi
    # Credential delivery feeds the value on stdin to `sh -c 'cat >> "$1"'`,
    # in pieces, and then asks stat for the file's size to check the whole
    # of it landed. Keep what was fed, per target, so stat can answer with
    # the truth rather than a fixed number.
    if [ "$1" = sh ] && case "${3:-}" in *'cat >'*|*'set -C'*) true ;; *) false ;; esac; then
      fed="$STUB_STATE.fed.$(printf '%s' "${5:-}" | tr / _)"
      case "$3" in
        *'set -C'*) rm -f "$fed" ;;   # the create step empties the file first
        *) cat >> "$fed" ;;
      esac
      exit 0
    fi
    case "$1" in
      /bin/true) exit 0 ;;
      cat)
        case "${2:-}" in
          /proc/self/mountinfo) [ -f "$STUB_STATE.mounts" ] && cat "$STUB_STATE.mounts" ;;
          /proc/swaps) printf 'Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n' ;;
          *) cat "$(cat "$STUB_STATE.share")/.brig-workspace" 2>/dev/null ;;
        esac
        ;;
      stat)
        # `stat -f -c %T <path>` asks what a path sits on; `stat -c ...` asks
        # about the file itself.
        if [ "${2:-}" = -f ]; then
          # A path inherits the filesystem of the nearest mount point above
          # it, so walk up until one matches rather than asking only about the
          # path itself: a credential file created inside a tmpfs has no mount
          # entry of its own, and answering virtiofs for it is a fstype the
          # guest would never report. Match on field 5, the mount point --
          # field 4 is the mount's root and reads `/` on every line, so a
          # looser match answers for a path under no mount at all.
          target="${5:-}"
          line=""
          probe="$target"
          while [ -n "$probe" ]; do
            line=$(awk -v p="$probe" '$5 == p' "$STUB_STATE.mounts" 2>/dev/null | tail -1)
            [ -n "$line" ] && break
            [ "$probe" = / ] && break
            probe="${probe%/*}"
            [ -z "$probe" ] && probe=/
          done
          case "$line" in
            *tmpfs) printf 'tmpfs\n' ;;
            *fuseblk) printf 'fuseblk\n' ;;
            *) printf 'virtiofs\n' ;;
          esac
        else
          case "${3:-}" in
            %s)
              fed="$STUB_STATE.fed.$(printf '%s' "${4:-}" | tr / _)"
              if [ -f "$fed" ]; then wc -c < "$fed" | tr -d ' '; else printf '512\n'; fi
              ;;
            *) printf 'regular file|root|600\n' ;;
          esac
        fi
        ;;
      mount)
        # `mount -t tmpfs -o ... tmpfs <target>` or `mount --bind <src> <target>`.
        target="${!#}"
        if [ "${2:-}" = -t ]; then
          printf '1 1 0:1 / %s rw - tmpfs\n' "$target" >> "$STUB_STATE.mounts"
        else
          printf '1 1 0:2 / %s rw - fuseblk\n' "$target" >> "$STUB_STATE.mounts"
        fi
        ;;
      *)
        printf 'env-token:%s\n' "${CLAUDE_CODE_OAUTH_TOKEN:-<unset>}" >> "$STUB_LOG"
        printf 'env-gh:%s\n' "${GH_TOKEN:-<unset>}" >> "$STUB_LOG"
        printf 'env-home:%s\n' "${HOME:-<unset>}" >> "$STUB_LOG"
        # The agent (or login shell) exec. STUB_EXEC_EXIT makes it exit non-zero,
        # which is how the --json case drives the child path: brig runs this as a
        # child rather than replacing itself, so it survives to report the status.
        [ -n "${STUB_EXEC_EXIT:-}" ] && exit "$STUB_EXEC_EXIT"
        ;;
    esac
    ;;
  network-gateway)
    # brig asks a gateway two things before it trusts one with rules: that its
    # help mentions --egress-default, so an older runtime is refused rather
    # than handed a flag it would reject, and that something answers on the
    # qemu socket, which is what it judges readiness on. Answer both, so a run
    # carrying a policy gets past the boot and the rules it passed can be read
    # out of the log above. Nothing here filters anything: what is under test
    # is whether the rules travel, and hull tests the filter itself.
    # STUB_NO_EGRESS is a hull whose gateway predates the rules, the
    # destination a teleport must refuse for a session under a policy.
    case " $* " in
      *" --help "*)
        [ -n "${STUB_NO_EGRESS:-}" ] && { printf -- '   --subnet string\n'; exit 0; }
        printf -- '   --egress-default string  verdict for a connection no egress rule matches\n'
        exit 0
        ;;
    esac
    qsock=""; prev=""
    for a in "$@"; do [ "$prev" = --qemu-socket ] && qsock="$a"; prev="$a"; done
    # exec, so the process brig started is the one holding the socket: it
    # records the gateway only for a pid that owns it.
    # The trailing arguments are for ps, not for python, which ignores them:
    # brig records a gateway as one it owns only when `ps -o command=` on the
    # pid it started shows both "network-gateway" and the control socket, and
    # exec leaves an argv holding neither otherwise. Without the record,
    # `brig rm --all` finds no gateway to stop and the listener outlives the
    # run. One line, so ps prints the whole argv on one line. A socket file
    # nothing answers on is removed first, the way hull claims the path: a
    # gateway brig replaced leaves its file behind.
    exec python3 -c "$STUB_GATEWAY" "$qsock" network-gateway "$@"
    ;;
  inspect)
    # An instance holds its name while it runs and once it stops. Any other
    # name is not there, in the words brig reads for that.
    #
    # STUB_INSPECT_ARGV makes the record the one hull prints for an hvi boot:
    # its id and the VMM argv, a --disk for a block root and a --share-rw for
    # each shared directory. Off by default, because the same record answers
    # SandboxNetwork, and the cases that do not ask for it are written
    # against a hull whose posture cannot be read back.
    # The migration record a checkpoint below leaves, which brig info reads.
    migration=""
    [ -f "$STUB_STATE.migration" ] && migration=", \"migration\": $(cat "$STUB_STATE.migration")"
    if [ "$1" = "$(cat "$STUB_STATE" 2>/dev/null)" ] ||
       [ "$1" = "$(cat "$STUB_STATE.stopped" 2>/dev/null)" ]; then
      if [ -n "${STUB_INSPECT_ARGV:-}" ]; then
        argv='"hvi", "boot", "--kernel", "/k"'
        [ "$(cat "$STUB_STATE.rootfs" 2>/dev/null)" = block ] && argv="$argv, \"--disk\", \"/s/rootfs.ext4\""
        # hull hands hvi the machine it was given, under the same flag, and
        # records it beside the argv.
        profile=""
        if [ -s "$STUB_STATE.profile" ]; then
          argv="$argv, \"--profile\", \"$(cat "$STUB_STATE.profile")\""
          profile=", \"profile\": \"$(cat "$STUB_STATE.profile")\""
        fi
        while IFS= read -r dir; do
          [ -n "$dir" ] && argv="$argv, \"--share-rw\", \"${dir%%:*}\", \"share\""
        done < "$STUB_STATE.shares"
        printf '{"id": "%s", "backend": "hvi", "cmdLine": [%s]%s%s}\n' "$1" "$argv" "$profile" "$migration"
      else
        printf '{"name": "%s"%s}\n' "$1" "$migration"
      fi
    else
      printf 'error: instance not found: %s\n' "$1" >&2
      exit 1
    fi
    ;;
  pause|resume)
    # hull pauses and resumes an hvi guest in place. A fenced guest never
    # runs again in its process, and hull refuses the resume in these words.
    [ "$1" = "$(cat "$STUB_STATE" 2>/dev/null)" ] || { printf 'instance not found: %s\n' "$1" >&2; exit 1; }
    if [ "$verb" = resume ] && grep -q '"fenced"' "$STUB_STATE.migration" 2>/dev/null; then
      printf 'instance %s was fenced by `hull checkpoint --then fence`\n' "$1" >&2; exit 1
    fi
    ;;
  checkpoint)
    # `hull checkpoint [--then continue|fence] [--dir DIR] [--timeout N] NAME`
    # writes a bundle and records it in the instance's migration object,
    # which inspect above then reports.
    then=continue; dir=""
    while [ $# -gt 1 ]; do
      case "$1" in
        --then) then="$2"; shift 2 ;;
        --dir) dir="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    [ "$1" = "$(cat "$STUB_STATE" 2>/dev/null)" ] || { printf 'instance %s is not running\n' "$1" >&2; exit 1; }
    [ -n "$dir" ] || dir="$STUB_STATE.bundle"
    state=checkpointed; [ "$then" = fence ] && state=fenced
    printf '{"state": "%s", "bundleDir": "%s", "txid": "t1"}' "$state" "$dir" > "$STUB_STATE.migration"
    printf 'Checkpoint of %s written to %s in 1ms (then %s)\n' "$1" "$dir" "$then"
    ;;
  restore)
    # `hull restore --detach [--paused] [--timeout N] NAME` brings a stopped
    # instance back from its bundle, on the same name.
    while [ $# -gt 1 ]; do
      case "$1" in --paused) paused=1 ;; esac; shift
    done
    [ "$1" = "$(cat "$STUB_STATE.stopped" 2>/dev/null)" ] || { printf 'instance %s is not stopped\n' "$1" >&2; exit 1; }
    mv "$STUB_STATE.stopped" "$STUB_STATE"
    state=restored; [ -n "${paused:-}" ] && state=restored-paused
    printf '{"state": "%s", "bundleDir": "%s.bundle", "txid": "t1"}' "$state" "$STUB_STATE" > "$STUB_STATE.migration"
    ;;
  migrate-prepare)
    # `hull migrate-prepare --to H [--remote-store D] [--remote-hull B]
    # [--json] NAME`: put the image, the kernel and the sandbox's base on H.
    # The first prepare of a host sends the base and a second finds it held,
    # as hull's is idempotent. Its steps go on stderr, and with --json its
    # report on stdout in hull's shape. A flag it does not know is refused,
    # as hull refuses it.
    to=""; json=""
    while [ $# -gt 1 ]; do
      case "$1" in
        --to) to="$2"; shift 2 ;;
        --remote-store|--remote-hull) shift 2 ;;
        --json) json=1; shift ;;
        *) printf 'error: flag provided but not defined: %s\n' "$1" >&2; exit 1 ;;
      esac
    done
    [ "$1" = "$(cat "$STUB_STATE" 2>/dev/null)" ] || { printf 'error: instance not found: %s\n' "$1" >&2; exit 1; }
    base="$(printf '%064d' 7)"; result=held
    grep -qxF "$to" "$STUB_STATE.prepared" 2>/dev/null || { result=sent; printf '%s\n' "$to" >> "$STUB_STATE.prepared"; }
    printf 'Preparing %s for %s\n  image 1a2b3c4d5e6f: held\n  base %s: %s\n' "$to" "$1" "${base:0:12}" "$result" >&2
    printf '%s is prepared: a move of %s sends only what the guest changed\n' "$to" "$1" >&2
    [ -z "$json" ] || printf '{"name":"%s","to":"%s","steps":[{"what":"image 1a2b3c4d5e6f","result":"held"},{"what":"base %s","result":"%s","bytes":1048576}],"holds":{"bases":["%s"],"profileKernels":{"portable-arm64-v1":"%064d"}},"stream":true}\n' \
      "$1" "$to" "${base:0:12}" "$result" "$base" 9
    ;;
  migrate)
    # `hull migrate --to H [--remote-store D] [--remote-hull B]
    # [--remote-gateway-sock S] [--txid X] [--dry-run] NAME`, and
    # `hull migrate --status NAME` and `hull migrate --abort TXID`. The move
    # record is "txid state peer" in $STUB_STATE.move, and --status prints
    # it the way hull does. A move hands the instance to the destination's
    # stub, whose state STUB_REMOTE_STATE names: hull's own migrate-receive
    # and migrate-commit over ssh, folded into one step. The source is left
    # fenced, as hull leaves it. STUB_MIGRATE_LOSE stops the move at
    # committing and fails, a move whose end the caller did not see.
    mode=move; to=""; txid=""; gw=""
    while [ $# -gt 1 ]; do
      case "$1" in
        --status) mode=status; shift ;;
        --abort) mode=abort; shift ;;
        --dry-run) mode=dry; shift ;;
        --to) to="$2"; shift 2 ;;
        --txid) txid="$2"; shift 2 ;;
        --remote-gateway-sock) gw="$2"; shift 2 ;;
        *) shift 2 ;;
      esac
    done
    case "$mode" in
      status)
        [ -f "$STUB_STATE.move" ] || { printf 'error: instance %s has no move on record\n' "$1" >&2; exit 1; }
        read -r mtx mstate mpeer < "$STUB_STATE.move"
        printf 'Move %s of %s: %s, to %s, epoch 1\n' "$mtx" "$1" "$mstate" "$mpeer"
        ;;
      abort)
        read -r mtx mstate mpeer < "$STUB_STATE.move"
        printf '%s aborted %s\n' "$mtx" "$mpeer" > "$STUB_STATE.move"
        rm -f "$STUB_STATE.migration"
        ;;
      dry) ;;
      move)
        [ "$1" = "$(cat "$STUB_STATE" 2>/dev/null)" ] || { printf 'error: instance not found: %s\n' "$1" >&2; exit 1; }
        printf '{"state": "migrating", "moveTxid": "%s", "peer": "%s"}' "$txid" "$to" > "$STUB_STATE.migration"
        if [ -n "${STUB_MIGRATE_LOSE:-}" ]; then
          printf '%s committing %s\n' "$txid" "$to" > "$STUB_STATE.move"
          printf 'error: ssh: connection reset by peer\n' >&2; exit 1
        fi
        # How the copy went, on stderr as hull prints it.
        printf 'Move %s of %s to %s, epoch 1\n' "$txid" "$1" "$to" >&2
        printf 'round 0: 1048576 bytes in 0.5 s (2.0 MiB/s), dirty 120 pages/s\n' >&2
        printf 'round 1: 65536 bytes in 0.1 s (0.6 MiB/s), dirty 40 pages/s\n' >&2
        printf '%s' "$1" > "$STUB_REMOTE_STATE"
        printf 'block' > "$STUB_REMOTE_STATE.rootfs"
        : > "$STUB_REMOTE_STATE.shares"
        printf '1 0 254:1 / / rw - ext4\n' > "$STUB_REMOTE_STATE.mounts"
        printf '%s' "$gw" > "$STUB_REMOTE_STATE.gw"
        printf '{"state": "restored", "txid": "%s", "moveTxid": "%s", "peer": "source"}' "$txid" "$txid" > "$STUB_REMOTE_STATE.migration"
        printf '{"state": "migrated", "moveTxid": "%s", "peer": "%s"}' "$txid" "$to" > "$STUB_STATE.migration"
        printf '%s committed %s\n' "$txid" "$to" > "$STUB_STATE.move"
        # hull stops the source's fenced VMM once the move committed.
        mv "$STUB_STATE" "$STUB_STATE.stopped"
        ;;
    esac
    ;;
  stop)
    [ -f "$STUB_STATE" ] && mv "$STUB_STATE" "$STUB_STATE.stopped"
    ;;
  rm)
    rm -f "$STUB_STATE" "$STUB_STATE.stopped" "$STUB_STATE.mounts" "$STUB_STATE.migration" "$STUB_STATE.move"
    ;;
esac
exit 0
STUB
chmod +x "$WORK/hull"
# The stub gateway's listener, also started by hand below as a gateway from an
# older brig.
export STUB_GATEWAY='import os, socket, sys, time; p = sys.argv[1]; c = socket.socket(socket.AF_UNIX); r = c.connect_ex(p); c.close(); r != 0 and os.path.exists(p) and os.unlink(p); s = socket.socket(socket.AF_UNIX); s.bind(p); s.listen(8); time.sleep(120)'

go build -o "$WORK/brig" ./cmd/brig || { echo "build failed"; exit 1; }

export BRIG_RUNTIME=hull
export BRIG_RUNTIME_BIN="$WORK/hull"
export BRIG_WORKSPACE="$WS"
export BRIG_READY_TIMEOUT=5
# brig remembers which workspace each sandbox was started with, in a file under
# its state directory. Scratch, like the profile directory: the test creates and
# removes sandboxes, and it must not edit the state of whoever is running it.
export BRIG_STATE_DIR="$WORK/state"
# The gateway directory is scratch for the same reason. Every boot records its
# network posture there, and rm and the listing prune those records against
# the stub's sandboxes, so the developer's own ~/.brig/networks.json would
# lose the posture of every real sandbox. It also holds the gateway sockets
# and the address ledger, which the EXIT trap takes with everything else. It
# fits: a sockaddr_un holds 103 bytes, and the longest path built under here
# -- $WORK/gw/sandbox-brig-claude-code.sock.qemu -- measures 101 on a Mac,
# where mktemp -d is at its longest.
export BRIG_GATEWAY_DIR="$WORK/gw"
# BRIG_GATEWAY_SOCK names the shared socket ahead of the directory, and rm
# --all stops the gateway on it. One exported by the caller is not the
# script's to stop.
unset BRIG_GATEWAY_SOCK
# The image checks get their own cases below, with a stub cosign. Everywhere
# else they are off: a CI runner has no cosign and must not reach a registry.
export BRIG_VERIFY=off
# The built-in profiles boot generically now, which means brig wants a kernel
# and an initrd before it starts anything. Point it at a pair of stand-ins: the
# stub runtime never reads them, and without this every run would try to
# download the real bundle from a registry -- on a CI runner, for a test that
# is about argv.
mkdir -p "$WORK/assets"
: > "$WORK/assets/Image"
: > "$WORK/assets/bzImage"
: > "$WORK/assets/container-initrd"
printf 'stand-in\n' > "$WORK/assets/Image"
printf 'stand-in\n' > "$WORK/assets/bzImage"
printf 'stand-in\n' > "$WORK/assets/container-initrd"
export BRIG_BOOT_ASSETS="$WORK/assets"
# A teleportable session on an arm64 host boots the portable machine, whose
# kernel is a file of its own beside the bundle's pair. A stand-in as well.
mkdir -p "$WORK/assets/portable-arm64-v1"
printf 'stand-in portable\n' > "$WORK/assets/portable-arm64-v1/Image"
# The machine a teleportable session boots, and its kernel: the portable one on
# an arm64 host, hvi's native machine with the bundle's kernel on any other.
# CI runs on amd64, so there the second branch is what is checked.
case "$(uname -m)" in
  arm64|aarch64) TP_MACHINE='[--profile][portable-arm64-v1]'; TP_KERNEL="$WORK/assets/portable-arm64-v1/Image" ;;
  x86_64|amd64) TP_MACHINE=''; TP_KERNEL="$WORK/assets/bzImage" ;;
  *) TP_MACHINE=''; TP_KERNEL="$WORK/assets/Image" ;;
esac
# The built-in profiles ask for hvi, and brig starts a network gateway for it
# before booting anything. Most cases here are about what reaches argv and have
# no use for one, so those fixtures explicitly select vz and shared. The
# default-network and egress cases override these settings per command, and
# the stub answers as a gateway for them.
export BRIG_HYPERVISOR=vz
export BRIG_NETWORK=shared
# Your own profiles go in a scratch directory, never the caller's own.
export BRIG_PROFILE_DIR="$WORK/profiles"
# Brig's own secret store is read here: it is the login keychain, so on a
# developer's Mac a stored secret (an imported claude credential, say) reaches
# these runs, and on CI's Linux host there is none. That is why the stub's
# stat answers for the root guest the shipped profiles use. hostCredential:,
# the one profile key that read the keychain outside that store, is removed,
# and so is BRIG_CREDENTIALS_CMD; the case below is what is left to assert
# about it.

echo "== run =="
CLAUDE_CODE_OAUTH_TOKEN=env-token-secret GH_TOKEN=gh-secret \
  "$WORK/brig" run claude -p hi > "$WORK/run.out" 2> "$WORK/run.err"
rc=$?
[ "$rc" = 0 ] && ok "run exits 0" || bad "run exits 0 -- got $rc: $(cat "$WORK/run.err")"

# The whole reason brig builds the command line itself: a forwarded value
# must never be readable in `ps`.
if grep -q 'env-token-secret\|gh-secret' "$STUB_LOG"; then
  if grep '^argv:' "$STUB_LOG" | grep -q 'env-token-secret\|gh-secret'; then
    bad "a credential value reached argv"
  else
    ok "credential values reach the runtime, but never through argv"
  fi
else
  bad "no credential reached the runtime at all"
fi
grep -q 'env-token:env-token-secret' "$STUB_LOG" \
  && ok "the environment token is forwarded" || bad "the environment token is forwarded"
# GH_TOKEN alone: claude-code delivers its OAuth credential as a file into a
# tmpfs rather than as a variable, so the only thing left on the env line is
# the token brig's git helper reads.
grep '^argv:' "$STUB_LOG" | grep -q -- '--env GH_TOKEN' \
  && ok "argv names the variables only" || bad "argv names the variables only"
# hull keeps its store under the HOME it runs with. The guest HOME travels on
# the command line, and hull keeps the caller's own (#337).
grep '^argv: run ' "$STUB_LOG" | grep -q -- "--env HOME=$GUEST_HOME " \
  && ok "the guest HOME is on the run line" || bad "the guest HOME is on the run line"
grep '^argv: exec ' "$STUB_LOG" | grep -- '-- claude -p hi' | grep -q -- "--env HOME=$GUEST_HOME " \
  && ok "the guest HOME is on the exec line" || bad "the guest HOME is on the exec line"
homes="$(grep '^env-home:' "$STUB_LOG" | sort -u)"
[ "$homes" = "env-home:$HOME" ] \
  && ok "hull runs with the caller's HOME" \
  || bad "hull runs with the caller's HOME ($HOME) -- got: $homes"

# shared_dir SPEC succeeds when a run line carries --shared-dir SPEC as a
# whole argument. A substring match would take SPEC:ro for SPEC, and the
# claims these checks back say read-write.
shared_dir() {
  awk -v want=" --shared-dir $1 " \
    '/^argv: run / && index($0 " ", want) { found = 1 } END { exit !found }' "$STUB_LOG"
}

shared_dir "$WS:$GUEST_HOME" \
  && ok "the workspace is mounted as the guest home" || bad "workspace is mounted as the guest home"
grep -q -- '-- claude -p hi' "$STUB_LOG" \
  && ok "agent arguments pass through" || bad "agent arguments pass through"

echo "== envelope =="
# The block is a notice, printed to stderr so it never pollutes the agent's
# stdout or a scripted create's sandbox name. It names the boundary before the
# boot noise.
#
# Behind --verbose: nine rows of boundary before the agent says anything is
# machinery, and `brig info` is the command whose whole output it is. So the
# assertions below read a --verbose run, and the first pair pins the
# default: the block is absent, and what a quiet run still says about the
# boundary is the verification line.
grep -q '^SANDBOX ' "$WORK/run.err" \
  && bad "a default run printed the envelope -- got: $(cat "$WORK/run.err")" \
  || ok "a default run does not print the envelope"
: > "$STUB_LOG"
CLAUDE_CODE_OAUTH_TOKEN=env-token-secret GH_TOKEN=gh-secret \
  "$WORK/brig" --verbose run claude -p hi > /dev/null 2> "$WORK/env.err"
grep -q '^SANDBOX .*brig-claude-code' "$WORK/env.err" \
  && ok "--verbose prints the execution envelope" \
  || bad "--verbose prints the execution envelope: $(cat "$WORK/env.err")"
grep -q '^WORKSPACE ' "$WORK/env.err" \
  && ok "the envelope names the workspace" || bad "the envelope names the workspace"
# What the sandbox stands on, not only what drives it. The stub is a hull
# stand-in and BRIG_HYPERVISOR is pinned to vz above, so the row names that
# backend; which containerd shim maps to which boundary is settled in the
# runtime package's own tests, there being no containerd here to ask.
grep -q '^ISOLATION .*microVM (hull, vz backend)' "$WORK/env.err" \
  && ok "the envelope names the isolation boundary" \
  || bad "the envelope names the isolation boundary -- got: $(grep '^ISOLATION' "$WORK/env.err")"
# A value must never reach the block, the same promise argv keeps. Scan the
# whole envelope output rather than a fixed list of rows, so a row added later
# is covered without touching this check.
grep -q 'env-token-secret\|gh-secret\|host-token' "$WORK/env.err" \
  && bad "the envelope printed a credential value" \
  || ok "the envelope names credentials, never values"

# -q prints no envelope either, which is no longer -q's doing -- the default
# prints none. Kept as the guard that asking for less never yields more.
: > "$STUB_LOG"
CLAUDE_CODE_OAUTH_TOKEN=env-token-secret GH_TOKEN=gh-secret \
  "$WORK/brig" run claude --quiet -p hi > /dev/null 2> "$WORK/quiet.err"
grep -q '^SANDBOX ' "$WORK/quiet.err" \
  && bad "--quiet printed the envelope" || ok "--quiet prints no envelope"

echo "== workspace =="
[ -f "$WS/.claude/.claude.json" ] && ok "onboarding is seeded" || bad "onboarding is seeded"
grep -q hasCompletedOnboarding "$WS/.claude/.claude.json" \
  && ok "the seed carries the onboarding flags" || bad "the seed carries the onboarding flags"
# Every file in the guest home, for every value the runs above delivered.
# grep exits 1 only when it read all of them and found none.
leaked="$(grep -rlF -e env-token-secret -e gh-secret "$WS")"
rc=$?
if [ "$rc" = 1 ] && ! grep -qi 'token' "$WS/.claude/.claude.json"; then
  ok "no credential is written into the workspace"
else
  bad "a credential was written into the workspace (grep exit $rc): $leaked"
fi
[ -f "$WS/.brig-workspace" ] && ok "the workspace marker is written" || bad "workspace marker is written"

echo "== reuse =="
: > "$STUB_LOG"
CLAUDE_CODE_OAUTH_TOKEN=env-token-secret "$WORK/brig" run claude -p again \
  > /dev/null 2>&1
grep -q '^argv: run ' "$STUB_LOG" \
  && bad "a second run booted a second sandbox" \
  || ok "a running sandbox is reused, not rebooted"

echo "== project =="
# The positional: `brig run <ref> <project>` mounts that directory at
# /work/<basename>, OUTSIDE the guest home, and starts the agent in it. Outside
# is the property that matters -- the home is the agent's home, dotfiles and
# state included, and a project under it could be mistaken for either.
"$WORK/brig" rm --all -y > /dev/null 2>&1
PROJ="$WORK/myproject"
mkdir -p "$PROJ"
: > "$STUB_LOG"
CLAUDE_CODE_OAUTH_TOKEN=env-token-secret \
  "$WORK/brig" --verbose run claude "$PROJ" -p hi > "$WORK/proj.out" 2> "$WORK/proj.err"
rc=$?
[ "$rc" = 0 ] && ok "a run with a project exits 0" \
  || bad "a run with a project exits 0 -- got $rc: $(cat "$WORK/proj.err")"
shared_dir "$PROJ:/work/myproject" \
  && ok "the project is mounted at /work/<basename>" \
  || bad "the project is mounted at /work/<basename> -- got: $(grep '^argv: run' "$STUB_LOG")"
grep -q -- "--shared-dir $WS:$GUEST_HOME" "$STUB_LOG" \
  && ok "the home share is unchanged by a project" \
  || bad "the home share is unchanged by a project -- got: $(grep '^argv: run' "$STUB_LOG")"
grep -q -- '--cwd /work/myproject' "$STUB_LOG" \
  && ok "the agent starts in the project" \
  || bad "the agent starts in the project -- got: $(grep '^argv: exec' "$STUB_LOG" | tail -1)"
grep -q -- '-- claude -p hi' "$STUB_LOG" \
  && ok "agent arguments still pass through beside a project" \
  || bad "agent arguments still pass through beside a project"
grep -q "^PROJECT .*myproject (read-write, mounted at /work/myproject)" "$WORK/proj.err" \
  && ok "the envelope names the project" \
  || bad "the envelope names the project -- got: $(cat "$WORK/proj.err")"
grep -q "\"project\": \"$PROJ\"" "$BRIG_STATE_DIR/sessions.json" \
  && ok "the index records the project the session ran with" \
  || bad "the index records the project -- got: $(cat "$BRIG_STATE_DIR/sessions.json" 2>&1)"

# A project is the documented spelling, so it runs without a notice. The
# one-release warning that the word used to reach the agent is gone (#315).
# Two markers, as in the Go test: the sentence that named the new reading,
# and the `--` hint that named the old one.
grep -q -e 'project directory this run mounts' -e 'put it after --' "$WORK/proj.err" \
  && bad "a run naming a project printed the old notice -- got: $(cat "$WORK/proj.err")" \
  || ok "a run naming a project prints no notice"

# A share is bound at boot and cannot be attached to a live sandbox, so the
# same project reuses the sandbox and a different one recreates it.
: > "$STUB_LOG"
CLAUDE_CODE_OAUTH_TOKEN=env-token-secret \
  "$WORK/brig" run claude "$PROJ" -p again > /dev/null 2>&1
grep -q '^argv: run ' "$STUB_LOG" \
  && bad "the same project restarted the sandbox" \
  || ok "the same project reuses the sandbox"
OTHER="$WORK/otherproject"
mkdir -p "$OTHER"
: > "$STUB_LOG"
CLAUDE_CODE_OAUTH_TOKEN=env-token-secret \
  "$WORK/brig" run claude "$OTHER" -p again > /dev/null 2> "$WORK/reproj.err"
grep -q '^argv: run ' "$STUB_LOG" \
  && ok "a different project recreates the sandbox" \
  || bad "a different project recreates the sandbox -- got: $(cat "$WORK/reproj.err")"
grep -q -- "--shared-dir $OTHER:/work/otherproject" "$STUB_LOG" \
  && ok "the recreated sandbox mounts the new project" \
  || bad "the recreated sandbox mounts the new project -- got: $(grep '^argv: run' "$STUB_LOG")"

# After an explicit -- the line said the word is the agent's, and it still
# reaches the agent untouched. This run names no project, so it recreates the
# sandbox the block above left mounting one -- which is the same rule, read in
# the other direction.
: > "$STUB_LOG"
CLAUDE_CODE_OAUTH_TOKEN=env-token-secret \
  "$WORK/brig" run claude --quiet -- --version > /dev/null 2> "$WORK/dashdash.err"
grep -q -- '-- claude --version' "$STUB_LOG" \
  && ok "a word after -- still reaches the agent" \
  || bad "a word after -- still reaches the agent -- got: $(grep '^argv: exec' "$STUB_LOG" | tail -1) $(cat "$WORK/dashdash.err")"

# A word that names no directory is refused rather than mounted, and the
# refusal carries the way past it. This is the line the grammar change breaks,
# so it has to fail loudly instead of booting something odd.
out="$("$WORK/brig" run claude notadirectory -p hi 2>&1)"; rc=$?
[ "$rc" != 0 ] && ok "a project that is not a directory refuses the run" \
  || bad "a project that is not a directory started anyway: $out"
case "$out" in
  *"put it after --"*) ok "the refusal names the way past it" ;;
  *) bad "the refusal names the way past it -- got: $out" ;;
esac

# sh takes no positional: a second bare word there is already the guest
# command, and this change must not take it away.
"$WORK/brig" rm --all -y > /dev/null 2>&1
"$WORK/brig" run claude -d > /dev/null 2>&1
: > "$STUB_LOG"
"$WORK/brig" sh claude echo hi > /dev/null 2> "$WORK/shproj.err"
grep -qF -- '[--][bash][-lc]["$@"][bash][echo][hi]' "$STUB_LOG" \
  && ok "sh still reads a second bare word as the guest command" \
  || bad "sh still reads a second bare word as the guest command -- got: $(grep '^words:' "$STUB_LOG" | tail -1) $(cat "$WORK/shproj.err")"

# The project is inherited by a verb that names none, the way the home already
# is. Reading that silence as "no project" is what real-runtime testing caught:
# `brig sh <ref>` after a run with a project stopped, removed and recreated the
# sandbox without the mount, taking the guest's memory-only state with it and
# overwriting the index entry that recorded the project.
#
# Driven on the ubuntu profile, which delivers no credential files and so
# reaches the guest exec on any host -- which is what makes the working
# directory assertable rather than only the boot.
"$WORK/brig" rm --all -y > /dev/null 2>&1
: > "$STUB_LOG"
"$WORK/brig" run ubuntu "$PROJ" -- pwd > /dev/null 2>&1
grep -q -- "--shared-dir $PROJ:/work/myproject" "$STUB_LOG" \
  && ok "a shell profile takes a project too" \
  || bad "a shell profile takes a project too -- got: $(grep '^argv: run' "$STUB_LOG")"
grep -q -- '--cwd /work/myproject' "$STUB_LOG" \
  && ok "the run starts in the project" \
  || bad "the run starts in the project -- got: $(grep '^argv: exec' "$STUB_LOG" | tail -1)"

: > "$STUB_LOG"
"$WORK/brig" sh ubuntu -- pwd > /dev/null 2>&1
grep -q '^argv: run ' "$STUB_LOG" \
  && bad "a flagless sh recreated the sandbox" \
  || ok "a flagless sh keeps the sandbox it was given"
grep -q -- '--cwd /work/myproject' "$STUB_LOG" \
  && ok "a flagless sh inherits the project" \
  || bad "a flagless sh inherits the project -- got: $(grep '^argv: exec' "$STUB_LOG" | tail -1)"
grep -q "\"project\": \"$PROJ\"" "$BRIG_STATE_DIR/sessions.json" \
  && ok "the index keeps the project across a flagless verb" \
  || bad "the index keeps the project -- got: $(cat "$BRIG_STATE_DIR/sessions.json" 2>&1)"

: > "$STUB_LOG"
"$WORK/brig" run ubuntu -- pwd > /dev/null 2>&1
grep -q '^argv: run ' "$STUB_LOG" \
  && bad "a flagless run recreated the sandbox" \
  || ok "a flagless run keeps the sandbox it was given"
grep -q -- '--cwd /work/myproject' "$STUB_LOG" \
  && ok "a flagless run inherits the project" \
  || bad "a flagless run inherits the project -- got: $(grep '^argv: exec' "$STUB_LOG" | tail -1)"

# Naming a different one is a request rather than an accident, so it still
# recreates -- inheritance is only about a line that named nothing.
: > "$STUB_LOG"
"$WORK/brig" run ubuntu "$OTHER" -- pwd > /dev/null 2>&1
grep -q '^argv: run ' "$STUB_LOG" \
  && ok "a different project still recreates the sandbox" \
  || bad "a different project still recreates the sandbox"
grep -q -- '--cwd /work/otherproject' "$STUB_LOG" \
  && ok "the recreated sandbox starts in the new project" \
  || bad "the recreated sandbox starts in the new project -- got: $(grep '^argv: exec' "$STUB_LOG" | tail -1)"
"$WORK/brig" rm --all -y > /dev/null 2>&1

echo "== the home flag =="
# --home is what sets the guest home now. -w and --workspace keep working and
# each say the one word that replaces them: a line that works today has to keep
# working, and a spelling that works silently never teaches anyone the new one.
HOMEDIR="$WORK/named-home"
: > "$STUB_LOG"
env -u BRIG_WORKSPACE "$WORK/brig" run claude --home "$HOMEDIR" -d \
  > /dev/null 2> "$WORK/home.err"
grep -q -- "--shared-dir $HOMEDIR:$GUEST_HOME" "$STUB_LOG" \
  && ok "--home is mounted as the guest home" \
  || bad "--home is mounted as the guest home -- got: $(grep '^argv: run' "$STUB_LOG")"
grep -q 'is now' "$WORK/home.err" \
  && bad "--home printed a deprecation notice -- got: $(cat "$WORK/home.err")" \
  || ok "--home prints no notice"
"$WORK/brig" rm --all -y > /dev/null 2>&1
for spelling in -w --workspace; do
  : > "$STUB_LOG"
  env -u BRIG_WORKSPACE "$WORK/brig" run claude "$spelling" "$HOMEDIR" -d \
    > /dev/null 2> "$WORK/oldhome.err"
  grep -q -- "--shared-dir $HOMEDIR:$GUEST_HOME" "$STUB_LOG" \
    && ok "$spelling still sets the guest home" \
    || bad "$spelling still sets the guest home -- got: $(grep '^argv: run' "$STUB_LOG")"
  grep -q "is now \`--home\`" "$WORK/oldhome.err" \
    && ok "$spelling names --home as its replacement" \
    || bad "$spelling names --home -- got: $(cat "$WORK/oldhome.err")"
  "$WORK/brig" rm --all -y > /dev/null 2>&1
done

echo "== network =="
# The posture the run resolved to is the posture the runtime is told about.
# Net was a hardcoded "shared" for the whole life of the field, so this is the
# check that the resolved value actually travels.
"$WORK/brig" rm --all -y > /dev/null 2>&1
: > "$STUB_LOG"
"$WORK/brig" --verbose run claude --offline -d > "$WORK/off.out" 2>&1
grep -q -- '--net none' "$STUB_LOG" \
  && ok "--offline reaches the runtime as --net none" \
  || bad "--offline reaches the runtime as --net none -- got: $(grep '^argv: run' "$STUB_LOG")"
grep -q '^NETWORK .*offline' "$WORK/off.out" \
  && ok "the envelope says the sandbox is offline" \
  || bad "the envelope says the sandbox is offline -- got: $(cat "$WORK/off.out")"

"$WORK/brig" rm --all -y > /dev/null 2>&1
: > "$STUB_LOG"
# An unset posture on vz still takes its documented shared fallback. Use a
# profile that leaves the field unset and remove the fixture's override, so
# this proves the fallback instead of merely echoing BRIG_NETWORK=shared.
mkdir -p "$BRIG_PROFILE_DIR"
cat > "$BRIG_PROFILE_DIR/vz-default.yaml" <<'YAML'
name: vz-default
image: smoke/image:latest
binary: sh
guestHome: /root
mem: 256
cpus: 1
hypervisor: vz
YAML
env -u BRIG_NETWORK "$WORK/brig" --verbose run vz-default -d > "$WORK/on.out" 2>&1
grep -q -- '--net shared' "$STUB_LOG" \
  && ok "a default run still asks for the shared network" \
  || bad "a default run still asks for the shared network -- got: $(grep '^argv: run' "$STUB_LOG")"
grep -q '^NETWORK .*shared' "$WORK/on.out" \
  && ok "the envelope names the shared posture" \
  || bad "the envelope names the shared posture -- got: $(cat "$WORK/on.out")"
grep -q '^NETWORK .*vz does not support isolated networking' "$WORK/on.out" \
  && ok "the vz default explains why its network is shared" \
  || bad "the vz default did not explain its fallback -- got: $(cat "$WORK/on.out")"
rm -f "$BRIG_PROFILE_DIR/vz-default.yaml"

# isolated gives the sandbox a network of its own, which brig can only do where
# it owns the gateway. This run is pinned to vz, where the network comes from
# vmnet, so the posture is refused rather than accepted and dropped -- it was
# accepted and dropped before, and the envelope reported a network of the
# sandbox's own over a sandbox on the shared one. The negative half is the half
# worth testing: "it booted" would pass with the whole thing deleted. That two
# sandboxes on separate networks genuinely cannot reach each other is a
# real-runtime fact, recorded in docs/manual-tests.
"$WORK/brig" rm --all -y > /dev/null 2>&1
: > "$STUB_LOG"
if "$WORK/brig" run claude --network isolated -d > "$WORK/iso.out" 2>&1; then
  bad "--network isolated was accepted on a backend that cannot give one"
else
  grep -q 'hvi' "$WORK/iso.out" \
    && ok "isolated refuses where brig owns no network, naming the backend that does" \
    || bad "the refusal does not name the backend -- got: $(cat "$WORK/iso.out")"
fi
grep -q '^argv: run' "$STUB_LOG" \
  && bad "the runtime was invoked for a posture it cannot honour" \
  || ok "nothing reached the runtime for isolated"
"$WORK/brig" rm --all -y > /dev/null 2>&1

# A policy is enforced at the user-mode gateway, which only the hvi backend
# uses. This run is pinned to vz, so the boot must be refused rather than
# started with the rules dropped: a sandbox that reports a policy and enforces
# nothing is worse than one with no policy. The negative half is the half worth
# testing -- "it booted" would pass with the whole feature deleted.
mkdir -p "$WORK/policies"
cat > "$WORK/policies/no-net.yaml" <<'YAML'
apiVersion: brig.sh/v1alpha1
name: no-net
egress:
  default: deny
  allow:
    - host: api.anthropic.com
YAML
export BRIG_POLICY_DIR="$WORK/policies"
"$WORK/brig" policy attach no-net claude-code > /dev/null 2>&1 \
  && ok "a policy attaches to a profile" \
  || bad "a policy attaches to a profile"
: > "$STUB_LOG"
if "$WORK/brig" run claude -d > "$WORK/pol.out" 2>&1; then
  bad "a policy on a backend that cannot enforce it was booted anyway"
else
  grep -q 'hvi' "$WORK/pol.out" \
    && ok "a policy brig cannot enforce refuses the boot, naming the backend that can" \
    || bad "the refusal does not name the backend -- got: $(cat "$WORK/pol.out")"
fi
grep -q '^argv: run' "$STUB_LOG" \
  && bad "the runtime was invoked for a policy that could not be enforced" \
  || ok "nothing reached the runtime"
"$WORK/brig" policy detach no-net claude-code > /dev/null 2>&1
unset BRIG_POLICY_DIR
"$WORK/brig" rm --all -y > /dev/null 2>&1

# And the other half of it: on a backend that can enforce a policy, the rules
# have to arrive. The case above proves brig refuses what it cannot enforce,
# which would still pass with the whole feature deleted -- an attach that
# writes a file and a boot that reads nothing would refuse on vz just the same.
# This one proves the rules travel. It and the default-network case below
# need a gateway, so they leave the vz fixture behind.
#
# The stub gateway is a python listener, so a runner without python3 would
# otherwise skip the only automated proof of #15 and stay green. In CI that is
# a failure; on a laptop it is a skip.
if ! command -v python3 > /dev/null 2>&1; then
  if [ -n "${CI:-}" ]; then
    bad "no python3, so the policy-reaches-the-gateway case could not run"
  else
    printf '  skip a policy reaches the gateway (no python3 for the stub gateway)\n'
  fi
else
# A fresh custom profile on hvi leaves network unset. Clear the fixture
# override: leaving it set would make every other assertion green while never
# testing the default. Hull still takes --net shared for an isolated sandbox,
# so the proof is the guest's own /30 and its exact gateway, not that flag alone.
: > "$STUB_LOG"
cat > "$BRIG_PROFILE_DIR/hvi-default.yaml" <<'YAML'
name: hvi-default
image: smoke/image:latest
binary: sh
guestHome: /root
mem: 256
cpus: 1
hypervisor: hvi
YAML
env -u BRIG_NETWORK BRIG_HYPERVISOR=hvi \
  "$WORK/brig" --verbose run hvi-default -d > "$WORK/default-net.out" 2>&1
defaultrc=$?
grep '^argv: network-gateway --socket' "$STUB_LOG" > "$WORK/gw-default.argv"
defaultsock="$(sed -n 's/.*--socket \([^ ]*\).*/\1/p' "$WORK/gw-default.argv")"
defaultrun="$(grep '^argv: run' "$STUB_LOG")"
if [ "$defaultrc" != 0 ]; then
  bad "the new default hvi run failed: $(cat "$WORK/default-net.out")"
elif [ -z "$defaultsock" ] || ! grep -q -- '--socket [^ ]*/sandbox-' "$WORK/gw-default.argv"; then
  bad "the new default started no gateway of its own: $(cat "$WORK/gw-default.argv")"
elif ! grep -qE -- '--subnet [0-9.]+/30' "$WORK/gw-default.argv"; then
  bad "the new default has no private subnet: $(cat "$WORK/gw-default.argv")"
elif ! printf '%s' "$defaultrun" | grep -qF -- "--gateway-sock $defaultsock" ||
     ! printf '%s' "$defaultrun" | grep -qE -- '--gateway-cidr [0-9.]+/30'; then
  bad "the new default guest was not attached to its own gateway: $defaultrun"
elif grep -q -- '--egress' "$WORK/gw-default.argv"; then
  bad "the new default applied an egress rule nobody attached: $(cat "$WORK/gw-default.argv")"
elif ! grep -q '^NETWORK .*isolated' "$WORK/default-net.out"; then
  bad "the new default envelope did not name isolation: $(cat "$WORK/default-net.out")"
else
  ok "a new default hvi run uses its own unfiltered gateway"
fi
"$WORK/brig" rm --all -y > /dev/null 2>&1
rm -f "$BRIG_PROFILE_DIR/hvi-default.yaml"

cat > "$WORK/policies/reachable.yaml" <<'YAML'
apiVersion: brig.sh/v1alpha1
name: reachable
egress:
  default: deny
  allow:
    - host: api.anthropic.com
    - cidr: 140.82.112.0/20
  deny:
    - host: telemetry.example.com
YAML
export BRIG_POLICY_DIR="$WORK/policies"
"$WORK/brig" policy attach reachable claude-code > /dev/null 2>&1
: > "$STUB_LOG"
env BRIG_HYPERVISOR=hvi "$WORK/brig" run claude -d > "$WORK/pol-on.out" 2>&1
polrc=$?
grep '^argv: network-gateway --socket' "$STUB_LOG" > "$WORK/gw-on.argv"
if [ "$polrc" != 0 ]; then
  bad "the run carrying a policy failed: $(cat "$WORK/pol-on.out")"
else
  missing=""
  for want in "--egress-default deny" \
              "--egress-allow host=api.anthropic.com" \
              "--egress-allow cidr=140.82.112.0/20" \
              "--egress-deny host=telemetry.example.com"; do
    grep -q -- "$want" "$WORK/gw-on.argv" || missing="$missing [$want]"
  done
  [ -z "$missing" ] \
    && ok "every rule of an attached policy reaches the gateway" \
    || bad "rules missing from the gateway command line:$missing -- got: $(cat "$WORK/gw-on.argv")"
fi

# A rule set is only enforceable if the guest is behind the gateway carrying
# it, so the boot has to name that gateway -- this exact socket, not merely
# some gateway. hull is passed --gateway-sock and --gateway-cidr for every
# networked run on this backend, shared or filtered, so asserting the flags
# are present asserts nothing: a boot that starts the filtered gateway and
# then attaches the guest to the shared one would satisfy it.
polsock="$(sed -n 's/.*--socket \([^ ]*\).*/\1/p' "$WORK/gw-on.argv")"
runline="$(grep '^argv: run' "$STUB_LOG")"
if [ -z "$polsock" ]; then
  bad "no policy gateway was started, so the sandbox could not be behind one"
elif ! printf '%s' "$runline" | grep -q -- "--gateway-sock $polsock"; then
  bad "the sandbox was not attached to the gateway carrying the rules ($polsock): $runline"
elif ! printf '%s' "$runline" | grep -qE -- '--gateway-cidr [0-9.]+/30'; then
  bad "the sandbox was not given an address on a network of its own: $runline"
else
  ok "and the sandbox is put on the network that gateway serves"
fi

# The explicit shared fixture stays open after a policy is detached. This
# also proves that shared remains available even though new runs isolate.
"$WORK/brig" rm --all -y > /dev/null 2>&1
"$WORK/brig" policy detach reachable claude-code > /dev/null 2>&1
: > "$STUB_LOG"
env BRIG_HYPERVISOR=hvi "$WORK/brig" run claude -d > "$WORK/pol-off.out" 2>&1
offrc=$?
grep '^argv: network-gateway --socket' "$STUB_LOG" > "$WORK/gw-off.argv"
# Four claims, because three of them pass on an empty file. A gateway has to
# have been started at all -- otherwise a reused one from the case above would
# let the rest pass while measuring nothing; it has to be a shared gateway
# rather than one raised for this sandbox alone, because the fixture asks
# for shared; and it must carry no rule. The shared one is named for its
# network and the per-sandbox
# one for its sandbox, and only the shared network is a /24 -- both read off
# the argv rather than compared against a literal subnet, which has moved once
# already.
if [ "$offrc" != 0 ]; then
  bad "the run with no policy failed: $(cat "$WORK/pol-off.out")"
elif [ ! -s "$WORK/gw-off.argv" ]; then
  bad "no gateway was started for a run with no policy, so nothing was measured"
elif ! grep -q -- '--socket [^ ]*/gateway-' "$WORK/gw-off.argv"; then
  bad "a run with nothing attached was given a gateway of its own: $(cat "$WORK/gw-off.argv")"
elif ! grep -qE -- '--subnet [0-9.]+/24' "$WORK/gw-off.argv"; then
  bad "a run with nothing attached was not put on the shared network: $(cat "$WORK/gw-off.argv")"
elif grep -q -- '--egress' "$WORK/gw-off.argv"; then
  bad "a run with nothing attached was filtered anyway: $(cat "$WORK/gw-off.argv")"
else
  ok "a run with no policy stays on the shared network and passes no egress rule"
fi

# rm --all stops the shared gateway too once no sandbox is on it. Before, it
# outlived every sandbox, and only a pkill stopped it. ps is read into a file
# first, so the grep does not find its own argv.
sharedsock="$(sed -n 's/.*--socket \([^ ]*\/gateway-[^ ]*\).*/\1/p' "$WORK/gw-off.argv" | head -n 1)"
"$WORK/brig" rm --all -y > /dev/null 2>&1
ps -A -ww -o pid=,command= > "$WORK/ps.out"
if [ -z "$sharedsock" ]; then
  bad "no shared gateway was started, so rm --all stopping it was not measured"
elif grep -qF -- "--socket $sharedsock " "$WORK/ps.out"; then
  bad "rm --all left the shared gateway running: $(grep -F -- "--socket $sharedsock " "$WORK/ps.out")"
else
  ok "rm --all stops the shared gateway once no sandbox is on it"
fi

# A shared gateway from an older brig has no --api, so it cannot publish a
# port. With no sandbox on it, the next boot replaces it with one that has
# --api. Started by hand here, with the argv such a gateway has.
if [ -n "$sharedsock" ]; then
  python3 -c "$STUB_GATEWAY" "$sharedsock.qemu" network-gateway --socket "$sharedsock" \
    --qemu-socket "$sharedsock.qemu" > /dev/null 2>&1 &
  oldpid=$!
  # The file appears at bind, a moment before listen.
  for _ in $(seq 50); do [ -S "$sharedsock.qemu" ] && break; sleep 0.1; done
  sleep 0.3
  : > "$STUB_LOG"
  env BRIG_HYPERVISOR=hvi "$WORK/brig" run claude -d > "$WORK/old-gw.out" 2>&1
  oldrc=$?
  newgw="$(grep "^argv: network-gateway --socket $sharedsock " "$STUB_LOG")"
  if [ "$oldrc" != 0 ]; then
    bad "a run behind a gateway from an older brig failed: $(cat "$WORK/old-gw.out")"
  elif ! printf '%s' "$newgw" | grep -q -- '--api '; then
    bad "the gateway from an older brig was not replaced with one that has --api: $newgw"
  elif ps -ww -o command= -p "$oldpid" 2>/dev/null | grep -q network-gateway; then
    bad "the gateway from an older brig (pid $oldpid) is still running beside its replacement"
  else
    ok "a boot replaces a shared gateway from an older brig that no sandbox is on"
  fi
  kill "$oldpid" > /dev/null 2>&1
  "$WORK/brig" rm --all -y > /dev/null 2>&1
fi
# Whatever a failed case above left behind outlives this script otherwise.
pkill -f "$BRIG_GATEWAY_DIR/" > /dev/null 2>&1
unset BRIG_POLICY_DIR
fi

# A posture brig does not know must stop the run rather than pick one.
out="$(BRIG_NETWORK=airgapped "$WORK/brig" info claude 2>&1)"; rc=$?
[ "$rc" != 0 ] && ok "an unknown BRIG_NETWORK refuses the run" \
  || bad "an unknown BRIG_NETWORK started anyway: $out"
"$WORK/brig" rm --all -y > /dev/null 2>&1

echo "== denylist =="
: > "$STUB_LOG"
out="$(ANTHROPIC_API_KEY=sk-metered BRIG_FORWARD_ENV='ANTHROPIC_API_KEY GH_TOKEN' \
  GH_TOKEN=gh-secret "$WORK/brig" info claude 2>&1)"
case "$out" in
  *ANTHROPIC_API_KEY*denylist*) ok "the metered key is refused, and says why" ;;
  *) bad "the metered key is refused -- got: $out" ;;
esac
case "$out" in
  *"forwarding to guest:"*GH_TOKEN*) ok "env reports forwarded names" ;;
  *) bad "env reports forwarded names -- got: $out" ;;
esac
case "$out" in
  *sk-metered*|*gh-secret*) bad "env printed a credential VALUE" ;;
  *) ok "env prints names, never values" ;;
esac

echo "== only declared names reach the guest =="
# The case above reads what info reports. These read what a real run hands
# the runtime. hull puts into the guest exactly the names given to it as
# `--env`, so the run and exec lines are the guest's environment. The stub
# itself inherits brig's whole environment, so no case asks the stub for these
# variables: a value the stub can see proves nothing about the guest.
#
# SNEAKY_SECRET stands for any ambient variable no profile declared. GH_TOKEN
# is the name claude-code declares. The first run is a plain `brig run`, with
# no BRIG_FORWARD_ENV and no hatch, because that is the path every run takes.
# env -u keeps a developer's own shell from turning either run into a
# different case. The per-agent spelling outranks the global one, so a
# BRIG_CLAUDE_CODE_FORWARD_ENV left in the shell replaces the list the second
# run sets, and the denylist is never asked about the key. BRIG_ENV_ARGV has
# only the global spelling.
unset_knobs=(-u BRIG_FORWARD_ENV -u BRIG_CLAUDE_CODE_FORWARD_ENV -u BRIG_ENV_ARGV
  -u BRIG_ALLOW_DENIED -u BRIG_CLAUDE_CODE_ALLOW_DENIED)
"$WORK/brig" rm --all -y > /dev/null 2>&1
: > "$STUB_LOG"
env "${unset_knobs[@]}" SNEAKY_SECRET=sk-ambient \
  GH_TOKEN=gh-secret CLAUDE_CODE_OAUTH_TOKEN=env-token-secret \
  "$WORK/brig" run claude -p hi > "$WORK/declared.out" 2> "$WORK/declared.err"
rc=$?
runline="$(grep '^argv: run ' "$STUB_LOG")"
execline="$(grep '^argv: exec ' "$STUB_LOG" | grep -- '-- claude -p hi')"
# Every negative below passes on an empty log, so first prove the run reached
# the runtime and the agent.
if [ "$rc" = 0 ] && [ -n "$runline" ] && [ -n "$execline" ]; then
  ok "a plain run with an ambient variable boots and execs the agent"
else
  bad "a plain run with an ambient variable boots -- got $rc: $(cat "$WORK/declared.err")"
fi
grep -q 'SNEAKY_SECRET\|sk-ambient' "$STUB_LOG" \
  && bad "an undeclared ambient variable reached the runtime: $(grep 'SNEAKY_SECRET\|sk-ambient' "$STUB_LOG")" \
  || ok "an undeclared ambient variable and its value reach no runtime argv or env line"
case "$runline" in
  *"--env GH_TOKEN"*) ok "the declared credential name reaches the guest" ;;
  *) bad "the declared credential name reaches the guest -- got: $runline" ;;
esac

# ANTHROPIC_API_KEY is on claude-code's denylist, and naming it in
# BRIG_FORWARD_ENV is the accident the denylist is for: a metered key swept in
# from the shell, outranking the subscription credential. BRIG_ENV_ARGV=1 is
# what gives the value check its teeth. With the hatch off no credential value
# goes on argv at all, so a denylist that let the key through still left
# sk-metered off the command line. With it on, a key that got past the
# denylist lands in argv as NAME=value.
"$WORK/brig" rm --all -y > /dev/null 2>&1
: > "$STUB_LOG"
env "${unset_knobs[@]}" SNEAKY_SECRET=sk-ambient ANTHROPIC_API_KEY=sk-metered \
  BRIG_FORWARD_ENV=ANTHROPIC_API_KEY BRIG_ENV_ARGV=1 GH_TOKEN=gh-secret \
  CLAUDE_CODE_OAUTH_TOKEN=env-token-secret \
  "$WORK/brig" run claude -p hi > "$WORK/declared.out" 2> "$WORK/declared.err"
rc=$?
runline="$(grep '^argv: run ' "$STUB_LOG")"
execline="$(grep '^argv: exec ' "$STUB_LOG" | grep -- '-- claude -p hi')"
if [ "$rc" = 0 ] && [ -n "$runline" ] && [ -n "$execline" ]; then
  ok "a run with a denied key in BRIG_FORWARD_ENV boots and execs the agent"
else
  bad "a run with a denied key in BRIG_FORWARD_ENV boots -- got $rc: $(cat "$WORK/declared.err")"
fi
# The two negatives after these pass on a run that never asked for the key or
# never had the hatch on, so prove the denylist refused the key and the hatch
# put a value on argv.
grep -q 'not forwarding ANTHROPIC_API_KEY: it is on the' "$WORK/declared.err" \
  && ok "the denylist refused the key this run asked for" \
  || bad "the denylist refused the key this run asked for -- got: $(cat "$WORK/declared.err")"
grep '^argv:' "$STUB_LOG" | grep -q 'GH_TOKEN=gh-secret' \
  && ok "the hatch put the declared value on argv, so the value check has teeth" \
  || bad "the hatch put the declared value on argv -- got: run=$runline exec=$execline"
grep -q 'SNEAKY_SECRET\|sk-ambient' "$STUB_LOG" \
  && bad "an undeclared ambient variable reached the runtime under an override: $(grep 'SNEAKY_SECRET\|sk-ambient' "$STUB_LOG")" \
  || ok "an undeclared ambient variable stays out under an override too"
case "$runline $execline" in
  *"--env ANTHROPIC_API_KEY"*) bad "a denied key reached the guest env line -- got: run=$runline exec=$execline" ;;
  *) ok "a denied key never reaches the guest env line of a run" ;;
esac
grep '^argv:' "$STUB_LOG" | grep -q 'sk-metered' \
  && bad "a denied key's value reached argv under BRIG_ENV_ARGV=1" \
  || ok "a denied key's value never reaches argv, even under BRIG_ENV_ARGV=1"
# The hatch put gh-secret on this run's argv. Emptying the log keeps a later
# case that greps it for credential values from tripping on that.
: > "$STUB_LOG"
"$WORK/brig" rm --all -y > /dev/null 2>&1

echo "== unresolved reference =="
out="$(GH_TOKEN='op://vault/item/field' "$WORK/brig" info claude 2>&1)"
case "$out" in
  *"unresolved secret reference"*) ok "a secret-manager reference is not forwarded" ;;
  *) bad "a secret-manager reference is not forwarded -- got: $out" ;;
esac

echo "== removed settings =="
# BRIG_CREDENTIALS_CMD named a command brig ran on every boot to read the host
# credential. It is removed, and a run that still sets it fails here rather
# than booting a sandbox without the login its user believes they configured --
# a failure that would otherwise surface as the guest asking them to log in.
out="$(BRIG_CREDENTIALS_CMD='printf {}' "$WORK/brig" info claude 2>&1)"; rc=$?
[ "$rc" != 0 ] && ok "a run still setting BRIG_CREDENTIALS_CMD fails" \
  || bad "a run still setting BRIG_CREDENTIALS_CMD started anyway"
case "$out" in
  *"brig secret import claude-code <name> --from-command"*)
    ok "the failure names the import that replaces it" ;;
  *) bad "the failure names the import that replaces it -- got: $out" ;;
esac

echo "== info =="
# The full report: the envelope, then everything env has always printed.
"$WORK/brig" info claude > "$WORK/info.out" 2> "$WORK/info.err"
grep -q '^SANDBOX .*brig-claude-code' "$WORK/info.out" \
  && ok "info prints the envelope" || bad "info prints the envelope: $(cat "$WORK/info.out")"
grep -q 'forwarding' "$WORK/info.out" \
  && ok "info prints the full report too" || bad "info prints the full report: $(cat "$WORK/info.out")"
grep -q 'is now `brig info`' "$WORK/info.err" \
  && bad "info printed a deprecation line about itself" || ok "info is not deprecated"

# info is the preview of what a run is about to trust, so the envelope it shows
# has to be the envelope the run prints. Compare the rows themselves rather than
# one grep each: a row that drifts between the preview and the run makes the
# preview a claim about a boundary nobody is going to use.
"$WORK/brig" --verbose run claude -d > "$WORK/runenv.out" 2>&1
"$WORK/brig" rm --all -y > /dev/null 2>&1
envelope_rows() { grep -E '^(SESSION|PROFILE|SANDBOX|ISOLATION|WORKSPACE|IMAGE|VERIFY|CREDENTIALS) ' "$1"; }
envelope_rows "$WORK/info.out" > "$WORK/rows.info"
envelope_rows "$WORK/runenv.out" > "$WORK/rows.run"
[ -s "$WORK/rows.info" ] \
  && ok "the info envelope has rows to compare" \
  || bad "the info envelope has rows to compare: $(cat "$WORK/info.out")"
if diff -q "$WORK/rows.info" "$WORK/rows.run" > /dev/null 2>&1; then
  ok "info shows the same envelope the run prints"
else
  bad "info and run disagree about the envelope:
$(diff "$WORK/rows.info" "$WORK/rows.run")"
fi

# env is the old spelling: the same output plus one line naming the new one.
"$WORK/brig" env claude > "$WORK/env.out" 2> "$WORK/env.err"
grep -q 'is now `brig info`' "$WORK/env.err" \
  && ok "env names the new spelling" || bad "env names the new spelling: $(cat "$WORK/env.err")"
grep -q '^SANDBOX .*brig-claude-code' "$WORK/env.out" \
  && ok "env still prints the report" || bad "env still prints the report: $(cat "$WORK/env.out")"

echo "== plan =="
# The permission view, read without opening a secret. It boots nothing, so the
# stub sees no run and no exec, and the values in the shell appear nowhere.
: > "$STUB_LOG"
out="$(CLAUDE_CODE_OAUTH_TOKEN=env-token-secret GH_TOKEN=gh-secret \
  "$WORK/brig" plan claude 2>&1)"; rc=$?
[ "$rc" = 0 ] && ok "plan exits 0" || bad "plan exits 0 -- got $rc: $out"
case "$out" in
  *"GH_TOKEN (env) from the environment"*) ok "plan names a credential and its delivery" ;;
  *) bad "plan names a credential and its delivery -- got: $out" ;;
esac
case "$out" in
  *env-token-secret*|*gh-secret*) bad "plan printed a credential VALUE" ;;
  *) ok "plan prints names, never values" ;;
esac
grep -Eq '^argv: (run|exec) ' "$STUB_LOG" \
  && bad "plan booted or entered a sandbox: $(cat "$STUB_LOG")" \
  || ok "plan boots nothing"
d1="$("$WORK/brig" --json plan claude 2>/dev/null | grep '"digest"')"
d2="$("$WORK/brig" plan claude --json 2>/dev/null | grep '"digest"')"
[ -n "$d1" ] && [ "$d1" = "$d2" ] \
  && ok "the plan digest is stable" || bad "the plan digest is stable -- got '$d1' and '$d2'"
d3="$("$WORK/brig" --json plan claude --mem 1234 2>/dev/null | grep '"digest"')"
[ -n "$d3" ] && [ "$d3" != "$d1" ] \
  && ok "the plan digest moves with the limits" || bad "the plan digest moves with the limits -- got '$d3'"
# A run brig refuses: the ubuntu profile asks for a network of its own, and vz
# cannot give one. BRIG_NETWORK is unset for these two, because this script
# exports shared, and shared is the way past the refusal. The plan says the
# run is refused and still exits 0, and the run then refuses with the same
# sentence and boots nothing.
: > "$STUB_LOG"
out="$(env -u BRIG_NETWORK BRIG_HYPERVISOR=vz "$WORK/brig" --json plan ubuntu 2>&1)"; rc=$?
[ "$rc" = 0 ] && ok "plan of a refused run exits 0" || bad "plan of a refused run exits 0 -- got $rc: $out"
case "$out" in
  *'"refused"'*'"reason": "the ubuntu profile sets network: isolated, and the vz backend cannot'*)
    ok "plan says the run is refused" ;;
  *) bad "plan says the run is refused -- got: $out" ;;
esac
out="$(env -u BRIG_NETWORK BRIG_HYPERVISOR=vz "$WORK/brig" run ubuntu -d 2>&1)"; rc=$?
case "$rc:$out" in
  0:*) bad "the run the plan called refused was not refused -- got: $out" ;;
  *"the ubuntu profile sets network: isolated, and the vz backend cannot"*)
    ok "the run refuses what the plan said it would" ;;
  *) bad "the run refuses what the plan said it would -- got $rc: $out" ;;
esac
grep -Eq '^argv: run ' "$STUB_LOG" \
  && bad "a refused run booted: $(cat "$STUB_LOG")" || ok "the refused run boots nothing"

echo "== named session =="
: > "$STUB_LOG"
"$WORK/brig" run claude --name 'My Big Refactor' -p hi > /dev/null 2>&1
grep -q -- "--shared-dir $WS-my-big-refactor:$GUEST_HOME" "$STUB_LOG" \
  && ok "a named session gets its own workspace" || bad "named session gets its own workspace"
grep -q -- '--name brig-claude-code-my-big-refactor' "$STUB_LOG" \
  && ok "a named session gets its own sandbox" || bad "named session gets its own sandbox"
grep -q -- "-- claude --name My Big Refactor -p hi" "$STUB_LOG" \
  && ok "the raw name reaches the agent" || bad "the raw name reaches the agent"

echo "== session collision =="
# Two names whose slugs land on one sandbox must not share it: brig used to drop
# the second into the first's home with its own credentials and only warn. Now
# the second is refused. See issue #26.
#
# The collision is sanitisation, which is the class that survives a slug no
# longer being cut to ten characters: the space and the case both go, so these
# two names are one slug however long they are.
"$WORK/brig" rm --all -y > /dev/null 2>&1
"$WORK/brig" run claude --name acme-corp-prod -d > /dev/null 2>&1
out="$("$WORK/brig" run claude --name 'Acme Corp Prod' -d 2>&1)"; rc=$?
[ "$rc" != 0 ] && ok "a colliding session name is refused" \
  || bad "a colliding session name started anyway -- got rc $rc"
case "$out" in
  *acme-corp-prod*) ok "the refusal names the session already holding the sandbox" ;;
  *) bad "the refusal names the other session -- got: $out" ;;
esac
# The same name returning is the ordinary repeat run, not a collision.
"$WORK/brig" run claude --name acme-corp-prod -d > /dev/null 2>&1 \
  && ok "the owning name runs again without complaint" \
  || bad "the owning name was refused its own sandbox"
# rm releases the claim, so the name that was refused can take the sandbox once
# the first is gone.
"$WORK/brig" rm claude --name acme-corp-prod > /dev/null 2>&1
"$WORK/brig" run claude --name 'Acme Corp Prod' -d > /dev/null 2>&1 \
  && ok "rm frees the sandbox for the other name" \
  || bad "the sandbox stayed claimed after rm"

# The claims an older release wrote under sessions.json are carried across to
# slug-claims.json rather than dropped: the two files have the same shape, so
# there is nothing to guess, and dropping them would leave the refusal off
# until every session had run again. Unlike the session index, whose keys
# cannot be read back into a ref at all.
"$WORK/brig" rm --all -y > /dev/null 2>&1
rm -f "$BRIG_STATE_DIR/slug-claims.json" "$BRIG_STATE_DIR/sessions.json"
printf '{"brig-claude-code-acme-corp-prod": "acme-corp-prod"}' > "$BRIG_STATE_DIR/sessions.json"
out="$("$WORK/brig" run claude --name 'Acme Corp Prod' -d 2>&1)"
case "$out" in
  *acme-corp-prod*) ok "a claim written under the old file name still refuses" ;;
  *) bad "the old claim was dropped -- got: $out" ;;
esac
grep -q 'acme-corp-prod' "$BRIG_STATE_DIR/slug-claims.json" \
  && ok "the old claims are carried over to slug-claims.json" \
  || bad "the old claims are not in slug-claims.json"

# An unnamed run carries them across too. It claims nothing itself, so it could
# have skipped the claim index entirely -- and then the rememberSession later in
# that same boot would have replaced the file the claims were still sitting in,
# which makes a plain `brig run claude` the one command that silently destroys
# them.
"$WORK/brig" rm --all -y > /dev/null 2>&1
rm -f "$BRIG_STATE_DIR/slug-claims.json" "$BRIG_STATE_DIR/sessions.json"
printf '{"brig-claude-code-acme-corp-prod": "acme-corp-prod"}' > "$BRIG_STATE_DIR/sessions.json"
"$WORK/brig" run claude -d > /dev/null 2>&1
grep -q 'acme-corp-prod' "$BRIG_STATE_DIR/slug-claims.json" \
  && ok "an unnamed run carries the old claims across" \
  || bad "an unnamed run dropped the old claims"

# And claims nothing of its own, which is the other half of that pair: every
# unnamed run of a profile is meant to be the one session, so there is no name
# for a later one to collide with and nothing to write down.
"$WORK/brig" rm --all -y > /dev/null 2>&1
rm -f "$BRIG_STATE_DIR/slug-claims.json" "$BRIG_STATE_DIR/sessions.json"
"$WORK/brig" run claude -d > /dev/null 2>&1
[ -e "$BRIG_STATE_DIR/slug-claims.json" ] \
  && bad "an unnamed run claimed a sandbox -- got: $(cat "$BRIG_STATE_DIR/slug-claims.json")" \
  || ok "an unnamed run claims nothing"

# The guard on all of that: a current sessions.json holds session entries,
# whose values are objects rather than the claims' strings, so it cannot be
# read as claims. Mistaking one for the other deletes the file, and every
# session in it loses its home.
"$WORK/brig" rm --all -y > /dev/null 2>&1
rm -f "$BRIG_STATE_DIR/slug-claims.json" "$BRIG_STATE_DIR/sessions.json"
"$WORK/brig" run claude --name rc23guard -d > /dev/null 2>&1
if [ -s "$BRIG_STATE_DIR/sessions.json" ]; then
  cp "$BRIG_STATE_DIR/sessions.json" "$WORK/sessions.before"
  # Cleared after the session exists, not before: a named run claims its own
  # slug, so the file it writes is the very thing that would stop the migration
  # from being attempted. Absent is the condition the migration waits for.
  rm -f "$BRIG_STATE_DIR/slug-claims.json"
  # rm of some other session is the instrument: it reads the claim index, which
  # is where the migration lives, and writes neither index -- so anything that
  # changes below changed because the migration fired.
  "$WORK/brig" rm claude --name rc23other > /dev/null 2>&1
  cmp -s "$WORK/sessions.before" "$BRIG_STATE_DIR/sessions.json" \
    && ok "a current session index is not mistaken for claims" \
    || bad "the session index was rewritten by a migration that must not fire"
  [ -e "$BRIG_STATE_DIR/slug-claims.json" ] \
    && bad "claims were invented out of session entries -- got: $(tr -d '\n' < "$BRIG_STATE_DIR/slug-claims.json")" \
    || ok "no claims are invented out of session entries"
else
  bad "no session index to guard: $(cat "$BRIG_STATE_DIR/sessions.json" 2>&1)"
fi
"$WORK/brig" rm --all -y > /dev/null 2>&1

echo "== the slug migration =="
# A --name longer than ten characters used to be cut to that, so it had a
# shorter sandbox and a shorter home than it gets now. Both come off the slug on
# every run, so such a session boots a new pair and the old one is orphaned: the
# work in the old workspace is on the host and is still there, state inside the
# old guest is not. Say so, and name both directories so the old one can be
# moved or deleted.
"$WORK/brig" rm --all -y > /dev/null 2>&1
mkdir -p "$WS-refactorin"
"$WORK/brig" run claude --name refactoring -d > /dev/null 2> "$WORK/moved.err"
grep -q -- "nothing reads the old ones now: $WS-refactorin (brig-claude-code-refactorin)" "$WORK/moved.err" \
  && ok "the migration notice names the home that was left behind" \
  || bad "the notice does not name the old home -- got: $(cat "$WORK/moved.err")"
grep -q -- "its new home and sandbox: $WS-refactoring (brig-claude-code-refactoring)" "$WORK/moved.err" \
  && ok "the migration notice names the home in use now" \
  || bad "the notice does not name the new home -- got: $(cat "$WORK/moved.err")"

# Nothing orphaned, nothing said. This name slugs differently than it used to,
# but no directory was ever created under the old slug, so there is nothing for
# the reader to move -- and a notice that reaches people it does not apply to is
# one they learn to skip.
"$WORK/brig" rm --all -y > /dev/null 2>&1
rm -rf "$WS-benchmarkin"
"$WORK/brig" run claude --name benchmarking -d > /dev/null 2> "$WORK/fresh.err"
grep -q 'used to be shortened' "$WORK/fresh.err" \
  && bad "a session with nothing orphaned was warned -- got: $(cat "$WORK/fresh.err")" \
  || ok "a session with nothing orphaned is not warned"

# And a name the old budget left alone slugs exactly as it always did, so its
# directory being there means nothing. Ten characters is the boundary, and this
# is on the silent side of it.
"$WORK/brig" rm --all -y > /dev/null 2>&1
mkdir -p "$WS-exactlyten"
"$WORK/brig" run claude --name exactlyten -d > /dev/null 2> "$WORK/short.err"
grep -q 'used to be shortened' "$WORK/short.err" \
  && bad "a name that was never cut was warned -- got: $(cat "$WORK/short.err")" \
  || ok "a name the old budget left alone is not warned"

# The ref form refused any label over ten characters. A long one is a sandbox
# and a directory of its own now, and is refused only for what is in it.
"$WORK/brig" rm --all -y > /dev/null 2>&1
: > "$STUB_LOG"
"$WORK/brig" run 'claude@a-long-refactor-label' -d > /dev/null 2>&1
grep -q -- '--name brig-claude-code-a-long-refactor-label' "$STUB_LOG" \
  && ok "a long label gets a sandbox of its own" \
  || bad "a long label did not reach the sandbox name"
out="$("$WORK/brig" run 'claude@A-Long-Refactor-Label' -d 2>&1)"
case "$out" in
  *a-long-refactor-label*) ok "a long label brig would rewrite is still refused" ;;
  *) bad "a long label brig would rewrite was not refused -- got: $out" ;;
esac
"$WORK/brig" rm --all -y > /dev/null 2>&1

echo "== stop =="
: > "$STUB_LOG"
"$WORK/brig" stop claude > /dev/null 2>&1
grep -q '^argv: stop brig-claude-code' "$STUB_LOG" \
  && ok "stop stops the sandbox" || bad "stop stops the sandbox"
# Stopping must not resolve credentials: on a real host that raises a
# keychain prompt for a command that needs none.
grep -q 'CLAUDE_CODE_OAUTH_TOKEN' "$STUB_LOG" \
  && bad "stop resolved credentials" || ok "stop resolves no credentials"

echo "== logs =="
# brig hands the runtime the log request built from the ref: the sandbox name
# the ref resolves to, and the flags the line carried. The stub logs its argv
# like every other verb, so this asserts what reached the runtime. --tail -1 is
# the default brig passes, which both runtimes read as "all".
#
# A sandbox has to exist for there to be a log to read: logs asks the runtime's
# list first and is a not-found otherwise, which the case below the lifecycle
# section pins. The stop above left none, so boot one here.
"$WORK/brig" run claude -d > /dev/null 2>&1
: > "$STUB_LOG"
"$WORK/brig" logs claude > /dev/null 2>&1
grep -q '^argv: logs --tail -1 brig-claude-code' "$STUB_LOG" \
  && ok "logs streams the sandbox log through the runtime" \
  || bad "logs streams the sandbox log -- got: $(grep '^argv:' "$STUB_LOG")"
: > "$STUB_LOG"
"$WORK/brig" logs claude --follow > /dev/null 2>&1
grep -q '^argv: logs --follow --tail -1 brig-claude-code' "$STUB_LOG" \
  && ok "logs --follow passes --follow to the runtime" \
  || bad "logs --follow passes --follow -- got: $(grep '^argv:' "$STUB_LOG")"

echo "== lifecycle verbs =="
: > "$STUB_LOG"
out="$("$WORK/brig" --verbose run claude -d 2>"$WORK/detach.err")"
[ "$out" = brig-claude-code ] && ok "run -d prints the sandbox name" \
  || bad "run -d prints the sandbox name -- got '$out'"
# The envelope is on stderr, so the scriptable name on stdout stays clean.
grep -q '^SANDBOX .*brig-claude-code' "$WORK/detach.err" \
  && ok "run -d prints the execution envelope" \
  || bad "run -d prints the execution envelope: $(cat "$WORK/detach.err")"
grep -q -- '-- claude' "$STUB_LOG" \
  && bad "run -d started the agent" || ok "run -d starts the sandbox, not the agent"

# sh attaches to a sandbox whose envelope the user already saw, so it does not
# repeat it. The sandbox started just above is still running.
"$WORK/brig" sh claude -- true > /dev/null 2>"$WORK/sh.err"
grep -q '^SANDBOX ' "$WORK/sh.err" \
  && bad "sh printed the envelope" || ok "sh does not print the envelope"

listing="$("$WORK/brig" ls 2>/dev/null)"
case "$listing" in
  *brig-claude-code*running*) ok "ls shows a running sandbox" ;;
  *) bad "ls shows a running sandbox -- got: $listing" ;;
esac
case "$listing" in
  *"$WS"*) ok "ls names the workspace" ;;
  *) bad "ls names the workspace -- got: $listing" ;;
esac

# The listing leads with the ref, which is the identifier every other verb
# takes. The sandbox's own name is not one, and printing only that is the fault
# this was filed as: a reader who copied what they saw got "unknown profile".
case "$listing" in
  REF*SANDBOX*STATE*WORKSPACE*) ok "ls heads the listing with the ref" ;;
  *) bad "ls heads the listing with the ref -- got: $listing" ;;
esac
"$WORK/brig" ls -q > "$WORK/refs.out" 2>&1
[ "$(cat "$WORK/refs.out")" = claude-code ] \
  && ok "ls -q prints the ref and nothing else" \
  || bad "ls -q prints the ref and nothing else -- got: $(cat "$WORK/refs.out")"

# `brig --json ls` is the machine-readable listing: it parses, and it carries
# the SandboxList kind of the shared JSON envelope. The global position is the
# one the help text teaches, so it is the one the smoke asserts.
"$WORK/brig" --json ls > "$WORK/ls.json" 2>/dev/null
if command -v python3 >/dev/null 2>&1; then
  python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$WORK/ls.json" 2>/dev/null \
    && ok "brig --json ls is valid JSON" \
    || bad "brig --json ls is not valid JSON -- got: $(cat "$WORK/ls.json")"
fi
case "$(cat "$WORK/ls.json")" in
  *'"kind": "SandboxList"'*) ok "brig --json ls carries kind SandboxList" ;;
  *) bad "brig --json ls carries kind SandboxList -- got: $(cat "$WORK/ls.json")" ;;
esac

# The round trip, against the real binary: every ref the listing prints is a
# word every verb takes.
#
# "Takes" is the absence of a refusal rather than a successful run, and the exit
# codes are what say which: 2 is a usage error and 3 is a name that resolves to
# nothing, so those two are brig declining the operand. Anything else is the verb
# having gone on to do its work, which the cases around this one assert. Read
# that way the loop does not depend on the host's credentials, which is what
# makes it a check about the grammar.
takes_ref() {
  local what="$1"; shift
  "$WORK/brig" "$@" > "$WORK/rt.out" 2>&1
  local rc=$?
  case "$rc" in
    2|3) bad "$what refused a ref that ls -q printed -- rc $rc: $(cat "$WORK/rt.out")" ;;
    *)   ok "$what takes a ref that ls -q printed" ;;
  esac
}
while read -r ref; do
  takes_ref "brig run"   run "$ref" -d
  takes_ref "brig sh"    sh "$ref" -- true
  takes_ref "brig info"  info "$ref"
  takes_ref "brig logs"  logs "$ref"
  takes_ref "brig stop"  stop "$ref"
  takes_ref "brig rm"    rm "$ref"
  # And the verbless form, which is taught nowhere and accepted anyway. Last,
  # because it is what puts the sandbox back for the cases below.
  takes_ref "brig <ref>" "$ref" -d
done < "$WORK/refs.out"

# A stopped sandbox still holds its name, which is the thing worth seeing.
"$WORK/brig" stop claude > /dev/null 2>&1
listing="$("$WORK/brig" ls 2>/dev/null)"
case "$listing" in
  *brig-claude-code*stopped*) ok "ls shows a stopped sandbox too" ;;
  *) bad "ls shows a stopped sandbox too -- got: $listing" ;;
esac

: > "$STUB_LOG"
out="$("$WORK/brig" rm claude 2>&1)"
grep -q '^argv: rm brig-claude-code' "$STUB_LOG" \
  && ok "rm removes the sandbox" || bad "rm removes the sandbox"
[ -d "$WS" ] && ok "rm leaves the workspace alone" || bad "rm deleted the workspace"
case "$out" in
  *"guest home $WS stays on the host"*) ok "rm says where the workspace still is" ;;
  *) bad "rm says where the workspace still is -- got: $out" ;;
esac

# A sandbox for `rm --all` to remove.
"$WORK/brig" run claude -d > /dev/null 2>&1

: > "$STUB_LOG"
"$WORK/brig" rm --all -y > /dev/null 2>&1
grep -q '^argv: rm brig-claude-code' "$STUB_LOG" \
  && ok "rm --all removes brig sandboxes" || bad "rm --all removes brig sandboxes"

echo "== ephemeral guest home =="
# Every other case runs with BRIG_WORKSPACE set. With it unset, brig creates the
# guest home under its state directory, says so once, and rm deletes it. HOME
# is scratch too: an older release's ~/brig/claude-code on the machine running
# this would otherwise be picked up as the session's home.
EPH="$BRIG_STATE_DIR/homes/brig-claude-code"
mkdir -p "$WORK/home"
out="$(env -u BRIG_WORKSPACE HOME="$WORK/home" "$WORK/brig" run claude -d 2>&1)"
case "$out" in
  *"the guest home of claude-code is temporary"*) ok "a run with no --home says its home is ephemeral" ;;
  *) bad "a run with no --home says its home is ephemeral -- got: $out" ;;
esac
[ -d "$EPH" ] && ok "a run with no --home creates its home under the state dir" \
  || bad "a run with no --home creates its home under the state dir"
out="$(env -u BRIG_WORKSPACE HOME="$WORK/home" "$WORK/brig" rm claude 2>&1)"
case "$out" in
  *"removed claude and its guest home $EPH"*) ok "rm names the ephemeral home it deleted" ;;
  *) bad "rm names the ephemeral home it deleted -- got: $out" ;;
esac
[ ! -e "$EPH" ] && ok "rm deletes the ephemeral home" || bad "rm left the ephemeral home at $EPH"
[ -d "$WS" ] && ok "rm leaves a named workspace alone" || bad "rm deleted the named workspace"

# A first boot that fails records no session, so rm finds nothing to remove.
# The home brig created for it goes with the failure. A --home it created stays.
env -u BRIG_WORKSPACE HOME="$WORK/home" STUB_RUN_FAIL=1 \
  "$WORK/brig" run claude -d > "$WORK/failboot.out" 2>&1 \
  && bad "a boot the runtime refused exited 0"
[ ! -e "$EPH" ] && ok "a failed first boot deletes the home it created" \
  || bad "a failed first boot left its home at $EPH: $(cat "$WORK/failboot.out")"
env -u BRIG_WORKSPACE HOME="$WORK/home" STUB_RUN_FAIL=1 \
  "$WORK/brig" run claude --home "$WORK/failhome" -d > /dev/null 2>&1
[ -f "$WORK/failhome/.brig-workspace" ] && ok "a failed boot keeps a --home it created" \
  || bad "a failed boot deleted the --home $WORK/failhome"

echo "== rm and logs with no sandbox =="
# Nothing holds the name now, so rm and logs name a sandbox that resolves to
# nothing: exit 3, the code the table gives a name that is not there, and a
# message naming the ref rather than the runtime's own "instance not found".
# They used to hand that error back and exit 1.
out="$("$WORK/brig" rm claude 2>&1)"; rc=$?
[ "$rc" = 3 ] && ok "rm of a missing sandbox exits 3" \
  || bad "rm of a missing sandbox exits 3 -- got $rc: $out"
case "$out" in
  *"no sandbox for claude"*) ok "rm names the ref, not the sandbox" ;;
  *) bad "rm names the ref -- got: $out" ;;
esac
out="$("$WORK/brig" logs claude 2>&1)"; rc=$?
[ "$rc" = 3 ] && ok "logs of a missing sandbox exits 3" \
  || bad "logs of a missing sandbox exits 3 -- got $rc: $out"
case "$out" in
  *"no sandbox for claude"*) ok "logs names the ref, not the sandbox" ;;
  *) bad "logs names the ref -- got: $out" ;;
esac

echo "== rm of a sandbox the runtime lost =="
# A sandbox removed behind brig's back still has its session. rm finds no
# sandbox, exits 3, and forgets the session the way ls would. The next run is
# a new sandbox on the default network, not the old posture, and is not
# refused for want of a network record.
mkdir -p "$BRIG_PROFILE_DIR"
cat > "$BRIG_PROFILE_DIR/vz-lost.yaml" <<'YAML'
name: vz-lost
image: smoke/image:latest
binary: sh
guestHome: /root
mem: 256
cpus: 1
hypervisor: vz
YAML
env -u BRIG_NETWORK "$WORK/brig" run vz-lost --network offline -d > "$WORK/lost.out" 2>&1 \
  || bad "the sandbox to lose did not boot: $(cat "$WORK/lost.out")"
rm -f "$STUB_STATE" "$STUB_STATE.stopped"
out="$("$WORK/brig" rm vz-lost 2>&1)"; rc=$?
[ "$rc" = 3 ] && ok "rm of a lost sandbox exits 3" \
  || bad "rm of a lost sandbox exits 3 -- got $rc: $out"
case "$out" in
  *"the runtime reported none, so brig forgot its session"*) ok "rm says it forgot the lost sandbox's session" ;;
  *) bad "rm says it forgot the lost sandbox's session -- got: $out" ;;
esac
: > "$STUB_LOG"
if env -u BRIG_NETWORK "$WORK/brig" --verbose run vz-lost -d > "$WORK/lost.out" 2>&1; then
  ok "a flagless run after rm of a lost sandbox boots"
else
  bad "a flagless run after rm of a lost sandbox was refused: $(cat "$WORK/lost.out")"
fi
grep -q '^NETWORK .*shared' "$WORK/lost.out" && grep -q -- '--net shared' "$STUB_LOG" \
  && ok "and it takes the default network, not the lost sandbox's offline" \
  || bad "the run after rm kept the lost sandbox's posture: $(grep '^NETWORK' "$WORK/lost.out")"
"$WORK/brig" rm vz-lost > /dev/null 2>&1
rm -f "$BRIG_PROFILE_DIR/vz-lost.yaml"

echo "== remembered workspace =="
# A session created with -w used to be restarted by the next verb that left the
# flag off: the workspace resolved back to the default, the running sandbox was
# mounting something else, and that read as a stale share. The workspace
# survives a restart because it is on the host; the guest's memory-only state
# does not, an in-sandbox login included.
#
# BRIG_WORKSPACE is removed from the environment for these, because it is a
# setting that names a directory and this is about the case where nothing does.
RC="$WORK/ws-rc23"
brig_bare() { env -u BRIG_WORKSPACE "$WORK/brig" "$@"; }
brig_bare run claude --name rc23 -w "$RC" -d > /dev/null 2>&1
: > "$STUB_LOG"
brig_bare sh claude --name rc23 -- uname -a > /dev/null 2>&1
grep -q '^argv: run ' "$STUB_LOG" \
  && bad "exec without -w restarted the session" \
  || ok "exec without -w finds the workspace the session was created with"

# The listing says what the sandbox is mounting, so the remembered directory
# wins there over the one the sandbox name would derive -- and over the
# BRIG_WORKSPACE this shell is carrying, which names neither.
listing="$("$WORK/brig" ls 2>/dev/null)"
case "$listing" in
  *"$RC-rc23"*) ok "ls names the workspace the session was created with" ;;
  *) bad "ls names the workspace the session was created with -- got: $listing" ;;
esac

# An explicit directory is a request rather than a guess, so it still wins and
# still restarts: a share cannot be moved on a live guest.
: > "$STUB_LOG"
brig_bare sh claude --name rc23 -w "$WORK/ws-other" -- uname -a > /dev/null 2>&1
grep -q '^argv: run ' "$STUB_LOG" \
  && ok "a -w naming a different directory still restarts the sandbox" \
  || bad "a -w naming a different directory still restarts the sandbox"

# The index is keyed by the ref, so a session started under one spelling of the
# profile is the entry the other spelling reads -- claude and claude-code are
# one profile through an alias, and two entries would be two homes drifting
# apart.
grep -q '"claude-code@rc23"' "$BRIG_STATE_DIR/sessions.json" \
  && ok "the session is filed under its ref" \
  || bad "the session is not filed under claude-code@rc23 -- got: $(cat "$BRIG_STATE_DIR/sessions.json" 2>&1)"
: > "$STUB_LOG"
brig_bare sh claude-code --name rc23 -- uname -a > /dev/null 2>&1
grep -q '^argv: run ' "$STUB_LOG" \
  && bad "the alias did not find the session's workspace" \
  || ok "the alias finds the workspace the session was created with"

brig_bare rm claude --name rc23 > /dev/null 2>&1
grep -q 'brig-claude-code-rc23' "$BRIG_STATE_DIR/sessions.json" \
  && bad "rm left the sandbox in the session index" \
  || ok "rm drops the remembered workspace"

brig_bare run claude --name rc23 -w "$RC" -d > /dev/null 2>&1
"$WORK/brig" rm --all -y > /dev/null 2>&1
grep -q 'brig-claude-code-rc23' "$BRIG_STATE_DIR/sessions.json" \
  && bad "reset left a sandbox in the session index" \
  || ok "reset drops the remembered workspaces"

# A session with no label is filed under the bare agent name -- the ref's own
# spelling for a session that has no label, rather than a trailing '@' or an
# invented default one, either of which would be a key no ref types.
rm -f "$BRIG_STATE_DIR/sessions.json"
brig_bare run claude -w "$WORK/ws-bare" -d > /dev/null 2>&1
grep -q '"claude-code":' "$BRIG_STATE_DIR/sessions.json" \
  && ok "an unlabelled session is filed under the bare agent name" \
  || bad "the unlabelled session is not keyed claude-code -- got: $(cat "$BRIG_STATE_DIR/sessions.json" 2>&1)"
grep -q 'claude-code@' "$BRIG_STATE_DIR/sessions.json" \
  && bad "the unlabelled session was given a label -- got: $(cat "$BRIG_STATE_DIR/sessions.json")" \
  || ok "the unlabelled session is given no label"
"$WORK/brig" rm --all -y > /dev/null 2>&1

# And the '@' form on the command line reaches the file: a session created as
# claude@label is filed exactly as --name label files it, key and value both,
# so the two spellings address one entry rather than two.
rm -f "$BRIG_STATE_DIR/sessions.json"
brig_bare run claude@rc23ref -w "$WORK/ws-ref" -d > /dev/null 2>&1
grep -q '"claude-code@rc23ref":' "$BRIG_STATE_DIR/sessions.json" \
  && ok "a session created as a ref is filed under that ref" \
  || bad "the ref form is not keyed claude-code@rc23ref -- got: $(cat "$BRIG_STATE_DIR/sessions.json" 2>&1)"
grep -q '"sandbox": "brig-claude-code-rc23ref"' "$BRIG_STATE_DIR/sessions.json" \
  && ok "the ref form records the sandbox --name would have named" \
  || bad "the ref form's sandbox is not brig-claude-code-rc23ref -- got: $(cat "$BRIG_STATE_DIR/sessions.json")"
"$WORK/brig" rm --all -y > /dev/null 2>&1

# The sandbox-keyed file the session index replaces is deleted rather than
# migrated: its keys cannot be read back into a ref without guessing which dash
# separated the agent from its label. One restart per session in it, which is
# what an absent entry has always cost.
printf '{"brig-claude-code-rc23": "%s"}' "$WORK/ws-legacy" > "$BRIG_STATE_DIR/workspaces.json"
brig_bare run claude --name rc23 -w "$RC" -d > /dev/null 2>&1
[ -e "$BRIG_STATE_DIR/workspaces.json" ] \
  && bad "the old sandbox-keyed index was left behind" \
  || ok "the old sandbox-keyed index is deleted on sight"
"$WORK/brig" rm --all -y > /dev/null 2>&1

# The index is bookkeeping, so an unusable one costs a restart and nothing
# more: every command still works, and the workspace resolves as it did before
# the file existed.
printf '{not json at all' > "$BRIG_STATE_DIR/sessions.json"
brig_bare run claude -d > "$WORK/corrupt.out" 2>&1
rc=$?
[ "$rc" = 0 ] && ok "a corrupt index is ignored rather than fatal" \
  || bad "a corrupt index failed the run -- got: $(cat "$WORK/corrupt.out")"
"$WORK/brig" rm --all -y > /dev/null 2>&1

echo "== argument hygiene =="
# `rm --all` removes every brig sandbox, so it says what it is about to remove
# and asks. stdin is /dev/null here: with nobody to answer it refuses (exit 1),
# names -y, and removes nothing, rather than assuming yes.
"$WORK/brig" run claude -d > /dev/null 2>&1
: > "$STUB_LOG"
out="$("$WORK/brig" rm --all 2>&1)"; rc=$?
[ "$rc" = 1 ] && ok "rm --all with no terminal exits 1" || bad "rm --all with no terminal exits 1 -- got $rc: $out"
case "$out" in
  *-y*) ok "rm --all with no terminal names -y" ;;
  *) bad "rm --all with no terminal names -y -- got: $out" ;;
esac
grep -q '^argv: rm ' "$STUB_LOG" \
  && bad "rm --all removed a sandbox without an answer" || ok "rm --all removes nothing without an answer"
# --dry-run is the list it would ask about, on stdout, as refs, and exit 0.
"$WORK/brig" rm --all --dry-run > "$WORK/dry.out" 2> "$WORK/dry.err"; rc=$?
[ "$rc" = 0 ] && ok "rm --all --dry-run exits 0" || bad "rm --all --dry-run exits 0 -- got $rc"
grep -q '^claude-code ' "$WORK/dry.out" \
  && ok "rm --all --dry-run lists the ref" || bad "rm --all --dry-run lists the ref -- got: $(cat "$WORK/dry.out")"
grep -q 'named with --home and projects stay on the host' "$WORK/dry.err" \
  && ok "rm --all --dry-run says workspaces stay" || bad "rm --all --dry-run says workspaces stay -- got: $(cat "$WORK/dry.err")"
grep -q '^argv: rm ' "$STUB_LOG" \
  && bad "rm --all --dry-run removed a sandbox" || ok "rm --all --dry-run removes nothing"
# The same preview for one sandbox.
out="$("$WORK/brig" rm claude --dry-run 2>&1)"; rc=$?
[ "$rc" = 0 ] && ok "rm claude --dry-run exits 0" || bad "rm claude --dry-run exits 0 -- got $rc: $out"
case "$out" in
  *"would remove claude"*"$WS"*) ok "rm claude --dry-run names the ref and the workspace" ;;
  *) bad "rm claude --dry-run names the ref and the workspace -- got: $out" ;;
esac
grep -q '^argv: rm ' "$STUB_LOG" \
  && bad "rm claude --dry-run removed the sandbox" || ok "rm claude --dry-run removes nothing"
# -y answers a question `rm <ref>` does not ask, so it is refused by name.
out="$("$WORK/brig" rm claude -y 2>&1)"; rc=$?
[ "$rc" = 2 ] && ok "rm claude -y exits 2" || bad "rm claude -y exits 2 -- got $rc: $out"
# A flag brig does not know must not be read past and ignored on the command
# that removes everything: refused (exit 2), named, and nothing removed.
out="$("$WORK/brig" rm --all --nope 2>&1)"; rc=$?
[ "$rc" = 2 ] && ok "rm --all --nope exits 2" || bad "rm --all --nope exits 2 -- got $rc: $out"
case "$out" in
  *--nope*) ok "rm --all --nope names the token" ;;
  *) bad "rm --all --nope names the token -- got: $out" ;;
esac
grep -q '^argv: rm ' "$STUB_LOG" \
  && bad "rm --all --nope removed a sandbox" || ok "rm --all --nope removes nothing"
# A ref beside --all is two requests on one line, so it is refused rather than
# resolved to either of them.
out="$("$WORK/brig" rm claude --all 2>&1)"; rc=$?
[ "$rc" = 2 ] && ok "rm claude --all exits 2" || bad "rm claude --all exits 2 -- got $rc: $out"
case "$out" in
  *claude*) ok "rm claude --all names the ref it will not take" ;;
  *) bad "rm claude --all names the ref -- got: $out" ;;
esac
"$WORK/brig" rm --all -y > /dev/null 2>&1

# An unknown flag to the left of the profile is refused and named, rather than
# consuming the profile and blaming it for being absent.
out="$("$WORK/brig" run --nope claude 2>&1)"; rc=$?
[ "$rc" = 2 ] && ok "run --nope claude exits 2" || bad "run --nope claude exits 2 -- got $rc"
case "$out" in
  *--nope*) ok "run --nope claude names the flag" ;;
  *) bad "run --nope claude names the flag -- got: $out" ;;
esac

# A verb that takes a profile and nothing more refuses a stray argument.
out="$("$WORK/brig" ls claude 2>&1)"; rc=$?
[ "$rc" = 2 ] && ok "ls refuses an argument" || bad "ls refuses an argument -- got $rc: $out"
out="$("$WORK/brig" stop claude extra 2>&1)"; rc=$?
[ "$rc" = 2 ] && ok "stop refuses a stray argument" || bad "stop refuses a stray argument -- got $rc: $out"

echo "== BRIG_NAME =="
"$WORK/brig" rm --all -y > /dev/null 2>&1
# A name set through BRIG_NAME still carries the prefix, so ls lists it and
# `rm --all` removes it the way they do any brig sandbox.
BRIG_NAME=brig-custom "$WORK/brig" run claude -d > /dev/null 2>&1
listing="$("$WORK/brig" ls 2>/dev/null)"
case "$listing" in
  *brig-custom*) ok "a BRIG_NAME sandbox appears in ls" ;;
  *) bad "a BRIG_NAME sandbox appears in ls -- got: $listing" ;;
esac
: > "$STUB_LOG"
"$WORK/brig" rm --all -y > /dev/null 2>&1
grep -q '^argv: rm brig-custom' "$STUB_LOG" \
  && ok "rm --all removes a BRIG_NAME sandbox" || bad "rm --all removes a BRIG_NAME sandbox"
# A BRIG_NAME without the prefix would be invisible to both, so it is refused at
# creation with a message naming the constraint.
out="$(BRIG_NAME=custom "$WORK/brig" run claude -d 2>&1)"; rc=$?
[ "$rc" != 0 ] && ok "a BRIG_NAME without the prefix is refused" \
  || bad "a BRIG_NAME without the prefix is refused -- got $rc"
case "$out" in
  *brig-*) ok "the refusal names the required prefix" ;;
  *) bad "the refusal names the required prefix -- got: $out" ;;
esac

# A sandbox brig cannot name a session for: BRIG_NAME took it off the
# <prefix><agent>-<slug> shape the name would otherwise decompose along, and the
# index entry that knew which session it was carrying is gone. The listing says
# so with a dash rather than guessing, and `ls -q` leaves the row out entirely --
# every line of that output has to be a word a verb takes.
BRIG_NAME=brig-mystery "$WORK/brig" run claude -d > /dev/null 2>&1
rm -f "$BRIG_STATE_DIR/sessions.json"
"$WORK/brig" ls > "$WORK/mystery.out" 2>/dev/null
grep -q '^-  *brig-mystery ' "$WORK/mystery.out" \
  && ok "a sandbox with no ref is listed with a dash" \
  || bad "a sandbox with no ref is listed with a dash -- got: $(cat "$WORK/mystery.out")"
"$WORK/brig" ls -q > "$WORK/mystery-refs.out" 2>/dev/null
[ -s "$WORK/mystery-refs.out" ] \
  && bad "ls -q printed a line for a sandbox with no ref: $(cat "$WORK/mystery-refs.out")" \
  || ok "ls -q leaves out a sandbox with no ref"
"$WORK/brig" rm --all -y > /dev/null 2>&1

echo "== no runtime =="
# With no runtime on PATH, env still reports what is knowable and marks only the
# runtime line unavailable, and ls answers that there is nothing to list. Both
# exit 0: the person most likely to run them is the one whose runtime is broken.
out="$(PATH="" BRIG_RUNTIME_BIN= "$WORK/brig" env claude 2>&1)"; rc=$?
[ "$rc" = 0 ] && ok "env with no runtime exits 0" || bad "env with no runtime exits 0 -- got $rc: $out"
# info is the new spelling of the same report, so it degrades the same way. A
# recommended spelling that failed where the deprecated one worked would send
# the person whose runtime is broken to the wrong command.
out="$(PATH="" BRIG_RUNTIME_BIN= "$WORK/brig" info claude 2>&1)"; rc=$?
[ "$rc" = 0 ] && ok "info with no runtime exits 0" || bad "info with no runtime exits 0 -- got $rc: $out"
# plan is the same kind of preview and answers the same way. Its own variable,
# because the cases below still read the report above.
pout="$(PATH="" BRIG_RUNTIME_BIN= "$WORK/brig" plan claude 2>&1)"; rc=$?
[ "$rc" = 0 ] && ok "plan with no runtime exits 0" || bad "plan with no runtime exits 0 -- got $rc: $pout"
case "$pout" in
  *"RUNTIME"*"unavailable"*) ok "plan marks the runtime unavailable" ;;
  *) bad "plan marks the runtime unavailable -- got: $pout" ;;
esac
case "$out" in
  *"runtime unavailable"*) ok "the envelope marks the runtime unavailable" ;;
  *) bad "the envelope marks the runtime unavailable -- got: $out" ;;
esac
case "$out" in
  *"runtime unavailable"*) ok "env marks the runtime line unavailable" ;;
  *) bad "env marks the runtime line unavailable -- got: $out" ;;
esac
# With nothing to ask, the isolation row says so. Printing the microVM a
# working install would have given would be the documentation's claim rather
# than this host's, on a report whose whole value is that it never does that.
case "$out" in
  *ISOLATION*"cannot tell"*) ok "the envelope will not name a boundary it cannot establish" ;;
  *) bad "the envelope will not name a boundary it cannot establish -- got: $out" ;;
esac
case "$out" in
  *"image "*) ok "env still reports the lines it can" ;;
  *) bad "env still reports the lines it can -- got: $out" ;;
esac
out="$(PATH="" BRIG_RUNTIME_BIN= "$WORK/brig" ls 2>&1)"; rc=$?
[ "$rc" = 0 ] && ok "ls with no runtime exits 0" || bad "ls with no runtime exits 0 -- got $rc: $out"
case "$out" in
  *none*) ok "ls with no runtime says there is nothing to list" ;;
  *) bad "ls with no runtime says there is nothing to list -- got: $out" ;;
esac
case "$out" in
  *BRIG_RUNTIME_BIN*) ok "ls with no runtime points at getting one" ;;
  *) bad "ls with no runtime points at getting one -- got: $out" ;;
esac

echo "== docker fallback =="
# With no nerdctl on PATH the nerdctl runtime drives docker. It used to do that
# with no output, so someone who installed brig for a microVM boundary got
# docker and no line said so (#30). The stubs answer every call with nothing,
# which is all info needs from them.
fb="$WORK/fallback-bin"
mkdir -p "$fb"
printf '#!/bin/sh\n' > "$fb/docker"
chmod +x "$fb/docker"
out="$(PATH="$fb" BRIG_RUNTIME=nerdctl BRIG_RUNTIME_BIN= "$WORK/brig" info claude 2>&1)"
case "$out" in
  *"brig is driving docker"*) ok "docker taken from PATH is reported" ;;
  *) bad "docker taken from PATH is reported -- got: $out" ;;
esac
# The quiet cases also check that info exited 0 and resolved the binary they
# are about. A missing note from an info that failed early proves nothing.
out="$(PATH="$fb" BRIG_RUNTIME=nerdctl BRIG_RUNTIME_BIN=docker "$WORK/brig" info claude 2>&1)"; rc=$?
[ "$rc" = 0 ] && ok "info with docker named exits 0" || bad "info with docker named exits 0 -- got $rc: $out"
case "$out" in
  *"runtime docker ($fb/docker)"*) ok "info with docker named resolves the stub docker" ;;
  *) bad "info with docker named resolves the stub docker -- got: $out" ;;
esac
case "$out" in
  *"brig is driving docker"*) bad "docker named by BRIG_RUNTIME_BIN is not a fallback -- got: $out" ;;
  *) ok "docker named by BRIG_RUNTIME_BIN is not a fallback" ;;
esac
# ls -q does not lower verbosity, so ls skips the note itself under -q. Plain
# ls goes first: an empty stderr from ls -q then means the guard held, not that
# ls never asked.
out="$(PATH="$fb" BRIG_RUNTIME=nerdctl BRIG_RUNTIME_BIN= "$WORK/brig" ls 2>&1)"
case "$out" in
  *"brig is driving docker"*) ok "ls reports docker taken from PATH" ;;
  *) bad "ls reports docker taken from PATH -- got: $out" ;;
esac
PATH="$fb" BRIG_RUNTIME=nerdctl BRIG_RUNTIME_BIN= "$WORK/brig" ls -q \
  > "$WORK/fallback-ls.out" 2> "$WORK/fallback-ls.err"; rc=$?
[ "$rc" = 0 ] && ok "ls -q on the docker fallback exits 0" \
  || bad "ls -q on the docker fallback exits 0 -- got $rc: $(cat "$WORK/fallback-ls.err")"
[ -s "$WORK/fallback-ls.err" ] \
  && bad "ls -q leaves out the fallback note -- got: $(cat "$WORK/fallback-ls.err")" \
  || ok "ls -q leaves out the fallback note"
printf '#!/bin/sh\n' > "$fb/nerdctl"
chmod +x "$fb/nerdctl"
out="$(PATH="$fb" BRIG_RUNTIME=nerdctl BRIG_RUNTIME_BIN= "$WORK/brig" info claude 2>&1)"; rc=$?
[ "$rc" = 0 ] && ok "info with nerdctl found exits 0" || bad "info with nerdctl found exits 0 -- got $rc: $out"
case "$out" in
  *"runtime nerdctl ($fb/nerdctl)"*) ok "info with nerdctl found resolves the stub nerdctl" ;;
  *) bad "info with nerdctl found resolves the stub nerdctl -- got: $out" ;;
esac
case "$out" in
  *"brig is driving docker"*) bad "nerdctl found says nothing about docker -- got: $out" ;;
  *) ok "nerdctl found says nothing about docker" ;;
esac

echo "== exit codes =="
# The documented set, checked end to end so the numbers a script keys on cannot
# drift from the README. Each verb here fails in a different way and the status
# is the whole point, so it is captured and compared rather than the message.
# A usage mistake is 2, kept apart from the general failure it used to share.
"$WORK/brig" run --nope claude > /dev/null 2>&1; rc=$?
[ "$rc" = 2 ] && ok "a usage error exits 2" || bad "a usage error exits 2 -- got $rc"
# A profile that does not exist is 3: "no such thing", not "it ran and failed".
"$WORK/brig" run nosuchprofile > /dev/null 2>&1; rc=$?
[ "$rc" = 3 ] && ok "an unknown profile exits 3" || bad "an unknown profile exits 3 -- got $rc"
# A runtime that is neither installed nor pinned is 4. run needs one to do its
# work, so unlike env and ls it fails rather than degrading.
PATH="" BRIG_RUNTIME_BIN= "$WORK/brig" run claude -d > /dev/null 2>&1; rc=$?
[ "$rc" = 4 ] && ok "a missing runtime exits 4" || bad "a missing runtime exits 4 -- got $rc"
# A runtime that is named and broken is the same class to a script: also 4.
BRIG_RUNTIME=podman "$WORK/brig" run claude -d > /dev/null 2>&1; rc=$?
[ "$rc" = 4 ] && ok "an unknown BRIG_RUNTIME exits 4" || bad "an unknown BRIG_RUNTIME exits 4 -- got $rc"
# So is a BRIG_RUNTIME_BIN pointing at nothing. It used to be taken on trust and
# reach the first exec, which failed as "cannot tell whether the sandbox is
# already running" with exit 1 and no mention of the variable that caused it.
out="$(BRIG_RUNTIME_BIN=/nonexistent "$WORK/brig" run claude -d 2>&1)"; rc=$?
[ "$rc" = 4 ] && ok "a BRIG_RUNTIME_BIN that is not there exits 4" || bad "a BRIG_RUNTIME_BIN that is not there exits 4 -- got $rc: $out"
case "$out" in
  *BRIG_RUNTIME_BIN*) ok "the refusal names BRIG_RUNTIME_BIN" ;;
  *) bad "the refusal names BRIG_RUNTIME_BIN -- got: $out" ;;
esac
# A credential a run required but could not resolve is 6. A profile of our own
# that declares a required secret nothing supplies fails at credential
# resolution, before the sandbox boots -- whether the store is absent (Linux) or
# present but empty (macOS), the class a script sees is the same.
mkdir -p "$BRIG_PROFILE_DIR"
cat > "$BRIG_PROFILE_DIR/needsecret.yaml" <<'YAML'
name: needsecret
image: docker.io/library/ubuntu:24.04
guestHome: /home/x
binary: bash
mem: 1024
cpus: 1
secrets:
  - name: NEEDSECRET_TOKEN
    required: true
YAML
"$WORK/brig" run needsecret -d > /dev/null 2>&1; rc=$?
[ "$rc" = 6 ] && ok "an unresolved required secret exits 6" \
  || bad "an unresolved required secret exits 6 -- got $rc"
rm -f "$BRIG_PROFILE_DIR/needsecret.yaml"
# A policy the backend cannot enforce is 7, kept apart from a boot that failed
# for any other reason. claude-code sets network: isolated, which vz and qemu
# refuse as well. The first runs leave BRIG_NETWORK unset, so the profile
# chooses the posture the way it does on a default run, and the policy refusal
# has to answer all the same.
mkdir -p "$WORK/cap-policies"
cat > "$WORK/cap-policies/no-net.yaml" <<'YAML'
apiVersion: brig.sh/v1alpha1
name: no-net
egress:
  default: deny
  allow:
    - host: api.anthropic.com
YAML
export BRIG_POLICY_DIR="$WORK/cap-policies"
"$WORK/brig" policy attach no-net claude-code > /dev/null 2>&1 \
  || bad "a policy attaches to a profile for the exit 7 cases"
: > "$STUB_LOG"
for hv in vz qemu; do
  out="$(env -u BRIG_NETWORK BRIG_HYPERVISOR=$hv "$WORK/brig" run claude -d 2>&1)"; rc=$?
  [ "$rc" = 7 ] && ok "a policy $hv cannot enforce exits 7" \
    || bad "a policy $hv cannot enforce exits 7 -- got $rc: $(printf '%s\n' "$out" | tail -n 1)"
done
env -u BRIG_NETWORK "$WORK/brig" run --json claude -d > "$WORK/cap.json" 2> /dev/null; rc=$?
last="$(tail -n1 "$WORK/cap.json")"
case "$rc:$last" in
  '7:'*'"stage":"brig","exit":7'*) ok "run --json reports a policy vz cannot enforce as stage brig, exit 7" ;;
  *) bad "run --json reports a policy vz cannot enforce as stage brig, exit 7 -- got $rc: $last" ;;
esac
# The same refusal when a setting chose the network, and on the verb that only
# records a port.
"$WORK/brig" run claude -d > /dev/null 2>&1; rc=$?
[ "$rc" = 7 ] && ok "a policy vz cannot enforce exits 7 under BRIG_NETWORK=shared" \
  || bad "a policy vz cannot enforce exits 7 under BRIG_NETWORK=shared -- got $rc"
env -u BRIG_NETWORK "$WORK/brig" network publish claude 3000 > /dev/null 2>&1; rc=$?
[ "$rc" = 7 ] && ok "network publish under a policy vz cannot enforce exits 7" \
  || bad "network publish under a policy vz cannot enforce exits 7 -- got $rc"
grep -q '^argv: run' "$STUB_LOG" \
  && bad "a run refused with exit 7 reached the runtime" \
  || ok "a run refused with exit 7 reached no runtime"
"$WORK/brig" policy detach no-net claude-code > /dev/null 2>&1
unset BRIG_POLICY_DIR
# With no policy, the isolated network vz cannot give is a separate refusal,
# and it stays a 1: no policy is involved, and 7 would send a script looking
# for one.
out="$(env -u BRIG_NETWORK "$WORK/brig" run claude -d 2>&1)"; rc=$?
case "$rc:$out" in
  '1:'*'sets network: isolated'*) ok "an isolated network vz cannot give, with no policy, exits 1" ;;
  *) bad "an isolated network vz cannot give, with no policy, exits 1 -- got $rc: $(printf '%s\n' "$out" | tail -n 1)" ;;
esac
"$WORK/brig" rm --all -y > /dev/null 2>&1

echo "== teleportable =="
# A teleportable session owns its storage: the stub must see a block root on
# hvi and no --shared-dir at all, and nothing may appear on the host where an
# ordinary session keeps its home. BRIG_WORKSPACE is the script's own setting
# for every other case, and it names a host home, so these runs drop it.
# Offline keeps the stub from needing a gateway; the network is not what is
# under test here.
tp() { env -u BRIG_WORKSPACE BRIG_HYPERVISOR=hvi BRIG_NETWORK=offline "$@"; }
mkdir -p "$WORK/tp-project"
: > "$STUB_LOG"
CLAUDE_CODE_OAUTH_TOKEN=tp-token-secret GH_TOKEN=tp-gh-secret \
  tp "$WORK/brig" run --teleportable claude -d > "$WORK/tp.out" 2>&1; rc=$?
[ "$rc" = 0 ] && ok "run --teleportable exits 0" || bad "run --teleportable exits 0 -- got $rc: $(cat "$WORK/tp.out")"
run_line="$(grep '^argv: run ' "$STUB_LOG")"
case "$run_line" in
  *--shared-dir*) bad "a teleportable boot shared a host directory in -- got: $run_line" ;;
  *) ok "a teleportable boot passes no --shared-dir" ;;
esac
case "$run_line" in
  *"--hypervisor hvi"*"--rootfs-type block"*) ok "a teleportable boot asks hvi for a block root" ;;
  *) bad "a teleportable boot asks hvi for a block root -- got: $run_line" ;;
esac
# On arm64 it asks for the portable machine and boots the portable kernel,
# with the bundle's initrd, and the flag sits after the root type and before
# the boot annotations: nothing else on the line moves. On amd64 there is no
# portable machine, and the line names none.
tp_words="$(grep '^words:\[run\]' "$STUB_LOG")"
want="[--rootfs-type][block]$TP_MACHINE[--annotation][com.urunc.unikernel.bootKernel=$TP_KERNEL]"
want="$want[--annotation][com.urunc.unikernel.bootInitrd=$WORK/assets/container-initrd]"
case "$tp_words" in
  *"$want"*) ok "a teleportable boot names the machine it boots, with its kernel, and nothing else ($(uname -m))" ;;
  *) bad "a teleportable boot names the machine it boots, with its kernel -- want $want in: $tp_words" ;;
esac
[ "$(grep -o -- '--profile' <<< "$tp_words" | wc -l | tr -d ' ')" = "$([ -n "$TP_MACHINE" ] && echo 1 || echo 0)" ] \
  && ok "a teleportable boot names a machine profile once on arm64 and never elsewhere" \
  || bad "a teleportable boot names a machine profile once on arm64 and never elsewhere -- got: $tp_words"
[ -e "$BRIG_STATE_DIR/homes/brig-claude-code" ] \
  && bad "a teleportable run made a guest home on the host" \
  || ok "a teleportable run makes no guest home on the host"
# Credentials go in exactly as on an ordinary run: a tmpfs mounted by an exec
# as root, values on stdin, never in argv.
grep -q '^argv: exec -u root .* mount -t tmpfs .* /brig/claude' "$STUB_LOG" \
  && ok "a teleportable session still mounts its credential tmpfs in the guest" \
  || bad "a teleportable session still mounts its credential tmpfs in the guest"
grep '^argv:' "$STUB_LOG" | grep -q 'tp-token-secret\|tp-gh-secret' \
  && bad "a credential value reached argv on a teleportable run" \
  || ok "no credential value reaches argv on a teleportable run"
# Not on the run at all, by name either. hull writes every --env of a run,
# value included, into the sandbox's own files (/urunit.conf on the root disk,
# /urunc-env in the boot initrd), and this disk is the session: kept for its
# life and moved with it. The run's environment loses the value too, so the
# stub reads it unset. The agent is an exec, and the exec hands it in.
run_words="$(grep '^words:\[run\]' "$STUB_LOG")"
run_gh="$(awk '/^argv: run /{r=1} r && /^env-gh:/{print; exit}' "$STUB_LOG")"
case "$run_words|$run_gh" in
  *"[--env][GH_TOKEN"*|*tp-gh-secret*|"|"*|*"|")
    bad "a teleportable run line names no credential and hands hull none -- got: $run_words $run_gh" ;;
  *"|env-gh:<unset>") ok "a teleportable run line names no credential and hands hull none" ;;
  *) bad "a teleportable run line names no credential and hands hull none -- got: $run_words $run_gh" ;;
esac
GH_TOKEN=tp-gh-secret tp "$WORK/brig" run claude -p hi > "$WORK/tp-exec.out" 2>&1; rc=$?
exec_line="$(grep '^argv: exec .* -- claude -p hi' "$STUB_LOG" | tail -1)"
exec_gh="$(awk '/^argv: exec .* -- claude -p hi/{r=1} r && /^env-gh:/{v=$0; r=0} END{print v}' "$STUB_LOG")"
case "$rc|$exec_line|$exec_gh" in
  "0|"*"--env GH_TOKEN "*"|env-gh:tp-gh-secret") ok "the agent's exec on a teleportable session carries GH_TOKEN" ;;
  *) bad "the agent's exec on a teleportable session carries GH_TOKEN -- got $rc: $exec_line $exec_gh $(cat "$WORK/tp-exec.out")" ;;
esac
# The onboarding seed a host home would have carried is fed into the guest
# home on the disk instead, and it is the seed and nothing else.
seed="$STUB_STATE.fed.$(printf '%s' "$GUEST_HOME/.claude/.claude.json" | tr / _)"
grep -q hasCompletedOnboarding "$seed" 2>/dev/null && ! grep -q 'tp-token-secret\|tp-gh-secret' "$seed" \
  && ok "the onboarding seed is written inside the guest home" \
  || bad "the onboarding seed is written inside the guest home -- got: $(cat "$seed" 2>&1)"

# ls and info say so.
tp "$WORK/brig" ls > "$WORK/tp-ls.out" 2>&1
grep -q '^claude-code .*(in the sandbox disk)$' "$WORK/tp-ls.out" && grep -q '^REF  *SANDBOX  *STATE  *WORKSPACE$' "$WORK/tp-ls.out" \
  && ok "ls marks the session teleportable" || bad "ls marks the session teleportable -- got: $(cat "$WORK/tp-ls.out")"
tp "$WORK/brig" info claude > "$WORK/tp-info.out" 2>&1
grep -q '^TELEPORTABLE ' "$WORK/tp-info.out" && grep -q '^WORKSPACE .*none on the host' "$WORK/tp-info.out" \
  && ok "info names a teleportable session and no host home" \
  || bad "info names a teleportable session and no host home -- got: $(cat "$WORK/tp-info.out")"
# The machine, read back from hull's record of the boot: the stub's record
# carries the --profile the run was handed, as hull's carries hvi's argv.
case "$TP_MACHINE" in
  "") tp_machine=native ;;
  *) tp_machine=portable-arm64-v1 ;;
esac
STUB_INSPECT_ARGV=1 tp "$WORK/brig" info claude > "$WORK/tp-info-m.out" 2>&1
STUB_INSPECT_ARGV=1 tp "$WORK/brig" info --json claude > "$WORK/tp-info-m.json" 2> /dev/null
grep -q "^MACHINE  *$tp_machine (" "$WORK/tp-info-m.out" \
  && python3 -c 'import json,sys; sys.exit(json.load(open(sys.argv[1]))["data"]["machine"] != sys.argv[2])' "$WORK/tp-info-m.json" "$tp_machine" \
  && ok "info names the machine a teleportable session runs, in text and JSON" \
  || bad "info names the machine a teleportable session runs -- want $tp_machine, got: $(cat "$WORK/tp-info-m.out" "$WORK/tp-info-m.json")"
# A record of a boot that named no machine is hvi's native one, and info says
# so rather than what a boot would ask for now: that is a session booted
# before brig chose the portable machine, and it would not move.
if [ -n "$TP_MACHINE" ]; then
  cp "$STUB_STATE.profile" "$WORK/tp-profile.keep"; : > "$STUB_STATE.profile"
  STUB_INSPECT_ARGV=1 tp "$WORK/brig" info claude > "$WORK/tp-info-m.out" 2>&1
  grep -q '^MACHINE  *native, as hull records the boot (a teleportable session boots portable-arm64-v1 now)$' "$WORK/tp-info-m.out" \
    && ok "info reports the native machine hull's record shows, not the one a boot asks for" \
    || bad "info reports the native machine hull's record shows -- got: $(cat "$WORK/tp-info-m.out")"
  cp "$WORK/tp-profile.keep" "$STUB_STATE.profile"
fi
# Where a checkpoint left the session, in hull's words, once hull has a
# record and not before. The checkpoint is the stub's, asked of it directly:
# no brig verb takes one yet.
grep -q '^TELEPORT ' "$WORK/tp-info.out" \
  && bad "info prints no TELEPORT row before hull records a checkpoint -- got: $(cat "$WORK/tp-info.out")" \
  || ok "info prints no TELEPORT row before hull records a checkpoint"
"$WORK/hull" checkpoint --then fence brig-claude-code > /dev/null
tp "$WORK/brig" info claude > "$WORK/tp-info.out" 2>&1
tp "$WORK/brig" info --json claude > "$WORK/tp-info.json" 2> /dev/null
grep -q "^TELEPORT  *fenced (checkpoint in $STUB_STATE.bundle)$" "$WORK/tp-info.out" \
  && python3 -c 'import json,sys; t=json.load(open(sys.argv[1]))["data"]["teleport"]; sys.exit(t["state"] != "fenced")' "$WORK/tp-info.json" \
  && ok "info shows the migration state hull records for a teleportable session" \
  || bad "info shows the migration state hull records for a teleportable session -- got: $(cat "$WORK/tp-info.out" "$WORK/tp-info.json")"
# A fenced guest still has its VMM up, so the sandbox reads as running. A run
# that reused it would exec the agent into a guest whose vCPUs are parked, so
# it is refused before anything is done to the sandbox: no exec, no recreate.
: > "$STUB_LOG"
out="$(tp "$WORK/brig" run claude -d 2>&1)"; rc=$?
case "$rc:$out" in
  1:*"fenced by a checkpoint"*"hull restore brig-claude-code"*"brig rm claude-code"*)
    grep -q '^argv: \(run\|rm\|stop\|exec\) ' "$STUB_LOG" \
      && bad "a run on a fenced teleportable sandbox is refused before anything is done to it -- got: $(grep '^argv:' "$STUB_LOG" | head -3)" \
      || ok "a run on a fenced teleportable sandbox is refused before anything is done to it" ;;
  *) bad "a run on a fenced teleportable sandbox is refused before anything is done to it -- got $rc: $out" ;;
esac
# Dropped from the stub's record, standing for a restore that brought the
# guest back, so the checks below find a sandbox that runs.
rm -f "$STUB_STATE.migration"

# The session keeps the property: a flagless run reuses the sandbox rather
# than taking it for a stale share and recreating it over its own disk.
: > "$STUB_LOG"
tp "$WORK/brig" run claude -d > "$WORK/tp-again.out" 2>&1; rc=$?
[ "$rc" = 0 ] && ! grep -q '^argv: \(run\|rm\|stop\) ' "$STUB_LOG" \
  && ok "a flagless run reuses the teleportable sandbox" \
  || bad "a flagless run reuses the teleportable sandbox -- got $rc: $(grep '^argv:' "$STUB_LOG" | head -3)"

# A project on the line that asks for a teleportable session contradicts
# itself: usage, 2. A project handed to a session that is teleportable from
# its record is refused at the boot: 1, naming rm as the way back.
: > "$STUB_LOG"
out="$(tp "$WORK/brig" run --teleportable claude "$WORK/tp-project" 2>&1)"; rc=$?
case "$rc:$out" in
  2:*--teleportable*"$WORK/tp-project"*) ok "--teleportable with a project is a usage error, exit 2" ;;
  *) bad "--teleportable with a project is a usage error, exit 2 -- got $rc: $out" ;;
esac
out="$(tp "$WORK/brig" run claude "$WORK/tp-project" -d 2>&1)"; rc=$?
case "$rc:$out" in
  1:*"mounts no project"*"brig rm claude-code"*) ok "a project on a teleportable session is refused, exit 1, naming rm" ;;
  *) bad "a project on a teleportable session is refused, exit 1, naming rm -- got $rc: $out" ;;
esac
out="$(env -u BRIG_NETWORK BRIG_HYPERVISOR=hvi BRIG_NETWORK=offline "$WORK/brig" run claude -d 2>&1)"; rc=$?
case "$rc:$out" in
  1:*BRIG_WORKSPACE*) ok "BRIG_WORKSPACE on a teleportable session is refused by name" ;;
  *) bad "BRIG_WORKSPACE on a teleportable session is refused by name -- got $rc: $out" ;;
esac
grep -q '^argv: \(run\|rm\|stop\) ' "$STUB_LOG" \
  && bad "a refused teleportable run touched the sandbox" \
  || ok "a refused teleportable run leaves the sandbox alone"

# A record lost (the index is not an inventory) must not turn into a
# recreate: hull's record of the boot shows a block root and no share, and
# recreating the sandbox would delete the disk the agent's state is on. The
# guest is not asked; its mount table still lists no home share, and that is
# not what decides.
cp "$BRIG_STATE_DIR/sessions.json" "$WORK/tp-sessions.json"
rm -f "$BRIG_STATE_DIR/sessions.json"
: > "$STUB_LOG"
out="$(STUB_INSPECT_ARGV=1 "$WORK/brig" run claude -d 2>&1)"; rc=$?
case "$rc:$out" in
  1:*"lost the record"*"--teleportable"*) ok "an ordinary run refuses an unrecorded sandbox that owns its disk" ;;
  *) bad "an ordinary run refuses an unrecorded sandbox that owns its disk -- got $rc: $out" ;;
esac
grep -q '^argv: \(run\|rm\|stop\) ' "$STUB_LOG" \
  && bad "an ordinary run tore down a teleportable sandbox" \
  || ok "an ordinary run leaves a teleportable sandbox's disk alone"
cp "$WORK/tp-sessions.json" "$BRIG_STATE_DIR/sessions.json"

# hull cannot start a stopped sandbox again without recreating it, and for
# this session that deletes the disk. stop says so, and the next run refuses
# instead of clearing the stopped instance.
out="$(tp "$WORK/brig" stop claude 2>&1)"; rc=$?
case "$rc:$out" in
  0:*"stopping a teleportable session ends it"*) ok "stop says it ends a teleportable session" ;;
  *) bad "stop says it ends a teleportable session -- got $rc: $out" ;;
esac
: > "$STUB_LOG"
out="$(tp "$WORK/brig" run claude -d 2>&1)"; rc=$?
case "$rc:$out" in
  1:*"is stopped"*"brig rm claude-code"*) ok "a run refuses to clear a stopped teleportable sandbox" ;;
  *) bad "a run refuses to clear a stopped teleportable sandbox -- got $rc: $out" ;;
esac
grep -q '^argv: \(run\|rm\) ' "$STUB_LOG" \
  && bad "a run cleared a stopped teleportable sandbox" \
  || ok "a stopped teleportable sandbox keeps its disk"
# The same with the record lost, which is what a `brig ls` on a hull whose
# `ps -a` fails used to cause: an ordinary run clears a stopped instance
# before it boots, and hull's record is what stops it here.
cp "$BRIG_STATE_DIR/sessions.json" "$WORK/tp-sessions.json"
rm -f "$BRIG_STATE_DIR/sessions.json"
: > "$STUB_LOG"
out="$(STUB_INSPECT_ARGV=1 "$WORK/brig" run claude -d 2>&1)"; rc=$?
case "$rc:$out" in
  1:*"stopped sandbox"*"lost the record"*"brig rm claude-code"*) ok "an ordinary run refuses to clear an unrecorded stopped sandbox that owns its disk" ;;
  *) bad "an ordinary run refuses to clear an unrecorded stopped sandbox that owns its disk -- got $rc: $out" ;;
esac
grep -q '^argv: \(run\|rm\) ' "$STUB_LOG" \
  && bad "an ordinary run cleared an unrecorded stopped teleportable sandbox" \
  || ok "an unrecorded stopped teleportable sandbox keeps its disk"
cp "$WORK/tp-sessions.json" "$BRIG_STATE_DIR/sessions.json"

# rm takes the disk, says the guest home goes with it, and names no host home
# it leaves behind. The preview says the same and removes nothing.
: > "$STUB_LOG"
out="$(tp "$WORK/brig" rm --dry-run claude 2>&1)"; rc=$?
case "$rc:$out" in
  0:"would remove claude (sandbox brig-claude-code) and its guest home inside the sandbox disk")
    ok "rm --dry-run of a teleportable session says its guest home goes with the disk" ;;
  *) bad "rm --dry-run of a teleportable session says its guest home goes with the disk -- got $rc: $out" ;;
esac
grep -q '^argv: \(rm\|stop\) ' "$STUB_LOG" \
  && bad "rm --dry-run of a teleportable session removed it" \
  || ok "rm --dry-run of a teleportable session removes nothing"
out="$(tp "$WORK/brig" rm claude 2>&1)"; rc=$?
case "$rc:$out" in
  0:*"stays on the host"*) bad "rm of a teleportable session names a host home -- got: $out" ;;
  0:*"removed claude and its guest home inside the sandbox disk"*)
    ok "rm of a teleportable session says its guest home went with the disk" ;;
  *) bad "rm of a teleportable session says its guest home went with the disk -- got $rc: $out" ;;
esac

# A backend that cannot keep the session whole is a capability refusal, 7,
# before anything boots.
: > "$STUB_LOG"
out="$(env -u BRIG_WORKSPACE BRIG_HYPERVISOR=vz "$WORK/brig" run --teleportable claude -d 2>&1)"; rc=$?
case "$rc:$out" in
  7:*"hull on vz cannot run a teleportable session"*) ok "a teleportable session on vz exits 7 and says why" ;;
  *) bad "a teleportable session on vz exits 7 and says why -- got $rc: $out" ;;
esac
grep -q '^argv: run ' "$STUB_LOG" \
  && bad "a teleportable run refused with exit 7 reached the runtime" \
  || ok "a teleportable run refused with exit 7 reached no runtime"
# A gui profile's window is the boot process and reads its credentials from
# the boot environment, which a teleportable boot keeps them out of. Refused
# for that reason, ahead of the backend, and nothing boots.
: > "$STUB_LOG"
out="$(tp "$WORK/brig" run --teleportable claude-desktop 2>&1)"; rc=$?
case "$rc:$out" in
  1:*--teleportable*"reads its credentials at boot"*) ok "--teleportable on a profile that reads credentials at boot is refused, saying why" ;;
  *) bad "--teleportable on a profile that reads credentials at boot is refused, saying why -- got $rc: $out" ;;
esac
grep -q '^argv: run ' "$STUB_LOG" \
  && bad "a teleportable gui run reached the runtime" \
  || ok "a teleportable gui run reached no runtime"
# No portable kernel, no teleportable boot on arm64: refused before the boot,
# naming the variable that points at it, so a session that could never move
# is not started.
if [ -n "$TP_MACHINE" ]; then
  : > "$STUB_LOG"
  out="$(BRIG_PORTABLE_KERNEL="$WORK/no-such-kernel" tp "$WORK/brig" run --teleportable claude -d 2>&1)"; rc=$?
  case "$rc:$out" in
    1:*portable-arm64-v1*"$WORK/no-such-kernel"*BRIG_PORTABLE_KERNEL*) ok "a teleportable run with no portable kernel is refused, naming BRIG_PORTABLE_KERNEL" ;;
    *) bad "a teleportable run with no portable kernel is refused, naming BRIG_PORTABLE_KERNEL -- got $rc: $out" ;;
  esac
  grep -q '^argv: run ' "$STUB_LOG" \
    && bad "a teleportable run with no portable kernel reached the runtime" \
    || ok "a teleportable run with no portable kernel reached no runtime"
  # A profile that is not genericBoot has no boot assets, so brig has no
  # portable kernel to hand hull and none to check. Refused on the profile,
  # before the kernel is looked for and before the runtime is asked, and the
  # plan says the same.
  cat > "$BRIG_PROFILE_DIR/tp-ownkernel.yaml" <<'YAML'
name: tp-ownkernel
image: smoke/image:latest
binary: sh
guestHome: /root
mem: 256
cpus: 1
hypervisor: hvi
YAML
  : > "$STUB_LOG"
  out="$(BRIG_PORTABLE_KERNEL="$WORK/no-such-kernel" tp "$WORK/brig" run --teleportable tp-ownkernel -d 2>&1)"; rc=$?
  case "$rc:$out" in
    1:*--teleportable*portable-arm64-v1*"genericBoot: true"*) ok "a teleportable run of a profile that is not genericBoot is refused, saying why" ;;
    *) bad "a teleportable run of a profile that is not genericBoot is refused, saying why -- got $rc: $out" ;;
  esac
  grep -q '^\(argv: run \|probe: \)' "$STUB_LOG" \
    && bad "a teleportable run of a profile that is not genericBoot reached the runtime -- got: $(grep '^\(argv\|probe\):' "$STUB_LOG" | head -3)" \
    || ok "a teleportable run of a profile that is not genericBoot reached no runtime"
  out="$(BRIG_PORTABLE_KERNEL="$WORK/no-such-kernel" tp "$WORK/brig" plan --teleportable tp-ownkernel 2>&1)"
  case "$out" in
    *"brig run refuses this"*"genericBoot: true"*) ok "the plan of a teleportable profile that is not genericBoot carries the refusal" ;;
    *) bad "the plan of a teleportable profile that is not genericBoot carries the refusal -- got: $out" ;;
  esac
  rm -f "$BRIG_PROFILE_DIR/tp-ownkernel.yaml"
  # A hull from before machine profiles has no --profile on run. brig asks
  # it first and refuses as a runtime that cannot run a teleportable session,
  # exit 7, rather than letting hull's flag parser answer.
  : > "$STUB_LOG"
  out="$(STUB_HULL_NO_PROFILE=1 tp "$WORK/brig" run --teleportable claude -d 2>&1)"; rc=$?
  case "$rc:$out" in
    7:*teleportable*portable-arm64-v1*"no --profile on run"*"Upgrade hull"*) ok "a hull with no --profile refuses a teleportable run, exit 7" ;;
    *) bad "a hull with no --profile refuses a teleportable run, exit 7 -- got $rc: $out" ;;
  esac
  grep -q '^probe: run --help' "$STUB_LOG" && ! grep -q '^argv: run ' "$STUB_LOG" \
    && ok "the probe ran and the run did not" \
    || bad "the probe ran and the run did not -- got: $(grep '^\(argv\|probe\):' "$STUB_LOG" | head -5)"
  "$WORK/brig" rm --all -y > /dev/null 2>&1
fi
# An ordinary run on hvi is the line it was: no machine profile, and the
# bundle's kernel.
: > "$STUB_LOG"
env -u BRIG_NETWORK BRIG_HYPERVISOR=hvi BRIG_NETWORK=offline "$WORK/brig" run claude -d > "$WORK/tp-plain.out" 2>&1; rc=$?
plain_words="$(grep '^words:\[run\]' "$STUB_LOG")"
plain_kernel="$WORK/assets/Image"
[ "$(uname -m)" = x86_64 ] && plain_kernel="$WORK/assets/bzImage"
case "$rc|$plain_words" in
  *--profile*) bad "an ordinary hvi run names a machine profile -- got: $plain_words" ;;
  "0|"*"[com.urunc.unikernel.bootKernel=$plain_kernel]"*)
    ok "an ordinary hvi run names no machine profile and boots the bundle's kernel" ;;
  *) bad "an ordinary hvi run names no machine profile and boots the bundle's kernel -- got $rc: $plain_words $(cat "$WORK/tp-plain.out")" ;;
esac
"$WORK/brig" rm --all -y > /dev/null 2>&1

echo "== teleport =="
# A teleport against the stub: the destination is this machine under a second
# state directory, gateway directory and stub instance, reached through a fake
# ssh that runs the remote command the way sshd hands it to a shell. It drops
# ssh's own options and the host and gives the one command string to sh, so a
# word brig failed to quote fails here as it would over a real ssh. A host it
# was not told about is refused the way ssh refuses an unknown one. Like sshd
# it hands the remote command none of the caller's environment: only what a
# login there would have, and the settings that make this machine the
# destination. A credential in this shell must not reach the destination, and
# a posture the destination resolved only because it inherited this shell's
# would hide a bug.
RSTATE="$WORK/remote-state"; RGW="$WORK/remote-gw"
export STUB_REMOTE_STATE="$WORK/remote-instance"
export SSH_LOG="$WORK/ssh.log" REMOTE_LOG="$WORK/remote-argv.log"
: > "$SSH_LOG"; : > "$REMOTE_LOG"
cat > "$WORK/ssh" <<SSH
#!/bin/bash
{ printf 'ssh:'; printf '[%s]' "\$@"; printf '\n'; } >> "\$SSH_LOG"
while [ \$# -gt 0 ] && [ "\$1" != -- ]; do shift; done
shift; host="\$1"; shift
[ "\$host" = me@box ] || { echo "ssh: Could not resolve hostname \$host" >&2; exit 255; }
exec env -i PATH="\$PATH" HOME="\$HOME" USER="\${USER:-}" HULL_TELEMETRY_DISABLED=1 \
  BRIG_RUNTIME=hull BRIG_RUNTIME_BIN="$WORK/hull" BRIG_BOOT_ASSETS="$WORK/assets" BRIG_VERIFY=off \
  BRIG_PROFILE_DIR="$WORK/remote-profiles" BRIG_POLICY_DIR="$WORK/remote-policies" \
  BRIG_READY_TIMEOUT=5 STUB_GATEWAY="\$STUB_GATEWAY" \
  BRIG_STATE_DIR="$RSTATE" BRIG_GATEWAY_DIR="$RGW" STUB_STATE="$STUB_REMOTE_STATE" \
  STUB_LOG="\$REMOTE_LOG" STUB_NO_EGRESS="\${REMOTE_NO_EGRESS:-}" sh -c "\$*"
SSH
chmod +x "$WORK/ssh"
# hull's hvi sits beside it; the probe looks there.
printf '#!/bin/sh\necho "hvi 0.0.0-stub"\n' > "$WORK/hvi"; chmod +x "$WORK/hvi"
tpr() { env -u BRIG_WORKSPACE BRIG_HYPERVISOR=hvi BRIG_NETWORK="${TP_NET:-offline}" STUB_INSPECT_ARGV=1 \
  BRIG_TELEPORT_SSH="$WORK/ssh" BRIG_TELEPORT_REMOTE_BRIG="$WORK/brig" "$@"; }

GH_TOKEN=tp-gh-secret tpr "$WORK/brig" run --teleportable claude -d > "$WORK/tpm.out" 2>&1 \
  || bad "a teleportable session for the move boots -- got: $(cat "$WORK/tpm.out")"
# --prepare: hull is asked to make the destination ready, with the store and
# the hull the destination's brig drives, and nothing moves. brig says what
# the host holds, passes hull's lines on as they come, and notes the host.
: > "$STUB_LOG"; : > "$SSH_LOG"
GH_TOKEN=tp-gh-secret tpr "$WORK/brig" teleport claude --to me@box --prepare > "$WORK/tpp.out" 2> "$WORK/tpp.err"; rc=$?
base7="$(printf '%064d' 7)"
case "$rc:$(cat "$WORK/tpp.out")" in
  "0:prepared me@box for claude-code"*"base 000000000000: sent"*"base digest: $base7"*) ok "teleport --prepare readies the host and says what it holds" ;;
  *) bad "teleport --prepare readies the host and says what it holds -- got $rc: $(cat "$WORK/tpp.out" "$WORK/tpp.err")" ;;
esac
grep -qE '^argv: migrate-prepare --to me@box --remote-hull [^ ]*/hull --json brig-claude-code$' "$STUB_LOG" \
  && ! grep -q '^argv: migrate ' "$STUB_LOG" && [ ! -s "$STUB_REMOTE_STATE" ] \
  && ok "a prepare asks hull with the destination's hull and moves nothing" \
  || bad "a prepare asks hull with the destination's hull and moves nothing -- got: $(grep '^argv: migrate' "$STUB_LOG")"
others="$(grep -v '^ssh:\(\[-o\]\[BatchMode=yes\]\)\?\[--\]\[me@box\]\[' "$SSH_LOG")"
[ -s "$SSH_LOG" ] && [ -z "$others" ] && ! grep -q 'teleport-receive' "$SSH_LOG" \
  && ok "a prepare asks the named host's brig only the probe" \
  || bad "a prepare asks the named host's brig only the probe -- got: $others $(cat "$SSH_LOG")"
grep -q '^brig: hull: base 000000000000: sent$' "$WORK/tpp.err" \
  && ok "a prepare passes hull's lines on to stderr" \
  || bad "a prepare passes hull's lines on to stderr -- got: $(cat "$WORK/tpp.err")"
GH_TOKEN=tp-gh-secret tpr "$WORK/brig" teleport claude --to me@box --prepare --json > "$WORK/tpp.out" 2> "$WORK/tpp.err"; rc=$?
python3 - "$WORK/tpp.out" "$BRIG_STATE_DIR/sessions.json" <<'PY' && [ "$rc" = 0 ] && grep -q '^brig: hull: base 000000000000: held$' "$WORK/tpp.err" \
  && ok "a second prepare is sent nothing, and the index notes the host" \
  || bad "a second prepare is sent nothing, and the index notes the host -- got $rc: $(cat "$WORK/tpp.out" "$WORK/tpp.err")"
import json, sys
doc, index = (json.load(open(p)) for p in sys.argv[1:])
p = doc["data"]["prepared"]
assert doc["kind"] == "Teleport" and p["host"] == "me@box" and p["base"] == "0" * 63 + "7", doc
assert p["stream"] and p["kernels"]["portable-arm64-v1"] == "0" * 63 + "9" and doc["data"]["state"] == "here", doc
assert [st["result"] for st in p["steps"]] == ["held", "held"], doc
e = index["claude-code"]
assert e["preparedHost"] == "me@box" and e["preparedBase"] == p["base"] and not e.get("pendingHost"), e
PY
: > "$STUB_LOG"; : > "$SSH_LOG"
GH_TOKEN=tp-gh-secret tpr "$WORK/brig" teleport claude --to me@box > "$WORK/tpm.out" 2> "$WORK/tpm.err"; rc=$?
case "$rc:$(cat "$WORK/tpm.out")" in
  "0:teleported claude-code to me@box (move "*")"*) ok "teleport moves the session and says where to" ;;
  *) bad "teleport moves the session and says where to -- got $rc: $(cat "$WORK/tpm.out" "$WORK/tpm.err")" ;;
esac
# hull's lines about the copy reach stderr as hull prints them, the move
# says the host was prepared, and the outcome ends with the numbers.
grep -q '^brig: me@box was prepared for claude-code at ' "$WORK/tpm.err" \
  && grep -q '^brig: hull: round 1: 65536 bytes in 0.1 s (0.6 MiB/s), dirty 40 pages/s$' "$WORK/tpm.err" \
  && grep -q "hull's move took [0-9.]* s" "$WORK/tpm.out" \
  && ok "a move passes hull's lines on and ends with its numbers" \
  || bad "a move passes hull's lines on and ends with its numbers -- got: $(cat "$WORK/tpm.out" "$WORK/tpm.err")"
# hull is asked for a dry run first and then the move, named by a txid of
# hull's shape, with the destination's hull, and the sandbox last.
grep -qE '^argv: migrate --to me@box --remote-hull [^ ]*/hull --dry-run brig-claude-code$' "$STUB_LOG" \
  && grep -qE '^argv: migrate --to me@box --remote-hull [^ ]*/hull --txid [0-9a-f]{32} brig-claude-code$' "$STUB_LOG" \
  && ok "hull is asked for a dry run and then the move, under a txid" \
  || bad "hull is asked for a dry run and then the move, under a txid -- got: $(grep '^argv: migrate' "$STUB_LOG")"
# The claim: what leaves the host goes over ssh to the host named, and to no
# other. Every ssh line names that host, right after --, and the move hull
# makes names it on --to.
others="$(grep -v '^ssh:\(\[-o\]\[BatchMode=yes\]\)\?\[--\]\[me@box\]\[' "$SSH_LOG")"
[ -s "$SSH_LOG" ] && [ -z "$others" ] && ! grep '^argv: migrate .*--to ' "$STUB_LOG" | grep -v -- '--to me@box ' | grep -q . \
  && ok "the move goes over ssh to the named host and no other" \
  || bad "the move goes over ssh to the named host and no other -- got: $others $(grep '^argv: migrate' "$STUB_LOG")"
grep -q 'tp-gh-secret' "$SSH_LOG" \
  && bad "a credential value reached an ssh line" \
  || ok "no credential value reaches an ssh line"
# The destination asked first, took the network before hull moved anything,
# and filed the session as its own.
grep -o 'brig teleport-[a-z]*\( --json --[a-z]*\)\?' "$SSH_LOG" | tr '\n' ',' > "$WORK/tpm.verbs"
case "$(cat "$WORK/tpm.verbs")" in
  *"teleport-probe --json --agent,"*"teleport-receive --json --ref,"*"teleport-receive --json --commit,") ok "the destination is probed, then takes the network, then is told the move finished" ;;
  *) bad "the destination is probed, then takes the network, then is told the move finished -- got: $(cat "$WORK/tpm.verbs")" ;;
esac
python3 - "$BRIG_STATE_DIR/sessions.json" "$RSTATE/sessions.json" "$RSTATE/arrivals.json" "$RGW/networks.json" <<'PY' \
  && ok "both hosts record where the session is" || bad "both hosts record where the session is"
import json, sys
here, there, arr, nets = (json.load(open(p)) for p in sys.argv[1:])
assert here["claude-code"]["host"] == "me@box" and not here["claude-code"].get("pendingHost"), here
assert there["claude-code"]["sandbox"] == "brig-claude-code" and there["claude-code"]["teleportable"], there
assert arr["brig-claude-code"]["state"] == "committed", arr
assert nets["brig-claude-code"] == "none", nets
PY

tpr "$WORK/brig" ls > "$WORK/tpm-ls.out" 2>&1
grep -q '^claude-code  *brig-claude-code  *moved  *(in the sandbox disk on me@box)$' "$WORK/tpm-ls.out" \
  && ok "ls names the host a teleported session runs on" \
  || bad "ls names the host a teleported session runs on -- got: $(cat "$WORK/tpm-ls.out")"

# sh on the teleported ref is the same verb on its host: ssh with the line as
# typed, quoted for the shell there, and the brig there runs it in its own
# sandbox. This host's runtime runs nothing.
: > "$STUB_LOG"; : > "$SSH_LOG"; : > "$REMOTE_LOG"
GH_TOKEN=tp-gh-secret tpr "$WORK/brig" sh claude -c 'echo "a b" | wc -w' > "$WORK/tpm-sh.out" 2> "$WORK/tpm-sh.err"; rc=$?
grep -qF -- "ssh:[--][me@box][$WORK/brig sh claude -c 'echo \"a b\" | wc -w']" "$SSH_LOG" \
  && ok "sh on a teleported session execs ssh to its host with the line as typed" \
  || bad "sh on a teleported session execs ssh to its host with the line as typed -- got $rc: $(cat "$SSH_LOG" "$WORK/tpm-sh.err")"
grep -qF -- '[--][bash][-lc][echo "a b" | wc -w]' "$REMOTE_LOG" \
  && ok "the brig there runs the script in its sandbox, one word, as typed" \
  || bad "the brig there runs the script in its sandbox, one word, as typed -- got: $(grep '^words: \?\[exec' "$REMOTE_LOG" | tail -2)"
grep -q '^argv: \(exec\|run\|ps\|inspect\) ' "$STUB_LOG" \
  && bad "sh on a teleported session asked this host's runtime -- got: $(head -3 "$STUB_LOG")" \
  || ok "sh on a teleported session asks this host's runtime nothing"
grep -q 'tp-gh-secret' "$SSH_LOG" \
  && bad "sh handed a credential value from this host to ssh" \
  || ok "sh hands no credential from this host to ssh"
# And the exec there carries none of this host's: the brig there resolves
# only what its own store and environment hold.
grep -q '^env-gh:<unset>$' "$REMOTE_LOG" && ! grep -q 'tp-gh-secret' "$REMOTE_LOG" \
  && ok "the exec on the destination carries no credential from this host" \
  || bad "the exec on the destination carries no credential from this host -- got: $(grep '^env-' "$REMOTE_LOG")"
tpr "$WORK/brig" info claude > "$WORK/tpm-info.out" 2> "$WORK/tpm-info.err"
grep -q '^TELEPORT .*arrived from ' "$WORK/tpm-info.out" && grep -q 'runs on me@box' "$WORK/tpm-info.err" \
  && ok "info on a teleported session is the brig there's, and says so" \
  || bad "info on a teleported session is the brig there's, and says so -- got: $(cat "$WORK/tpm-info.out" "$WORK/tpm-info.err")"

# rm removes the fenced copy left here, once the brig there says the session
# runs, and keeps the record that reaches it. It asks nothing.
: > "$STUB_LOG"
out="$(tpr "$WORK/brig" rm claude 2>&1)"; rc=$?
case "$rc:$out" in
  "0:removed the copy of claude left on this host; it runs on me@box") ok "rm removes the fenced copy left on this host" ;;
  *) bad "rm removes the fenced copy left on this host -- got $rc: $out" ;;
esac
grep -q '^argv: rm brig-claude-code$' "$STUB_LOG" && [ ! -f "$STUB_STATE.stopped" ] && [ -f "$STUB_REMOTE_STATE" ] \
  && ok "rm took the local instance and left the one there" \
  || bad "rm took the local instance and left the one there -- got: $(grep '^argv:' "$STUB_LOG")"
tpr "$WORK/brig" ls > "$WORK/tpm-ls.out" 2>&1
grep -q '^claude-code  *brig-claude-code  *moved ' "$WORK/tpm-ls.out" \
  && ok "the session is still listed, on its host" \
  || bad "the session is still listed, on its host -- got: $(cat "$WORK/tpm-ls.out")"
# With no copy left here, rm forgets the session on this host and leaves the
# one there alone.
: > "$SSH_LOG"
out="$(tpr "$WORK/brig" rm claude 2>&1)"; rc=$?
case "$rc:$out" in
  "0:forgot claude on this host; it runs on me@box, untouched there"*)
    [ -f "$STUB_REMOTE_STATE" ] && [ ! -s "$SSH_LOG" ] \
      && ok "a second rm forgets the session here and touches nothing there" \
      || bad "a second rm forgets the session here and touches nothing there -- got: $(cat "$SSH_LOG")" ;;
  *) bad "a second rm forgets the session here and touches nothing there -- got $rc: $out" ;;
esac

# The default posture moves too: an isolated session under a policy, to a
# destination that can enforce it. The destination's brig starts a gateway on
# the same /30 with the same rules before hull is asked, hull is handed that
# gateway, and the brig there then runs the session as its own: the rules it
# resolves are the carried ones, since that host binds no policy, so sh there
# is not refused as a changed policy.
mkdir -p "$WORK/tp-policies"
cat > "$WORK/tp-policies/carry.yaml" <<'YAML'
apiVersion: brig.sh/v1alpha1
name: carry
egress:
  default: deny
  allow:
    - host: api.anthropic.com
YAML
export BRIG_POLICY_DIR="$WORK/tp-policies"
"$WORK/brig" policy attach carry claude-code -n mv > /dev/null 2>&1 \
  || bad "a policy attaches to the session that moves under it"
TP_NET=isolated tpr "$WORK/brig" run --teleportable claude@mv -d > "$WORK/tpi.out" 2>&1 \
  || bad "an isolated teleportable session under a policy boots -- got: $(cat "$WORK/tpi.out")"
: > "$STUB_LOG"; : > "$SSH_LOG"; : > "$REMOTE_LOG"
TP_NET=isolated tpr "$WORK/brig" teleport claude@mv --to me@box > "$WORK/tpi.out" 2>&1; rc=$?
rsock="$(sed -n 's/^argv: network-gateway --socket \([^ ]*\) .*/\1/p' "$REMOTE_LOG" | head -1)"
case "$rc:$rsock" in
  0:"$RGW"/*) grep -q "^argv: network-gateway --socket $rsock .*--egress-default deny --egress-allow host=api.anthropic.com" "$REMOTE_LOG" \
      && ok "the destination starts the session's gateway with the rules it carries" \
      || bad "the destination starts the session's gateway with the rules it carries -- got: $(grep '^argv: network-gateway' "$REMOTE_LOG")" ;;
  *) bad "the destination starts the session's gateway with the rules it carries -- got $rc: $(cat "$WORK/tpi.out") $(grep '^argv: network-gateway' "$REMOTE_LOG")" ;;
esac
[ -n "$rsock" ] && grep -q "^argv: migrate .*--remote-gateway-sock $rsock.qemu --txid [0-9a-f]* brig-claude-code-mv$" "$STUB_LOG" \
  && ok "hull is handed the stream socket of the gateway the destination started" \
  || bad "hull is handed the stream socket of the gateway the destination started -- got: $(grep '^argv: migrate' "$STUB_LOG")"
: > "$REMOTE_LOG"
out="$(TP_NET=isolated tpr "$WORK/brig" sh claude@mv -c true 2>&1)"; rc=$?
[ "$rc" = 0 ] && grep -qF -- '[--][bash][-lc][true]' "$REMOTE_LOG" \
  && ok "the brig there runs the moved session under the rules it carries" \
  || bad "the brig there runs the moved session under the rules it carries -- got $rc: $out"
"$WORK/brig" policy detach carry claude-code -n mv > /dev/null 2>&1
unset BRIG_POLICY_DIR
# The copy here, then the record here, then the session there.
tpr "$WORK/brig" rm claude@mv > /dev/null 2>&1
tpr "$WORK/brig" rm claude@mv > /dev/null 2>&1
"$WORK/ssh" -- me@box "$WORK/brig rm claude@mv" > /dev/null 2>&1
[ -n "$(tpr "$WORK/brig" ls -q 2>/dev/null)" ] \
  && bad "the moved sessions were not cleared from this host -- got: $(tpr "$WORK/brig" ls -q 2>&1)"

# An offline session under a policy has no gateway on either host, so it
# moves to a destination whose gateway could not enforce rules. Its rules go
# with it, and the brig there resolves them and keeps it offline: sh there is
# not refused as a changed policy.
mkdir -p "$WORK/tp-policies"
cat > "$WORK/tp-policies/shut.yaml" <<'YAML'
apiVersion: brig.sh/v1alpha1
name: shut
egress:
  default: deny
YAML
export BRIG_POLICY_DIR="$WORK/tp-policies"
"$WORK/brig" policy attach shut claude-code -n off > /dev/null 2>&1 \
  || bad "a policy attaches to the offline session that moves under it"
tpr "$WORK/brig" run --teleportable claude@off -d > "$WORK/tpo.out" 2>&1 \
  || bad "an offline teleportable session under a policy boots -- got: $(cat "$WORK/tpo.out")"
: > "$STUB_LOG"; : > "$SSH_LOG"; : > "$REMOTE_LOG"
out="$(REMOTE_NO_EGRESS=1 tpr "$WORK/brig" teleport claude@off --to me@box 2>&1)"; rc=$?
[ "$rc" = 0 ] && grep -q 'teleport-receive .*--network none .*"default":"deny"' "$SSH_LOG" \
  && ! grep -q '^argv: network-gateway --socket' "$REMOTE_LOG" \
  && ok "an offline session under a policy moves with no gateway there" \
  || bad "an offline session under a policy moves with no gateway there -- got $rc: $out"
out="$(tpr "$WORK/brig" sh claude@off -c true 2>&1)"; rc=$?
[ "$rc" = 0 ] && grep -qF -- '[--][bash][-lc][true]' "$REMOTE_LOG" \
  && ok "the brig there runs the offline session under the rules it carries" \
  || bad "the brig there runs the offline session under the rules it carries -- got $rc: $out"
"$WORK/brig" policy detach shut claude-code -n off > /dev/null 2>&1
unset BRIG_POLICY_DIR
tpr "$WORK/brig" rm claude@off > /dev/null 2>&1
tpr "$WORK/brig" rm claude@off > /dev/null 2>&1
"$WORK/ssh" -- me@box "$WORK/brig rm claude@off" > /dev/null 2>&1
[ -n "$(tpr "$WORK/brig" ls -q 2>/dev/null)" ] \
  && bad "the moved offline session was not cleared from this host -- got: $(tpr "$WORK/brig" ls -q 2>&1)"

# The refusals come before anything is paused: no pause, no checkpoint, no
# move, and the destination is asked to take nothing. A session under a
# policy the destination's gateway cannot enforce is a capability, 7.
mkdir -p "$WORK/tp-policies"
cat > "$WORK/tp-policies/no-net.yaml" <<'YAML'
apiVersion: brig.sh/v1alpha1
name: no-net
egress:
  default: deny
  allow:
    - host: api.anthropic.com
YAML
export BRIG_POLICY_DIR="$WORK/tp-policies"
"$WORK/brig" policy attach no-net claude-code -n pol > /dev/null 2>&1 \
  || bad "a policy attaches to the session that cannot move"
TP_NET=isolated tpr "$WORK/brig" run --teleportable claude@pol -d > "$WORK/tpp.out" 2>&1 \
  || bad "a teleportable session under a policy boots -- got: $(cat "$WORK/tpp.out")"
: > "$STUB_LOG"; : > "$SSH_LOG"
out="$(REMOTE_NO_EGRESS=1 TP_NET=isolated tpr "$WORK/brig" teleport claude@pol --to me@box 2>&1)"; rc=$?
case "$rc:$out" in
  7:*"egress policy"*"cannot enforce"*"Nothing was paused"*) ok "a policy the destination cannot enforce is refused with exit 7" ;;
  *) bad "a policy the destination cannot enforce is refused with exit 7 -- got $rc: $out" ;;
esac
grep -q '^argv: \(migrate\|pause\|checkpoint\|stop\|rm\) ' "$STUB_LOG" || grep -q 'teleport-receive' "$SSH_LOG" \
  && bad "a refused move paused, moved or reserved something -- got: $(grep '^argv:' "$STUB_LOG" | head -3)" \
  || ok "the refused move paused nothing and reserved nothing there"
"$WORK/brig" policy detach no-net claude-code -n pol > /dev/null 2>&1
unset BRIG_POLICY_DIR
tpr "$WORK/brig" rm claude@pol > /dev/null 2>&1
# An ordinary session owns no disk of its own to move: 7, the same class.
"$WORK/brig" run claude -d > /dev/null 2>&1
: > "$STUB_LOG"; : > "$SSH_LOG"
out="$(BRIG_TELEPORT_SSH="$WORK/ssh" "$WORK/brig" teleport claude --to me@box 2>&1)"; rc=$?
case "$rc:$out" in
  7:*"not a teleportable session"*) ok "a session that is not teleportable is refused with exit 7" ;;
  *) bad "a session that is not teleportable is refused with exit 7 -- got $rc: $out" ;;
esac
[ -s "$SSH_LOG" ] || grep -q '^argv: migrate' "$STUB_LOG" \
  && bad "a move refused as not teleportable reached ssh or hull" \
  || ok "a move refused as not teleportable reaches neither ssh nor hull"
"$WORK/brig" rm claude > /dev/null 2>&1 || true
"$WORK/brig" rm --all -y > /dev/null 2>&1

echo "== flags =="
: > "$STUB_LOG"
"$WORK/brig" run claude -t ghcr.io/me/img:latest -w "$WORK/other" -m 8192 --cpus 2 -d \
  > /dev/null 2>&1
grep -q -- '--mem 8192 --cpus 2' "$STUB_LOG" \
  && ok "-m and --cpus reach the runtime" || bad "-m and --cpus reach the runtime"
grep -q -- "--shared-dir $WORK/other:$GUEST_HOME" "$STUB_LOG" \
  && ok "-w overrides the workspace" || bad "-w overrides the workspace"
grep -q 'ghcr.io/me/img:latest' "$STUB_LOG" \
  && ok "-t overrides the image" || bad "-t overrides the image"
"$WORK/brig" rm --all -y > /dev/null 2>&1

echo "== ubuntu =="
# The command goes after --, because run's second bare word is the project
# directory now: for a shell profile the trailing words are the guest command,
# and -- is what still says so.
: > "$STUB_LOG"
"$WORK/brig" run ubuntu -- uname -a > /dev/null 2>&1
grep -qF -- '[--][bash][-lc]["$@"][bash][uname][-a]' "$STUB_LOG" \
  && ok "a shell profile runs the command in a shell" \
  || bad "a shell profile runs the command in a shell"
grep -q -- ':/root/work' "$STUB_LOG" \
  && ok "ubuntu mounts the workspace at /root/work" || bad "ubuntu mounts at /root/work"

echo "== agents =="
# YAML is the default spelling, and the header is what makes a printed agent a
# starting point rather than a puzzle.
"$WORK/brig" agent show claude-code > "$WORK/mine.yaml" 2>/dev/null
grep -q '^# A brig profile' "$WORK/mine.yaml" \
  && ok "show explains its own fields" || bad "show explains its own fields"
printf '\n# why: the vendored CLI needs a bigger guest\n' >> "$WORK/mine.yaml"
sed -i.bak 's/^name: claude-code$/name: mine/; s|ghcr.io/brig-sh/claude-code[^ ]*|docker.io/me/mine:latest|' \
  "$WORK/mine.yaml"
"$WORK/brig" agent import "$WORK/mine.yaml" > "$WORK/import.out" 2>&1 \
  && ok "an edited YAML export imports back" || bad "an edited YAML export imports back: $(cat "$WORK/import.out")"
grep -q 'why: the vendored CLI' "$BRIG_PROFILE_DIR/mine.yaml" 2>/dev/null \
  && ok "import keeps the comments you wrote" || bad "import keeps the comments you wrote"

# JSON still parses, since it is a subset of YAML.
printf '{"name":"jsonagent","image":"docker.io/me/j:latest","guestHome":"/home/j","binary":"j","mem":1024,"cpus":1}' \
  > "$WORK/j.json"
"$WORK/brig" agent import "$WORK/j.json" > /dev/null 2>&1 \
  && ok "a JSON profile still imports" || bad "a JSON profile still imports"
[ -f "$BRIG_PROFILE_DIR/jsonagent.json" ] \
  && ok "a JSON profile keeps its own extension" || bad "JSON profile keeps its extension"
profiles="$("$WORK/brig" agent ls 2>/dev/null)"
case "$profiles" in
  *"mine"*"(file)"*) ok "a profile of your own is listed, and marked" ;;
  *) bad "a profile of your own is listed -- got: $profiles" ;;
esac
grep -q 'cannot verify its signature' "$WORK/import.out" \
  && ok "import says an outside image cannot be verified" \
  || bad "import says an outside image cannot be verified: $(cat "$WORK/import.out")"
case "$profiles" in
  *bring-your-own-image*) ok "agent ls points at the image documentation" ;;
  *) bad "agent ls points at the image documentation" ;;
esac
# A name that would escape the profile directory, or collide with a path.
printf 'name: ../evil\nimage: i\nguestHome: /home/x\nbinary: x\nmem: 1\ncpus: 1\n' \
  | "$WORK/brig" agent import - > /dev/null 2>&1 \
  && bad "an unsafe profile name was accepted" || ok "an unsafe profile name is refused"
# A misspelled field would forward no credentials, which looks exactly like a
# broken sandbox.
printf 'name: typo\nimage: i\nguestHome: /home/x\nbinary: x\nmem: 1\ncpus: 1\nforwards: [GH_TOKEN]\n' \
  | "$WORK/brig" agent import - > /dev/null 2>&1 \
  && bad "a misspelled field was accepted" || ok "a misspelled field is refused"

# The built-ins are embedded, so nothing is pre-seeded: every profile comes from
# the binary until you put a file there yourself.
[ -f "$BRIG_PROFILE_DIR/claude-code.yaml" ] \
  && bad "brig pre-seeded the profile directory" \
  || ok "brig does not pre-seed the profile directory"

# show writes the file brig ships, comments and all -- that is what makes
# "start from the closest agent" work rather than handing back a struct dump.
# It goes to stdout, which is also how you get a copy anywhere other than the
# profile directory.
"$WORK/brig" agent show codex > "$WORK/codex.yaml" 2>/dev/null
grep -q 'metered path' "$WORK/codex.yaml" 2>/dev/null \
  && ok "show keeps the comments explaining the deny list" \
  || bad "show keeps the comments explaining the deny list"

# show prints one agent and writes nothing, so a second word is the old
# `brig export <p> <name>` habit rather than a destination. It says which
# command took that job over.
"$WORK/brig" agent show codex spare > "$WORK/showdest.out" 2>&1 \
  && bad "agent show took a destination" || ok "agent show refuses a destination"
grep -q 'brig agent new spare --from codex' "$WORK/showdest.out" \
  && ok "agent show names the command that copies one" \
  || bad "agent show names the command that copies one: $(cat "$WORK/showdest.out")"
[ -f "$BRIG_PROFILE_DIR/spare.yaml" ] \
  && bad "agent show wrote a file" || ok "agent show wrote nothing"

# new copies an agent, so it has to be told which one.
"$WORK/brig" agent new lonely > "$WORK/nofrom.out" 2>&1 \
  && bad "agent new was accepted without --from" || ok "agent new needs --from"
grep -q -- '--from' "$WORK/nofrom.out" \
  && ok "agent new names the flag it wants" \
  || bad "agent new names the flag it wants: $(cat "$WORK/nofrom.out")"

# A destination is a name and nothing else. brig writes one directory, so a
# path -- or a typo that looks like one -- is refused rather than honoured.
"$WORK/brig" agent export codex "$WORK/escape.yaml" > /dev/null 2>&1 \
  && bad "export wrote to a path outside the profile directory" \
  || ok "export refuses a path destination"
[ -f "$WORK/escape.yaml" ] \
  && bad "export wrote outside the profile directory" \
  || ok "export writes the profile directory and nowhere else"

# A bare destination is a name, and brig resolves it in the profile directory.
"$WORK/brig" agent export codex bare > /dev/null 2>&1
[ -f "$BRIG_PROFILE_DIR/bare.yaml" ] \
  && ok "a bare export destination lands in the profile directory" \
  || bad "a bare export destination lands in the profile directory"
"$WORK/brig" agent export codex bare > /dev/null 2>&1 \
  && bad "export overwrote an existing file" \
  || ok "export refuses to overwrite without --force"
"$WORK/brig" agent export codex bare --force > /dev/null 2>&1 \
  && ok "--force overwrites" || bad "--force overwrites"
# A mistyped flag must not become a file name.
"$WORK/brig" agent export codex --jsonn > /dev/null 2>&1 \
  && bad "a mistyped flag was taken as a destination" \
  || ok "a mistyped flag is refused rather than written"
ls "$BRIG_PROFILE_DIR" | grep -q -- '--json' \
  && bad "a file was written for a mistyped flag" \
  || ok "no file is written for a mistyped flag"
# The destination is the name written into the file, and a profile of that name
# wins the lookup over an alias -- so exporting onto one would take every
# `brig run claude` from claude-code.
"$WORK/brig" agent export claude-code claude > /dev/null 2>&1 \
  && bad "export wrote a profile named after an alias" \
  || ok "export refuses a destination that is an alias"
[ -f "$BRIG_PROFILE_DIR/claude.yaml" ] \
  && bad "the aliased destination was written anyway" \
  || ok "no file is written for an aliased destination"
# Asking for help is not a mistake, though the flag package calls it an error.
"$WORK/brig" agent rm --help > "$WORK/rmhelp.out" 2>&1 \
  && ok "agent rm --help exits 0" \
  || bad "agent rm --help exits 0: $(cat "$WORK/rmhelp.out")"
grep -q 'brig agent rm' "$WORK/rmhelp.out" \
  && ok "agent rm --help prints the usage" \
  || bad "agent rm --help prints the usage: $(cat "$WORK/rmhelp.out")"
# Every top-level verb answers a help request, and the two spellings --
# `brig <verb> --help` / -h and `brig help <verb>` -- print the same text.
# stdout alone is compared: `help <verb>` loads the profiles first, so a notice
# about one of them can precede the usage on stderr.
for v in run sh info rm stop ls logs doctor version; do
  "$WORK/brig" "$v" --help > "$WORK/vhelp.out" 2>&1 \
    && ok "brig $v --help exits 0" \
    || bad "brig $v --help exits 0: $(cat "$WORK/vhelp.out")"
  grep -q -- "brig $v" "$WORK/vhelp.out" \
    && ok "brig $v --help prints its usage" \
    || bad "brig $v --help prints its usage: $(cat "$WORK/vhelp.out")"
  "$WORK/brig" "$v" -h > "$WORK/vhelp-h.out" 2>/dev/null \
    && ok "brig $v -h exits 0" \
    || bad "brig $v -h exits 0: $(cat "$WORK/vhelp-h.out")"
  cmp -s "$WORK/vhelp.out" "$WORK/vhelp-h.out" \
    && ok "brig $v -h prints the same text as --help" \
    || bad "brig $v -h prints the same text as --help"
  "$WORK/brig" help "$v" > "$WORK/vhelp-help.out" 2>/dev/null \
    && ok "brig help $v exits 0" \
    || bad "brig help $v exits 0: $(cat "$WORK/vhelp-help.out")"
  cmp -s "$WORK/vhelp.out" "$WORK/vhelp-help.out" \
    && ok "brig help $v prints the same text as --help" \
    || bad "brig help $v prints the same text as --help"
done
# rm resolves the name inside the file, not the file name: rename what
# bare.yaml declares, and codex is the word that reaches it. Nothing is deleted
# for that word without a question first, though -- the file it would take is
# not the file anyone named, and stdin here has nobody on it to ask.
sed 's/^name: bare$/name: codex/' "$BRIG_PROFILE_DIR/bare.yaml" > "$WORK/bare.yaml"
cp "$WORK/bare.yaml" "$BRIG_PROFILE_DIR/bare.yaml"
"$WORK/brig" agent rm codex < /dev/null > "$WORK/rm.out" 2>&1 \
  && bad "rm deleted a file nobody named, without asking" \
  || ok "rm refuses to delete a file you did not name with no terminal to ask on"
grep -q 'bare.yaml' "$WORK/rm.out" \
  && ok "rm names the file it would delete" \
  || bad "rm names the file it would delete: $(cat "$WORK/rm.out")"
[ -f "$BRIG_PROFILE_DIR/bare.yaml" ] \
  && ok "the file nobody named is still there" \
  || bad "the file nobody named was deleted anyway"
"$WORK/brig" agent rm codex -y < /dev/null > /dev/null 2>&1
[ -f "$BRIG_PROFILE_DIR/bare.yaml" ] \
  && bad "rm did not resolve the profile inside the file" \
  || ok "rm resolves the profile a file declares, not its file name"

# Two files can declare one profile, because a file need not be named after the
# profile in it. brig says which won, and rm takes both -- removing only the
# winner would promote the other and leave the profile listed.
"$WORK/brig" agent export codex dup > /dev/null 2>&1
sed 's/^name: dup$/name: dupagent/' "$BRIG_PROFILE_DIR/dup.yaml" > "$WORK/dup2.yaml"
cp "$WORK/dup2.yaml" "$BRIG_PROFILE_DIR/dup.yaml"
cp "$WORK/dup2.yaml" "$BRIG_PROFILE_DIR/dupother.yaml"
"$WORK/brig" agent ls > "$WORK/dup.out" 2>&1
grep -q 'duplicate profile name' "$WORK/dup.out" \
  && ok "two files claiming one profile are reported" \
  || bad "two files claiming one profile are reported: $(cat "$WORK/dup.out")"
# -y: neither file is named after the profile they both declare, and rm asks
# before deleting a file the argument did not name.
"$WORK/brig" agent rm dupagent -y < /dev/null > /dev/null 2>&1
{ [ -f "$BRIG_PROFILE_DIR/dup.yaml" ] || [ -f "$BRIG_PROFILE_DIR/dupother.yaml" ]; } \
  && bad "rm left a file still declaring the profile" \
  || ok "rm takes every file declaring the profile"

# A file of your own shadows the built-in, and the listing says so rather than
# leaving you to wonder which image is booting.
"$WORK/brig" agent export claude-code claude-code > /dev/null 2>&1
sed -i.bak 's|ghcr.io/brig-sh/claude-code:[^ ]*|docker.io/me/pinned:latest|' \
  "$BRIG_PROFILE_DIR/claude-code.yaml"
rm -f "$BRIG_PROFILE_DIR/claude-code.yaml.bak"
case "$("$WORK/brig" agent ls 2>/dev/null)" in
  *"overrides built-in"*) ok "a file shadowing a built-in is marked as one" ;;
  *) bad "a file shadowing a built-in is marked as one" ;;
esac
rm -f "$BRIG_PROFILE_DIR/claude-code.yaml"

# edit needs a file. A built-in has none, and nothing is created for it.
EDITOR=true "$WORK/brig" agent edit codex > "$WORK/edit.out" 2>&1 \
  && bad "agent edit accepted a built-in" || ok "agent edit refuses a built-in"
grep -q 'built in' "$WORK/edit.out" \
  && ok "agent edit says how to make a file" \
  || bad "agent edit says how to make a file: $(cat "$WORK/edit.out")"
grep -q 'brig agent new codex --from codex' "$WORK/edit.out" \
  && ok "agent edit names the command that makes one" \
  || bad "agent edit names the command that makes one: $(cat "$WORK/edit.out")"
[ -f "$BRIG_PROFILE_DIR/codex.yaml" ] \
  && bad "agent edit created a file for a built-in" \
  || ok "agent edit creates nothing for a built-in"

# ...and opens one that exists, honouring the editor's own arguments.
mkdir -p "$WORK/bin"
cat > "$WORK/bin/fake-editor" <<'EDIT'
#!/bin/sh
printf '\n# edited by the smoke test\n' >> "$1"
EDIT
chmod +x "$WORK/bin/fake-editor"
EDITOR="$WORK/bin/fake-editor" "$WORK/brig" agent edit mine > /dev/null 2>&1 \
  && ok "agent edit opens a file-backed profile" \
  || bad "agent edit opens a file-backed profile"
grep -q 'edited by the smoke test' "$BRIG_PROFILE_DIR/mine.yaml" \
  && ok "agent edit saves what the editor wrote" \
  || bad "agent edit saves what the editor wrote"

# The recipe brig prints, end to end: copy the closest built-in under a name of
# your own, edit it, run it, remove it by the name you chose. Every step
# addresses the name the person picked, which is why new writes that name into
# the file rather than leaving the profile called what it was copied from.
"$WORK/brig" agent new mytool --from claude-code > /dev/null 2>&1
grep -q '^name: mytool$' "$BRIG_PROFILE_DIR/mytool.yaml" 2>/dev/null \
  && ok "agent new writes the name it was given into the file" \
  || bad "agent new writes the name it was given into the file"
grep -q '^name: claude-code$' "$BRIG_PROFILE_DIR/mytool.yaml" 2>/dev/null \
  && bad "the copy still declares the agent it came from" \
  || ok "the copy no longer declares the agent it came from"
grep -q 'outrank' "$BRIG_PROFILE_DIR/mytool.yaml" 2>/dev/null \
  && ok "a renamed copy still carries the comments" \
  || bad "a renamed copy still carries the comments"
EDITOR="$WORK/bin/fake-editor" "$WORK/brig" agent edit mytool > /dev/null 2>&1 \
  && ok "the copy edits under the name it was given" \
  || bad "the copy edits under the name it was given"
: > "$STUB_LOG"
"$WORK/brig" run mytool -w "$WORK/ws-mytool" -p hi > /dev/null 2>&1
grep -q -- '--name brig-mytool' "$STUB_LOG" \
  && ok "the copy runs under the name it was given" \
  || bad "the copy runs under the name it was given"
grep -q -- '-- claude -p hi' "$STUB_LOG" \
  && ok "the run is the agent it was copied from, renamed" \
  || bad "the run is the agent it was copied from, renamed"
# rm refuses while a sandbox of the profile is up (#367). Once the file is
# gone, `brig rm mytool` answers "unknown profile" and the sandbox is left for
# `rm --all`. So it names the `brig rm` to run first, and deletes nothing.
"$WORK/brig" agent rm mytool < /dev/null > "$WORK/rm-busy.out" 2>&1 \
  && bad "agent rm removed a profile its sandbox still uses" \
  || ok "agent rm refuses while a sandbox of the profile exists"
[ -f "$BRIG_PROFILE_DIR/mytool.yaml" ] \
  && ok "the refused rm leaves the file in place" \
  || bad "the refused rm deleted the file anyway"
grep -q 'brig rm mytool' "$WORK/rm-busy.out" \
  && ok "the refusal names the brig rm to run first" \
  || bad "the refusal names the brig rm to run first: $(cat "$WORK/rm-busy.out")"
"$WORK/brig" rm mytool > /dev/null 2>&1 \
  && ok "brig rm removes the sandbox the refusal named" \
  || bad "brig rm removes the sandbox the refusal named"
"$WORK/brig" agent rm mytool < /dev/null > /dev/null 2>&1
[ -f "$BRIG_PROFILE_DIR/mytool.yaml" ] \
  && bad "rm did not remove the copy under the name it was given" \
  || ok "rm removes the copy under the name it was given, asking nothing"
"$WORK/brig" rm --all -y > /dev/null 2>&1

# Every spelling this release retires keeps working, and says the one that
# replaces it. Both halves matter: a spelling that fails breaks every script
# that has it, and one that works silently never teaches anyone the new word.
#
# The replacement is matched as a whole command line, which also catches a
# notice pointing at another retired spelling -- `brig agents` used to say
# "is now `brig profiles`", one hop short of an answer.
"$WORK/brig" agent new dep --from codex > /dev/null 2>&1
while IFS='|' read -r line replacement; do
  [ -n "$line" ] || continue
  # shellcheck disable=SC2086 -- the table holds whole command lines.
  "$WORK/brig" $line < /dev/null > "$WORK/dep.out" 2>&1
  rc=$?
  [ "$rc" = 0 ] \
    && ok "brig $line still works" \
    || bad "brig $line still works -- exit $rc: $(cat "$WORK/dep.out")"
  grep -q "is now \`$replacement\`" "$WORK/dep.out" \
    && ok "brig $line names $replacement" \
    || bad "brig $line names $replacement -- got: $(cat "$WORK/dep.out")"
done <<'TABLE'
profiles|brig agent ls
policies|brig policy ls
agents|brig agent ls
profile ls|brig agent ls
profile list|brig agent ls
profile export codex|brig agent export
profile save codex|brig agent export
agent list|brig agent ls
agent save codex|brig agent export
policy list|brig policy ls
export codex|brig agent export
template ls|brig agent
TABLE
# import reads a file rather than stdin, so it is spelled out rather than
# driven from the table above.
for spelling in "import" "profile import" "profile load" "agent load"; do
  # shellcheck disable=SC2086 -- $spelling is a command line, not a word.
  "$WORK/brig" $spelling "$WORK/codex.yaml" > "$WORK/dep.out" 2>&1 \
    && ok "brig $spelling still works" \
    || bad "brig $spelling still works: $(cat "$WORK/dep.out")"
  grep -q 'is now `brig agent import`' "$WORK/dep.out" \
    && ok "brig $spelling names brig agent import" \
    || bad "brig $spelling names brig agent import -- got: $(cat "$WORK/dep.out")"
done
# The verbs that need a file of their own to work on, so they run last: edit
# and rm each take the copy made above.
EDITOR=true "$WORK/brig" profile edit dep > "$WORK/dep.out" 2>&1 \
  && ok "brig profile edit still works" \
  || bad "brig profile edit still works: $(cat "$WORK/dep.out")"
grep -q 'is now `brig agent edit`' "$WORK/dep.out" \
  && ok "brig profile edit names brig agent edit" \
  || bad "brig profile edit names brig agent edit -- got: $(cat "$WORK/dep.out")"
"$WORK/brig" profile rm dep -y < /dev/null > "$WORK/dep.out" 2>&1 \
  && ok "brig profile rm still works" \
  || bad "brig profile rm still works: $(cat "$WORK/dep.out")"
grep -q 'is now `brig agent rm`' "$WORK/dep.out" \
  && ok "brig profile rm names brig agent rm" \
  || bad "brig profile rm names brig agent rm -- got: $(cat "$WORK/dep.out")"
"$WORK/brig" template edit mine > /dev/null 2>&1 \
  && bad "brig template edit exists" || ok "there is no brig template edit"

# And the current spellings say nothing. A notice on a command that is not
# going anywhere is how a reader learns to ignore the ones that are.
for line in "agent ls" "agent show codex" "policy ls"; do
  # shellcheck disable=SC2086 -- $line is a command line, not a word.
  "$WORK/brig" $line > "$WORK/cur.out" 2>&1
  grep -q 'is now' "$WORK/cur.out" \
    && bad "brig $line printed a deprecation notice: $(cat "$WORK/cur.out")" \
    || ok "brig $line printed no deprecation notice"
done

echo "== image verification =="
# A stub cosign, so the decision table is exercised without a network.
mkdir -p "$WORK/bin"
cat > "$WORK/bin/cosign" <<'COSIGN'
#!/bin/bash
# `triangulate <ref>` names the signature tag for the digest a reference
# resolves to, which is how brig learns the digest to verify and boot. cosign v3
# opens every triangulate with a deprecation warning on stderr, so this does too.
if [ "$1" = triangulate ]; then
  echo "Command \"triangulate\" is deprecated, triangulate will be removed in v4.0.0; use 'cosign tree' or 'cosign download signature' instead" >&2
  if [ "${COSIGN_DENY:-0}" = 1 ]; then
    echo "Error: GET https://ghcr.io/token?scope=repository%3Abrig-sh%2Fclaude-code%3Apull&service=ghcr.io: DENIED: requested access to the resource is denied" >&2
    exit 1
  fi
  printf '%s:sha256-%s.sig\n' "${2%%:*}" "$(printf 'a%.0s' $(seq 64))"
  exit 0
fi
[ "${COSIGN_FAIL:-0}" = 1 ] && { echo "Error: no matching signatures"; exit 1; }
exit 0
COSIGN
chmod +x "$WORK/bin/cosign"
export BRIG_COSIGN_BIN="$WORK/bin/cosign"
# The boot-asset digest check reads the verified bundle's manifest over https.
# A proxy on a closed port keeps that request on this machine, so it fails at
# once, and hull's provenance record beside the stand-ins is read instead. It
# names the digest the stub cosign resolves and the sha256 of "stand-in\n".
export HTTPS_PROXY=http://127.0.0.1:9
standin=f5f8eeae987b3e68b4592b2c02ba1dd277eba1fbfa716c3c829c7dbb513a2773
bundle="sha256:$(printf 'a%.0s' $(seq 64))"
cat > "$WORK/assets/provenance.json" <<PROVENANCE
{"digest":"$bundle","verifiedDigest":"$bundle","files":{
  "Image":{"sha256":"$standin"},"bzImage":{"sha256":"$standin"},
  "container-initrd":{"sha256":"$standin"}}}
PROVENANCE

fresh() { "$WORK/brig" stop claude > /dev/null 2>&1; }

fresh
# A signature that checks out is narration, not a warning: there is nothing for
# anyone to act on, so it sits behind --verbose. The default run below
# asserts the other half -- that the line is gone from the output a person gets
# without asking for detail -- and everything else this section checks is a
# refusal or a gap, which stays on screen either way.
out="$(BRIG_VERIFY=warn "$WORK/brig" --verbose run claude -p hi 2>&1)"
case "$out" in
  *"signature verified"*) ok "a signature that checks out is reported under --verbose" ;;
  *) bad "a signature that checks out is reported under --verbose -- got: $out" ;;
esac
fresh
out="$(BRIG_VERIFY=warn "$WORK/brig" run claude -p hi 2>&1)"
case "$out" in
  *"signature verified"*) bad "a default run reported the per-check detail -- got: $out" ;;
  *) ok "a default run holds back the per-check detail" ;;
esac
grep 'argv: run ' "$STUB_LOG" | tail -1 | grep -q '@sha256:' \
  && ok "the verified digest is what hull was told to boot" \
  || bad "hull was told to boot the tag, not the verified digest: $(grep 'argv: run ' "$STUB_LOG" | tail -1)"

# What the default run gets instead of the detail is the outcome, in one line.
# One line for the whole step, not one per check. The registry is out of reach
# here, so the stand-ins match only hull's record beside them, and that record
# does not verify them: the summary names the image alone, and one line says
# why the boot assets are missing from it.
case "$out" in
  *"boot assets verified"*) bad "hull's record verified the boot assets -- got: $out" ;;
  *"brig: image verified"*) ok "a default run says verification held" ;;
  *) bad "a default run says verification held -- got: $out" ;;
esac
case "$out" in
  *"only hull's provenance record lists them"*) ok "a match with hull's record alone is not a verification" ;;
  *) bad "a match with hull's record alone is not a verification -- got: $out" ;;
esac
# The policy that outcome held under is the envelope's VERIFY row, which
# --verbose carries. Stating neither would leave "it verified" to be inferred
# from an absence, on the one subject where a reader must never have to.
fresh
out="$(BRIG_VERIFY=warn "$WORK/brig" --verbose run claude -p hi 2>&1)"
case "$out" in
  *"VERIFY       warn"*) ok "the envelope names the verify mode" ;;
  *) bad "the envelope names the verify mode -- got: $out" ;;
esac

# -q takes the outcome with everything else between an identifier and an error.
fresh
out="$(BRIG_VERIFY=warn "$WORK/brig" -q run claude -p hi 2>&1)"
case "$out" in
  *" verified"*) bad "brig -q printed the verification outcome -- got: $out" ;;
  *) ok "brig -q drops the verification outcome" ;;
esac

# But a verification PROBLEM is never suppressed, -q included. It is the one
# claim brig exists to make, and the caller that asked for silence is the
# unattended one where an unchecked image matters most.
fresh
out="$(BRIG_VERIFY=off "$WORK/brig" -q run claude -p hi 2>&1)"
case "$out" in
  *"BRIG_VERIFY=off"*) ok "brig -q still says nothing checked the image" ;;
  *) bad "brig -q hid that the image was never checked -- got: $out" ;;
esac
# An older hull cannot boot the bytes it checked, which is a caveat on the
# claim rather than a note beside it, so it survives -q too.
fresh
out="$(STUB_HULL_VERSION=0.1.0-rc21 BRIG_VERIFY=warn "$WORK/brig" -q run claude -p hi 2>&1)"
case "$out" in
  *"cannot boot by digest"*) ok "brig -q still says the boot is not pinned to what verified" ;;
  *) bad "brig -q hid that the boot is not pinned -- got: $out" ;;
esac
# And an ordinary warning is still an ordinary warning: the level means
# verification, not "important", or -q is back to printing what it suppresses.
fresh
out="$(GH_TOKEN='op://vault/item/field' "$WORK/brig" -q run claude -p hi 2>&1)"
case "$out" in
  *"unresolved secret reference"*) bad "brig -q printed an ordinary warning -- got: $out" ;;
  *) ok "brig -q still drops an ordinary warning" ;;
esac

# And nothing is claimed when nothing was checked. Off says so itself, in the
# default output, and a run that added "verified" beside it would be false.
fresh
out="$(BRIG_VERIFY=off "$WORK/brig" run claude -p hi 2>&1)"
case "$out" in
  *" verified"*) bad "BRIG_VERIFY=off claimed something verified -- got: $out" ;;
  *) ok "BRIG_VERIFY=off claims no verification" ;;
esac
# Exactly once, whichever half says it. The default run has no envelope, so
# the standalone line carries the fact; a --verbose run has the VERIFY row, so
# the standalone line stands down. A fact stated twice is one a reader skips.
count=$(printf '%s\n' "$out" | grep -c 'BRIG_VERIFY=off')
[ "$count" = 1 ] \
  && ok "BRIG_VERIFY=off is stated exactly once by default" \
  || bad "BRIG_VERIFY=off stated $count times by default -- got: $out"
fresh
out="$(BRIG_VERIFY=off "$WORK/brig" --verbose run claude -p hi 2>&1)"
case "$out" in
  *"VERIFY       off"*) ok "the envelope names the mode when checking is off" ;;
  *) bad "the envelope names the mode when checking is off -- got: $out" ;;
esac
count=$(printf '%s\n' "$out" | grep -c 'BRIG_VERIFY=off')
[ "$count" = 1 ] \
  && ok "BRIG_VERIFY=off is stated exactly once under --verbose" \
  || bad "BRIG_VERIFY=off stated $count times under --verbose -- got: $out"

fresh
out="$(STUB_HULL_VERSION=0.1.0-rc21 BRIG_VERIFY=warn "$WORK/brig" run claude -p hi 2>&1)"
case "$out" in
  *"cannot boot by digest"*) ok "an older hull is told why the tag is booted" ;;
  *) bad "an older hull is told why the tag is booted -- got: $out" ;;
esac
grep 'argv: run ' "$STUB_LOG" | tail -1 | grep -q '@sha256:' \
  && bad "an older hull was handed a digest it cannot resolve" \
  || ok "an older hull boots the tag"

fresh
out="$(BRIG_VERIFY=warn COSIGN_FAIL=1 "$WORK/brig" run claude -p hi 2>&1)"; rc=$?
case "$out" in
  *"DID NOT VERIFY"*) ok "a bad signature on our own image is reported" ;;
  *) bad "a bad signature is reported -- got: $out" ;;
esac
# A boot refused over verification has its own documented code, so a script can
# tell it apart from a run that started and failed.
[ "$rc" = 5 ] && ok "a bad signature stops the boot (exit 5) with no terminal to ask" \
  || bad "a bad signature stops the boot with exit 5 -- got $rc"

# A resolve that fails gives cosign's reason. The deprecation warning cosign
# prints first is not one, and brig quoted it as the reason the registry was
# not reachable.
fresh
out="$(BRIG_VERIFY=require COSIGN_DENY=1 "$WORK/brig" run claude -p hi 2>&1)"; rc=$?
case "$out" in
  *"is deprecated"*) bad "a failed resolve quoted cosign's deprecation warning -- got: $out" ;;
  *"DENIED"*) ok "a failed resolve gives cosign's reason, not its deprecation warning" ;;
  *) bad "a failed resolve gives cosign's reason -- got: $out" ;;
esac
[ "$rc" = 5 ] && ok "require refuses an image it cannot resolve (exit 5)" \
  || bad "require refuses an image it cannot resolve with exit 5 -- got $rc"

fresh
out="$(BRIG_VERIFY=warn BRIG_IMAGE=docker.io/library/ubuntu:24.04 \
  "$WORK/brig" run claude -p hi 2>&1)"; rc=$?
case "$out" in
  *"not published by brig-sh"*) ok "a third-party image warns" ;;
  *) bad "a third-party image warns -- got: $out" ;;
esac
[ "$rc" = 0 ] && ok "a third-party image still boots" || bad "a third-party image was blocked"

fresh
out="$(BRIG_VERIFY=require BRIG_IMAGE=docker.io/library/ubuntu:24.04 \
  "$WORK/brig" run claude -p hi 2>&1)"; rc=$?
[ "$rc" = 5 ] && ok "require refuses what it cannot check (exit 5)" \
  || bad "require refuses what it cannot check with exit 5 -- got $rc"

# The kernel and initrd are compared with the bundle that verified. Here the
# record beside them is for another bundle. BRIG_BOOT_ASSETS names the
# directory, so warn states the difference and boots, and require refuses.
mkdir -p "$WORK/other-assets"
cp "$WORK/assets/Image" "$WORK/assets/bzImage" "$WORK/assets/container-initrd" "$WORK/other-assets/"
other="sha256:$(printf 'b%.0s' $(seq 64))"
printf '{"digest":"%s","verifiedDigest":"%s","files":{}}\n' "$other" "$other" \
  > "$WORK/other-assets/provenance.json"
fresh
out="$(BRIG_VERIFY=warn BRIG_BOOT_ASSETS="$WORK/other-assets" "$WORK/brig" run claude -p hi 2>&1)"; rc=$?
case "$out" in
  *"not the bundle that verified"*) ok "warn states that a named directory is not the verified bundle" ;;
  *) bad "warn states that a named directory is not the verified bundle -- got: $out" ;;
esac
case "$out" in
  *"boot assets verified"*) bad "the summary named boot assets that did not match -- got: $out" ;;
  *) ok "the summary leaves out boot assets that did not match" ;;
esac
[ "$rc" = 0 ] && ok "warn boots a named directory that differs" || bad "warn refused a named directory -- got $rc"
fresh
out="$(BRIG_VERIFY=require BRIG_BOOT_ASSETS="$WORK/other-assets" "$WORK/brig" run claude -p hi 2>&1)"; rc=$?
case "$out" in
  *"not the bundle that verified"*"BRIG_VERIFY=require"*) ok "require names the bundle difference it refuses" ;;
  *) bad "require names the bundle difference it refuses -- got: $out" ;;
esac
[ "$rc" = 5 ] && ok "require refuses a named directory that is not the verified bundle (exit 5)" \
  || bad "require refuses a named directory that differs with exit 5 -- got $rc: $out"

unset BRIG_COSIGN_BIN HTTPS_PROXY
export BRIG_VERIFY=off
# The built-in profiles boot generically now, which means brig wants a kernel
# and an initrd before it starts anything. Point it at a pair of stand-ins: the
# stub runtime never reads them, and without this every run would try to
# download the real bundle from a registry -- on a CI runner, for a test that
# is about argv.
mkdir -p "$WORK/assets"
: > "$WORK/assets/Image"
: > "$WORK/assets/bzImage"
: > "$WORK/assets/container-initrd"
printf 'stand-in\n' > "$WORK/assets/Image"
printf 'stand-in\n' > "$WORK/assets/bzImage"
printf 'stand-in\n' > "$WORK/assets/container-initrd"
export BRIG_BOOT_ASSETS="$WORK/assets"
# The built-in profiles ask for hvi, and brig starts a network gateway for it
# before booting anything. Most cases here are about what reaches argv and have
# no use for one, so the fixture explicitly uses vz and shared. The network
# cases above exercise the isolated default and policy gateway separately.
export BRIG_HYPERVISOR=vz
export BRIG_NETWORK=shared

echo "== sh =="
: > "$STUB_LOG"
"$WORK/brig" sh claude echo hi there > /dev/null 2>"$WORK/shell.err"
grep -qF -- '[--][bash][-lc]["$@"][bash][echo][hi][there]' "$STUB_LOG" \
  && ok "a trailing command reaches bash as its own arguments" \
  || bad "a trailing command reaches bash as its own arguments"
grep -q '^SANDBOX ' "$WORK/shell.err" \
  && bad "sh printed the envelope" || ok "sh does not print the envelope"

# -c asks for the one parse the plain form never does: the script reaches bash
# as a single word, for the login shell to read in the guest.
: > "$STUB_LOG"
"$WORK/brig" sh claude -c 'echo hi | wc -l' > /dev/null 2>&1
grep -qF -- '[--][bash][-lc][echo hi | wc -l]' "$STUB_LOG" \
  && ok "sh -c hands the script to the login shell as one word" \
  || bad "sh -c hands the script to the login shell as one word -- got: $(grep '^words:' "$STUB_LOG" | tail -1)"

# A script typed as one word still runs as a command name, and brig names -c
# before it does.
"$WORK/brig" sh claude 'echo hi | wc -l' > /dev/null 2>"$WORK/shhint.err"
grep -qF 'put -c in front of it' "$WORK/shhint.err" \
  && ok "a one-word script gets the -c hint" \
  || bad "a one-word script gets the -c hint -- got: $(cat "$WORK/shhint.err")"

# And with no command it is a login shell. One verb, and which of the two you
# get is said by whether you typed a command -- not by which word you reached
# for, which is what exec and shell made you choose.
: > "$STUB_LOG"
"$WORK/brig" sh claude > /dev/null 2>&1
grep -q -- '-- bash -l$' "$STUB_LOG" \
  && ok "sh with no command opens a login shell" \
  || bad "sh with no command opens a login shell -- got: $(grep '^argv: exec' "$STUB_LOG" | tail -1)"

echo "== retired spellings =="
# Every spelling this release retires still works and says what replaces it. An
# old spelling has to survive the commit that renames it, or that commit breaks
# every script anyone has written.
#
# Both halves are asserted, and neither is enough alone: a spelling that fails
# breaks those scripts, and one that works silently never teaches anyone the new
# word, so the old one is still in them at the release that removes it.
retired() {
  local want="$1"; shift
  local line="brig $*"
  "$WORK/brig" "$@" > "$WORK/retired.out" 2>&1
  local rc=$?
  case "$rc" in
    2|3) bad "$line was refused -- rc $rc: $(cat "$WORK/retired.out")" ;;
    *)   ok "$line still works" ;;
  esac
  grep -q "is now \`$want\`" "$WORK/retired.out" \
    && ok "$line names $want as its replacement" \
    || bad "$line names $want -- got: $(cat "$WORK/retired.out")"
}
"$WORK/brig" run claude -d > /dev/null 2>&1
retired 'brig sh' exec claude -- true
retired 'brig info' env claude
retired 'brig run -d' create claude
retired '<agent>@<label>' run claude --name retn -d
retired '<agent>@<label>' run claude -n retn -d
# A removed spelling is the other end of that: refused as a usage error that
# names its replacement, with nothing asked of the runtime.
removed() {
  local want="$1"; shift
  local line="brig $*"
  : > "$STUB_LOG"
  "$WORK/brig" "$@" > "$WORK/removed.out" 2>&1
  local rc=$?
  [ "$rc" = 2 ] \
    && ok "$line is a usage error" \
    || bad "$line exited $rc, want 2 -- got: $(cat "$WORK/removed.out")"
  grep -qF "use \`$want\`" "$WORK/removed.out" \
    && ok "$line names $want" \
    || bad "$line names $want -- got: $(cat "$WORK/removed.out")"
  [ -s "$STUB_LOG" ] \
    && bad "$line reached the runtime: $(cat "$STUB_LOG")" \
    || ok "$line starts nothing"
}
removed 'brig sh <ref> [command...]' shell claude echo hi
# Last of the group: it removes what the others were acting on.
retired 'brig rm --all' reset

echo "== what a run says =="
# By default, print what the user has to act on. brig's own progress and the
# runtime's own output wait for --verbose; -q is identifiers and errors.
"$WORK/brig" rm --all -y > /dev/null 2>&1
: > "$STUB_LOG"
CLAUDE_CODE_OAUTH_TOKEN=env-token-secret \
  "$WORK/brig" run claude -d > "$WORK/say.out" 2> "$WORK/say.err"
grep -q '^SANDBOX ' "$WORK/say.err" \
  && bad "a default run printed the envelope -- got: $(cat "$WORK/say.err")" \
  || ok "a default run prints no envelope"
grep -q 'starting sandbox' "$WORK/say.err" \
  && bad "a default run narrated the boot -- got: $(cat "$WORK/say.err")" \
  || ok "a default run does not narrate the boot"
grep -q 'pulling ghcr.io' "$WORK/say.err" \
  && bad "a default run passed the runtime's output through -- got: $(cat "$WORK/say.err")" \
  || ok "a default run holds the runtime's output"

# An image pull inside `hull run` gets a line each end by default (#306). stderr
# is a file here, so the lines are plain; a terminal gets a spinner instead.
"$WORK/brig" rm --all -y > /dev/null 2> "$WORK/pull-rm.err" \
  || bad "clean up before the pull case -- got: $(cat "$WORK/pull-rm.err")"
CLAUDE_CODE_OAUTH_TOKEN=env-token-secret STUB_RUN_PULL=1 \
  "$WORK/brig" run claude -d > /dev/null 2> "$WORK/pull.err"
grep -q '^brig: pulling .*\.\.\.$' "$WORK/pull.err" && grep -q '^brig: .* pulled$' "$WORK/pull.err" \
  && ok "a default run announces an image pull" \
  || bad "a default run announces an image pull -- got: $(cat "$WORK/pull.err")"
grep -q '^pull: ' "$WORK/pull.err" \
  && bad "a default run passed hull's pull lines through -- got: $(cat "$WORK/pull.err")" \
  || ok "a default run holds hull's pull lines"
"$WORK/brig" rm --all -y > /dev/null 2> "$WORK/pull-rm.err" \
  || bad "clean up before the -q pull case -- got: $(cat "$WORK/pull-rm.err")"
CLAUDE_CODE_OAUTH_TOKEN=env-token-secret STUB_RUN_PULL=1 \
  "$WORK/brig" -q run claude -d > /dev/null 2> "$WORK/qpull.err"
grep -q '^brig: pulling' "$WORK/qpull.err" \
  && bad "brig -q announced an image pull -- got: $(cat "$WORK/qpull.err")" \
  || ok "brig -q does not announce an image pull"

# --verbose asks for both, and gets both.
"$WORK/brig" rm --all -y > /dev/null 2>&1
CLAUDE_CODE_OAUTH_TOKEN=env-token-secret \
  "$WORK/brig" --verbose run claude -d > /dev/null 2> "$WORK/verbose.err"
grep -q 'starting sandbox' "$WORK/verbose.err" \
  && ok "--verbose narrates the boot" \
  || bad "--verbose narrates the boot -- got: $(cat "$WORK/verbose.err")"
grep -q 'pulling ghcr.io' "$WORK/verbose.err" \
  && ok "--verbose prints the runtime's own output" \
  || bad "--verbose prints the runtime's own output -- got: $(cat "$WORK/verbose.err")"
grep -q '^SANDBOX ' "$WORK/verbose.err" \
  && ok "--verbose prints the envelope" \
  || bad "--verbose prints the envelope -- got: $(cat "$WORK/verbose.err")"

# The one that matters most: a boot that fails still says what the runtime said,
# with nothing asked for. Hold the output and lose it and a broken boot becomes
# unreportable.
"$WORK/brig" rm --all -y > /dev/null 2>&1
CLAUDE_CODE_OAUTH_TOKEN=env-token-secret STUB_RUN_FAIL=1 \
  "$WORK/brig" run claude -d > /dev/null 2> "$WORK/failed.err"
rc=$?
[ "$rc" != 0 ] && ok "a boot the runtime refused fails the run" \
  || bad "a boot the runtime refused exited 0"
grep -q 'no space left on device' "$WORK/failed.err" \
  && ok "a failed boot quotes what the runtime said" \
  || bad "a failed boot quotes what the runtime said -- got: $(cat "$WORK/failed.err")"

# -q is identifiers and errors only: the envelope goes, and so do the warnings
# that stand in the default output.
"$WORK/brig" rm --all -y > /dev/null 2>&1
: > "$STUB_LOG"
CLAUDE_CODE_OAUTH_TOKEN=env-token-secret GH_TOKEN='op://vault/item/field' \
  "$WORK/brig" -q run claude -d > "$WORK/q.out" 2> "$WORK/q.err"
grep -q '^SANDBOX ' "$WORK/q.err" \
  && bad "brig -q printed the envelope" || ok "brig -q drops the envelope"
grep -q 'unresolved secret reference' "$WORK/q.err" \
  && bad "brig -q printed a warning -- got: $(cat "$WORK/q.err")" \
  || ok "brig -q drops brig's warnings"
# The other half of "identifiers and errors only": an error is not something -q
# takes away, and neither is the evidence under it. A boot that fails has to be
# reportable at every level brig has.
"$WORK/brig" rm --all -y > /dev/null 2>&1
CLAUDE_CODE_OAUTH_TOKEN=env-token-secret STUB_RUN_FAIL=1 \
  "$WORK/brig" -q run claude -d > /dev/null 2> "$WORK/qfail.err"
rc=$?
[ "$rc" != 0 ] && ok "brig -q still fails a boot the runtime refused" \
  || bad "brig -q exited 0 on a boot the runtime refused"
grep -q 'no space left on device' "$WORK/qfail.err" \
  && ok "brig -q still quotes what the runtime said" \
  || bad "brig -q still quotes what the runtime said -- got: $(cat "$WORK/qfail.err")"

# The same flag after the verb, which is where it used to live. It keeps
# working until v0.5.0 and names the position it moved to.
"$WORK/brig" rm --all -y > /dev/null 2>&1
CLAUDE_CODE_OAUTH_TOKEN=env-token-secret \
  "$WORK/brig" run claude -q -d > /dev/null 2> "$WORK/runq.err"
grep -q '^SANDBOX ' "$WORK/runq.err" \
  && bad "run -q printed the envelope" || ok "-q after the verb still means quiet"
grep -q 'is now `brig -q <verb> <ref>`' "$WORK/runq.err" \
  && ok "-q after the verb names the global position" \
  || bad "-q after the verb names the global position -- got: $(cat "$WORK/runq.err")"

# -v is not brig's. It is Claude Code's version flag, codex's verbose flag and
# Docker's volume flag, so brig claims neither reading: left of the verb it is
# an unknown token, and right of the ref it is the agent's word.
"$WORK/brig" -v run claude > "$WORK/dashv.out" 2>&1
rc=$?
[ "$rc" = 2 ] && ok "-v before the command is a usage error" \
  || bad "-v before the command is a usage error -- rc $rc: $(cat "$WORK/dashv.out")"
CLAUDE_CODE_OAUTH_TOKEN=env-token-secret \
  "$WORK/brig" run claude -v > /dev/null 2> "$WORK/agentv.err"
rc=$?
# Not a usage error: right of the ref the vocabulary is the agent's, and brig
# hands the token over rather than reading it. Any other status is the run
# itself, which the cases above assert.
[ "$rc" != 2 ] && ok "-v after the ref is not brig's to refuse" \
  || bad "-v after the ref was refused: $(cat "$WORK/agentv.err")"
grep -q -- "-v is one of brig's own flags" "$WORK/agentv.err" \
  && bad "brig claimed -v as its own -- got: $(cat "$WORK/agentv.err")" \
  || ok "brig claims no reading of -v"

# And the global -q reaches ls as the meaning ls already had: refs alone.
"$WORK/brig" -q ls > "$WORK/globalq.out" 2>&1
[ "$(cat "$WORK/globalq.out")" = claude-code ] \
  && ok "brig -q ls prints the refs alone" \
  || bad "brig -q ls prints the refs alone -- got: $(cat "$WORK/globalq.out")"
"$WORK/brig" ls -q > "$WORK/lsq.out" 2>&1
[ "$(cat "$WORK/lsq.out")" = claude-code ] \
  && ok "ls -q is unchanged" || bad "ls -q is unchanged -- got: $(cat "$WORK/lsq.out")"

echo "== run --json =="
# --json runs the agent as a child instead of replacing brig with it, so brig
# survives the exec and reports the outcome on one JSON line. The stub's exec
# exits 7; brig exits 7 too, with that status carried on a parseable last line --
# which is how a script tells "the agent failed" from "brig refused to start it".
"$WORK/brig" rm --all -y > /dev/null 2>&1
STUB_EXEC_EXIT=7 "$WORK/brig" --json run ubuntu -- true \
  > "$WORK/json.out" 2> "$WORK/json.err"
rc=$?
[ "$rc" = 7 ] && ok "brig --json run exits with the agent's own status" \
  || bad "brig --json run exits with the agent's own status -- got $rc: $(cat "$WORK/json.err")"
# The rule a script follows: the last line of stdout is brig's, and it parses.
last="$(tail -n1 "$WORK/json.out")"
if command -v python3 >/dev/null 2>&1; then
  python3 -c 'import json,sys
d = json.loads(sys.argv[1])
assert d["kind"] == "Run", d
assert d["data"]["stage"] == "agent", d
assert d["data"]["exit"] == 7, d' "$last" 2>/dev/null \
    && ok "the last stdout line is a Run object with stage agent and exit 7" \
    || bad "the last stdout line is not the expected Run object -- got: $last"
fi
case "$last" in
  *'"kind":"Run"'*) ok "brig --json run prints a compact Run object" ;;
  *) bad "brig --json run prints a compact Run object -- got: $last" ;;
esac
"$WORK/brig" rm --all -y > /dev/null 2>&1

echo "== doctor =="
# brig doctor reports the prerequisites a first run hits, in boot order. On the
# stub host the runtime resolves to the stand-in hull, so the report exits 0 and
# names the version the stub prints -- which is the whole point of the runtime
# line, and the one fact this end-to-end case can pin that the unit tests (with
# a runtime that answers no --version) cannot.
"$WORK/brig" doctor > "$WORK/doctor.out" 2> "$WORK/doctor.err"
rc=$?
[ "$rc" = 0 ] && ok "doctor exits 0 on the stub host" \
  || bad "doctor exits 0 -- got $rc: $(cat "$WORK/doctor.err")"
grep -q '^  ok  runtime .*0.1.0-rc23' "$WORK/doctor.out" \
  && ok "doctor names the runtime version" \
  || bad "doctor names the runtime version -- got: $(grep runtime "$WORK/doctor.out")"
# The first row is the build that produced the report, the same line
# `brig version` prints after the word brig.
build="$("$WORK/brig" version | sed 's/^brig //')"
[ "$(head -1 "$WORK/doctor.out")" = "  ok  brig      $build" ] \
  && ok "doctor opens with the brig build" \
  || bad "doctor opens with the brig build -- want '$build', got: $(head -1 "$WORK/doctor.out")"

echo "== completion =="
# The completion scripts are shell code. The unit tests cover the engine's
# replies; these cover the scripts that render them.
#
# Driven under `set -u`, which is where this failed once: before bash 4.4 an
# empty array expanded under `set -u` is an unbound variable, and the error
# printed over the prompt on the first word. /bin/bash is preferred over
# whatever `bash` resolves to because on macOS /bin/bash is 3.2, the version
# that reproduces it, while `bash` on PATH is often a Homebrew 5.x that cannot.
# On Linux CI both are 5.x, so there this only checks the script runs clean.
BASH_OLDEST=bash
[ -x /bin/bash ] && BASH_OLDEST=/bin/bash
"$WORK/brig" completion bash > "$WORK/brig.bash" 2>/dev/null \
  && ok "completion bash prints a script" || bad "completion bash prints a script"

cat > "$WORK/bashdrive.sh" <<'DRIVE'
set -u
source "$1"
shift
COMP_WORDS=("$@"); COMP_CWORD=$(( ${#COMP_WORDS[@]} - 1 )); COMPREPLY=()
_brig_completion
printf '%s\n' "${COMPREPLY[@]-}"
DRIVE

# The first word, where the array left of the cursor is empty.
out="$("$BASH_OLDEST" "$WORK/bashdrive.sh" "$WORK/brig.bash" "$WORK/brig" "" 2>&1)"
case "$out" in
  *"unbound variable"*) bad "the bash script survives set -u -- got: $out" ;;
  *run*) ok "the bash script survives set -u and offers the verbs" ;;
  *) bad "the bash script offers the verbs -- got: $out" ;;
esac

# The boundary the engine and split() must agree on: a brig flag right of the
# ref, which split() still reads.
out="$("$BASH_OLDEST" "$WORK/bashdrive.sh" "$WORK/brig.bash" "$WORK/brig" run claude --m 2>&1)"
[ "$out" = "--mem" ] && ok "the bash script offers brig's flags right of the ref" \
  || bad "the bash script offers --mem right of the ref -- got: $out"

# The inline `--flag=value` shape, which only zsh and fish produce: bash has
# "=" in COMP_WORDBREAKS and sends the halves separately. Checked through the
# engine rather than a shell, since it is the argv shape that matters.
out="$("$WORK/brig" __complete -- run "--network=is" 2>/dev/null | tr '\n' ' ')"
[ "$out" = ":names --network=isolated " ] \
  && ok "an inline flag value completes, qualified with its flag" \
  || bad "inline --network=is completes -- got: $out"

# fish, when installed. Skipped otherwise: it is not on a stock macOS or CI
# image.
if command -v fish >/dev/null 2>&1; then
  "$WORK/brig" completion fish > "$WORK/brig.fish" 2>/dev/null
  out="$(PATH="$WORK:$PATH" fish -c "source $WORK/brig.fish; complete -C 'brig run cl'" 2>&1)"
  case "$out" in
    *claude*) ok "the fish script offers refs" ;;
    *) bad "the fish script offers refs -- got: $out" ;;
  esac
else
  echo "  --   fish not installed, skipping its script"
fi

[ "$fail" = 0 ] && echo PASS || echo FAILURES
exit "$fail"
