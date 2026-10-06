#!/bin/bash
# Real-runtime checks for brig on a SIP-enabled Mac.
#
# It installs hull from the newest hull@main channel release and brig and
# brigd built from this checkout, then boots real microVMs through them. It
# runs the Gatekeeper gate and the macOS half of the checks, and writes
# results.json (schema 1) for render-report.py.
#
# The Mac is shared, so everything lives under a scratch HOME the caller
# makes and removes: hull's store, ~/.brig, ~/.config/brig, the Go caches.
# The script refuses to run with the account's real HOME.
# script/e2e/cleanup-macos.sh stops what is left and removes the scratch HOME.
#
# Settings:
#
#   LEVEL           canary (default) or nightly, which boots more often
#   E2E_OUT         where logs/ go (default $HOME/e2e-out)
#   E2E_RESULTS     the results file (default $E2E_OUT/results.json)
#   BRIG_BUILD_DIR  a directory holding brig and brigd built from this checkout
#   E2E_GATEKEEPER  1 to run the Gatekeeper gate against the notarized channel
#                   build of this commit. Otherwise the gate is skipped
#   GH_TOKEN        a token for the GitHub API, optional
#
# It exits 1 on a no-go verdict, after results.json is written. Written for
# the bash 3.2 that macOS ships.

# Guest command text is single-quoted, so that the guest's shell expands it.
# shellcheck disable=SC2016

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"

LEVEL="${LEVEL:-canary}"
case "$LEVEL" in
  canary)  SEQ_N=3;  CLAUDE_N=3 ;;
  nightly) SEQ_N=10; CLAUDE_N=10 ;;
  *) echo "canary-macos: LEVEL must be canary or nightly, not $LEVEL" >&2; exit 2 ;;
esac

REAL_HOME="$(dscl . -read "/Users/$(id -un)" NFSHomeDirectory | awk '{ print $2 }')"
case "$HOME" in
  "$REAL_HOME" | "" | /)
    echo "canary-macos: HOME is $HOME. Point it at a scratch directory first." >&2
    exit 2 ;;
esac
SCRATCH="$HOME"

OUT="${E2E_OUT:-$HOME/e2e-out}"
RESULTS="${E2E_RESULTS:-$OUT/results.json}"
LOGS="$OUT/logs"
mkdir -p "$LOGS"
export E2E_RECORDS="$OUT/records.jsonl"
: > "$E2E_RECORDS"
exec > >(tee -a "$LOGS/canary.log") 2>&1

export DO_NOT_TRACK=1
unset DOCKER_CONFIG BRIG_NETWORK BRIG_HYPERVISOR
BIN="$SCRATCH/.e2e/bin"
mkdir -p "$BIN"
export PATH="$BIN:$PATH"
# There is no Homebrew install here to put hull on PATH for brig, so brig is
# told where it is.
export BRIG_RUNTIME_BIN="$BIN/hull"

# ---------------------------------------------------------------- helpers

say() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*"; }
res() { python3 "$HERE/results.py" "$@"; }
now() { perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }
dt() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.2f", b - a }'; }
one_line() { tr '\n' ' ' | tr -s ' ' | cut -c1-"${1:-400}"; }
field() { res field - "$1"; }

# tmo SECONDS CMD...: runs CMD, and SIGALRM ends it after SECONDS. macOS has
# no timeout(1).
tmo() {
  local s=$1
  shift
  perl -e 'alarm shift @ARGV; exec @ARGV or die "exec: $!"' "$s" "$@"
}

# run SECONDS CMD...: logs CMD, runs it bounded and returns its status.
run() {
  local s=$1
  shift
  echo "\$ $*"
  local rc=0
  tmo "$s" "$@" < /dev/null || rc=$?
  echo "[exit $rc]"
  return "$rc"
}

# gsh REF TEXT: runs the script TEXT in the guest with `brig sh REF -c`, on a
# pty from script(1), and prints what the guest said. script(1) echoes ^D and
# two backspaces ahead of the output, and the pty adds CRs, so both go. brig's
# own notices share the pty, so its "brig: " lines go to the log only.
gsh() {
  local ref=$1 text=$2 out rc=0
  out="$(tmo "${GSH_TIMEOUT:-300}" script -q /dev/null brig sh "$ref" -c "$text" < /dev/null)" || rc=$?
  out="$(printf '%s' "$out" | tr -d '\r\b' | sed 's/^\^D//')"
  printf '$ brig sh %s -c %q\n%s\n[exit %s]\n' "$ref" "$text" "$out" "$rc" >&2
  printf '%s\n' "$out" | grep -v '^brig: ' || true
  return "$rc"
}

# ours: the monitor processes that belong to this scratch HOME.
ours() {
  pgrep -fl 'hvi|vz-runner|qemu-system' 2> /dev/null | grep -F "$SCRATCH" |
    grep -v -E 'network-gateway|pgrep|grep' || true
}

# hull_rows: the instances hull lists, without its header.
hull_rows() {
  tmo 60 hull ps < /dev/null 2>&1 | awk 'NR > 1 && NF > 0' || true
}

leftovers() {
  local tag=$1 bad="" out
  out="$(tmo 60 brig ls -q < /dev/null 2>&1 | grep -v '^(none' || true)"
  [ -z "$out" ] || bad+="brig ls: $(echo "$out" | one_line 200); "
  out="$(hull_rows)"
  [ -z "$out" ] || bad+="hull ps: $(echo "$out" | one_line 200); "
  out="$(ls -A "$HOME/.brig/homes" 2> /dev/null || true)"
  [ -z "$out" ] || bad+="guest homes: $(echo "$out" | one_line 200); "
  out="$(ours)"
  [ -z "$out" ] || bad+="processes: $(echo "$out" | one_line 300); "
  echo "leftovers after $tag: ${bad:-none}"
  if [ -z "$bad" ]; then
    res check Leftovers "After $tag" pass "no sandbox, hull instance, guest home or monitor process"
  else
    res check Leftovers "After $tag" fail "$bad"
    tmo 300 brig rm --all -y < /dev/null || true
  fi
}

# block NAME FUNCTION: see canary-linux.sh. Call it as a plain command.
block() {
  local name=$1 fn=$2 rc=0 t0
  t0="$(now)"
  say "== $name"
  set +e
  (
    set -e
    "$fn"
  ) > "$LOGS/$name.log" 2>&1
  rc=$?
  set -e
  if [ "$rc" != 0 ]; then
    say "   $name stopped with exit $rc; the end of its log:"
    tail -n 15 "$LOGS/$name.log" | sed 's/^/   | /'
    res check Harness "The $name block ran to the end" fail \
      "exit $rc. Last lines: $(tail -n 4 "$LOGS/$name.log" | one_line 500)"
  fi
  if [ "$name" != setup ]; then
    leftovers "$name" >> "$LOGS/$name.log" 2>&1 || true
  fi
  say "   $name: exit $rc, $(dt "$t0" "$(now)") s"
}

# api PATH: a GitHub API answer, with the token when there is one.
api() {
  if [ -n "${GH_TOKEN:-}" ]; then
    curl -fsSL -H "Authorization: Bearer $GH_TOKEN" "https://api.github.com/$1"
  else
    curl -fsSL "https://api.github.com/$1"
  fi
}

# channel_assets REPO SUFFIX: the tag and the asset URLs of the newest
# channel-main release of REPO whose tag ends with SUFFIX, one per line.
channel_assets() {
  api "repos/$1/releases?per_page=50" | python3 -c '
import json, sys
suffix = sys.argv[1]
rels = [r for r in json.load(sys.stdin)
        if r["tag_name"].startswith("channel-main-") and r["tag_name"].endswith(suffix) and not r["draft"]]
rels.sort(key=lambda r: r["created_at"], reverse=True)
if rels:
    print(rels[0]["tag_name"])
    for a in rels[0]["assets"]:
        print(a["browser_download_url"])
' "$2"
}

# fetch_checked DIR URL CHECKSUMS_URL: downloads URL into DIR and checks it
# against the release's checksums.txt.
fetch_checked() {
  local dir=$1 url=$2 sums=$3 name
  name="${url##*/}"
  tmo 600 curl -fsSL -o "$dir/$name" "$url"
  tmo 60 curl -fsSL -o "$dir/checksums.txt" "$sums"
  (cd "$dir" && grep " $name\$" checksums.txt | shasum -a 256 -c -)
}

# ---------------------------------------------------------------- setup

setup() {
  sw_vers
  uname -m
  echo "HOME: $HOME (the account's own is $REAL_HOME, left alone)"
  local sip
  sip="$(csrutil status)"
  echo "$sip"
  case "$sip" in
    *enabled*) ;;
    *) echo "this runner is labeled sip-enabled, and SIP is not enabled"; return 1 ;;
  esac
  df -h "$HOME"

  # hull, and hvi and vz-runner next to it, where hull looks for them.
  local rel tag url sums
  rel="$(channel_assets brig-sh/hull "")"
  tag="$(echo "$rel" | head -n 1)"
  url="$(echo "$rel" | grep -E '/hull-[^/]*-arm64\.tar\.gz$' | head -n 1)"
  sums="$(echo "$rel" | grep '/checksums.txt$' | head -n 1)"
  if [ -z "$url" ] || [ -z "$sums" ]; then
    echo "no hull@main channel build found"
    return 1
  fi
  echo "hull channel release: $tag"
  mkdir -p "$SCRATCH/.e2e/hull"
  fetch_checked "$SCRATCH/.e2e/hull" "$url" "$sums"
  tar -xzf "$SCRATCH/.e2e/hull/${url##*/}" -C "$SCRATCH/.e2e/hull"
  local f
  for f in hull hvi vz-runner; do
    install -m 0755 "$SCRATCH/.e2e/hull/$f" "$BIN/$f"
  done

  [ -n "${BRIG_BUILD_DIR:-}" ] || { echo "BRIG_BUILD_DIR is not set"; return 1; }
  install -m 0755 "$BRIG_BUILD_DIR/brig" "$BIN/brig"
  install -m 0755 "$BRIG_BUILD_DIR/brigd" "$BIN/brigd"
  command -v brig hull

  local head version commit hull_version
  head="$(git -C "$REPO" rev-parse HEAD)"
  version="$(tmo 30 brig version < /dev/null)"
  commit="$(tmo 30 brig version --json < /dev/null | field data.commit)"
  hull_version="$(tmo 30 hull --version < /dev/null)"
  echo "brig: $version"
  echo "hull: $hull_version"
  res meta commit "$head"
  res meta target "brig ${head:0:7} · hull ${tag#channel-main-}"
  res fact "brig version" "$version"
  res fact hull "$hull_version" "https://github.com/brig-sh/hull/releases/tag/$tag"
  res meta host_detail "$(sysctl -n hw.ncpu) CPUs, $(( $(sysctl -n hw.memsize) / 1073741824 )) GB, macOS $(sw_vers -productVersion), SIP enabled. A scratch HOME, and hull from the hull@main channel on PATH."
  check_backends "${tag##*.}"
  if [ "$commit" != "$head" ]; then
    res check Install "brig version names this commit" fail "$version; commit $commit, checkout $head"
    return 1
  fi
  touch "$OUT/setup.ok"
}

# check_backends HULL_COMMIT records the hvi the hull build carries, and
# checks the build is the one for hull's main. A channel build bundles the hvi
# its commit pins as the hvi-vmm submodule. hvi --version names no commit.
check_backends() {
  local hc=$1 hvi hvi_main hull_main note
  hvi="$(api "repos/brig-sh/hull/contents/hvi-vmm?ref=$hc" | field sha || true)"
  hvi_main="$(api "repos/brig-sh/hvi-vmm/commits/main" | field sha || true)"
  hull_main="$(api "repos/brig-sh/hull/commits/main" | field sha || true)"
  echo "hull build $hc, hull main ${hull_main:0:7}, hvi ${hvi:0:7}, hvi-vmm main ${hvi_main:0:7}"
  if [ -n "$hvi" ]; then
    note="pinned by hull $hc"
    if [ -n "$hvi_main" ] && [ "$hvi" != "$hvi_main" ]; then
      note="$note; hvi-vmm main is ${hvi_main:0:7}"
    fi
    res fact hvi "${hvi:0:7} ($note)" "https://github.com/brig-sh/hvi-vmm/commit/$hvi"
  else
    res fact hvi "unknown: the API did not name hull $hc's hvi-vmm commit"
  fi
  if [ -z "$hull_main" ]; then
    res check Install "hull is the newest hull@main build" skip "the API did not name hull's main"
  elif [ "${hull_main:0:${#hc}}" = "$hc" ]; then
    res check Install "hull is the newest hull@main build" pass "hull $hc is main, with hvi ${hvi:0:7}"
  else
    res check Install "hull is the newest hull@main build" skip \
      "hull main is ${hull_main:0:7} and its channel build is not published yet; this run has $hc"
  fi
}

# record_boot_assets records the boot assets brig fetched into hull's store
# for this run, from the bundle record hull keeps beside them.
record_boot_assets() {
  local b
  b="$(tmo 30 hull assets dir < /dev/null)/bundle.json"
  if [ ! -f "$b" ]; then
    res fact "boot assets" "none in hull's store"
    return 0
  fi
  cat "$b"
  res fact "boot assets" "$(field version < "$b"), kernel $(field kernel.version < "$b"), urunc $(field sources.urunc.ref < "$b" | cut -c1-7)"
}

check_version() {
  local version modified
  version="$(tmo 30 brig version < /dev/null)"
  modified="$(tmo 30 brig version --json < /dev/null | field data.modified)"
  if [ "$modified" = false ]; then
    res check Install "brig version names this commit" pass "$version; hull: $(tmo 30 hull --version < /dev/null)"
  else
    res check Install "brig version names this commit" fail "$version; modified $modified"
  fi
}

# check_doctor runs after the first boot. Before it, doctor rightly says the
# boot assets are missing: brig fetches them on the first run.
check_doctor() {
  local out rc=0 bad
  out="$(run 300 brig doctor 2>&1)" || rc=$?
  echo "$out"
  bad="$(echo "$out" | grep -E '^ *!!' || true)"
  if [ "$rc" != 0 ]; then
    res check Install "brig doctor" fail "exit $rc: $(echo "$out" | one_line 500)"
  elif [ -n "$bad" ]; then
    res check Install "brig doctor" warn "exit 0 with findings: $(echo "$bad" | one_line 400)"
  else
    res check Install "brig doctor" pass \
      "exit 0; ok: $(echo "$out" | awk '$1 == "ok" { print $2 }' | paste -s -d, - | sed 's/,/, /g')"
  fi
}

# ---------------------------------------------------------------- gates

gate_gatekeeper() {
  if [ "${E2E_GATEKEEPER:-0}" != 1 ]; then
    res gate gatekeeper skip "Runs on schedule and dispatch only. A pull request has no notarized channel build."
    return 0
  fi
  local head rel tag url sums dir t0 t1 out rc=0 spctl
  head="$(git -C "$REPO" rev-parse HEAD)"
  rel="$(channel_assets brig-sh/brig "-${head:0:12}")"
  tag="$(echo "$rel" | head -n 1)"
  url="$(echo "$rel" | grep -E '/brig-[^/]*-darwin-arm64\.tar\.gz$' | head -n 1)"
  sums="$(echo "$rel" | grep '/checksums.txt$' | head -n 1)"
  if [ -z "$url" ] || [ -z "$sums" ]; then
    res gate gatekeeper skip "No channel-main build of ${head:0:7} yet. The channel publishes one after each merge to main."
    return 0
  fi
  echo "channel release: $tag"
  dir="$SCRATCH/.e2e/gatekeeper"
  mkdir -p "$dir"
  fetch_checked "$dir" "$url" "$sums"
  tar -xzf "$dir/${url##*/}" -C "$dir"
  # The attribute Homebrew sets on everything a cask installs.
  xattr -w com.apple.quarantine "0181;$(printf '%x' "$(date +%s)");Homebrew Cask;$(uuidgen)" "$dir/brig"
  xattr -p com.apple.quarantine "$dir/brig"
  t0="$(now)"
  out="$(tmo 20 "$dir/brig" version < /dev/null 2>&1)" || rc=$?
  t1="$(now)"
  echo "brig version: exit $rc in $(dt "$t0" "$t1") s: $out"
  spctl="$(spctl -a -vvv -t open --context context:primary-signature "$dir/brig" 2>&1 || true)"
  echo "$spctl"
  local note
  note="Quarantined $tag answered in $(dt "$t0" "$t1") s (exit $rc): $(echo "$out" | one_line 120). spctl: $(echo "$spctl" | sed "s|$dir/||" | one_line 160)"
  if [ "$rc" = 0 ] && echo "$out" | grep -q "${head:0:7}"; then
    res gate gatekeeper pass "$note"
  else
    res gate gatekeeper fail "$note"
  fi
}

# ---------------------------------------------------------------- checks

check_profiles() {
  local p img bin rc_run rc_sh t0 t1 out free
  for p in ubuntu claude-code codex gemini grok opencode; do
    img="$(tmo 30 brig agent show "$p" --json < /dev/null | field image)"
    bin="$(tmo 30 brig agent show "$p" --json < /dev/null | field binary)"
    free="$(df -g "$HOME" | awk 'NR == 2 { print $4 }')"
    if [ "$free" -lt 15 ]; then
      res check Profiles "$p boots" skip "only $free GB free on this shared Mac"
      continue
    fi
    rc_run=0; rc_sh=0
    t0="$(now)"
    run 900 brig run -d "$p" || rc_run=$?
    t1="$(now)"
    out="$(gsh "$p" "echo UID=\$(id -u) HOME=\$HOME; command -v $bin")" || rc_sh=$?
    run 300 brig rm "$p" || true
    # ubuntu and claude-code stay for the blocks after this one.
    case "$p" in
      ubuntu | claude-code) ;;
      *) run 300 hull rmi "$img" || true ;;
    esac
    if [ "$rc_run" = 0 ] && [ "$rc_sh" = 0 ] && echo "$out" | grep -q "/$bin\$"; then
      res check Profiles "$p boots" pass "brig run -d in $(dt "$t0" "$t1") s, pull included; guest: $(echo "$out" | one_line 200)"
    else
      res check Profiles "$p boots" fail "run exit $rc_run, sh exit $rc_sh; guest: $(echo "$out" | one_line 200)"
    fi
  done
  local c rc_c=0
  c="$(tmo 120 brig run -d cursor < /dev/null 2>&1)" || rc_c=$?
  echo "cursor: exit $rc_c: $c"
  if [ "$rc_c" != 0 ] && echo "$c" | grep -q 'do not publish an image'; then
    res check Profiles "cursor refuses by name" pass "$(echo "$c" | head -n 1)"
  else
    res check Profiles "cursor refuses by name" fail "exit $rc_c: $(echo "$c" | one_line 200)"
  fi
  res check Profiles "claude-desktop boots" skip "It opens a window, and a runner has no console session to show it."
}

check_cycles() {
  local i rc t0 t1 out fails=0 ref
  for ref in ubuntu claude-code; do
    local n=$SEQ_N metric="ubuntu boot"
    if [ "$ref" = claude-code ]; then n=$CLAUDE_N; metric="claude-code boot"; fi
    fails=0
    for i in $(seq 1 "$n"); do
      rc=0
      t0="$(now)"
      run 900 brig run -d "$ref@seq" || rc=$?
      t1="$(now)"
      out="$(gsh "$ref@seq" "echo cycle-$i")" || rc=$?
      run 300 brig rm "$ref@seq" || rc=$?
      if [ "$rc" = 0 ] && [ "$out" = "cycle-$i" ]; then
        res sample "$metric" "$(dt "$t0" "$t1")"
      else
        fails=$((fails + 1))
      fi
    done
    res note "$metric" "median of $n runs of brig run -d $ref, in boot, command and rm cycles, image already pulled"
    if [ "$fails" = 0 ]; then
      res check Cycles "$n sequential $ref cycles" pass "boot, one command and rm, $n times with no failure"
    else
      res check Cycles "$n sequential $ref cycles" fail "$fails of $n cycles failed"
    fi
  done
}

check_rm_home() {
  local home="$HOME/.brig/homes/brig-ubuntu-rmh" had=no out
  run 900 brig run -d ubuntu@rmh
  gsh ubuntu@rmh 'echo keep > $HOME/state.txt' > /dev/null || true
  [ -f "$home/state.txt" ] && had=yes
  run 300 brig rm ubuntu@rmh || true
  if [ "$had" = yes ] && [ ! -e "$home" ]; then
    res check Homes "rm deletes the guest home brig made" pass "$home held the guest's state.txt, and brig rm deleted it"
  else
    res check Homes "rm deletes the guest home brig made" fail "state.txt written: $had; after rm: $(ls -ld "$home" 2>&1)"
  fi

  local keep="$HOME/e2e-keep-home" after_rm
  rm -rf "$keep"
  mkdir -p "$keep"
  run 900 brig run -d ubuntu --home "$keep"
  gsh ubuntu 'echo survived > $HOME/state.txt' > /dev/null || true
  run 300 brig rm ubuntu || true
  after_rm="$(cat "$keep/state.txt" 2>&1 || true)"
  run 900 brig run -d ubuntu --home "$keep"
  out="$(gsh ubuntu 'cat $HOME/state.txt')" || true
  run 300 brig rm ubuntu || true
  if [ "$after_rm" = survived ] && [ "$out" = survived ]; then
    res check Homes "--home survives rm, and the next run sees it" pass "state.txt kept on the host after rm, and read back by the next boot"
  else
    res check Homes "--home survives rm, and the next run sees it" fail "after rm: [$after_rm]; next boot read: [$out]"
  fi
  rm -rf "$keep"
}

check_failed_boot() {
  local rc=0 home="$HOME/.brig/homes/brig-ubuntu-ghost"
  run 600 brig run -d ubuntu@ghost --image docker.io/library/brig-e2e-no-such-image:nope || rc=$?
  if [ "$rc" != 0 ] && [ ! -e "$home" ]; then
    res check Homes "A failed first boot leaves no guest home (#344)" pass "brig run exit $rc on an image that does not exist, and no $home"
  else
    res check Homes "A failed first boot leaves no guest home (#344)" fail "brig run exit $rc; $(ls -ld "$home" 2>&1)"
  fi
  tmo 60 brig rm ubuntu@ghost < /dev/null > /dev/null 2>&1 || true
}

check_shell_removed() {
  local out rc=0 ls
  out="$(tmo 120 script -q /dev/null brig shell ubuntu < /dev/null)" || rc=$?
  out="$(printf '%s' "$out" | tr -d '\r\b' | sed 's/^\^D//')"
  echo "brig shell ubuntu: exit $rc: $out"
  ls="$(tmo 30 brig ls -q < /dev/null | grep -v '^(none' || true)"
  if [ "$rc" = 2 ] && echo "$out" | grep -q 'was removed' && [ -z "$ls" ]; then
    res check CLI "brig shell exits 2 and starts nothing (#341)" pass "exit 2: $(echo "$out" | one_line 120)"
  else
    res check CLI "brig shell exits 2 and starts nothing (#341)" fail "exit $rc: $(echo "$out" | one_line 200); brig ls: [$ls]"
  fi
}

# PROBE prints the guest's marker, its address and its egress. claude-code's
# image has curl, and ubuntu's does not.
PROBE='echo MARKER=$(cat /tmp/e2e-marker 2> /dev/null || echo NONE); echo GUEST_IP=$(hostname -I 2> /dev/null); curl -sS -m 10 -o /dev/null -w "EGRESS_HTTP=%{http_code}\n" https://example.com 2> /dev/null || true'
MARK='echo m-$(date +%s%N) > /tmp/e2e-marker; cat /tmp/e2e-marker'

# probe_field TEXT KEY: the value of KEY=... in a probe's output.
probe_field() { echo "$1" | sed -n "s/^$2=//p" | head -n 1 | tr -d ' '; }

check_postures() {
  local s o i ip_s ip_i m_o m_i p_o p_i err rc=0 p w
  # One posture at a time: three claude-code guests at once are a lot for a
  # shared Mac. The profile asks for isolated, so shared is named.
  run 900 brig run -d claude-code@pshared --network shared --mem 2048
  s="$(gsh claude-code@pshared "$PROBE")" || true
  ip_s="$(probe_field "$s" GUEST_IP)"
  run 300 brig rm claude-code@pshared || true

  run 900 brig run -d claude-code@piso --network isolated --mem 2048
  m_i="$(gsh claude-code@piso "$MARK")" || true
  i="$(gsh claude-code@piso "$PROBE")" || true
  ip_i="$(probe_field "$i" GUEST_IP)"

  run 900 brig run -d claude-code@poff --network offline --mem 2048
  m_o="$(gsh claude-code@poff "$MARK")" || true
  o="$(gsh claude-code@poff "$PROBE")" || true

  if [ "$(probe_field "$s" EGRESS_HTTP)" = 200 ] && [ "$(probe_field "$i" EGRESS_HTTP)" = 200 ] &&
     [ "$(probe_field "$o" EGRESS_HTTP)" != 200 ] && [ -n "$ip_s" ] && [ -n "$ip_i" ] &&
     [ "${ip_s%.*}" != "${ip_i%.*}" ]; then
    res check Network "shared, isolated and offline each get what they promise (#342)" pass \
      "shared $ip_s: HTTP 200. isolated $ip_i, a network of its own: HTTP 200. offline: HTTP $(probe_field "$o" EGRESS_HTTP), address [$(probe_field "$o" GUEST_IP)]"
  else
    res check Network "shared, isolated and offline each get what they promise (#342)" fail \
      "shared: $(echo "$s" | one_line 100) | isolated: $(echo "$i" | one_line 100) | offline: $(echo "$o" | one_line 100)"
  fi

  p_i="$(gsh claude-code@piso "$PROBE")" || true
  p_o="$(gsh claude-code@poff "$PROBE")" || true
  if [ "$(probe_field "$p_i" MARKER)" = "$m_i" ] && [ "$(probe_field "$p_i" GUEST_IP)" = "$ip_i" ] &&
     [ "$(probe_field "$p_o" MARKER)" = "$m_o" ] && [ "$(probe_field "$p_o" EGRESS_HTTP)" != 200 ]; then
    res check Network "A flagless brig sh keeps the posture (#342)" pass "isolated and offline guests kept their markers, the isolated address and the offline block"
  else
    res check Network "A flagless brig sh keeps the posture (#342)" fail "isolated: $(echo "$p_i" | one_line 120) | offline: $(echo "$p_o" | one_line 120)"
  fi
  run 300 brig rm claude-code@piso || true

  err="$(tmo 900 brig run -d claude-code@poff --network shared < /dev/null 2>&1 > /dev/null)" || rc=$?
  echo "$err"
  p="$(gsh claude-code@poff "$PROBE")" || true
  w="$(echo "$err" | grep -A2 'started with the offline posture' || true)"
  if [ "$rc" = 0 ] && echo "$w" | grep -q 'brig restarts it' && [ "$(probe_field "$p" MARKER)" = NONE ] && [ "$(probe_field "$p" EGRESS_HTTP)" = 200 ]; then
    res check Network "--network shared restarts it, with the warning (#342)" pass "$(echo "$w" | one_line 240). New guest (no marker), HTTP 200"
  else
    res check Network "--network shared restarts it, with the warning (#342)" fail "exit $rc; warning: [$(echo "$w" | one_line 160)]; guest: $(echo "$p" | one_line 120)"
  fi
  run 300 brig rm claude-code@poff || true

  local vz vz_rc=0
  vz="$(tmo 120 env BRIG_HYPERVISOR=vz brig run -d ubuntu@vz --network isolated < /dev/null 2>&1)" || vz_rc=$?
  echo "vz: exit $vz_rc: $vz"
  if [ "$vz_rc" != 0 ] && echo "$vz" | grep -q 'hvi backend' && [ -z "$(hull_rows)" ]; then
    res check Network "vz refuses --network isolated by name" pass "exit $vz_rc: $(echo "$vz" | one_line 160)"
  else
    res check Network "vz refuses --network isolated by name" fail "exit $vz_rc: $(echo "$vz" | one_line 200)"
  fi
  tmo 60 brig rm ubuntu@vz < /dev/null > /dev/null 2>&1 || true
}

SRV='nohup setsid perl -MIO::Socket::INET -e '"'"'$s = IO::Socket::INET->new(LocalAddr => "0.0.0.0", LocalPort => 8000, Listen => 5, ReuseAddr => 1) or die "listen: $!"; while ($c = $s->accept) { <$c>; print $c "HTTP/1.0 200 OK\r\n\r\nhello-from-guest\n"; close $c }'"'"' > /tmp/srv.log 2>&1 < /dev/null & sleep 1'

# first_answer PORT SECONDS: the seconds until the port first answers, or
# nothing when it does not answer within SECONDS.
first_answer() {
  local t0 t
  t0="$(now)"
  while :; do
    t="$(dt "$t0" "$(now)")"
    if [ "$(curl -sS -m 1 "http://127.0.0.1:$1/" 2> /dev/null)" = hello-from-guest ]; then
      echo "$t"
      return 0
    fi
    awk -v t="$t" -v m="$2" 'BEGIN { exit !(t < m) }' || return 0
    sleep 0.5
  done
}

# port_check NAME PORT SECONDS: records whether the port answered, and warns
# when it took longer than a few seconds.
port_check() {
  local name=$1 t
  t="$(first_answer "$2" "$3")"
  if [ -z "$t" ]; then
    res check Network "$name" fail "no answer on 127.0.0.1:$2 within $3 s"
  elif awk -v t="$t" 'BEGIN { exit !(t > 5) }'; then
    res check Network "$name" warn "first answer after $t s. Known: a guest that gets an address an earlier guest had comes with a new MAC, and a stale entry holds the port for about 30 s"
  else
    res check Network "$name" pass "curl 127.0.0.1:$2 -> guest 8000 answered after $t s"
  fi
}

check_ports() {
  local port="" p after
  for p in $(seq 28480 28499); do
    lsof -nP -iTCP:"$p" -sTCP:LISTEN > /dev/null 2>&1 || { port=$p; break; }
  done
  [ -n "$port" ] || { res check Network "A published port answers from the host" skip "no free port in 28480-28499"; return 0; }
  run 900 brig run -d ubuntu@net --publish "$port:8000"
  gsh ubuntu@net "$SRV" > /dev/null || true
  port_check "A published port answers from the host" "$port" 60

  run 300 brig stop ubuntu@net || true
  run 900 brig run -d ubuntu@net || true
  gsh ubuntu@net "$SRV" > /dev/null || true
  port_check "The port answers again after stop and run" "$port" 60
  run 300 brig rm ubuntu@net || true
  after="$(curl -sS -m 3 "http://127.0.0.1:$port/" 2>&1 || true)"
  echo "after rm: $after"
}

# canary_sum DIR: a checksum over every name, type, size, mode, inode, mtime
# and file content under DIR.
canary_sum() {
  (
    cd "$1"
    find . -exec stat -f '%N %HT %z %p %i %m' {} + | sort
    find . -type f -exec shasum -a 256 {} + | sort
  ) | shasum -a 256 | cut -c1-64
}

check_symlink() {
  local canary="$HOME/e2e-canary332" proj="$HOME/e2e-p332" sum0 sum1 planted rc refused=0 out target control
  rm -rf "$canary" "$proj"
  mkdir -p "$canary/sub" "$proj/real-sub"
  echo "canary $(date -u +%s)" > "$canary/secret.txt"
  echo inner > "$canary/sub/inner.txt"
  echo hello > "$proj/real-sub/readme"
  sum0="$(canary_sum "$canary")"

  run 900 brig run ubuntu@plant "$proj" -d
  gsh ubuntu@plant "ln -s $canary /work/e2e-p332/evil && ln -s ../e2e-canary332 /work/e2e-p332/evil-rel && echo PLANTED" || true
  planted="$(readlink "$proj/evil" 2>&1) | $(readlink "$proj/evil-rel" 2>&1)"
  echo "planted, as the host sees them: $planted"
  for target in "$proj/evil" "$proj/evil-rel" "$proj/evil/sub"; do
    rc=0
    out="$(tmo 300 brig run ubuntu@victim "$target" -d < /dev/null 2>&1)" || rc=$?
    echo "brig run ubuntu@victim $target: exit $rc: $out"
    if [ "$rc" != 0 ] && echo "$out" | grep -q 'refusing to use'; then
      refused=$((refused + 1))
    fi
    tmo 60 brig rm ubuntu@victim < /dev/null > /dev/null 2>&1 || true
  done
  local victim_home
  victim_home="$(ls -d "$HOME/.brig/homes/brig-ubuntu-victim" 2> /dev/null || true)"
  run 900 brig run ubuntu@real "$proj/real-sub" -d || true
  control="$(gsh ubuntu@real 'cat /work/real-sub/readme')" || true
  run 300 brig rm ubuntu@real || true
  run 300 brig rm ubuntu@plant || true
  sum1="$(canary_sum "$canary")"

  if [ "$refused" = 3 ] && [ -z "$victim_home" ] && [ "$control" = hello ]; then
    res check Boundary "A project reached through a planted link is refused (#332)" pass \
      "3 of 3 runs through links the guest planted ($planted) refused; no guest home made; the real directory next to them boots and reads back"
  else
    res check Boundary "A project reached through a planted link is refused (#332)" fail \
      "$refused of 3 refused; victim home: [$victim_home]; control read [$control]; links: $planted"
  fi
  if [ "$sum0" = "$sum1" ]; then
    res check Boundary "The canary outside the project is unchanged" pass "sha256 over names, modes, inodes, mtimes and contents: ${sum0:0:16}... before and after"
  else
    res check Boundary "The canary outside the project is unchanged" fail "before ${sum0:0:16}, after ${sum1:0:16}"
  fi
  rm -rf "$canary" "$proj"
}

# check_brigd_lock plants a symlink and a hard link at brigd's socket lock path
# and asserts brigd refuses each rather than following it and truncating the
# target. macOS is the platform this bites on: /tmp is sticky but has no
# protected-symlinks equivalent, so a shared --socket dir is reachable. brigd
# opens the lock with O_NOFOLLOW and verifies a regular, singly-linked file it
# owns; a scratch directory stands in for the shared dir here.
check_brigd_lock() {
  local d canary before after rc out refused=0
  d="$(mktemp -d "$OUT/brigd-lock.XXXXXX")"
  canary="$d/operator-file"
  echo "operator-only $(date -u +%s)" > "$canary"
  before="$(cat "$canary")"

  ln -s "$canary" "$d/sock.lock"
  rc=0
  out="$(tmo 30 "$BIN/brigd" --socket "$d/sock" < /dev/null 2>&1)" || rc=$?
  echo "symlink at the lock path: exit $rc: $out"
  if [ "$rc" != 0 ] && echo "$out" | grep -qiE 'symlink|planted'; then
    refused=$((refused + 1))
  fi
  rm -f "$d/sock.lock" "$d/sock"

  ln "$canary" "$d/sock.lock"
  rc=0
  out="$(tmo 30 "$BIN/brigd" --socket "$d/sock" < /dev/null 2>&1)" || rc=$?
  echo "hard link at the lock path: exit $rc: $out"
  if [ "$rc" != 0 ] && echo "$out" | grep -qi 'hard link'; then
    refused=$((refused + 1))
  fi
  rm -f "$d/sock.lock" "$d/sock"

  after="$(cat "$canary")"
  if [ "$refused" = 2 ] && [ "$before" = "$after" ]; then
    res check Boundary "brigd refuses a symlinked or hard-linked socket lock (#406)" pass \
      "a symlink and a hard link at <socket>.lock were each refused before the socket bound; the target file is unchanged"
  else
    res check Boundary "brigd refuses a symlinked or hard-linked socket lock (#406)" fail \
      "refused $refused of 2; target file before [$before] after [$after]"
  fi
  rm -rf "$d"
}

# ---------------------------------------------------------------- fixes

# ENVCHK prints each variable the credential check looks for, or UNSET.
ENVCHK='for v in E2E_FWD E2E_SNEAKY ANTHROPIC_API_KEY; do echo "$v=${!v:-UNSET}"; done'

# env_field TEXT NAME: the value ENVCHK printed for NAME.
env_field() { echo "$1" | sed -n "s/^$2=//p" | head -n 1 | tr -d ' '; }

# Where a file from the guest's /brig/claude would land if it reached the
# host: brig's homes and the runtime's own store.
CLAUDE_LEAK_ROOTS=("$HOME/.brig" "$HOME/.hull")

# check_claude_tmpfs: claude-code keeps its credential directory on a tmpfs at
# /brig/claude, off the home share, and CLAUDE_CONFIG_DIR points there (#462).
# Its state files reach the guest as hostmounts inside that tmpfs. Three
# checks on one sandbox:
#   - a file the guest writes in /brig/claude reaches no host directory;
#   - brig rewrites a hostmounted .claude.json in place, so the guest's bind
#     still sees it. A rename leaves a virtiofsd bind on the old file;
#   - a host rename of the home's .claude, as git clean -fdx makes, leaves
#     the tmpfs in place, and what the guest writes next stays off the host.
# The probe files carry a name no host file has, so find is the leak check.
check_claude_tmpfs() {
  local ref=claude-code@cct home tok rc=0 out ccd cfs leak1 leak2 f ino0 ino1 keys moved=no
  home="$HOME/.brig/homes/brig-claude-code-cct"
  tok="e2e-cc-$RANDOM$RANDOM"
  run 900 brig run -d "$ref" --mem 2048 || rc=$?

  out="$(gsh "$ref" "echo CCD=\$CLAUDE_CONFIG_DIR; echo CFS=\$(stat -f -c %T /brig/claude); echo $tok > /brig/claude/.$tok-1")" || true
  ccd="$(env_field "$out" CCD)"
  cfs="$(env_field "$out" CFS)"
  leak1="$(find "${CLAUDE_LEAK_ROOTS[@]}" -name ".$tok-*" -print 2> /dev/null | head -n 3 || true)"
  if [ "$rc" = 0 ] && [ "$ccd" = /brig/claude ] && [ "$cfs" = tmpfs ] && [ -z "$leak1" ]; then
    res check Credentials "claude-code's credential directory is a tmpfs off the home share (#462)" pass \
      "CLAUDE_CONFIG_DIR=$ccd on $cfs; a file the guest wrote there is in none of: ${CLAUDE_LEAK_ROOTS[*]}"
  else
    res check Credentials "claude-code's credential directory is a tmpfs off the home share (#462)" fail \
      "run exit $rc; CLAUDE_CONFIG_DIR=[$ccd], /brig/claude on [$cfs]; on the host: [$(echo "$leak1" | one_line 200)]"
  fi

  # The trust key brig writes for the guest's working directory. Drop it on
  # the host without replacing the file, so the next brig sh writes it again.
  f="$home/.claude/.claude.json"
  ino0="$(python3 -c 'import os, sys; print(os.stat(sys.argv[1]).st_ino)' "$f" 2> /dev/null || true)"
  python3 - "$f" << 'EOF' || true
import json, sys
with open(sys.argv[1], "r+") as fh:
    d = json.load(fh)
    d.pop("projects", None)
    fh.seek(0)
    fh.write(json.dumps(d))
    fh.truncate()
EOF
  out="$(gsh "$ref" "echo KEYS=\$(grep -c '\"projects\"' /brig/claude/.claude.json)")" || true
  keys="$(env_field "$out" KEYS)"
  ino1="$(python3 -c 'import os, sys; print(os.stat(sys.argv[1]).st_ino)' "$f" 2> /dev/null || true)"
  if [ -n "$ino0" ] && [ "$ino0" = "$ino1" ] && [ "${keys:-0}" -ge 1 ] 2> /dev/null && grep -q '"projects"' "$f"; then
    res check Credentials "brig rewrites a hostmounted .claude.json in place, and the guest sees it" pass \
      "inode $ino0 before and after brig sh wrote the trust key again; the guest's bind reads it"
  else
    res check Credentials "brig rewrites a hostmounted .claude.json in place, and the guest sees it" fail \
      "inode [$ino0] -> [$ino1]; projects in the guest's copy: [$keys]; on the host: $(grep -c '"projects"' "$f" 2> /dev/null || echo none)"
  fi

  if [ -d "$home/.claude" ] && mv "$home/.claude" "$home/.claude.e2e-moved"; then
    moved=yes
  fi
  out="$(gsh "$ref" "echo CFS=\$(stat -f -c %T /brig/claude); echo P1=\$(cat /brig/claude/.$tok-1); echo $tok > /brig/claude/.$tok-2 && echo W2=ok")" || true
  leak2="$(find "${CLAUDE_LEAK_ROOTS[@]}" -name ".$tok-*" -print 2> /dev/null | head -n 3 || true)"
  run 300 brig rm "$ref" || true
  rm -rf "$home/.claude.e2e-moved"
  if [ "$moved" = yes ] && [ "$(env_field "$out" CFS)" = tmpfs ] && [ "$(env_field "$out" P1)" = "$tok" ] &&
     [ "$(env_field "$out" W2)" = ok ] && [ -z "$leak2" ]; then
    res check Credentials "A host rename of the home's .claude leaves the credential tmpfs in place (#462)" pass \
      "after mv .claude on the host: /brig/claude still tmpfs, the first file still there, a second written, neither on the host"
  else
    res check Credentials "A host rename of the home's .claude leaves the credential tmpfs in place (#462)" fail \
      "moved: $moved; guest: $(echo "$out" | grep -E '^(CFS|P1|W2)=' | one_line 200); on the host: [$(echo "$leak2" | one_line 200)]"
  fi
}

# check_creds boots claude-code with three variables in the shell: one named
# in BRIG_FORWARD_ENV, one named nowhere, and ANTHROPIC_API_KEY, which is
# named but on the claude-code denylist. Only the first may reach the guest.
check_creds() {
  local err out fwd sneaky denied rc=0
  export E2E_FWD="fwd-$RANDOM$RANDOM" E2E_SNEAKY="sneaky-$RANDOM$RANDOM" ANTHROPIC_API_KEY="sk-e2e-$RANDOM$RANDOM"
  export BRIG_FORWARD_ENV="E2E_FWD ANTHROPIC_API_KEY"
  unset BRIG_CLAUDE_CODE_FORWARD_ENV BRIG_ALLOW_DENIED BRIG_CLAUDE_CODE_ALLOW_DENIED
  err="$(tmo 900 brig run -d claude-code@creds --mem 2048 < /dev/null 2>&1 > /dev/null)" || rc=$?
  echo "$err"
  out="$(gsh claude-code@creds "$ENVCHK")" || true
  run 300 brig rm claude-code@creds || true
  fwd="$(env_field "$out" E2E_FWD)"
  sneaky="$(env_field "$out" E2E_SNEAKY)"
  denied="$(env_field "$out" ANTHROPIC_API_KEY)"
  if [ "$rc" = 0 ] && [ "$fwd" = "$E2E_FWD" ] && [ "$sneaky" = UNSET ] && [ "$denied" = UNSET ] &&
     echo "$err" | grep -q 'not forwarding ANTHROPIC_API_KEY'; then
    res check Credentials "Only the variables brig is told to forward reach the guest (#236)" pass \
      "E2E_FWD, named in BRIG_FORWARD_ENV, arrived. E2E_SNEAKY, set and never named, did not. ANTHROPIC_API_KEY, named but on the claude-code denylist, did not, and brig said so"
  else
    res check Credentials "Only the variables brig is told to forward reach the guest (#236)" fail \
      "run exit $rc; guest: E2E_FWD=[$fwd] E2E_SNEAKY=[$sneaky] ANTHROPIC_API_KEY=[$denied]; stderr: $(echo "$err" | grep -i forward | one_line 200)"
  fi
}

check_agent_rm() {
  local w f rc=0 out kept=no rc2=0 out2
  w="$(tmo 60 brig agent export ubuntu e2e-rm --force < /dev/null 2>&1)"
  echo "$w"
  f="$(echo "$w" | sed -n 's/^wrote .* -> //p' | head -n 1)"
  run 900 brig run -d e2e-rm
  out="$(tmo 120 brig agent rm e2e-rm < /dev/null 2>&1)" || rc=$?
  echo "agent rm with the sandbox up: exit $rc: $out"
  [ -n "$f" ] && [ -f "$f" ] && kept=yes
  run 300 brig rm e2e-rm || true
  out2="$(tmo 120 brig agent rm e2e-rm < /dev/null 2>&1)" || rc2=$?
  echo "agent rm after brig rm: exit $rc2: $out2"
  if [ "$rc" != 0 ] && echo "$out" | grep -q 'still has sandboxes' && echo "$out" | grep -q 'brig rm e2e-rm' &&
     [ "$kept" = yes ] && [ "$rc2" = 0 ] && [ ! -e "$f" ]; then
    res check CLI "agent rm refuses while a sandbox of the profile exists (#367)" pass \
      "exit $rc with brig-e2e-rm up, the file kept and brig rm e2e-rm named; after brig rm, agent rm removed $f"
  else
    res check CLI "agent rm refuses while a sandbox of the profile exists (#367)" fail \
      "first rm: exit $rc, file kept: $kept: $(echo "$out" | one_line 200); second rm: exit $rc2: $(echo "$out2" | one_line 120)"
  fi
  [ -z "$f" ] || rm -f "$f"
}

# check_files_mode: a files: binding's mode must be quoted (#453). YAML reads
# an unquoted 0640 as the number 416, and brig created the file 0o640 read as
# decimal digits. Now the profile is refused and the message says to quote
# it. A file that overrides a built-in is ignored the same way, and the
# built-in runs.
check_files_mode() {
  local w f o out rc=0 ls ok rc2=0 out2 said
  w="$(tmo 60 brig agent export ubuntu e2e-mode --force < /dev/null 2>&1)"
  echo "$w"
  f="$(echo "$w" | sed -n 's/^wrote .* -> //p' | head -n 1)"
  if [ -z "$f" ] || [ ! -f "$f" ]; then
    res check Profile "An unquoted files: mode is refused, and a quoted one parses (#453)" fail "agent export wrote no file: $(echo "$w" | one_line 200)"
    return 0
  fi
  printf '%s\n' 'secrets:' '  - name: e2e-mode' '    required: false' \
    'volumes:' '  - kind: tmpfs' '    path: .e2e' 'files:' \
    '  - ref: secrets.e2e-mode' '    path: .e2e/token' '    mode: "0640"' >> "$f"
  ls="$(tmo 60 brig agent ls < /dev/null 2>&1)" || true
  echo "agent ls with mode \"0640\": $ls"
  sed -i.bak 's/^    mode: "0640"$/    mode: 0640/' "$f" && rm -f "$f.bak"
  grep -n 'mode:' "$f"
  out="$(tmo 300 brig run -d e2e-mode < /dev/null 2>&1)" || rc=$?
  echo "brig run e2e-mode with mode 0640: exit $rc: $out"
  if echo "$ls" | grep -qw e2e-mode && ! echo "$ls" | grep -q unusable &&
     [ "$rc" != 0 ] && echo "$out" | grep -q 'mode must be a quoted string' && echo "$out" | grep -qF "$(basename "$f")" &&
     ! tmo 60 brig ls -q < /dev/null 2> /dev/null | grep -q '^e2e-mode'; then
    res check Profile "An unquoted files: mode is refused, and a quoted one parses (#453)" pass \
      "\"0640\" listed; 0640: exit $rc, no sandbox: $(echo "$out" | grep 'quoted' | one_line 200)"
  else
    res check Profile "An unquoted files: mode is refused, and a quoted one parses (#453)" fail \
      "agent ls with \"0640\": $(echo "$ls" | one_line 160); with 0640: exit $rc: $(echo "$out" | one_line 240)"
  fi

  o="$(dirname "$f")/e2e-ubuntu-override.yaml"
  sed 's/^name: .*/name: ubuntu/' "$f" > "$o"
  rm -f "$f"
  out2="$(tmo 900 brig run -d ubuntu@mode < /dev/null 2>&1)" || rc2=$?
  echo "brig run ubuntu@mode with an override that has mode 0640: exit $rc2: $out2"
  ok="$(gsh ubuntu@mode 'echo mode-ok')" || true
  said="$(echo "$out2" | grep -E 'unusable|quoted' | one_line 240)"
  if [ "$rc2" = 0 ] && echo "$out2" | grep -q 'mode must be a quoted string' && echo "$out2" | grep -qF "$(basename "$o")" &&
     echo "$ok" | grep -qx mode-ok; then
    res check Profile "An override with an unquoted mode is ignored, and the built-in runs (#453)" pass \
      "exit 0, the built-in ubuntu answered: $said"
  else
    res check Profile "An override with an unquoted mode is ignored, and the built-in runs (#453)" fail \
      "exit $rc2: $(echo "$out2" | one_line 240); brig sh: [$(echo "$ok" | one_line 80)]"
  fi
  rm -f "$o"
  run 300 brig rm ubuntu@mode || true
}

new_policy() { tmo 60 env -u VISUAL EDITOR=true brig policy create e2e-deny --force < /dev/null; }

check_policy_refusals() {
  local vz vz_rc=0 out rc=0 bp="$SCRATCH/.e2e/badprobe"
  new_policy
  run 60 brig policy attach e2e-deny claude-code -n pvz
  vz="$(tmo 120 env BRIG_HYPERVISOR=vz brig run -d claude-code@pvz --mem 2048 < /dev/null 2>&1)" || vz_rc=$?
  echo "vz: exit $vz_rc: $vz"
  if [ "$vz_rc" != 0 ] && echo "$vz" | grep -q 'hull on vz cannot enforce the egress policy' && [ -z "$(hull_rows)" ]; then
    res check Policy "A policy on vz is refused before boot, naming the backend (#237)" pass "exit $vz_rc: $(echo "$vz" | one_line 200)"
  else
    res check Policy "A policy on vz is refused before boot, naming the backend (#237)" fail "exit $vz_rc; hull ps: [$(hull_rows | one_line 120)]; $(echo "$vz" | one_line 300)"
  fi
  run 60 brig policy detach e2e-deny claude-code -n pvz || true
  tmo 60 brig rm claude-code@pvz < /dev/null > /dev/null 2>&1 || true

  # A hull whose network-gateway --help fails, and the real hull for the rest.
  mkdir -p "$bp"
  cat > "$bp/hull" << EOS
#!/bin/bash
if [ "\$1" = network-gateway ] && [ "\${2:-}" = --help ]; then
  echo "e2e: this hull answers no --help" >&2
  exit 1
fi
exec "$BIN/hull" "\$@"
EOS
  chmod +x "$bp/hull"
  run 60 brig policy attach e2e-deny claude-code -n pprobe
  out="$(tmo 900 env BRIG_RUNTIME_BIN="$bp/hull" brig run -d claude-code@pprobe --mem 2048 < /dev/null 2>&1)" || rc=$?
  echo "failing probe: exit $rc: $out"
  if [ "$rc" != 0 ] && echo "$out" | grep -q 'whether hull on hvi enforces the egress policy is unknown' && [ -z "$(hull_rows)" ]; then
    res check Policy "A policy boot is refused when the gateway probe fails (#171)" pass "exit $rc, no instance: $(echo "$out" | grep 'is unknown' | one_line 240)"
  else
    res check Policy "A policy boot is refused when the gateway probe fails (#171)" fail "exit $rc; hull ps: [$(hull_rows | one_line 120)]; $(echo "$out" | one_line 300)"
  fi
  run 60 brig policy detach e2e-deny claude-code -n pprobe || true
  tmo 60 brig rm claude-code@pprobe < /dev/null > /dev/null 2>&1 || true
  rm -rf "$bp"
}

check_posture_info() {
  local before after want="isolated (a network of this sandbox's own); shared from its next boot"
  new_policy
  run 60 brig policy attach e2e-deny claude-code -n pinfo
  # The profile asks for isolated. #368 is a shared boot a policy narrowed.
  run 900 brig run -d claude-code@pinfo --network shared --mem 2048
  before="$(tmo 60 brig info claude-code@pinfo < /dev/null 2> /dev/null | grep -E '^NETWORK' || true)"
  run 60 brig policy detach e2e-deny claude-code -n pinfo
  after="$(tmo 60 brig info claude-code@pinfo < /dev/null 2> /dev/null | grep -E '^NETWORK' || true)"
  echo "with the policy: $before"
  echo "after detach: $after"
  run 300 brig rm claude-code@pinfo || true
  if echo "$before" | grep -q isolated && echo "$after" | grep -qF "$want"; then
    res check Network "brig info names the posture a running sandbox has (#368)" pass \
      "isolated by a policy, then the policy detached: $(echo "$after" | one_line 160)"
  else
    res check Network "brig info names the posture a running sandbox has (#368)" fail \
      "with the policy: [$(echo "$before" | one_line 120)]; after detach: [$(echo "$after" | one_line 160)]"
  fi
}

GWSOCK="$HOME/.brig/gateway-198-18-0-0_24.sock"

# gw_pids: the shared gateways serving this scratch HOME's socket, as pid and
# arguments.
gw_pids() { pgrep -fl "network-gateway --socket $GWSOCK " 2> /dev/null || true; }

gone() { ! kill -0 "$1" 2> /dev/null; }

check_shared_gateway() {
  local before after old rc=0 t port="" p now waited=0
  run 900 brig run -d ubuntu@gw1 --network shared
  before="$(gw_pids)"
  run 300 brig rm ubuntu@gw1 || true
  run 300 brig rm --all -y || true
  sleep 2
  after="$(gw_pids)"
  echo "with a sandbox: [$before]; after rm --all: [$after]"
  if [ -n "$before" ] && [ -z "$after" ]; then
    res check Network "rm --all stops the shared gateway once no sandbox is on it (#366)" pass \
      "pid $(echo "$before" | awk '{ print $1 }' | paste -s -d, -) served the sandbox; after brig rm and rm --all, no gateway is left"
  else
    res check Network "rm --all stops the shared gateway once no sandbox is on it (#366)" fail \
      "with a sandbox: [$(echo "$before" | one_line 160)]; after rm --all: [$(echo "$after" | one_line 160)]"
  fi

  # A gateway started the way brig 0.2.0 started it, with no --api, so it
  # cannot publish a port. It runs detached, so launchd reaps it.
  (nohup hull network-gateway --socket "$GWSOCK" --qemu-socket "$GWSOCK.qemu" \
    --subnet 198.18.0.0/24 --gateway-ip 198.18.0.1 > "$LOGS/gateway-0.2.0.log" 2>&1 < /dev/null &
   echo $! > "$SCRATCH/.e2e/old-gateway.pid")
  old="$(cat "$SCRATCH/.e2e/old-gateway.pid")"
  while [ ! -S "$GWSOCK.qemu" ] && [ "$waited" -lt 40 ]; do sleep 0.5; waited=$((waited + 1)); done
  echo "0.2.0-style gateway: pid $old; $(gw_pids)"
  for p in $(seq 28480 28499); do
    lsof -nP -iTCP:"$p" -sTCP:LISTEN > /dev/null 2>&1 || { port=$p; break; }
  done
  [ -n "$port" ] || { res check Network "A boot replaces the gateway an older brig left (#366)" skip "no free port in 28480-28499"; kill "$old" 2> /dev/null || true; return 0; }
  run 900 brig run -d ubuntu@gw2 --network shared --publish "$port:8000" || rc=$?
  gsh ubuntu@gw2 "$SRV" > /dev/null || true
  t="$(first_answer "$port" 60)"
  now="$(gw_pids)"
  if [ "$rc" = 0 ] && gone "$old" && [ -n "$t" ] && echo "$now" | grep -q -- '--api'; then
    res check Network "A boot replaces the gateway an older brig left (#366)" pass \
      "pid $old, started without --api, was stopped; the new gateway has --api, and 127.0.0.1:$port answered after $t s"
  else
    res check Network "A boot replaces the gateway an older brig left (#366)" fail \
      "run exit $rc; old pid $old gone: $(gone "$old" && echo yes || echo no); gateways: [$(echo "$now" | one_line 200)]; port answered after: [$t]"
  fi
  run 300 brig rm ubuntu@gw2 || true
  run 300 brig rm --all -y || true
  kill "$old" 2> /dev/null || true
}

# fetch_txt IP: guest text that fetches http://IP:8000/ with bash alone. The
# ubuntu image has no curl.
fetch_txt() { printf 'timeout 10 bash -c %q' "exec 3<>/dev/tcp/$1/8000; printf 'GET / HTTP/1.0\r\n\r\n' >&3; cat <&3"; }

# guest_fetch REF IP SECONDS: fetches http://IP:8000/ from REF until it
# answers or SECONDS pass, and prints the last answer. A fresh guest can wait
# seconds for entropy, and perl waits with it before it listens.
guest_fetch() {
  local ref=$1 ip=$2 s=$3 t0 out
  t0="$(now)"
  while :; do
    out="$(gsh "$ref" "$(fetch_txt "$ip")")" || true
    if echo "$out" | grep -q hello-from-guest ||
       ! awk -v t="$(dt "$t0" "$(now)")" -v m="$s" 'BEGIN { exit !(t < m) }'; then
      echo "$out"
      return 0
    fi
    sleep 2
  done
}

check_reach() {
  local hv out rc refused=0 ev="" ip_a ctl got
  for hv in vz qemu; do
    rc=0
    out="$(tmo 120 env BRIG_HYPERVISOR=$hv brig run -d "ubuntu@iso-$hv" --network isolated < /dev/null 2>&1)" || rc=$?
    echo "$hv: exit $rc: $out"
    if [ "$rc" != 0 ] && echo "$out" | grep -q 'separate hosts' && [ -z "$(hull_rows)" ]; then
      refused=$((refused + 1))
    fi
    ev+="$hv: exit $rc; "
    tmo 60 brig rm "ubuntu@iso-$hv" < /dev/null > /dev/null 2>&1 || true
  done
  if [ "$refused" = 2 ]; then
    res check Network "vz and qemu refuse --network isolated and point at separate hosts (#364)" pass "$ev$(echo "$out" | one_line 200)"
  else
    res check Network "vz and qemu refuse --network isolated and point at separate hosts (#364)" fail "$refused of 2 named separate hosts; $ev$(echo "$out" | one_line 200)"
  fi

  # The profile asks for isolated, so shared is named.
  run 900 brig run -d ubuntu@ra --network shared
  run 900 brig run -d ubuntu@rb --network shared
  gsh ubuntu@ra "$SRV" > /dev/null || true
  ip_a="$(hull_rows | awk '$1 == "brig-ubuntu-ra" { print $5 }')"
  ctl="$(guest_fetch ubuntu@ra 127.0.0.1 90)"
  got="$(guest_fetch ubuntu@rb "$ip_a" 30)"
  run 300 brig rm ubuntu@ra || true
  run 300 brig rm ubuntu@rb || true
  if echo "$got" | grep -q hello-from-guest; then
    res check Network "Shared sandboxes on hvi reach each other, as docs/security.md says (#364)" pass \
      "ubuntu@rb fetched http://$ip_a:8000/, which only ubuntu@ra serves"
  elif ! echo "$ctl" | grep -q hello-from-guest; then
    res check Network "Shared sandboxes on hvi reach each other, as docs/security.md says (#364)" fail \
      "the listener in ubuntu@ra did not answer from inside it, so nothing was measured: $(echo "$ctl" | one_line 160)"
  else
    res check Network "Shared sandboxes on hvi reach each other, as docs/security.md says (#364)" fail \
      "ubuntu@ra answered itself, and ubuntu@rb got nothing from $ip_a:8000, which docs/security.md says it reaches: $(echo "$got" | one_line 160)"
  fi
}

# check_cosign_hang points DOCKER_CONFIG at a credsStore helper that never
# answers, the shape Docker Desktop leaves when it is installed and not
# running. cosign asks that helper for registry credentials and hangs.
# claude-code, because brig verifies only the images brig-sh signs.
check_cosign_hang() {
  local d="$SCRATCH/.e2e/dockercfg" out rc=0 t0 t1 left ls
  if ! command -v cosign > /dev/null; then
    res check Verify "A hung credential helper is named, and killed with cosign (#365)" skip "no cosign on this Mac, so brig verifies nothing here"
    return 0
  fi
  rm -rf "$d"
  mkdir -p "$d/bin"
  printf '{"credsStore": "e2ehang"}\n' > "$d/config.json"
  printf '#!/bin/sh\nexec sleep 600\n' > "$d/bin/docker-credential-e2ehang"
  chmod +x "$d/bin/docker-credential-e2ehang"
  t0="$(now)"
  out="$(PATH="$d/bin:$PATH" DOCKER_CONFIG="$d" BRIG_VERIFY=require tmo 300 brig run -d claude-code@cos --mem 2048 < /dev/null 2>&1)" || rc=$?
  t1="$(now)"
  echo "brig run claude-code@cos: exit $rc after $(dt "$t0" "$t1") s: $out"
  sleep 2
  left="$(pgrep -fl 'docker-credential-e2ehang' || true)"
  ls="$(tmo 30 brig ls -q < /dev/null 2> /dev/null | grep -v '^(none' || true)"
  if [ "$rc" = 5 ] && echo "$out" | grep -q 'It waits on docker-credential-e2ehang' && [ -z "$left" ] && [ -z "$ls" ]; then
    res check Verify "A hung credential helper is named, and killed with cosign (#365)" pass \
      "exit 5 after $(dt "$t0" "$t1") s under BRIG_VERIFY=require, naming docker-credential-e2ehang; no helper process and no sandbox left"
  else
    res check Verify "A hung credential helper is named, and killed with cosign (#365)" fail \
      "exit $rc after $(dt "$t0" "$t1") s; helpers left: [$(echo "$left" | one_line 120)]; brig ls: [$ls]; $(echo "$out" | one_line 300)"
  fi
  pkill -f 'docker-credential-e2ehang' 2> /dev/null || true
  tmo 60 brig rm claude-code@cos < /dev/null > /dev/null 2>&1 || true
  rm -rf "$d"
}

# boot_try MODE REF [ENV...]: one brig run -d REF under BRIG_VERIFY=MODE, with
# ENV added. It prints what brig said, removes the sandbox and returns brig's
# exit status.
boot_try() {
  local mode=$1 ref=$2 out rc=0
  shift 2
  out="$(env BRIG_VERIFY="$mode" "$@" perl -e 'alarm shift @ARGV; exec @ARGV or die "exec: $!"' 900 \
    brig run -d "$ref" --mem 2048 < /dev/null 2>&1)" || rc=$?
  printf '$ BRIG_VERIFY=%s %s brig run -d %s\n%s\n[exit %s]\n' "$mode" "$*" "$ref" "$out" "$rc" >&2
  tmo 120 brig rm "$ref" < /dev/null > /dev/null 2>&1 || true
  printf '%s\n' "$out"
  return "$rc"
}

BOOT_DIGEST_CHECKS=(
  "A first fetch is pinned to the digest that verified (#403)"
  "A changed kernel where brig fetches refuses under warn, and is not fetched over (#234)"
  "A named directory that differs refuses under require (#234)"
  "An older bundle where brig fetches is fetched again by digest (#403)"
)

# An older published bundle for the re-fetch case. Any tag the moving
# darwin-arm64 tag has left behind will do.
OLD_BUNDLE=ghcr.io/nofireai/hull-assets:0.1.6-darwin-arm64

# check_boot_digests runs the boot-asset digest checks against a scratch
# directory that HULL_BOOT_ASSETS hands hull, so hull's own store is left
# alone. brig counts that directory as its own choice, the one it fetches into.
check_boot_digests() {
  local a="$SCRATCH/.e2e/assets-bd" n="$SCRATCH/.e2e/assets-named" head name
  head="$(git -C "$REPO" rev-parse --short HEAD)"
  if ! command -v cosign > /dev/null; then
    for name in "${BOOT_DIGEST_CHECKS[@]}"; do
      res check Verify "$name" skip "no cosign on this Mac, so brig verifies nothing here"
    done
    return 0
  fi
  if ! grep -q 'func (c \*Config) checkBootDigests' "$REPO/internal/wrap/verify.go"; then
    for name in "${BOOT_DIGEST_CHECKS[@]}"; do
      res check Verify "$name" skip "brig at $head does not compare the boot assets with the bundle"
    done
    return 0
  fi
  local pinned=false
  if grep -q 'func (c \*Config) refetchBootAssets' "$REPO/internal/wrap/verify.go"; then
    pinned=true
  fi
  # shellcheck disable=SC2064
  trap "rm -rf '$a' '$n'" EXIT
  rm -rf "$a" "$n"
  mkdir -p "$a"

  local r0=0 o0 ref0
  o0="$(boot_try require claude-code@bd HULL_BOOT_ASSETS="$a")" || r0=$?
  ref0="$(field ref < "$a/provenance.json" 2> /dev/null || true)"
  if ! $pinned; then
    res check Verify "${BOOT_DIGEST_CHECKS[0]}" skip "brig at $head fetches the boot bundle by its tag"
  elif [ "$r0" = 0 ] && echo "$o0" | grep -q 'image and boot assets verified' && [ "${ref0#*@sha256:}" != "$ref0" ]; then
    res check Verify "${BOOT_DIGEST_CHECKS[0]}" pass \
      "an empty directory under require: exit 0, image and boot assets verified; hull's record names $ref0"
  else
    res check Verify "${BOOT_DIGEST_CHECKS[0]}" fail "exit $r0; hull's record names [$ref0]; $(echo "$o0" | one_line 300)"
  fi

  local w1=0 o1 before after
  if [ ! -f "$a/Image" ]; then
    res check Verify "${BOOT_DIGEST_CHECKS[1]}" fail "the first fetch left no Image in $a: $(cd "$a" && printf '%s ' *)"
  else
    printf 'x' >> "$a/Image"
    before="$(shasum -a 256 "$a/Image" | cut -c1-64)"
    o1="$(boot_try warn ubuntu@bd HULL_BOOT_ASSETS="$a")" || w1=$?
    after="$(shasum -a 256 "$a/Image" | cut -c1-64)"
    if [ "$w1" = 5 ] && echo "$o1" | grep -q 'are not the bundle that verified' && [ "$before" = "$after" ]; then
      res check Verify "${BOOT_DIGEST_CHECKS[1]}" pass \
        "one byte appended to Image: exit 5 under warn, and the file is still the changed one. $(echo "$o1" | grep refusing | one_line 200)"
    else
      res check Verify "${BOOT_DIGEST_CHECKS[1]}" fail \
        "exit $w1; Image ${before:0:12} before, ${after:0:12} after; $(echo "$o1" | one_line 300)"
    fi
  fi

  local r2=0 o2
  cp -R "$a" "$n"
  o2="$(boot_try require claude-code@bd BRIG_BOOT_ASSETS="$n")" || r2=$?
  if [ "$r2" = 5 ] && echo "$o2" | grep -q 'BRIG_BOOT_ASSETS names' && echo "$o2" | grep -q 'BRIG_VERIFY=require'; then
    res check Verify "${BOOT_DIGEST_CHECKS[2]}" pass "a named copy with the changed Image: exit 5 under require, before hull"
  else
    res check Verify "${BOOT_DIGEST_CHECKS[2]}" fail "exit $r2; $(echo "$o2" | one_line 300)"
  fi

  if ! $pinned; then
    res check Verify "${BOOT_DIGEST_CHECKS[3]}" skip "brig at $head refuses an older bundle and leaves the files to delete"
    return 0
  fi
  local w3=0 o3 old new ref3
  rm -rf "$a"
  mkdir -p "$a"
  HULL_BOOT_ASSETS="$a" run 600 hull assets pull "$OLD_BUNDLE" || true
  old="$(field digest < "$a/provenance.json" 2> /dev/null || true)"
  o3="$(boot_try warn ubuntu@bd HULL_BOOT_ASSETS="$a")" || w3=$?
  new="$(field digest < "$a/provenance.json" 2> /dev/null || true)"
  ref3="$(field ref < "$a/provenance.json" 2> /dev/null || true)"
  if [ -n "$old" ] && [ "$old" = "$new" ] && [ "$w3" = 0 ]; then
    res check Verify "${BOOT_DIGEST_CHECKS[3]}" skip "the moving tag still names $OLD_BUNDLE ($old)"
  elif [ "$w3" = 0 ] && [ -n "$old" ] && [ "$new" != "$old" ] && [ "${ref3#*@sha256:}" != "$ref3" ] &&
       echo "$o3" | grep -q 'boot assets verified'; then
    res check Verify "${BOOT_DIGEST_CHECKS[3]}" pass \
      "$OLD_BUNDLE (${old:0:19}) in the directory: warn boots, boot assets verified, and hull's record now names $ref3"
  else
    res check Verify "${BOOT_DIGEST_CHECKS[3]}" fail \
      "exit $w3; record before [${old:0:19}], after [${new:0:19}], ref [$ref3]; $(echo "$o3" | one_line 300)"
  fi
}

# ---------------------------------------------------------------- main

say "brig e2e $LEVEL on macOS, results in $RESULTS"
res host mac
res meta host_os macos
res meta level "$LEVEL"
res meta run_note "LEVEL=$LEVEL: $SEQ_N ubuntu and $CLAUDE_N claude-code cycles, every profile once."
if [ -n "${GITHUB_RUN_ID:-}" ]; then
  res meta run_id "$GITHUB_RUN_ID"
  res meta run_url "${GITHUB_SERVER_URL:-https://github.com}/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID"
  res meta source "GitHub Actions run $GITHUB_RUN_ID, attempt ${GITHUB_RUN_ATTEMPT:-1} (${GITHUB_EVENT_NAME:-unknown})"
fi

block setup setup
if [ -f "$OUT/setup.ok" ]; then
  block version check_version
  block gatekeeper gate_gatekeeper
  block profiles check_profiles
  block doctor check_doctor
  block cycles check_cycles
  block assets record_boot_assets
  block homes check_rm_home
  block failed-boot check_failed_boot
  block shell check_shell_removed
  block postures check_postures
  block ports check_ports
  block symlink check_symlink
  block brigd-lock check_brigd_lock
  block claude-tmpfs check_claude_tmpfs
  block creds check_creds
  block agent-rm check_agent_rm
  block files-mode check_files_mode
  block policy check_policy_refusals
  block posture-info check_posture_info
  block gateway check_shared_gateway
  block reach check_reach
  block cosign check_cosign_hang
  block boot-digests check_boot_digests
else
  say "setup failed, so no gate ran"
fi

verdict="$(res build "$RESULTS")"
say "verdict: $verdict ($RESULTS)"
[ "$verdict" = go ]
