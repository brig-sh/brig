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
export STUB_LOG="$WORK/argv.log"
export STUB_STATE="$WORK/instance"
WS="$WORK/ws"

# Assertions on brig's own output capture it first rather than piping into
# `grep -q`. grep -q exits at the first match, brig then dies of SIGPIPE
# writing the rest, and `set -o pipefail` reports the pipeline as failed --
# intermittently, depending on how much was left to write. Reading from a file
# is fine; reading from a process is not.
fail=0
ok()  { printf '  ok   %s\n' "$1"; }
bad() { printf '  FAIL %s\n' "$1"; fail=1; }

# --- the stub runtime ---
# It answers the four questions brig asks (ps, run, exec, stop/rm) and logs
# every argument it is given, so the test can assert on what reached argv.
cat > "$WORK/hull" <<'STUB'
#!/bin/bash
{ printf 'argv:'; printf ' %s' "$@"; printf '\n'; } >> "$STUB_LOG"
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
    name=""; share=""
    : > "$STUB_STATE.shares"
    while [ $# -gt 0 ]; do
      case "$1" in
        --name) name="$2"; shift 2 ;;
        --shared-dir)
          printf '%s\n' "$2" >> "$STUB_STATE.shares"
          [ -z "$share" ] && share="$2"
          shift 2 ;;
        *) shift ;;
      esac
    done
    printf '%s' "$name" > "$STUB_STATE"
    printf '%s' "${share%%:*}" > "$STUB_STATE.share"
    # Record which credential values arrived through the environment rather
    # than through argv.
    printf 'env-token:%s\n' "${CLAUDE_CODE_OAUTH_TOKEN:-<unset>}" >> "$STUB_LOG"
    printf 'env-gh:%s\n' "${GH_TOKEN:-<unset>}" >> "$STUB_LOG"
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
            %s) printf '512\n' ;;
            *) printf 'regular file|claude|600\n' ;;
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
      *) printf 'env-token:%s\n' "${CLAUDE_CODE_OAUTH_TOKEN:-<unset>}" >> "$STUB_LOG" ;;
    esac
    ;;
  stop)
    [ -f "$STUB_STATE" ] && mv "$STUB_STATE" "$STUB_STATE.stopped"
    ;;
  rm)
    rm -f "$STUB_STATE" "$STUB_STATE.stopped" "$STUB_STATE.mounts"
    ;;
esac
exit 0
STUB
chmod +x "$WORK/hull"

go build -o "$WORK/brig" ./cmd/brig || { echo "build failed"; exit 1; }

export BRIG_RUNTIME=hull
export BRIG_RUNTIME_BIN="$WORK/hull"
export BRIG_WORKSPACE="$WS"
export BRIG_READY_TIMEOUT=5
# brig remembers which workspace each sandbox was started with, in a file under
# its state directory. Scratch, like the profile directory: the test creates and
# removes sandboxes, and it must not edit the state of whoever is running it.
export BRIG_STATE_DIR="$WORK/state"
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
# The built-in profiles ask for hvi, and brig starts a network gateway for it
# before booting anything -- a real subcommand of a real runtime, which the
# stub below is not. This test is about what reaches argv, so pin the backend
# to the one that needs no gateway rather than teach the stub to fake one.
export BRIG_HYPERVISOR=vz
# Your own profiles go in a scratch directory, never the caller's own.
export BRIG_PROFILE_DIR="$WORK/profiles"
# The keychain is never read here, and nothing below arranges for it to be: no
# shipped profile declares hostCredential:, which is the only thing that reads
# it. BRIG_CREDENTIALS_CMD used to stand in for that read from this script; it
# is removed, and the case below is what is left to assert about it.

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

grep -q -- "--shared-dir $WS:/home/claude" "$STUB_LOG" \
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
[ -f "$WS/.claude.json" ] && ok "onboarding is seeded" || bad "onboarding is seeded"
grep -q hasCompletedOnboarding "$WS/.claude.json" \
  && ok "the seed carries the onboarding flags" || bad "the seed carries the onboarding flags"
grep -qi 'token' "$WS/.claude.json" \
  && bad "a credential was written into the workspace" \
  || ok "no credential is written into the workspace"
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
"$WORK/brig" rm --all > /dev/null 2>&1
PROJ="$WORK/myproject"
mkdir -p "$PROJ"
: > "$STUB_LOG"
CLAUDE_CODE_OAUTH_TOKEN=env-token-secret \
  "$WORK/brig" --verbose run claude "$PROJ" -p hi > "$WORK/proj.out" 2> "$WORK/proj.err"
rc=$?
[ "$rc" = 0 ] && ok "a run with a project exits 0" \
  || bad "a run with a project exits 0 -- got $rc: $(cat "$WORK/proj.err")"
grep -q -- "--shared-dir $PROJ:/work/myproject" "$STUB_LOG" \
  && ok "the project is mounted at /work/<basename>" \
  || bad "the project is mounted at /work/<basename> -- got: $(grep '^argv: run' "$STUB_LOG")"
grep -q -- "--shared-dir $WS:/home/claude" "$STUB_LOG" \
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

# The one-release warning. That word used to reach the AGENT, so giving it a
# new meaning is a breaking change, and the notice names both readings so
# somebody can pick the one they meant before it goes.
grep -q 'project directory this run mounts' "$WORK/proj.err" \
  && ok "a second bare word says what it now means" \
  || bad "a second bare word says what it now means -- got: $(cat "$WORK/proj.err")"
grep -q -- 'put it after --' "$WORK/proj.err" \
  && ok "the notice names the way to keep the old reading" \
  || bad "the notice names the way to keep the old reading"

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

# And after an explicit -- there is nothing to point out: the line said the
# word is the agent's, and it still reaches the agent untouched. This run names
# no project, so it recreates the sandbox the block above left mounting one --
# which is the same rule, read in the other direction.
: > "$STUB_LOG"
CLAUDE_CODE_OAUTH_TOKEN=env-token-secret \
  "$WORK/brig" run claude --quiet -- --version > /dev/null 2> "$WORK/dashdash.err"
grep -q 'project directory this run mounts' "$WORK/dashdash.err" \
  && bad "a tail after -- was warned about -- got: $(cat "$WORK/dashdash.err")" \
  || ok "a tail after -- is not warned about"
grep -q -- '-- claude --version' "$STUB_LOG" \
  && ok "a word after -- still reaches the agent" \
  || bad "a word after -- still reaches the agent -- got: $(grep '^argv: exec' "$STUB_LOG" | tail -1)"

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
"$WORK/brig" rm --all > /dev/null 2>&1
"$WORK/brig" run claude -d > /dev/null 2>&1
: > "$STUB_LOG"
"$WORK/brig" sh claude echo hi > /dev/null 2> "$WORK/shproj.err"
grep -q -- '-- bash -lc echo hi' "$STUB_LOG" \
  && ok "sh still reads a second bare word as the guest command" \
  || bad "sh still reads a second bare word as the guest command -- got: $(grep '^argv: exec' "$STUB_LOG" | tail -1)"
grep -q 'project directory this run mounts' "$WORK/shproj.err" \
  && bad "sh warned about its own guest command" \
  || ok "sh says nothing about its guest command"

# The project is inherited by a verb that names none, the way the home already
# is. Reading that silence as "no project" is what real-runtime testing caught:
# `brig sh <ref>` after a run with a project stopped, removed and recreated the
# sandbox without the mount, taking the guest's memory-only state with it and
# overwriting the index entry that recorded the project.
#
# Driven on the ubuntu profile, which delivers no credential files and so
# reaches the guest exec on any host -- which is what makes the working
# directory assertable rather than only the boot.
"$WORK/brig" rm --all > /dev/null 2>&1
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
"$WORK/brig" rm --all > /dev/null 2>&1

echo "== the home flag =="
# --home is what sets the guest home now. -w and --workspace keep working and
# each say the one word that replaces them: a line that works today has to keep
# working, and a spelling that works silently never teaches anyone the new one.
HOMEDIR="$WORK/named-home"
: > "$STUB_LOG"
env -u BRIG_WORKSPACE "$WORK/brig" run claude --home "$HOMEDIR" -d \
  > /dev/null 2> "$WORK/home.err"
grep -q -- "--shared-dir $HOMEDIR:/home/claude" "$STUB_LOG" \
  && ok "--home is mounted as the guest home" \
  || bad "--home is mounted as the guest home -- got: $(grep '^argv: run' "$STUB_LOG")"
grep -q 'is now' "$WORK/home.err" \
  && bad "--home printed a deprecation notice -- got: $(cat "$WORK/home.err")" \
  || ok "--home prints no notice"
"$WORK/brig" rm --all > /dev/null 2>&1
for spelling in -w --workspace; do
  : > "$STUB_LOG"
  env -u BRIG_WORKSPACE "$WORK/brig" run claude "$spelling" "$HOMEDIR" -d \
    > /dev/null 2> "$WORK/oldhome.err"
  grep -q -- "--shared-dir $HOMEDIR:/home/claude" "$STUB_LOG" \
    && ok "$spelling still sets the guest home" \
    || bad "$spelling still sets the guest home -- got: $(grep '^argv: run' "$STUB_LOG")"
  grep -q "is now \`--home\`" "$WORK/oldhome.err" \
    && ok "$spelling names --home as its replacement" \
    || bad "$spelling names --home -- got: $(cat "$WORK/oldhome.err")"
  "$WORK/brig" rm --all > /dev/null 2>&1
done

echo "== network =="
# The posture the run resolved to is the posture the runtime is told about.
# Net was a hardcoded "shared" for the whole life of the field, so this is the
# check that the resolved value actually travels.
"$WORK/brig" rm --all > /dev/null 2>&1
: > "$STUB_LOG"
"$WORK/brig" --verbose run claude --offline -d > "$WORK/off.out" 2>&1
grep -q -- '--net none' "$STUB_LOG" \
  && ok "--offline reaches the runtime as --net none" \
  || bad "--offline reaches the runtime as --net none -- got: $(grep '^argv: run' "$STUB_LOG")"
grep -q '^NETWORK .*offline' "$WORK/off.out" \
  && ok "the envelope says the sandbox is offline" \
  || bad "the envelope says the sandbox is offline -- got: $(cat "$WORK/off.out")"

"$WORK/brig" rm --all > /dev/null 2>&1
: > "$STUB_LOG"
"$WORK/brig" --verbose run claude -d > "$WORK/on.out" 2>&1
grep -q -- '--net shared' "$STUB_LOG" \
  && ok "a default run still asks for the shared network" \
  || bad "a default run still asks for the shared network -- got: $(grep '^argv: run' "$STUB_LOG")"
grep -q '^NETWORK .*shared' "$WORK/on.out" \
  && ok "the envelope names the shared posture" \
  || bad "the envelope names the shared posture -- got: $(cat "$WORK/on.out")"

# isolated gives the sandbox a network of its own, which brig can only do where
# it owns the gateway. This run is pinned to vz, where the network comes from
# vmnet, so the posture is refused rather than accepted and dropped -- it was
# accepted and dropped before, and the envelope reported a network of the
# sandbox's own over a sandbox on the shared one. The negative half is the half
# worth testing: "it booted" would pass with the whole thing deleted. That two
# sandboxes on separate networks genuinely cannot reach each other is a
# real-runtime fact, recorded in docs/manual-tests.
"$WORK/brig" rm --all > /dev/null 2>&1
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
"$WORK/brig" rm --all > /dev/null 2>&1


# A posture brig does not know must stop the run rather than pick one.
out="$(BRIG_NETWORK=airgapped "$WORK/brig" info claude 2>&1)"; rc=$?
[ "$rc" != 0 ] && ok "an unknown BRIG_NETWORK refuses the run" \
  || bad "an unknown BRIG_NETWORK started anyway: $out"
"$WORK/brig" rm --all > /dev/null 2>&1

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
"$WORK/brig" rm --all > /dev/null 2>&1
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

echo "== named session =="
: > "$STUB_LOG"
"$WORK/brig" run claude --name 'My Big Refactor' -p hi > /dev/null 2>&1
grep -q -- "--shared-dir $WS-my-big-refactor:/home/claude" "$STUB_LOG" \
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
"$WORK/brig" rm --all > /dev/null 2>&1
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
"$WORK/brig" rm --all > /dev/null 2>&1
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
"$WORK/brig" rm --all > /dev/null 2>&1
rm -f "$BRIG_STATE_DIR/slug-claims.json" "$BRIG_STATE_DIR/sessions.json"
printf '{"brig-claude-code-acme-corp-prod": "acme-corp-prod"}' > "$BRIG_STATE_DIR/sessions.json"
"$WORK/brig" run claude -d > /dev/null 2>&1
grep -q 'acme-corp-prod' "$BRIG_STATE_DIR/slug-claims.json" \
  && ok "an unnamed run carries the old claims across" \
  || bad "an unnamed run dropped the old claims"

# And claims nothing of its own, which is the other half of that pair: every
# unnamed run of a profile is meant to be the one session, so there is no name
# for a later one to collide with and nothing to write down.
"$WORK/brig" rm --all > /dev/null 2>&1
rm -f "$BRIG_STATE_DIR/slug-claims.json" "$BRIG_STATE_DIR/sessions.json"
"$WORK/brig" run claude -d > /dev/null 2>&1
[ -e "$BRIG_STATE_DIR/slug-claims.json" ] \
  && bad "an unnamed run claimed a sandbox -- got: $(cat "$BRIG_STATE_DIR/slug-claims.json")" \
  || ok "an unnamed run claims nothing"

# The guard on all of that: a current sessions.json holds session entries,
# whose values are objects rather than the claims' strings, so it cannot be
# read as claims. Mistaking one for the other deletes the file, and every
# session in it loses its home.
"$WORK/brig" rm --all > /dev/null 2>&1
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
"$WORK/brig" rm --all > /dev/null 2>&1

echo "== the slug migration =="
# A --name longer than ten characters used to be cut to that, so it had a
# shorter sandbox and a shorter home than it gets now. Both come off the slug on
# every run, so such a session boots a new pair and the old one is orphaned: the
# work in the old workspace is on the host and is still there, state inside the
# old guest is not. Say so, and name both directories so the old one can be
# moved or deleted.
"$WORK/brig" rm --all > /dev/null 2>&1
mkdir -p "$WS-refactorin"
"$WORK/brig" run claude --name refactoring -d > /dev/null 2> "$WORK/moved.err"
grep -q -- "instead of $WS-refactorin (brig-claude-code-refactorin)" "$WORK/moved.err" \
  && ok "the migration notice names the home that was left behind" \
  || bad "the notice does not name the old home -- got: $(cat "$WORK/moved.err")"
grep -q -- "new sandbox: $WS-refactoring (brig-claude-code-refactoring)" "$WORK/moved.err" \
  && ok "the migration notice names the home in use now" \
  || bad "the notice does not name the new home -- got: $(cat "$WORK/moved.err")"

# Nothing orphaned, nothing said. This name slugs differently than it used to,
# but no directory was ever created under the old slug, so there is nothing for
# the reader to move -- and a notice that reaches people it does not apply to is
# one they learn to skip.
"$WORK/brig" rm --all > /dev/null 2>&1
rm -rf "$WS-benchmarkin"
"$WORK/brig" run claude --name benchmarking -d > /dev/null 2> "$WORK/fresh.err"
grep -q 'used to be shortened' "$WORK/fresh.err" \
  && bad "a session with nothing orphaned was warned -- got: $(cat "$WORK/fresh.err")" \
  || ok "a session with nothing orphaned is not warned"

# And a name the old budget left alone slugs exactly as it always did, so its
# directory being there means nothing. Ten characters is the boundary, and this
# is on the silent side of it.
"$WORK/brig" rm --all > /dev/null 2>&1
mkdir -p "$WS-exactlyten"
"$WORK/brig" run claude --name exactlyten -d > /dev/null 2> "$WORK/short.err"
grep -q 'used to be shortened' "$WORK/short.err" \
  && bad "a name that was never cut was warned -- got: $(cat "$WORK/short.err")" \
  || ok "a name the old budget left alone is not warned"

# The ref form refused any label over ten characters. A long one is a sandbox
# and a directory of its own now, and is refused only for what is in it.
"$WORK/brig" rm --all > /dev/null 2>&1
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
"$WORK/brig" rm --all > /dev/null 2>&1

echo "== stop =="
: > "$STUB_LOG"
"$WORK/brig" stop claude > /dev/null 2>&1
grep -q '^argv: stop brig-claude-code' "$STUB_LOG" \
  && ok "stop stops the sandbox" || bad "stop stops the sandbox"
# Stopping must not resolve credentials: on a real host that raises a
# keychain prompt for a command that needs none.
grep -q 'CLAUDE_CODE_OAUTH_TOKEN' "$STUB_LOG" \
  && bad "stop resolved credentials" || ok "stop resolves no credentials"

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
"$WORK/brig" rm claude > /dev/null 2>&1
grep -q '^argv: rm brig-claude-code' "$STUB_LOG" \
  && ok "rm removes the sandbox" || bad "rm removes the sandbox"
[ -d "$WS" ] && ok "rm leaves the workspace alone" || bad "rm deleted the workspace"

# A sandbox for `rm --all` to remove.
"$WORK/brig" run claude -d > /dev/null 2>&1

: > "$STUB_LOG"
"$WORK/brig" rm --all > /dev/null 2>&1
grep -q '^argv: rm brig-claude-code' "$STUB_LOG" \
  && ok "rm --all removes brig sandboxes" || bad "rm --all removes brig sandboxes"

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
"$WORK/brig" rm --all > /dev/null 2>&1
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
"$WORK/brig" rm --all > /dev/null 2>&1

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
"$WORK/brig" rm --all > /dev/null 2>&1

# The sandbox-keyed file the session index replaces is deleted rather than
# migrated: its keys cannot be read back into a ref without guessing which dash
# separated the agent from its label. One restart per session in it, which is
# what an absent entry has always cost.
printf '{"brig-claude-code-rc23": "%s"}' "$WORK/ws-legacy" > "$BRIG_STATE_DIR/workspaces.json"
brig_bare run claude --name rc23 -w "$RC" -d > /dev/null 2>&1
[ -e "$BRIG_STATE_DIR/workspaces.json" ] \
  && bad "the old sandbox-keyed index was left behind" \
  || ok "the old sandbox-keyed index is deleted on sight"
"$WORK/brig" rm --all > /dev/null 2>&1

# The index is bookkeeping, so an unusable one costs a restart and nothing
# more: every command still works, and the workspace resolves as it did before
# the file existed.
printf '{not json at all' > "$BRIG_STATE_DIR/sessions.json"
brig_bare run claude -d > "$WORK/corrupt.out" 2>&1
rc=$?
[ "$rc" = 0 ] && ok "a corrupt index is ignored rather than fatal" \
  || bad "a corrupt index failed the run -- got: $(cat "$WORK/corrupt.out")"
"$WORK/brig" rm --all > /dev/null 2>&1

echo "== argument hygiene =="
# A flag typed to make a destructive command safe must not be read past and
# ignored. `rm --all` takes nothing beside --all, so --dry-run is refused
# (exit 2) and removes nothing rather than stopping every sandbox.
"$WORK/brig" run claude -d > /dev/null 2>&1
: > "$STUB_LOG"
"$WORK/brig" rm --all --dry-run > "$WORK/dry.out" 2>&1; rc=$?
[ "$rc" = 2 ] && ok "rm --all --dry-run exits 2" || bad "rm --all --dry-run exits 2 -- got $rc"
grep -q -- '--dry-run' "$WORK/dry.out" \
  && ok "rm --all --dry-run names the token" || bad "rm --all --dry-run names the token"
grep -q '^argv: rm ' "$STUB_LOG" \
  && bad "rm --all --dry-run removed a sandbox" || ok "rm --all --dry-run removes nothing"
# A ref beside --all is two requests on one line, so it is refused rather than
# resolved to either of them.
out="$("$WORK/brig" rm claude --all 2>&1)"; rc=$?
[ "$rc" = 2 ] && ok "rm claude --all exits 2" || bad "rm claude --all exits 2 -- got $rc: $out"
case "$out" in
  *claude*) ok "rm claude --all names the ref it will not take" ;;
  *) bad "rm claude --all names the ref -- got: $out" ;;
esac
"$WORK/brig" rm --all > /dev/null 2>&1

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
"$WORK/brig" rm --all > /dev/null 2>&1
# A name set through BRIG_NAME still carries the prefix, so ls lists it and
# `rm --all` removes it the way they do any brig sandbox.
BRIG_NAME=brig-custom "$WORK/brig" run claude -d > /dev/null 2>&1
listing="$("$WORK/brig" ls 2>/dev/null)"
case "$listing" in
  *brig-custom*) ok "a BRIG_NAME sandbox appears in ls" ;;
  *) bad "a BRIG_NAME sandbox appears in ls -- got: $listing" ;;
esac
: > "$STUB_LOG"
"$WORK/brig" rm --all > /dev/null 2>&1
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
"$WORK/brig" rm --all > /dev/null 2>&1

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

echo "== flags =="
: > "$STUB_LOG"
"$WORK/brig" run claude -t ghcr.io/me/img:latest -w "$WORK/other" -m 8192 --cpus 2 -d \
  > /dev/null 2>&1
grep -q -- '--mem 8192 --cpus 2' "$STUB_LOG" \
  && ok "-m and --cpus reach the runtime" || bad "-m and --cpus reach the runtime"
grep -q -- "--shared-dir $WORK/other:/home/claude" "$STUB_LOG" \
  && ok "-w overrides the workspace" || bad "-w overrides the workspace"
grep -q 'ghcr.io/me/img:latest' "$STUB_LOG" \
  && ok "-t overrides the image" || bad "-t overrides the image"
"$WORK/brig" rm --all > /dev/null 2>&1

echo "== ubuntu =="
# The command goes after --, because run's second bare word is the project
# directory now: for a shell profile the trailing words are the guest command,
# and -- is what still says so.
: > "$STUB_LOG"
"$WORK/brig" run ubuntu -- uname -a > /dev/null 2>&1
grep -q -- '-- bash -lc uname -a' "$STUB_LOG" \
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
"$WORK/brig" agent rm mytool < /dev/null > /dev/null 2>&1
[ -f "$BRIG_PROFILE_DIR/mytool.yaml" ] \
  && bad "rm did not remove the copy under the name it was given" \
  || ok "rm removes the copy under the name it was given, asking nothing"
"$WORK/brig" rm --all > /dev/null 2>&1

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
# resolves to, which is how brig learns the digest to verify and boot.
if [ "$1" = triangulate ]; then
  printf '%s:sha256-%s.sig\n' "${2%%:*}" "$(printf 'a%.0s' $(seq 64))"
  exit 0
fi
[ "${COSIGN_FAIL:-0}" = 1 ] && { echo "Error: no matching signatures"; exit 1; }
exit 0
COSIGN
chmod +x "$WORK/bin/cosign"
export BRIG_COSIGN_BIN="$WORK/bin/cosign"

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
# One line for the whole step, not one per check: the shipped profiles boot a
# downloaded kernel as well as an image, so both checks reach the same summary.
case "$out" in
  *"brig: image and boot assets verified"*) ok "a default run says verification held" ;;
  *) bad "a default run says verification held -- got: $out" ;;
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

unset BRIG_COSIGN_BIN
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
# before booting anything -- a real subcommand of a real runtime, which the
# stub below is not. This test is about what reaches argv, so pin the backend
# to the one that needs no gateway rather than teach the stub to fake one.
export BRIG_HYPERVISOR=vz

echo "== sh =="
: > "$STUB_LOG"
"$WORK/brig" sh claude echo hi there > /dev/null 2>"$WORK/shell.err"
grep -q -- '-- bash -lc echo hi there' "$STUB_LOG" \
  && ok "a trailing command reaches bash as one argument" \
  || bad "a trailing command reaches bash as one argument"
grep -q '^SANDBOX ' "$WORK/shell.err" \
  && bad "sh printed the envelope" || ok "sh does not print the envelope"

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
retired 'brig sh' shell claude echo hi
retired 'brig info' env claude
retired 'brig run -d' create claude
retired '<agent>@<label>' run claude --name retn -d
retired '<agent>@<label>' run claude -n retn -d
# Last of the group: it removes what the others were acting on.
retired 'brig rm --all' reset

echo "== what a run says =="
# By default, print what the user has to act on. brig's own progress and the
# runtime's own output wait for --verbose; -q is identifiers and errors.
"$WORK/brig" rm --all > /dev/null 2>&1
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

# --verbose asks for both, and gets both.
"$WORK/brig" rm --all > /dev/null 2>&1
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
"$WORK/brig" rm --all > /dev/null 2>&1
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
"$WORK/brig" rm --all > /dev/null 2>&1
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
"$WORK/brig" rm --all > /dev/null 2>&1
CLAUDE_CODE_OAUTH_TOKEN=env-token-secret STUB_RUN_FAIL=1 \
  "$WORK/brig" -q run claude -d > /dev/null 2> "$WORK/qfail.err"
rc=$?
[ "$rc" != 0 ] && ok "brig -q still fails a boot the runtime refused" \
  || bad "brig -q exited 0 on a boot the runtime refused"
grep -q 'no space left on device' "$WORK/qfail.err" \
  && ok "brig -q still quotes what the runtime said" \
  || bad "brig -q still quotes what the runtime said -- got: $(cat "$WORK/qfail.err")"

# The same flag after the verb, which is where it used to live. It keeps
# working for this release and names the position it moved to.
"$WORK/brig" rm --all > /dev/null 2>&1
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

[ "$fail" = 0 ] && echo PASS || echo FAILURES
exit "$fail"
