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
    while [ $# -gt 0 ] && [ "$1" != "--" ]; do shift; done
    shift
    case "$1" in
      /bin/true) exit 0 ;;
      cat) cat "$(cat "$STUB_STATE.share")/.brig-workspace" 2>/dev/null ;;
      *) printf 'env-token:%s\n' "${CLAUDE_CODE_OAUTH_TOKEN:-<unset>}" >> "$STUB_LOG" ;;
    esac
    ;;
  stop)
    [ -f "$STUB_STATE" ] && mv "$STUB_STATE" "$STUB_STATE.stopped"
    ;;
  rm)
    rm -f "$STUB_STATE" "$STUB_STATE.stopped"
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
# The image checks get their own cases below, with a stub cosign. Everywhere
# else they are off: a CI runner has no cosign and must not reach a registry.
export BRIG_VERIFY=off
# Custom templates go in a scratch directory, never the caller's own.
export BRIG_TEMPLATE_DIR="$WORK/templates"
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
grep '^argv:' "$STUB_LOG" | grep -q -- '--env CLAUDE_CODE_OAUTH_TOKEN --env GH_TOKEN' \
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
  && ok "a shell template runs the command in a shell" \
  || bad "a shell template runs the command in a shell"
grep -q -- ':/root/work' "$STUB_LOG" \
  && ok "ubuntu mounts the workspace at /root/work" || bad "ubuntu mounts at /root/work"

echo "== templates =="
# YAML is the default spelling, and the header is what makes an export a
# starting point rather than a puzzle.
"$WORK/brig" export claude-code > "$WORK/mine.yaml" 2>/dev/null
grep -q '^# A brig agent template' "$WORK/mine.yaml" \
  && ok "export explains its own fields" || bad "export explains its own fields"
printf '\n# why: the vendored CLI needs a bigger guest\n' >> "$WORK/mine.yaml"
sed -i.bak 's/^name: claude-code$/name: mine/; s|ghcr.io/brig-sh/claude-code:[^ ]*|docker.io/me/mine:latest|' \
  "$WORK/mine.yaml"
"$WORK/brig" import "$WORK/mine.yaml" > "$WORK/import.out" 2>&1 \
  && ok "an edited YAML export imports back" || bad "an edited YAML export imports back: $(cat "$WORK/import.out")"
grep -q 'why: the vendored CLI' "$BRIG_TEMPLATE_DIR/mine.yaml" 2>/dev/null \
  && ok "import keeps the comments you wrote" || bad "import keeps the comments you wrote"

# JSON still parses, since it is a subset of YAML.
printf '{"name":"jsonagent","image":"docker.io/me/j:latest","guestHome":"/home/j","binary":"j","mem":1024,"cpus":1}' \
  > "$WORK/j.json"
"$WORK/brig" import "$WORK/j.json" > /dev/null 2>&1 \
  && ok "a JSON template still imports" || bad "a JSON template still imports"
[ -f "$BRIG_TEMPLATE_DIR/jsonagent.json" ] \
  && ok "a JSON template keeps its own extension" || bad "JSON template keeps its extension"
agents="$("$WORK/brig" agents 2>/dev/null)"
case "$agents" in
  *"mine"*"(custom)"*) ok "a custom template is listed, and marked custom" ;;
  *) bad "custom template is listed -- got: $agents" ;;
esac
grep -q 'cannot verify its signature' "$WORK/import.out" \
  && ok "import says an outside image cannot be verified" \
  || bad "import says an outside image cannot be verified: $(cat "$WORK/import.out")"
case "$agents" in
  *bring-your-own-image*) ok "agents points at the image documentation" ;;
  *) bad "agents points at the image documentation" ;;
esac
# A name that would escape the template directory, or collide with a path.
printf 'name: ../evil\nimage: i\nguestHome: /home/x\nbinary: x\nmem: 1\ncpus: 1\n' \
  | "$WORK/brig" import - > /dev/null 2>&1 \
  && bad "an unsafe template name was accepted" || ok "an unsafe template name is refused"
# A misspelled field would forward no credentials, which looks exactly like a
# broken sandbox.
printf 'name: typo\nimage: i\nguestHome: /home/x\nbinary: x\nmem: 1\ncpus: 1\nforwards: [GH_TOKEN]\n' \
  | "$WORK/brig" import - > /dev/null 2>&1 \
  && bad "a misspelled field was accepted" || ok "a misspelled field is refused"

echo "== image verification =="
# A stub cosign, so the decision table is exercised without a network.
mkdir -p "$WORK/bin"
cat > "$WORK/bin/cosign" <<'COSIGN'
#!/bin/bash
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

echo "== shell =="
: > "$STUB_LOG"
"$WORK/brig" shell claude echo hi there > /dev/null 2>&1
grep -q -- '-- bash -lc echo hi there' "$STUB_LOG" \
  && ok "a trailing command reaches bash as one argument" \
  || bad "a trailing command reaches bash as one argument"

[ "$fail" = 0 ] && echo PASS || echo FAILURES
exit "$fail"
