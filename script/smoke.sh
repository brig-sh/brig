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
    # Remember the instance name and the shared directory the way a real
    # runtime binds them at boot.
    name=""; share=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --name) name="$2"; shift 2 ;;
        --shared-dir) share="$2"; shift 2 ;;
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
          target="${5:-}"
          line=$(grep " $target " "$STUB_STATE.mounts" 2>/dev/null | tail -1)
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
# Never touch the real keychain from a test.
export BRIG_CREDENTIALS_CMD='printf {"claudeAiOauth":{"accessToken":"host-token","expiresAt":99999999999999}}'

echo "== run =="
CLAUDE_CODE_OAUTH_TOKEN=env-token-secret GH_TOKEN=gh-secret \
  "$WORK/brig" run claude -p hi > "$WORK/run.out" 2> "$WORK/run.err"
rc=$?
[ "$rc" = 0 ] && ok "run exits 0" || bad "run exits 0 -- got $rc: $(cat "$WORK/run.err")"

# The whole reason brig builds the command line itself: a forwarded value
# must never be readable in `ps`.
if grep -q 'env-token-secret\|gh-secret\|host-token' "$STUB_LOG"; then
  if grep '^argv:' "$STUB_LOG" | grep -q 'env-token-secret\|gh-secret\|host-token'; then
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

echo "== denylist =="
: > "$STUB_LOG"
out="$(ANTHROPIC_API_KEY=sk-metered BRIG_FORWARD_ENV='ANTHROPIC_API_KEY GH_TOKEN' \
  GH_TOKEN=gh-secret "$WORK/brig" env claude 2>&1)"
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
out="$(GH_TOKEN='op://vault/item/field' "$WORK/brig" env claude 2>&1)"
case "$out" in
  *"unresolved secret reference"*) ok "a secret-manager reference is not forwarded" ;;
  *) bad "a secret-manager reference is not forwarded -- got: $out" ;;
esac

echo "== named session =="
: > "$STUB_LOG"
"$WORK/brig" run claude --name 'My Big Refactor' -p hi > /dev/null 2>&1
grep -q -- "--shared-dir $WS-my-big-ref:/home/claude" "$STUB_LOG" \
  && ok "a named session gets its own workspace" || bad "named session gets its own workspace"
grep -q -- '--name brig-claude-code-my-big-ref' "$STUB_LOG" \
  && ok "a named session gets its own sandbox" || bad "named session gets its own sandbox"
grep -q -- "-- claude --name My Big Refactor -p hi" "$STUB_LOG" \
  && ok "the raw name reaches the agent" || bad "the raw name reaches the agent"

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
out="$("$WORK/brig" create claude 2>/dev/null)"
[ "$out" = brig-claude-code ] && ok "create prints the sandbox name" \
  || bad "create prints the sandbox name -- got '$out'"
grep -q -- '-- claude' "$STUB_LOG" \
  && bad "create started the agent" || ok "create starts the sandbox, not the agent"

listing="$("$WORK/brig" ls 2>/dev/null)"
case "$listing" in
  *brig-claude-code*running*) ok "ls shows a running sandbox" ;;
  *) bad "ls shows a running sandbox -- got: $listing" ;;
esac
case "$listing" in
  *"$WS"*) ok "ls names the workspace" ;;
  *) bad "ls names the workspace -- got: $listing" ;;
esac

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

out="$("$WORK/brig" run claude -d 2>/dev/null)"
[ "$out" = brig-claude-code ] && ok "run -d prints the name and exits" \
  || bad "run -d prints the name -- got '$out'"

: > "$STUB_LOG"
"$WORK/brig" reset > /dev/null 2>&1
grep -q '^argv: rm brig-claude-code' "$STUB_LOG" \
  && ok "reset removes brig sandboxes" || bad "reset removes brig sandboxes"

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
brig_bare create claude --name rc23 -w "$RC" > /dev/null 2>&1
: > "$STUB_LOG"
brig_bare exec claude --name rc23 -- uname -a > /dev/null 2>&1
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
brig_bare exec claude --name rc23 -w "$WORK/ws-other" -- uname -a > /dev/null 2>&1
grep -q '^argv: run ' "$STUB_LOG" \
  && ok "a -w naming a different directory still restarts the sandbox" \
  || bad "a -w naming a different directory still restarts the sandbox"

brig_bare rm claude --name rc23 > /dev/null 2>&1
grep -q 'brig-claude-code-rc23' "$BRIG_STATE_DIR/workspaces.json" \
  && bad "rm left the sandbox in the workspace index" \
  || ok "rm drops the remembered workspace"

brig_bare create claude --name rc23 -w "$RC" > /dev/null 2>&1
"$WORK/brig" reset > /dev/null 2>&1
grep -q 'brig-claude-code-rc23' "$BRIG_STATE_DIR/workspaces.json" \
  && bad "reset left a sandbox in the workspace index" \
  || ok "reset drops the remembered workspaces"

# The index is bookkeeping, so an unusable one costs a restart and nothing
# more: every command still works, and the workspace resolves as it did before
# the file existed.
printf '{not json at all' > "$BRIG_STATE_DIR/workspaces.json"
brig_bare run claude -d > "$WORK/corrupt.out" 2>&1
rc=$?
[ "$rc" = 0 ] && ok "a corrupt index is ignored rather than fatal" \
  || bad "a corrupt index failed the run -- got: $(cat "$WORK/corrupt.out")"
"$WORK/brig" reset > /dev/null 2>&1

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
"$WORK/brig" reset > /dev/null 2>&1

echo "== ubuntu =="
: > "$STUB_LOG"
"$WORK/brig" run ubuntu uname -a > /dev/null 2>&1
grep -q -- '-- bash -lc uname -a' "$STUB_LOG" \
  && ok "a shell profile runs the command in a shell" \
  || bad "a shell profile runs the command in a shell"
grep -q -- ':/root/work' "$STUB_LOG" \
  && ok "ubuntu mounts the workspace at /root/work" || bad "ubuntu mounts at /root/work"

echo "== profiles =="
# YAML is the default spelling, and the header is what makes an export a
# starting point rather than a puzzle.
"$WORK/brig" profile export claude-code > "$WORK/mine.yaml" 2>/dev/null
grep -q '^# A brig profile' "$WORK/mine.yaml" \
  && ok "export explains its own fields" || bad "export explains its own fields"
printf '\n# why: the vendored CLI needs a bigger guest\n' >> "$WORK/mine.yaml"
sed -i.bak 's/^name: claude-code$/name: mine/; s|ghcr.io/brig-sh/claude-code[^ ]*|docker.io/me/mine:latest|' \
  "$WORK/mine.yaml"
"$WORK/brig" profile import "$WORK/mine.yaml" > "$WORK/import.out" 2>&1 \
  && ok "an edited YAML export imports back" || bad "an edited YAML export imports back: $(cat "$WORK/import.out")"
grep -q 'why: the vendored CLI' "$BRIG_PROFILE_DIR/mine.yaml" 2>/dev/null \
  && ok "import keeps the comments you wrote" || bad "import keeps the comments you wrote"

# JSON still parses, since it is a subset of YAML.
printf '{"name":"jsonagent","image":"docker.io/me/j:latest","guestHome":"/home/j","binary":"j","mem":1024,"cpus":1}' \
  > "$WORK/j.json"
"$WORK/brig" profile import "$WORK/j.json" > /dev/null 2>&1 \
  && ok "a JSON profile still imports" || bad "a JSON profile still imports"
[ -f "$BRIG_PROFILE_DIR/jsonagent.json" ] \
  && ok "a JSON profile keeps its own extension" || bad "JSON profile keeps its extension"
profiles="$("$WORK/brig" profiles 2>/dev/null)"
case "$profiles" in
  *"mine"*"(file)"*) ok "a profile of your own is listed, and marked" ;;
  *) bad "a profile of your own is listed -- got: $profiles" ;;
esac
grep -q 'cannot verify its signature' "$WORK/import.out" \
  && ok "import says an outside image cannot be verified" \
  || bad "import says an outside image cannot be verified: $(cat "$WORK/import.out")"
case "$profiles" in
  *bring-your-own-image*) ok "profiles points at the image documentation" ;;
  *) bad "profiles points at the image documentation" ;;
esac
# A name that would escape the profile directory, or collide with a path.
printf 'name: ../evil\nimage: i\nguestHome: /home/x\nbinary: x\nmem: 1\ncpus: 1\n' \
  | "$WORK/brig" profile import - > /dev/null 2>&1 \
  && bad "an unsafe profile name was accepted" || ok "an unsafe profile name is refused"
# A misspelled field would forward no credentials, which looks exactly like a
# broken sandbox.
printf 'name: typo\nimage: i\nguestHome: /home/x\nbinary: x\nmem: 1\ncpus: 1\nforwards: [GH_TOKEN]\n' \
  | "$WORK/brig" profile import - > /dev/null 2>&1 \
  && bad "a misspelled field was accepted" || ok "a misspelled field is refused"

# The built-ins are embedded, so nothing is pre-seeded: every profile comes from
# the binary until you put a file there yourself.
[ -f "$BRIG_PROFILE_DIR/claude-code.yaml" ] \
  && bad "brig pre-seeded the profile directory" \
  || ok "brig does not pre-seed the profile directory"

# Export writes the file brig ships, comments and all -- that is what makes
# "start from the closest profile" work rather than handing back a struct dump.
# With no destination it goes to stdout, which is also how you get a copy
# anywhere other than the profile directory.
"$WORK/brig" profile export codex > "$WORK/codex.yaml" 2>/dev/null
grep -q 'metered path' "$WORK/codex.yaml" 2>/dev/null \
  && ok "export keeps the comments explaining the deny list" \
  || bad "export keeps the comments explaining the deny list"

# A destination is a name and nothing else. brig writes one directory, so a
# path -- or a typo that looks like one -- is refused rather than honoured.
"$WORK/brig" profile export codex "$WORK/escape.yaml" > /dev/null 2>&1 \
  && bad "export wrote to a path outside the profile directory" \
  || ok "export refuses a path destination"
[ -f "$WORK/escape.yaml" ] \
  && bad "export wrote outside the profile directory" \
  || ok "export writes the profile directory and nowhere else"

# A bare destination is a name, and brig resolves it in the profile directory.
"$WORK/brig" profile export codex bare > /dev/null 2>&1
[ -f "$BRIG_PROFILE_DIR/bare.yaml" ] \
  && ok "a bare export destination lands in the profile directory" \
  || bad "a bare export destination lands in the profile directory"
"$WORK/brig" profile export codex bare > /dev/null 2>&1 \
  && bad "export overwrote an existing file" \
  || ok "export refuses to overwrite without --force"
"$WORK/brig" profile export codex bare --force > /dev/null 2>&1 \
  && ok "--force overwrites" || bad "--force overwrites"
# A mistyped flag must not become a file name.
"$WORK/brig" profile export codex --jsonn > /dev/null 2>&1 \
  && bad "a mistyped flag was taken as a destination" \
  || ok "a mistyped flag is refused rather than written"
ls "$BRIG_PROFILE_DIR" | grep -q -- '--json' \
  && bad "a file was written for a mistyped flag" \
  || ok "no file is written for a mistyped flag"
# rm resolves the name inside the file, not the file name: bare.yaml declares
# codex, so that is what removes it.
"$WORK/brig" profile rm codex > /dev/null 2>&1
[ -f "$BRIG_PROFILE_DIR/bare.yaml" ] \
  && bad "rm did not resolve the profile inside the file" \
  || ok "rm resolves the profile a file declares, not its file name"

# Two files can declare one profile, because a file need not be named after the
# profile in it. brig says which won, and rm takes both -- removing only the
# winner would promote the other and leave the profile listed.
"$WORK/brig" profile export codex dup > /dev/null 2>&1
sed 's/^name: codex$/name: dupagent/' "$BRIG_PROFILE_DIR/dup.yaml" > "$WORK/dup2.yaml"
cp "$WORK/dup2.yaml" "$BRIG_PROFILE_DIR/dup.yaml"
cp "$WORK/dup2.yaml" "$BRIG_PROFILE_DIR/dupother.yaml"
"$WORK/brig" profiles > "$WORK/dup.out" 2>&1
grep -q 'duplicate profile name' "$WORK/dup.out" \
  && ok "two files claiming one profile are reported" \
  || bad "two files claiming one profile are reported: $(cat "$WORK/dup.out")"
"$WORK/brig" profile rm dupagent > /dev/null 2>&1
{ [ -f "$BRIG_PROFILE_DIR/dup.yaml" ] || [ -f "$BRIG_PROFILE_DIR/dupother.yaml" ]; } \
  && bad "rm left a file still declaring the profile" \
  || ok "rm takes every file declaring the profile"

# A file of your own shadows the built-in, and the listing says so rather than
# leaving you to wonder which image is booting.
"$WORK/brig" profile export claude-code claude-code > /dev/null 2>&1
sed -i.bak 's|ghcr.io/brig-sh/claude-code:[^ ]*|docker.io/me/pinned:latest|' \
  "$BRIG_PROFILE_DIR/claude-code.yaml"
rm -f "$BRIG_PROFILE_DIR/claude-code.yaml.bak"
case "$("$WORK/brig" profiles 2>/dev/null)" in
  *"overrides built-in"*) ok "a file shadowing a built-in is marked as one" ;;
  *) bad "a file shadowing a built-in is marked as one" ;;
esac
rm -f "$BRIG_PROFILE_DIR/claude-code.yaml"

# edit needs a file. A built-in has none, and nothing is created for it.
EDITOR=true "$WORK/brig" profile edit codex > "$WORK/edit.out" 2>&1 \
  && bad "profile edit accepted a built-in" || ok "profile edit refuses a built-in"
grep -q 'built in' "$WORK/edit.out" \
  && ok "profile edit says how to make a file" \
  || bad "profile edit says how to make a file: $(cat "$WORK/edit.out")"
[ -f "$BRIG_PROFILE_DIR/codex.yaml" ] \
  && bad "profile edit created a file for a built-in" \
  || ok "profile edit creates nothing for a built-in"

# ...and opens one that exists, honouring the editor's own arguments.
mkdir -p "$WORK/bin"
cat > "$WORK/bin/fake-editor" <<'EDIT'
#!/bin/sh
printf '\n# edited by the smoke test\n' >> "$1"
EDIT
chmod +x "$WORK/bin/fake-editor"
EDITOR="$WORK/bin/fake-editor" "$WORK/brig" profile edit mine > /dev/null 2>&1 \
  && ok "profile edit opens a file-backed profile" \
  || bad "profile edit opens a file-backed profile"
grep -q 'edited by the smoke test' "$BRIG_PROFILE_DIR/mine.yaml" \
  && ok "profile edit saves what the editor wrote" \
  || bad "profile edit saves what the editor wrote"

# The old spellings keep working for one release, and say so.
"$WORK/brig" agents > "$WORK/dep.out" 2>&1
grep -q 'is now `brig profiles`' "$WORK/dep.out" \
  && ok "brig agents works and names the new spelling" \
  || bad "brig agents works and names the new spelling: $(cat "$WORK/dep.out")"
"$WORK/brig" template ls > "$WORK/dep2.out" 2>&1
grep -q 'is now `brig profile`' "$WORK/dep2.out" \
  && ok "brig template works and names the new spelling" \
  || bad "brig template works and names the new spelling"
"$WORK/brig" template edit mine > /dev/null 2>&1 \
  && bad "brig template edit exists" || ok "there is no brig template edit"

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
out="$(BRIG_VERIFY=warn "$WORK/brig" run claude -p hi 2>&1)"
case "$out" in
  *"signature verified"*) ok "a signature that checks out is reported" ;;
  *) bad "a signature that checks out is reported -- got: $out" ;;
esac
grep 'argv: run ' "$STUB_LOG" | tail -1 | grep -q '@sha256:' \
  && ok "the verified digest is what hull was told to boot" \
  || bad "hull was told to boot the tag, not the verified digest: $(grep 'argv: run ' "$STUB_LOG" | tail -1)"

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
[ "$rc" != 0 ] && ok "a bad signature stops the boot with no terminal to ask" \
  || bad "a bad signature booted anyway"

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
[ "$rc" != 0 ] && ok "require refuses what it cannot check" || bad "require booted an unchecked image"

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

echo "== shell =="
: > "$STUB_LOG"
"$WORK/brig" shell claude echo hi there > /dev/null 2>&1
grep -q -- '-- bash -lc echo hi there' "$STUB_LOG" \
  && ok "a trailing command reaches bash as one argument" \
  || bad "a trailing command reaches bash as one argument"

[ "$fail" = 0 ] && echo PASS || echo FAILURES
exit "$fail"
