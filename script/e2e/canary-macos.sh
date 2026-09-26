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

# gsh REF TEXT: runs TEXT in the guest with `brig sh REF`, on a pty from
# script(1), and prints what the guest said. script(1) echoes ^D and two
# backspaces ahead of the output, and the pty adds CRs, so both go. brig's own
# notices share the pty, so its "brig: " lines go to the log only.
gsh() {
  local ref=$1 text=$2 out rc=0
  out="$(tmo "${GSH_TIMEOUT:-300}" script -q /dev/null brig sh "$ref" "$text" < /dev/null)" || rc=$?
  out="$(printf '%s' "$out" | tr -d '\r\b' | sed 's/^\^D//')"
  printf '$ brig sh %s %q\n%s\n[exit %s]\n' "$ref" "$text" "$out" "$rc" >&2
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
  if [ "$commit" != "$head" ]; then
    res check Install "brig version names this commit" fail "$version; commit $commit, checkout $head"
    return 1
  fi
  touch "$OUT/setup.ok"
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
  # shared Mac.
  run 900 brig run -d claude-code@pshared --mem 2048
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
  w="$(echo "$err" | grep 'is being restarted' || true)"
  if [ "$rc" = 0 ] && [ -n "$w" ] && [ "$(probe_field "$p" MARKER)" = NONE ] && [ "$(probe_field "$p" EGRESS_HTTP)" = 200 ]; then
    res check Network "--network shared restarts it, with the warning (#342)" pass "$(echo "$w" | cut -c1-160)... New guest (no marker), HTTP 200"
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
  block homes check_rm_home
  block failed-boot check_failed_boot
  block shell check_shell_removed
  block postures check_postures
  block ports check_ports
  block symlink check_symlink
else
  say "setup failed, so no gate ran"
fi

verdict="$(res build "$RESULTS")"
say "verdict: $verdict ($RESULTS)"
[ "$verdict" = go ]
