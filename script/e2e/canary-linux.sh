#!/usr/bin/env bash
# Real-runtime checks for brig on a fresh GitHub-hosted ubuntu-24.04 runner.
#
# It installs brig the way a user does, with this checkout's install.sh as a
# rootless user install, and swaps in brig and brigd built from this checkout.
# Then it runs one gate per past release blocker and a set of checks, and
# writes results.json (schema 1) for render-report.py.
#
# The host needs /dev/kvm, passwordless sudo and a few GB of free disk. The
# script changes it: packages, udev rules, a lingering user session and a
# second user. Run it on a throwaway VM only.
#
# Settings:
#
#   LEVEL           canary (default) or nightly. nightly boots more often,
#                   and adds a parallel block and stop/start churn
#   E2E_HOST        the host id in the report (default linux)
#   BRIG_RUNTIME_VERSION
#                   a brig-standalone-linux tag to install in place of the
#                   one install.sh pins. install.sh reads it itself
#   E2E_OUT         where logs/ go
#                   (default $RUNNER_TEMP/brig-e2e, else /tmp/brig-e2e)
#   E2E_RESULTS     the results file (default $E2E_OUT/results.json)
#   BRIG_BUILD_DIR  a directory holding brig and brigd built from this
#                   checkout. Unset, the script runs make build itself
#   E2E_BASELINE    a results.json to compare with. The workflow compares
#                   in its report job instead
#
# It exits 1 on a no-go verdict, after results.json is written.

# Guest command text is single-quoted, so that the guest's shell expands it.
# shellcheck disable=SC2016

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"

LEVEL="${LEVEL:-canary}"
case "$LEVEL" in
  canary)  SEQ_N=3;  CLAUDE_N=3;  PAR_N=0; CHURN_N=0 ;;
  nightly) SEQ_N=20; CLAUDE_N=20; PAR_N=2; CHURN_N=10 ;;
  *) echo "canary-linux: LEVEL must be canary or nightly, not $LEVEL" >&2; exit 2 ;;
esac
EXEC_N=100

HOST="${E2E_HOST:-linux}"
RUNTIME_OVERRIDE="${BRIG_RUNTIME_VERSION:-}"
if [ -n "$RUNTIME_OVERRIDE" ] &&
   ! printf '%s\n' "$RUNTIME_OVERRIDE" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-rc[0-9]+)?$'; then
  echo "canary-linux: BRIG_RUNTIME_VERSION must look like v0.1.0 or v0.1.0-rc10, not $RUNTIME_OVERRIDE" >&2
  exit 2
fi

OUT="${E2E_OUT:-${RUNNER_TEMP:-/tmp}/brig-e2e}"
RESULTS="${E2E_RESULTS:-$OUT/results.json}"
LOGS="$OUT/logs"
mkdir -p "$LOGS"
export E2E_RECORDS="$OUT/records.jsonl"
: > "$E2E_RECORDS"
exec > >(tee -a "$LOGS/canary.log") 2>&1

# brig's own usage data stays off for the whole run. --dnt would test a flag,
# not the default path a user takes.
export DO_NOT_TRACK=1
# The HOME gate is about a pull with no DOCKER_CONFIG, so nothing here sets it.
unset DOCKER_CONFIG
export PATH="$HOME/.local/bin:$PATH"
USER="${USER:-$(id -un)}"
XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export XDG_RUNTIME_DIR
export DBUS_SESSION_BUS_ADDRESS="unix:path=$XDG_RUNTIME_DIR/bus"
BUNDLE_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/brig/data"

# ---------------------------------------------------------------- helpers

say() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*"; }
res() { python3 "$HERE/results.py" "$@"; }
now() { date +%s.%N; }
dt() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.2f", b - a }'; }
one_line() { tr '\n' ' ' | tr -s ' ' | cut -c1-"${1:-400}"; }

# run SECONDS CMD...: logs CMD, runs it bounded and returns its status.
run() {
  local s=$1
  shift
  echo "\$ $*"
  local rc=0
  timeout --kill-after=10 "$s" "$@" || rc=$?
  echo "[exit $rc]"
  return "$rc"
}

# gsh REF TEXT: runs the script TEXT in the guest with `brig sh REF -c` and
# prints what came back, without CRs. brig sh needs a console on stdin and
# stdout, so it runs on a pty from script(1). A copy goes to stderr, which is
# the block's log.
gsh() {
  local ref=$1 text=$2 f out rc=0
  f="$(mktemp "$OUT/gsh.XXXXXX")"
  printf '#!/usr/bin/env bash\nexec brig sh %q -c %q\n' "$ref" "$text" > "$f"
  chmod +x "$f"
  out="$(timeout --kill-after=10 "${GSH_TIMEOUT:-300}" script -q -e -c "$f" /dev/null < /dev/null)" || rc=$?
  rm -f "$f"
  out="${out//$'\r'/}"
  printf '$ brig sh %s -c %q\n%s\n[exit %s]\n' "$ref" "$text" "$out" "$rc" >&2
  printf '%s\n' "$out"
  return "$rc"
}

# rk ARGS...: nerdctl against this user's rootless containerd, with the
# environment the bundle's launcher gives brig.
rk() {
  (
    set +u
    # shellcheck disable=SC1091
    . "$BUNDLE_DIR/etc/brig-env.sh"
    timeout --kill-after=10 "${RK_TIMEOUT:-900}" nerdctl "$@"
  )
}

# image_present REF: whether REF is in the rootless store.
image_present() {
  [ -n "$(rk images -q "$1" 2> /dev/null)" ]
}

# field ARGS...: one field of the JSON document on stdin.
field() { res field - "$1"; }

# vmms: the monitor, shim and virtiofsd processes still running.
vmms() {
  pgrep -a -f 'cloud-hypervisor|qemu-system|firecracker|virtiofsd|containerd-shim-urunc' || true
}

# leftovers TAG: records whether anything outlived a block: a sandbox brig
# lists, a container or task in the runtime, a guest home, or a monitor.
leftovers() {
  local tag=$1 bad="" out
  out="$(timeout 60 brig ls -q 2>&1 | grep -v '^(none' || true)"
  [ -z "$out" ] || bad+="brig ls: $(echo "$out" | one_line 200); "
  out="$(rk ps -a --format '{{.Names}}' 2>&1 || true)"
  [ -z "$out" ] || bad+="nerdctl ps -a: $(echo "$out" | one_line 200); "
  out="$(timeout 60 "$BUNDLE_DIR/bin/brig-ctl" ctr tasks ls -q 2>&1 || true)"
  [ -z "$out" ] || bad+="ctr tasks: $(echo "$out" | one_line 200); "
  out="$(ls -A "$HOME/.brig/homes" 2> /dev/null || true)"
  [ -z "$out" ] || bad+="guest homes: $(echo "$out" | one_line 200); "
  out="$(vmms)"
  [ -z "$out" ] || bad+="processes: $(echo "$out" | one_line 300); "
  echo "leftovers after $tag: ${bad:-none}"
  if [ -z "$bad" ]; then
    res check Leftovers "After $tag" pass "no sandbox, container, task, guest home or monitor process"
  else
    res check Leftovers "After $tag" fail "$bad"
    # The next block starts clean, so one leftover is reported once.
    timeout 300 brig rm --all -y || true
  fi
}

# block NAME FUNCTION: runs FUNCTION in a subshell with errexit on, its output
# in logs/NAME.log, then looks for leftovers. A block that stops early is a
# failed check, and any gate it owns stays unrecorded, which is a no-go.
#
# Call it as a plain command. In the context of || or an if, bash ignores
# errexit in the subshell too.
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

# ---------------------------------------------------------------- setup

setup() {
  echo "runner: $(uname -a)"
  echo "cpus: $(nproc), memory: $(free -m | awk '/^Mem:/ { print $2 }') MB"
  df -h / /mnt 2> /dev/null || df -h /
  ls -l /dev/kvm /dev/vhost-vsock 2>&1 || true
  [ -e /dev/kvm ] || { echo "no /dev/kvm on this runner"; return 1; }

  # Four images of about 1.7 GB each are pulled one at a time. The hosted
  # image leaves less than that free, so the largest toolchains go.
  local avail
  avail="$(df --output=avail -BG / | tail -n 1 | tr -dc 0-9)"
  if [ "$avail" -lt 30 ]; then
    echo "only ${avail} GB free on /, removing unused toolchains"
    sudo rm -rf /usr/local/lib/android /usr/share/dotnet /opt/ghc /usr/local/.ghcup \
      /opt/hostedtoolcache/CodeQL
    df -h /
  fi

  # The installer stops, and prints commands for root, when newuidmap,
  # setfacl or a subuid range is missing. It installs none of them itself.
  # A run under a policy refuses to boot without nft, so the egress checks
  # need it too.
  local pkgs=()
  command -v newuidmap > /dev/null || pkgs+=(uidmap)
  command -v setfacl > /dev/null || pkgs+=(acl)
  command -v nft > /dev/null || [ -x /usr/sbin/nft ] || pkgs+=(nftables)
  if [ "${#pkgs[@]}" -gt 0 ]; then
    run 300 sudo apt-get update -qq
    run 600 sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "${pkgs[@]}"
  fi
  if ! grep -q "^$USER:" /etc/subuid || ! grep -q "^$USER:" /etc/subgid; then
    local first
    first="$(cat /etc/subuid /etc/subgid 2> /dev/null |
      awk -F: '$2 + $3 > m { m = $2 + $3 } END { print (m > 100000 ? m : 100000) }')"
    run 30 sudo usermod --add-subuids "$first-$((first + 65535))" \
      --add-subgids "$first-$((first + 65535))" "$USER"
  fi
  grep "^$USER:" /etc/subuid /etc/subgid

  # The runner user gets the devices by name, the way the bundle's own rule
  # grants them. A mode of 0666 would also open them to the second user the
  # no-sudo gate creates, and that user must find them closed.
  run 30 sudo modprobe vhost_vsock
  printf '%s\n' \
    "KERNEL==\"kvm\", SUBSYSTEM==\"misc\", MODE=\"0660\", RUN+=\"/usr/bin/setfacl -m u:$USER:rw /dev/kvm\"" \
    "KERNEL==\"vhost-vsock\", SUBSYSTEM==\"misc\", MODE=\"0660\", RUN+=\"/usr/bin/setfacl -m u:$USER:rw /dev/vhost-vsock\"" |
    sudo tee /etc/udev/rules.d/99-brig-e2e-devices.rules
  run 30 sudo udevadm control --reload-rules
  run 30 sudo udevadm trigger --subsystem-match=misc --sysname-match=kvm
  run 30 sudo udevadm trigger --subsystem-match=misc --sysname-match=vhost-vsock
  run 30 sudo udevadm settle --timeout=10 || true
  getfacl -p /dev/kvm /dev/vhost-vsock

  # A rootless install runs containerd as a systemd --user unit, which needs a
  # user manager. The runner has no login session, so lingering starts one.
  run 30 sudo loginctl enable-linger "$USER"
  local i
  for i in $(seq 1 30); do
    [ -S "$XDG_RUNTIME_DIR/bus" ] && break
    sleep 1
  done
  ls -la "$XDG_RUNTIME_DIR"
  run 30 systemctl --user is-system-running || true

  # The installer asks the unauthenticated API for the newest brig release.
  # That limit is per address, and hosted runners share addresses. gh gives
  # the same answer with the job's token. The canary replaces that brig anyway.
  local release=""
  if [ -n "${GH_TOKEN:-}" ] && command -v gh > /dev/null; then
    release="$(timeout 60 gh api 'repos/brig-sh/brig/releases?per_page=100' \
      --jq '[.[] | select((.draft | not) and (.tag_name | startswith("v")))][0].tag_name' || true)"
  fi
  echo "${release}" > "$OUT/brig-release"
  echo "brig release for install.sh: ${release:-resolved by install.sh}"

  # The install the way a user runs it: this commit's install.sh, as this
  # user, so a rootless user install. It uses sudo for the host preparation.
  local rc=0
  (cd "$HOME" && run 1200 env ${release:+BRIG_VERSION="$release"} sh "$REPO/install.sh") || rc=$?
  [ "$rc" = 0 ] || { echo "install.sh failed with exit $rc"; return 1; }
  [ -x "$BUNDLE_DIR/bin/brig" ] || { echo "no brig at $BUNDLE_DIR/bin/brig"; return 1; }
  cat "$HOME/.local/bin/brig"

  # Swap in this checkout's brig and brigd where the bundle's launchers
  # exec them, as install.sh does with a release.
  local build="${BRIG_BUILD_DIR:-}"
  if [ -z "$build" ]; then
    build="$OUT/build"
    mkdir -p "$build"
    run 900 make -C "$REPO" build BINDIR="$build"
  fi
  install -m 0755 "$build/brig" "$BUNDLE_DIR/bin/brig"
  install -m 0755 "$build/brigd" "$BUNDLE_DIR/bin/brigd"
  sha256sum "$build/brig" "$build/brigd" "$BUNDLE_DIR/bin/brig" "$BUNDLE_DIR/bin/brigd"

  local head version commit
  head="$(git -C "$REPO" rev-parse HEAD)"
  version="$(timeout 30 brig version)"
  commit="$(timeout 30 brig version --json | field data.commit)"
  echo "brig version: $version"
  echo "commit: $commit, checkout: $head"
  sed -i "s/^BRIG_VERSION=.*/BRIG_VERSION=${version%% *}/" "$BUNDLE_DIR/pins.env"
  cp "$BUNDLE_DIR/pins.env" "$LOGS/pins.env"
  cat "$LOGS/pins.env"

  local bundle urunc
  bundle="$(awk -F= '$1 == "BUNDLE_VERSION" { print $2 }' "$BUNDLE_DIR/pins.env")"
  urunc="$(awk -F= '$1 == "URUNC_REF" { print $2 }' "$BUNDLE_DIR/pins.env")"
  res meta commit "$head"
  # The bundle is read from what was installed, so the page names what ran.
  local source="install.sh pin" short=Linux
  if [ -n "$RUNTIME_OVERRIDE" ]; then
    source=override
    short="Linux ${bundle#v*-}"
  fi
  res meta host_short "$short"
  res meta host_label "GitHub-hosted ubuntu-24.04, x64, bundle $bundle ($source)"
  res meta target "brig ${head:0:7} · bundle $bundle"
  res fact "brig version" "$version"
  res fact "runtime bundle ($short)" "$bundle ($source)" "https://github.com/NOFireAI/brig-standalone-linux/releases/tag/$bundle"
  res fact "urunc ($short)" "${urunc:0:7}" "https://github.com/urunc-dev/urunc/commit/$urunc"
  local assets monitors
  assets="$(awk -F= '$1 == "ASSETS_VERSION" { print $2 }' "$BUNDLE_DIR/pins.env")"
  monitors="$(awk -F= '$1 == "MONITORS_VERSION" { print $2 }' "$BUNDLE_DIR/pins.env")"
  res fact "boot assets ($short)" "hull-assets ${assets:-unknown}"
  res fact "VMMs ($short)" "${monitors:-unknown}"
  res meta host_detail "$(nproc) vCPUs, $(free -g | awk '/^Mem:/ { print $2 }') GB, kernel $(uname -r), nested KVM. A rootless user install from install.sh at ${head:0:7}, with runtime bundle $bundle ($source)."

  if [ "$commit" != "$head" ]; then
    echo "brig version names $commit, not the checkout's $head"
    res check Install "brig version names this commit" fail "$version; commit $commit, checkout $head"
    return 1
  fi
  touch "$OUT/setup.ok"
}

# ---------------------------------------------------------------- checks

check_version() {
  local version commit modified out rc=0 bad
  version="$(timeout 30 brig version)"
  commit="$(timeout 30 brig version --json | field data.commit)"
  modified="$(timeout 30 brig version --json | field data.modified)"
  if [ "$commit" = "$(git -C "$REPO" rev-parse HEAD)" ] && [ "$modified" = false ] &&
     cmp -s "$BUNDLE_DIR/bin/brigd" "${BRIG_BUILD_DIR:-$OUT/build}/brigd"; then
    res check Install "brig version names this commit" pass \
      "$version; brigd is the same build ($(sha256sum "$BUNDLE_DIR/bin/brigd" | cut -c1-12))"
  else
    res check Install "brig version names this commit" fail "$version; commit $commit, modified $modified"
  fi

  out="$(run 120 brig doctor 2>&1)" || rc=$?
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

gate_home() {
  local ref=docker.io/library/ubuntu:latest had=no rc=0 t0 t1 pulled size out
  echo "DOCKER_CONFIG: ${DOCKER_CONFIG:-unset}; ~/.docker: $(ls -ld "$HOME/.docker" 2>&1)"
  if image_present "$ref"; then
    had=yes
    rk rmi -f "$ref"
  fi
  if image_present "$ref"; then
    res gate home fail "$ref is still in the store after nerdctl rmi, so the boot could not be forced to pull."
    return 0
  fi
  t0="$(now)"
  run 900 env -u DOCKER_CONFIG brig --verbose run -d ubuntu@pull > "$LOGS/home-pull.log" 2>&1 || rc=$?
  t1="$(now)"
  tr '\r' '\n' < "$LOGS/home-pull.log" | grep -v 'fetching image content' | tail -n 40
  pulled="$(tr '\r' '\n' < "$LOGS/home-pull.log" | grep -c -E 'Completed pull|Pulling from OCI Registry' || true)"
  size="$(tr '\r' '\n' < "$LOGS/home-pull.log" | grep 'Completed pull' | grep -o -E 'total: *[0-9.]+ *[A-Za-z]+' | tail -n 1 | tr -s ' ' || true)"
  out="$(gsh ubuntu@pull 'echo HOME=$HOME; env | grep -c DOCKER_CONFIG')" || true
  run 300 brig rm ubuntu@pull || true

  local where="removed from the store first"
  [ "$had" = yes ] || where="not in the store"
  if [ "$rc" = 0 ] && [ "$pulled" -gt 0 ] && image_present "$ref" && echo "$out" | grep -q '^HOME='; then
    res gate home pass "ubuntu:latest $where, then pulled by brig run (${size:-size not printed}) with DOCKER_CONFIG unset. Booted in $(dt "$t0" "$t1") s and answered."
  else
    local why
    why="$(tr '\r' '\n' < "$LOGS/home-pull.log" | grep -i -E 'denied|EACCES|permission|error|fatal' | head -n 3 | one_line 300)"
    res gate home fail "brig run exit $rc, pull lines $pulled. ${why:-No error line.}"
  fi
}

gate_exec() {
  local sb i out rc miss_a=0 wrong_a=0 miss_b=0 wrong_b=0 t0
  run 900 brig run -d ubuntu
  sb="$(timeout 30 brig ls | awk '$1 == "ubuntu" { print $2 }')"
  [ -n "$sb" ] || { res gate exec fail "brig run -d ubuntu started no sandbox"; return 0; }

  t0="$(now)"
  for i in $(seq 1 "$EXEC_N"); do
    rc=0
    out="$(gsh ubuntu "echo ok-$i" 2> /dev/null)" || rc=$?
    if [ -z "$out" ]; then
      miss_a=$((miss_a + 1)); echo "A $i: empty (exit $rc)"
    elif [ "$out" != "ok-$i" ]; then
      wrong_a=$((wrong_a + 1)); echo "A $i: wrong: $(printf '%q' "$out") (exit $rc)"
    fi
  done
  echo "brig sh x$EXEC_N: $miss_a empty, $wrong_a wrong, $(dt "$t0" "$(now)") s"

  t0="$(now)"
  for i in $(seq 1 "$EXEC_N"); do
    rc=0
    out="$(RK_TIMEOUT=60 rk exec "$sb" echo "ok-$i" 2>&1)" || rc=$?
    if [ -z "$out" ]; then
      miss_b=$((miss_b + 1)); echo "B $i: empty (exit $rc)"
    elif [ "$out" != "ok-$i" ]; then
      wrong_b=$((wrong_b + 1)); echo "B $i: wrong: $(printf '%q' "$out") (exit $rc)"
    fi
  done
  echo "nerdctl exec x$EXEC_N: $miss_b empty, $wrong_b wrong, $(dt "$t0" "$(now)") s"
  run 300 brig rm ubuntu || true

  local note="brig sh: $((EXEC_N - miss_a - wrong_a))/$EXEC_N correct ($miss_a empty, $wrong_a wrong). nerdctl exec: $((EXEC_N - miss_b - wrong_b))/$EXEC_N correct ($miss_b empty, $wrong_b wrong)."
  if [ $((miss_a + wrong_a + miss_b + wrong_b)) = 0 ]; then
    res gate exec pass "$note"
  else
    res gate exec fail "$note"
  fi
}

# Phrases from brig's checks of the claude-code guest's .claude mounts
# (internal/wrap/secretfiles.go), each printed with the path it checked.
REFUSAL='should be ephemeral|is not mounted|should be kept on the host|rather than an ephemeral mount|could not check what'

gate_claude() {
  local i rc_run rc_sh rc_rm t0 t1 ro so refusals=0 failures=0 fs
  rk pull -q ghcr.io/brig-sh/claude-code-stock:root
  : > "$OUT/claude-fs.txt"
  for i in $(seq 1 "$CLAUDE_N"); do
    rc_run=0; rc_sh=0; rc_rm=0
    t0="$(now)"
    ro="$(timeout --kill-after=10 600 brig run -d claude 2>&1)" || rc_run=$?
    t1="$(now)"
    so="$(gsh claude "echo ok-$i; stat -f -c %T /root/.claude")" || rc_sh=$?
    timeout --kill-after=10 300 brig rm claude || rc_rm=$?
    fs="$(echo "$so" | grep -v "^ok-$i\$" | tail -n 1)"
    echo "$fs" >> "$OUT/claude-fs.txt"
    echo "boot $i: run exit $rc_run in $(dt "$t0" "$t1") s, sh exit $rc_sh, rm exit $rc_rm, /root/.claude on [$fs]"
    if printf '%s\n%s\n' "$ro" "$so" | grep -E "$REFUSAL" | grep -q '\.claude'; then
      refusals=$((refusals + 1))
      printf '%s\n%s\n' "$ro" "$so" | grep -E "$REFUSAL" | sed 's/^/  refusal: /'
    elif [ "$rc_run" != 0 ] || [ "$rc_sh" != 0 ] || ! echo "$so" | grep -q "^ok-$i\$"; then
      failures=$((failures + 1))
      echo "  run said: $(echo "$ro" | one_line 300)"
      echo "  sh said: $(echo "$so" | one_line 300)"
    fi
    [ "$rc_run" != 0 ] || res sample "claude-code boot" "$(dt "$t0" "$t1")"
  done
  res note "claude-code boot" "median of $CLAUDE_N runs of brig run -d claude, image already pulled"
  local seen
  seen="$(sort "$OUT/claude-fs.txt" | uniq -c | awk '{ $1 = $1; print }' | paste -s -d, - | sed 's/,/, /g')"
  local note="$refusals refusals naming .claude and $failures other failures in $CLAUDE_N boots. /root/.claude: $seen."
  if [ "$refusals" = 0 ] && [ "$failures" = 0 ]; then
    res gate claude pass "$note"
  else
    res gate claude fail "$note"
  fi
}

gate_nosudo() {
  local u=brig-e2e-nosudo uid rc=0 t0 t1 out release sudo_says fetched lines
  # useradd has hung on a hosted runner once, so it gets a second try and
  # the log says what held the account files.
  local try
  for try in 1 2; do
    id "$u" > /dev/null 2>&1 && break
    run 120 sudo useradd --create-home --shell /bin/bash "$u" && break
    echo "useradd did not finish (try $try). What holds the account files:"
    sudo lsof /etc/passwd /etc/shadow /etc/group /etc/subuid /etc/subgid /etc/.pwd.lock 2>&1 | head -n 20 || true
    pgrep -a -f 'useradd|usermod|groupadd|adduser|chpasswd' || true
  done
  if ! id "$u" > /dev/null 2>&1; then
    res gate nosudo fail "Could not create the second user: useradd did not finish in 120 s, twice. The log has what held the account files."
    return 0
  fi
  uid="$(id -u "$u")"
  # A user with a login session and no sudo: the one the installer has to
  # stop for, before it downloads anything.
  run 30 sudo loginctl enable-linger "$u"
  for _ in $(seq 1 30); do
    [ -d "/run/user/$uid" ] && break
    sleep 1
  done
  sudo_says="$(sudo -u "$u" sudo -n true 2>&1)" && {
    res gate nosudo fail "$u can run sudo without a password, so this is not the no-sudo case"
    return 0
  }
  echo "sudo -n true as $u: $sudo_says"

  install -m 0644 "$REPO/install.sh" /tmp/brig-e2e-install.sh
  release="$(cat "$OUT/brig-release")"
  # The bundle's installer downloads into a private directory under /tmp, so
  # only root can watch for its tarball there.
  rm -f "$OUT/nosudo.done"
  (
    while [ ! -e "$OUT/nosudo.done" ]; do
      sudo find /tmp -maxdepth 3 -user "$u" -type f -printf '%s %p\n' 2> /dev/null || true
      sleep 0.5
    done
  ) > "$OUT/nosudo-watch.txt" &
  local watch=$!
  t0="$(now)"
  # shellcheck disable=SC2024 # the output file is this user's, not $u's
  sudo -u "$u" env -i HOME="/home/$u" USER="$u" LOGNAME="$u" SHELL=/bin/bash \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    XDG_RUNTIME_DIR="/run/user/$uid" DO_NOT_TRACK=1 ${release:+BRIG_VERSION="$release"} \
    ${RUNTIME_OVERRIDE:+BRIG_RUNTIME_VERSION="$RUNTIME_OVERRIDE"} \
    timeout --kill-after=10 600 script -q -e -c "cd && sh /tmp/brig-e2e-install.sh" /dev/null \
    < /dev/null > "$OUT/nosudo.out" 2>&1 || rc=$?
  t1="$(now)"
  touch "$OUT/nosudo.done"
  wait "$watch" || true
  out="$(tr '\r' '\n' < "$OUT/nosudo.out")"
  echo "$out"
  fetched="$(awk '{ print $2 }' "$OUT/nosudo-watch.txt" | sed 's|.*/||' | grep -E '^(cosign|brig-.*\.tar\.gz)$' |
    sort -u | paste -s -d, - | sed 's/,/, /g')"
  echo "files $u had under /tmp during the run: ${fetched:-none}"
  lines="$(echo "$out" | sed -n "/<<'BRIG_ROOT'/,/^BRIG_ROOT/p" | grep -c . || true)"

  local problems=""
  [ "$rc" != 0 ] || problems+="the installer exited 0; "
  case "$rc" in 124 | 137) problems+="the installer timed out after 600 s; " ;; esac
  echo "$out" | grep -q 'is not set up for a rootless brig' || problems+="no 'not set up' error; "
  [ "$lines" -gt 2 ] || problems+="no block of commands for root; "
  ! echo "$out" | grep -q -i -E 'password for|\[sudo\]' || problems+="a sudo prompt appeared; "
  ! grep -q -E 'bundle\.tar\.gz|brig-standalone-.*\.tar\.gz' "$OUT/nosudo-watch.txt" || problems+="the bundle tarball was downloaded; "
  ! sudo test -e "/home/$u/.local/share/brig" || problems+="/home/$u/.local/share/brig exists; "

  run 30 sudo loginctl disable-linger "$u" || true
  run 30 sudo loginctl terminate-user "$u" || true
  run 60 sudo userdel -r "$u" || true
  rm -f /tmp/brig-e2e-install.sh

  if [ -z "$problems" ]; then
    res gate nosudo pass "Stopped after $(dt "$t0" "$t1") s with exit $rc and a $lines-line block of commands for root, with no prompt. No bundle tarball appeared. brig's own installer had fetched: ${fetched:-nothing}."
  else
    res gate nosudo fail "Exit $rc after $(dt "$t0" "$t1") s: $problems"
  fi
}

# vmm_cpus: the --cpus boot= value of the one cloud-hypervisor running.
vmm_cpus() {
  pgrep -a -f cloud-hypervisor | grep -o -E 'boot=[0-9]+' | head -n 1 | cut -d= -f2 || true
}

gate_cpus() {
  local want n0 n3 b0 b3 bundle rc=0
  want="$(timeout 30 brig agent show ubuntu --json | field cpus)"
  run 900 brig run -d ubuntu@cpu
  n0="$(gsh ubuntu@cpu nproc | tail -n 1)" || true
  b0="$(vmm_cpus)"
  run 300 brig rm ubuntu@cpu || true
  run 900 brig run -d ubuntu@cpu3 --cpus 3
  n3="$(gsh ubuntu@cpu3 nproc | tail -n 1)" || true
  b3="$(vmm_cpus)"
  run 300 brig rm ubuntu@cpu3 || true
  bundle="$(awk -F= '$1 == "BUNDLE_VERSION" { print $2 }' "$BUNDLE_DIR/pins.env")"
  local note="Profile default (cpus: $want): nproc=$n0, cloud-hypervisor boot=${b0:-?}. --cpus 3: nproc=$n3, boot=${b3:-?}. Bundle $bundle."
  res at-least "$bundle" v0.1.0-rc10 || rc=$?
  if [ "$n0" = "$want" ] && [ "$n3" = 3 ]; then
    res gate cpus pass "$note"
  elif [ "$rc" = 1 ] && [ "$n0" = 1 ] && [ "$n3" = 1 ]; then
    res gate cpus expected "$note The fix ships in bundle rc10, and this gate is enforced from there."
  else
    res gate cpus fail "$note"
  fi
}

# release_sig DIR IDENTITY: the last line the bundle's cosign prints when it
# checks the signature on DIR/checksums.txt against IDENTITY.
release_sig() {
  timeout --kill-after=10 120 "$BUNDLE_DIR/bin/cosign" verify-blob \
    --certificate "$1/checksums.txt.pem" --signature "$1/checksums.txt.sig" \
    --certificate-identity-regexp "$2" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    "$1/checksums.txt" 2>&1 | tail -n 1
}

# check_bundle_record reads what a bundle from rc12 on keeps in share/guest:
# SHA256SUMS, the sha256 of its kernel and initrd, beside the release's
# checksums.txt and its signature. brig checks the same chain before a boot.
# Here the bundle's own cosign checks the signature against the identity
# install.sh pins, and against another identity, which must be refused.
check_bundle_record() {
  local g="$BUNDLE_DIR/share/guest" bundle rc=0 sums rec listed id good other
  bundle="$(awk -F= '$1 == "BUNDLE_VERSION" { print $2 }' "$BUNDLE_DIR/pins.env")"
  ls -la "$g"
  if [ ! -f "$g/SHA256SUMS" ]; then
    res at-least "$bundle" v0.1.0-rc12 || rc=$?
    if [ "$rc" = 1 ]; then
      res check Install "The bundle keeps a record of its kernel and initrd" skip \
        "bundle $bundle is older than rc12, the first to keep one"
      res check Install "The release's signature on that record verifies" skip \
        "bundle $bundle is older than rc12, the first to keep one"
    else
      res check Install "The bundle keeps a record of its kernel and initrd" fail \
        "bundle $bundle, and no SHA256SUMS in share/guest: $(cd "$g" && printf '%s ' *)"
    fi
    return 0
  fi

  sums="$(cd "$g" && sha256sum -c SHA256SUMS 2>&1)" || rc=$?
  rec="$(sha256sum "$g/SHA256SUMS" | cut -d' ' -f1)"
  listed="$(awk -v r="$rec" '$1 == r { print $2 }' "$g/checksums.txt" 2> /dev/null || true)"
  echo "$sums"
  echo "record sha256 $rec, listed in checksums.txt as: ${listed:-nothing}"
  if [ "$rc" = 0 ] && [ -n "$listed" ]; then
    res check Install "The bundle keeps a record of its kernel and initrd" pass \
      "$(echo "$sums" | one_line 120); checksums.txt lists the record as $listed"
  else
    res check Install "The bundle keeps a record of its kernel and initrd" fail \
      "sha256sum -c exit $rc: $(echo "$sums" | one_line 160); listed as: ${listed:-nothing}"
  fi

  id="$(sed -n "s/^RUNTIME_SIG_IDENTITY='\(.*\)'\$/\1/p" "$REPO/install.sh")"
  good="$(release_sig "$g" "$id")" || true
  other="$(release_sig "$g" '^https://github\.com/brig-sh/brig/')" || true
  echo "release identity $id: $good"
  echo "brig's release identity: $other"
  if [ "$good" = "Verified OK" ] && echo "$other" | grep -q 'none of the expected identities matched'; then
    res check Install "The release's signature on that record verifies" pass \
      "cosign verify-blob: Verified OK against install.sh's RUNTIME_SIG_IDENTITY; brig's own release identity is refused"
  else
    res check Install "The release's signature on that record verifies" fail \
      "release identity: $(echo "$good" | one_line 160); another identity: $(echo "$other" | one_line 160)"
  fi
}

# ---------------------------------------------------------------- checks

check_cycles() {
  local i rc t0 t1 out fails=0
  for i in $(seq 1 "$SEQ_N"); do
    rc=0
    t0="$(now)"
    run 900 brig run -d ubuntu@seq || rc=$?
    t1="$(now)"
    out="$(gsh ubuntu@seq "echo cycle-$i")" || rc=$?
    run 300 brig rm ubuntu@seq || rc=$?
    if [ "$rc" = 0 ] && [ "$out" = "cycle-$i" ]; then
      res sample "ubuntu boot" "$(dt "$t0" "$t1")"
    else
      fails=$((fails + 1))
    fi
  done
  res note "ubuntu boot" "median of $SEQ_N runs of brig run -d ubuntu, in boot, command and rm cycles"
  if [ "$fails" = 0 ]; then
    res check Cycles "$SEQ_N sequential ubuntu cycles" pass "boot, one command and rm, $SEQ_N times with no failure"
  else
    res check Cycles "$SEQ_N sequential ubuntu cycles" fail "$fails of $SEQ_N cycles failed"
  fi
}

check_profiles() {
  local p img bin free rc_run rc_sh t0 t1 out
  for p in ubuntu claude-code codex gemini grok opencode; do
    img="$(timeout 30 brig agent show "$p" --json | field image)"
    bin="$(timeout 30 brig agent show "$p" --json | field binary)"
    free="$(df --output=avail -BG "$HOME" | tail -n 1 | tr -dc 0-9)"
    if [ "$free" -lt 6 ]; then
      res check Profiles "$p boots" skip "only $free GB free on $HOME, and $img needs about 3 GB"
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
      *) rk rmi -f "$img" > /dev/null || true ;;
    esac
    if [ "$rc_run" = 0 ] && [ "$rc_sh" = 0 ] && echo "$out" | grep -q "/$bin\$"; then
      res check Profiles "$p boots" pass "brig run -d in $(dt "$t0" "$t1") s; guest: $(echo "$out" | one_line 200)"
    else
      res check Profiles "$p boots" fail "run exit $rc_run, sh exit $rc_sh; guest: $(echo "$out" | one_line 200)"
    fi
  done

  local d c rc_d=0 rc_c=0
  d="$(timeout 120 brig run -d claude-desktop 2>&1)" || rc_d=$?
  c="$(timeout 120 brig run -d cursor 2>&1)" || rc_c=$?
  echo "claude-desktop: exit $rc_d: $d"
  echo "cursor: exit $rc_c: $c"
  if [ "$rc_d" != 0 ] && echo "$d" | grep -q 'graphical window' &&
     [ "$rc_c" != 0 ] && echo "$c" | grep -q 'do not publish an image'; then
    res check Profiles "claude-desktop and cursor refuse by name" pass \
      "claude-desktop: $(echo "$d" | head -n 1 | cut -c1-120)... cursor: $(echo "$c" | head -n 1)"
  else
    res check Profiles "claude-desktop and cursor refuse by name" fail \
      "claude-desktop exit $rc_d: $(echo "$d" | one_line 200); cursor exit $rc_c: $(echo "$c" | one_line 200)"
  fi
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

  local keep="$HOME/e2e-keep-home"
  rm -rf "$keep"
  mkdir -p "$keep"
  run 900 brig run -d ubuntu --home "$keep"
  gsh ubuntu 'echo survived > $HOME/state.txt' > /dev/null || true
  run 300 brig rm ubuntu || true
  local after_rm
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
  if [ "$rc" != 0 ] && [ ! -e "$home" ] && ! timeout 30 brig ls -q | grep -q 'ubuntu@ghost'; then
    res check Homes "A failed first boot leaves no guest home (#344)" pass "brig run exit $rc on an image that does not exist; no $home, and brig ls lists nothing"
  else
    res check Homes "A failed first boot leaves no guest home (#344)" fail "brig run exit $rc; $(ls -ld "$home" 2>&1)"
  fi
  timeout 60 brig rm ubuntu@ghost > /dev/null 2>&1 || true
}

check_ports() {
  local proj="$HOME/e2e-netproj" body="" i after
  rm -rf "$proj"
  mkdir -p "$proj"
  cat > "$proj/srv.pl" <<'PERL'
use IO::Socket::INET;
my $s = IO::Socket::INET->new(LocalAddr => "0.0.0.0", LocalPort => 8000, Listen => 5, ReuseAddr => 1) or die "listen: $!";
while (my $c = $s->accept) { my $req = <$c>; print $c "HTTP/1.0 200 OK\r\nContent-Type: text/plain\r\n\r\nhello-from-guest\n"; close $c; }
PERL
  run 900 brig run ubuntu@net "$proj" --publish 18080:8000 -d
  run 60 brig network ls ubuntu@net || true
  gsh ubuntu@net 'setsid nohup perl /work/e2e-netproj/srv.pl > /tmp/srv.log 2>&1 < /dev/null & sleep 1' > /dev/null || true
  for i in $(seq 1 10); do
    body="$(curl -sS -m 5 http://127.0.0.1:18080/ 2>&1 || true)"
    [ "$body" = hello-from-guest ] && break
    sleep 2
  done
  echo "curl: $body (try $i)"
  run 300 brig rm ubuntu@net || true
  after="$(curl -sS -m 5 http://127.0.0.1:18080/ 2>&1 || true)"
  echo "curl after rm: $after"
  if [ "$body" = hello-from-guest ] && [ "$after" != hello-from-guest ]; then
    res check Network "A published port answers from the host" pass "curl 127.0.0.1:18080 -> guest 8000: $body; after rm the port is closed"
  else
    res check Network "A published port answers from the host" fail "curl: $(echo "$body" | one_line 200); after rm: $(echo "$after" | one_line 100)"
  fi
  rm -rf "$proj"
}

# EGRESS prints DNS and TCP reachability of github.com from the guest. The
# ubuntu image has no curl, and bash's /dev/tcp needs no tool.
EGRESS='if getent hosts github.com > /dev/null; then echo DNS_OK; else echo DNS_FAIL; fi; if timeout 8 bash -c "exec 3<>/dev/tcp/github.com/443" 2> /dev/null; then echo TCP_OK; else echo TCP_FAIL; fi'
MARK='echo m-$(date +%s%N) > /tmp/e2e-marker; cat /tmp/e2e-marker'
READ_MARK='cat /tmp/e2e-marker 2> /dev/null || echo NONE'

check_postures() {
  local m1 e1 m2 e2 info err rc=0 m3 e3
  run 900 brig run -d ubuntu@post --network offline
  m1="$(gsh ubuntu@post "$MARK")" || true
  e1="$(gsh ubuntu@post "$EGRESS")" || true
  if echo "$e1" | grep -q TCP_FAIL && echo "$e1" | grep -q DNS_FAIL; then
    res check Network "An offline sandbox has no egress (#342)" pass "from the guest: $(echo "$e1" | one_line 60)"
  else
    res check Network "An offline sandbox has no egress (#342)" fail "from the guest: $(echo "$e1" | one_line 200)"
  fi

  m2="$(gsh ubuntu@post "$READ_MARK")" || true
  e2="$(gsh ubuntu@post "$EGRESS")" || true
  info="$(timeout 60 brig info ubuntu@post 2> /dev/null | grep -E '^NETWORK' || true)"
  if [ -n "$m1" ] && [ "$m2" = "$m1" ] && echo "$e2" | grep -q TCP_FAIL && echo "$info" | grep -q offline; then
    res check Network "A flagless brig sh keeps the posture (#342)" pass "same guest (marker kept), still no egress; brig info: $(echo "$info" | one_line 80)"
  else
    res check Network "A flagless brig sh keeps the posture (#342)" fail "marker [$m1] -> [$m2]; egress: $(echo "$e2" | one_line 60); $info"
  fi

  err="$(timeout --kill-after=10 900 brig run -d ubuntu@post --network shared 2>&1 > /dev/null)" || rc=$?
  echo "$err"
  m3="$(gsh ubuntu@post "$READ_MARK")" || true
  e3="$(gsh ubuntu@post "$EGRESS")" || true
  local warned
  warned="$(echo "$err" | grep -A2 'started with the offline posture' || true)"
  if [ "$rc" = 0 ] && echo "$warned" | grep -q 'brig restarts it' && [ "$m3" = NONE ] && echo "$e3" | grep -q TCP_OK; then
    res check Network "--network shared restarts it, with the warning (#342)" pass "$(echo "$warned" | one_line 240). New guest (no marker), egress: $(echo "$e3" | one_line 60)"
  else
    res check Network "--network shared restarts it, with the warning (#342)" fail "exit $rc; warning: [$(echo "$warned" | one_line 160)]; marker [$m3]; egress: $(echo "$e3" | one_line 60)"
  fi
  run 300 brig rm ubuntu@post || true
}

check_shell_removed() {
  local out rc=0 ls
  out="$(timeout 120 script -q -e -c 'brig shell ubuntu' /dev/null < /dev/null)" || rc=$?
  out="${out//$'\r'/}"
  echo "brig shell ubuntu: exit $rc: $out"
  ls="$(timeout 30 brig ls -q | grep -v '^(none' || true)"
  if [ "$rc" = 2 ] && echo "$out" | grep -q 'was removed' && [ -z "$ls" ]; then
    res check CLI "brig shell exits 2 and starts nothing (#341)" pass "exit 2: $(echo "$out" | one_line 120)"
  else
    res check CLI "brig shell exits 2 and starts nothing (#341)" fail "exit $rc: $(echo "$out" | one_line 200); brig ls: [$ls]"
  fi
}

# canary_sum DIR: a checksum over every name, type, size, mode, inode, mtime
# and file content under DIR.
canary_sum() {
  (
    cd "$1"
    find . -printf '%p %y %s %m %i %T@\n' | sort
    find . -type f -exec sha256sum {} + | sort
  ) | sha256sum | cut -c1-64
}

check_symlink() {
  local canary="$HOME/e2e-canary332" proj="$HOME/e2e-p332" sum0 sum1 planted rc refused=0 out
  rm -rf "$canary" "$proj"
  mkdir -p "$canary/sub" "$proj/real-sub"
  echo "canary $(date -u +%s)" > "$canary/secret.txt"
  echo inner > "$canary/sub/inner.txt"
  echo hello > "$proj/real-sub/readme"
  sum0="$(canary_sum "$canary")"

  # The guest plants the links in a project it has read-write.
  run 900 brig run ubuntu@plant "$proj" -d
  gsh ubuntu@plant "ln -s $canary /work/e2e-p332/evil && ln -s ../e2e-canary332 /work/e2e-p332/evil-rel && echo PLANTED" || true
  planted="$(readlink "$proj/evil" 2>&1) | $(readlink "$proj/evil-rel" 2>&1)"
  echo "planted, as the host sees them: $planted"

  local target
  for target in "$proj/evil" "$proj/evil-rel" "$proj/evil/sub"; do
    rc=0
    out="$(timeout --kill-after=10 300 brig run ubuntu@victim "$target" -d 2>&1)" || rc=$?
    echo "brig run ubuntu@victim $target: exit $rc: $out"
    if [ "$rc" != 0 ] && echo "$out" | grep -q 'refusing to use'; then
      refused=$((refused + 1))
    fi
    timeout 60 brig rm ubuntu@victim > /dev/null 2>&1 || true
  done
  local victim_home
  victim_home="$(ls -d "$HOME/.brig/homes/brig-ubuntu-victim" 2> /dev/null || true)"

  # The real directory next to the links still works, so the refusals above
  # are about the links.
  local control=""
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

# check_hostmount_symlink plants a symlink at a hostmount volume SOURCE inside a
# stopped sandbox's workspace and asserts the next boot refuses it rather than
# handing the runtime a source that now points out of the workspace. The
# workspace is the guest home, which the guest holds read-write, so a .claude
# hostmount source it swapped for a link would otherwise be bound host-side into
# a later boot. brig re-checks each source before the share is built.
check_hostmount_symlink() {
  local ref=claude-code@hmv home src canary sum0 sum1 rc=0 out refused=no
  home="$HOME/.brig/homes/brig-claude-code-hmv"
  canary="$HOME/e2e-hm-canary"
  rm -rf "$canary"
  mkdir -p "$canary"
  echo "operator-only $(date -u +%s)" > "$canary/secret.txt"
  sum0="$(canary_sum "$canary")"

  # Boot once so brig creates the .claude/* hostmount sources, then stop so the
  # runtime releases them and the source can be replaced on the host.
  run 900 brig run -d "$ref"
  run 120 brig stop "$ref" || true
  src="$home/.claude/sessions"
  if [ ! -d "$src" ]; then
    res check Boundary "A hostmount source reached through a planted link is refused (#405)" fail \
      "no hostmount source at $src after a boot; the home layout may have changed"
    run 300 brig rm "$ref" || true
    rm -rf "$canary"
    return
  fi

  # The swap: the source now points at a directory outside the workspace.
  rm -rf "$src"
  ln -s "$canary" "$src"
  echo "planted: $src -> $(readlink "$src")"

  out="$(timeout --kill-after=10 300 brig run -d "$ref" 2>&1)" || rc=$?
  echo "brig run $ref after the swap: exit $rc: $out"
  if [ "$rc" != 0 ] && echo "$out" | grep -qiE 'refusing to (mount|use)|symlink'; then
    refused=yes
  fi

  run 300 brig rm "$ref" || true
  sum1="$(canary_sum "$canary")"

  if [ "$refused" = yes ]; then
    res check Boundary "A hostmount source reached through a planted link is refused (#405)" pass \
      "the boot refused a .claude/sessions source swapped for a symlink out of the workspace: $(echo "$out" | one_line 200)"
  else
    res check Boundary "A hostmount source reached through a planted link is refused (#405)" fail \
      "exit $rc: $(echo "$out" | one_line 300)"
  fi
  if [ "$sum0" = "$sum1" ]; then
    res check Boundary "The directory a hostmount link pointed at is unchanged" pass "sha256 ${sum0:0:16}... before and after"
  else
    res check Boundary "The directory a hostmount link pointed at is unchanged" fail "before ${sum0:0:16}, after ${sum1:0:16}"
  fi
  rm -rf "$canary"
}

# check_brigd_lock plants a symlink and a hard link at brigd's socket lock path
# and asserts brigd refuses each rather than following it and truncating the
# target. It is reachable when --socket names a directory another local user
# can write; a scratch directory stands in for that here. brigd opens the lock
# with O_NOFOLLOW and verifies a regular, singly-linked file it owns.
check_brigd_lock() {
  local brigd d canary before after rc out refused=0
  brigd="${BRIG_BUILD_DIR:-$OUT/build}/brigd"
  d="$(mktemp -d "$OUT/brigd-lock.XXXXXX")"
  canary="$d/operator-file"
  echo "operator-only $(date -u +%s)" > "$canary"
  before="$(cat "$canary")"

  ln -s "$canary" "$d/sock.lock"
  rc=0
  out="$(timeout --kill-after=5 30 "$brigd" --socket "$d/sock" 2>&1)" || rc=$?
  echo "symlink at the lock path: exit $rc: $out"
  if [ "$rc" != 0 ] && echo "$out" | grep -qiE 'symlink|planted'; then
    refused=$((refused + 1))
  fi
  rm -f "$d/sock.lock" "$d/sock"

  ln "$canary" "$d/sock.lock"
  rc=0
  out="$(timeout --kill-after=5 30 "$brigd" --socket "$d/sock" 2>&1)" || rc=$?
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

check_parallel() {
  local j t0 t1 ok=0 rc out
  t0="$(now)"
  for j in $(seq 1 "$PAR_N"); do
    (
      rc=0
      timeout --kill-after=10 900 brig run -d "claude@par$j" > "$OUT/par$j.log" 2>&1 || rc=$?
      echo "$rc" > "$OUT/par$j.rc"
    ) &
  done
  wait
  t1="$(now)"
  for j in $(seq 1 "$PAR_N"); do
    cat "$OUT/par$j.log"
    rc="$(cat "$OUT/par$j.rc")"
    out="$(gsh "claude@par$j" "echo par-$j")" || true
    [ "$rc" = 0 ] && [ "$out" = "par-$j" ] && ok=$((ok + 1))
    run 300 brig rm "claude@par$j" || true
  done
  res sample "claude-code in parallel" "$(dt "$t0" "$t1")"
  res note "claude-code in parallel" "wall time until $PAR_N claude-code sandboxes are up"
  if [ "$ok" = "$PAR_N" ]; then
    res check Cycles "$PAR_N claude-code sandboxes in parallel" pass "all $PAR_N up in $(dt "$t0" "$t1") s and answering"
  else
    res check Cycles "$PAR_N claude-code sandboxes in parallel" fail "$ok of $PAR_N up and answering"
  fi
}

check_churn() {
  local i rc t0 t1 out fails=0
  run 900 brig run -d claude@churn
  for i in $(seq 1 "$CHURN_N"); do
    rc=0
    run 300 brig stop claude@churn || rc=$?
    t0="$(now)"
    run 900 brig run -d claude@churn || rc=$?
    t1="$(now)"
    out="$(gsh claude@churn "echo churn-$i")" || rc=$?
    if [ "$rc" = 0 ] && [ "$out" = "churn-$i" ]; then
      res sample "claude-code restart" "$(dt "$t0" "$t1")"
    else
      fails=$((fails + 1))
    fi
  done
  run 300 brig rm claude@churn || true
  res note "claude-code restart" "median of $CHURN_N runs of brig run -d after brig stop"
  if [ "$fails" = 0 ]; then
    res check Cycles "$CHURN_N stop/start cycles of claude-code" pass "every restart came back and answered"
  else
    res check Cycles "$CHURN_N stop/start cycles of claude-code" fail "$fails of $CHURN_N restarts failed"
  fi
}

# ---------------------------------------------------------------- fixes

# ENVCHK prints each variable the credential check looks for, or UNSET.
ENVCHK='for v in E2E_FWD E2E_SNEAKY ANTHROPIC_API_KEY; do echo "$v=${!v:-UNSET}"; done'

# env_field TEXT NAME: the value ENVCHK printed for NAME.
env_field() { echo "$1" | sed -n "s/^$2=//p" | head -n 1 | tr -d ' '; }

# check_creds boots claude-code with three variables in the shell: one named
# in BRIG_FORWARD_ENV, one named nowhere, and ANTHROPIC_API_KEY, which is
# named but on the claude-code denylist. Only the first may reach the guest.
check_creds() {
  local err out fwd sneaky denied rc=0
  export E2E_FWD="fwd-$RANDOM$RANDOM" E2E_SNEAKY="sneaky-$RANDOM$RANDOM" ANTHROPIC_API_KEY="sk-e2e-$RANDOM$RANDOM"
  export BRIG_FORWARD_ENV="E2E_FWD ANTHROPIC_API_KEY"
  unset BRIG_CLAUDE_CODE_FORWARD_ENV BRIG_ALLOW_DENIED BRIG_CLAUDE_CODE_ALLOW_DENIED
  err="$(timeout --kill-after=10 900 brig run -d claude-code@creds 2>&1 > /dev/null)" || rc=$?
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
  w="$(timeout 60 brig agent export ubuntu e2e-rm --force 2>&1 < /dev/null)"
  echo "$w"
  f="$(echo "$w" | sed -n 's/^wrote .* -> //p' | head -n 1)"
  run 900 brig run -d e2e-rm
  out="$(timeout 120 brig agent rm e2e-rm 2>&1 < /dev/null)" || rc=$?
  echo "agent rm with the sandbox up: exit $rc: $out"
  [ -n "$f" ] && [ -f "$f" ] && kept=yes
  run 300 brig rm e2e-rm || true
  out2="$(timeout 120 brig agent rm e2e-rm 2>&1 < /dev/null)" || rc2=$?
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

# EG_PROBE prints, from the guest, the first IPv4 address each name resolves
# to (DNS_<name>, NONE when it does not resolve) and whether a TCP connection
# to port 443 opens (TCP_<target>, OPEN or CLOSED). bash alone, as EGRESS.
EG_PROBE='for n in example.com example.net; do a=$(getent ahostsv4 $n 2> /dev/null | awk "NR == 1 { print \$1 }"); echo "DNS_$n=${a:-NONE}"; done; for t in example.com 9.9.9.9; do if timeout 8 bash -c "exec 3<>/dev/tcp/$t/443" 2> /dev/null; then echo "TCP_$t=OPEN"; else echo "TCP_$t=CLOSED"; fi; done'

# eg FIELD TEXT: one field of EG_PROBE's output.
eg() { echo "$2" | sed -n "s/^$1=//p" | head -n 1 | tr -d ' '; }

# eg_tables: the brig egress tables in the namespace that holds the rootless
# nerdctl bridges, entered the way brig enters it.
eg_tables() {
  local dir="$XDG_RUNTIME_DIR/containerd-rootless" pid ns=-n nft
  pid="$(cat "$dir/child_pid" 2> /dev/null)" || { echo "(no rootless containerd)"; return 0; }
  [ ! -e "$dir/netns" ] || ns="--net=/proc/$pid/root$dir/netns"
  nft="$(command -v nft || echo /usr/sbin/nft)"
  timeout 30 nsenter -U --preserve-credentials -t "$pid" "$ns" -- "$nft" list tables 2>&1 |
    awk '$3 ~ /^brig_egress_/ { print $3 }'
}

# eg_resolvers: the egress resolvers still running.
eg_resolvers() { pgrep -a -f -- '__egress-resolver' || true; }

# eg_records SANDBOX: the resolver records brig keeps for SANDBOX.
eg_records() { ls "$HOME/.brig/egress/$1".{pid,spec} 2> /dev/null || true; }

# eg_tcp SANDBOX IP: whether IP:443 opens from SANDBOX, through nerdctl exec,
# since brig sh would restart a sandbox whose resolver is gone.
eg_tcp() {
  RK_TIMEOUT=30 rk exec "$1" bash -c "timeout 5 bash -c 'exec 3<>/dev/tcp/$2/443' 2> /dev/null && echo OPEN || echo CLOSED" 2>&1 |
    tr -d '\r' | tail -n 1
}

# write_policy NAME LINES...: a policy whose egress section is LINES.
write_policy() {
  local name=$1
  shift
  env -u VISUAL EDITOR=true timeout 60 brig policy create "$name" --force < /dev/null
  {
    echo 'apiVersion: brig.sh/v1alpha1'
    echo "name: $name"
    echo 'egress:'
    printf '  %s\n' "$@"
  } > "${XDG_CONFIG_HOME:-$HOME/.config}/brig/policies/$name.yaml"
  timeout 60 brig policy show "$name"
}

# check_files_mode: a files: binding's mode must be quoted (#453). YAML reads
# an unquoted 0640 as the number 416, and brig created the file 0o640 read as
# decimal digits. Now the profile is refused and the message says to quote
# it. A file that overrides a built-in is ignored the same way, and the
# built-in runs.
check_files_mode() {
  local w f o out rc=0 ls ok rc2=0 out2 said
  w="$(timeout 60 brig agent export ubuntu e2e-mode --force < /dev/null 2>&1)"
  echo "$w"
  f="$(echo "$w" | sed -n 's/^wrote .* -> //p' | head -n 1)"
  if [ -z "$f" ] || [ ! -f "$f" ]; then
    res check Profile "An unquoted files: mode is refused, and a quoted one parses (#453)" fail "agent export wrote no file: $(echo "$w" | one_line 200)"
    return 0
  fi
  printf '%s\n' 'secrets:' '  - name: e2e-mode' '    required: false' \
    'volumes:' '  - kind: tmpfs' '    path: .e2e' 'files:' \
    '  - ref: secrets.e2e-mode' '    path: .e2e/token' '    mode: "0640"' >> "$f"
  ls="$(timeout 60 brig agent ls < /dev/null 2>&1)" || true
  echo "agent ls with mode \"0640\": $ls"
  sed -i.bak 's/^    mode: "0640"$/    mode: 0640/' "$f" && rm -f "$f.bak"
  grep -n 'mode:' "$f"
  out="$(timeout 300 brig run -d e2e-mode < /dev/null 2>&1)" || rc=$?
  echo "brig run e2e-mode with mode 0640: exit $rc: $out"
  if echo "$ls" | grep -qw e2e-mode && ! echo "$ls" | grep -q unusable &&
     [ "$rc" != 0 ] && echo "$out" | grep -q 'mode must be a quoted string' && echo "$out" | grep -qF "$(basename "$f")" &&
     ! timeout 60 brig ls -q < /dev/null 2> /dev/null | grep -q '^e2e-mode'; then
    res check Profile "An unquoted files: mode is refused, and a quoted one parses (#453)" pass \
      "\"0640\" listed; 0640: exit $rc, no sandbox: $(echo "$out" | grep 'quoted' | one_line 200)"
  else
    res check Profile "An unquoted files: mode is refused, and a quoted one parses (#453)" fail \
      "agent ls with \"0640\": $(echo "$ls" | one_line 160); with 0640: exit $rc: $(echo "$out" | one_line 240)"
  fi

  o="$(dirname "$f")/e2e-ubuntu-override.yaml"
  sed 's/^name: .*/name: ubuntu/' "$f" > "$o"
  rm -f "$f"
  out2="$(timeout 900 brig run -d ubuntu@mode < /dev/null 2>&1)" || rc2=$?
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

# check_policy_enforced runs docs/manual-tests/egress-policy-linux.md: nerdctl
# enforces a policy with brig's resolver and an nftables table on the
# sandbox's own bridge. A sandbox with no policy is the control, so a denied
# target is shown to be reachable from this runner without one. A policy
# attaches to an agent profile only, so every sandbox here is claude-code's.
check_policy_enforced() {
  local free deny allow ip pid1 pid2 before after waited=0 m1 p tables recs procs left
  run 900 brig run -d claude-code@egn
  free="$(gsh claude-code@egn "$EG_PROBE")" || true

  write_policy e2e-egdeny 'default: deny' 'allow:' '  - host: example.com'
  run 60 brig policy attach e2e-egdeny claude-code -n egd
  run 900 brig run -d claude-code@egd
  m1="$(gsh claude-code@egd "$MARK")" || true
  deny="$(gsh claude-code@egd "$EG_PROBE")" || true
  tables="$(eg_tables)"
  echo "tables: $tables"
  if [ "$(eg TCP_9.9.9.9 "$free")" = OPEN ] && [ "$(eg DNS_example.net "$free")" != NONE ] &&
     [ "$(eg TCP_example.com "$deny")" = OPEN ] && [ "$(eg DNS_example.net "$deny")" = NONE ] &&
     [ "$(eg TCP_9.9.9.9 "$deny")" = CLOSED ] && echo "$tables" | grep -qx 'brig_egress_claude-code-egd'; then
    res check Policy "nerdctl enforces default: deny, and the allowed name still reaches (#266)" pass \
      "under the policy: $(echo "$deny" | one_line 160). With no policy: $(echo "$free" | one_line 160). Table brig_egress_claude-code-egd"
  else
    res check Policy "nerdctl enforces default: deny, and the allowed name still reaches (#266)" fail \
      "under the policy: $(echo "$deny" | one_line 160); with no policy: $(echo "$free" | one_line 160); tables: [$(echo "$tables" | one_line 120)]"
  fi

  write_policy e2e-egallow 'default: allow' 'deny:' '  - host: example.com'
  run 60 brig policy attach e2e-egallow claude-code -n ega
  run 900 brig run -d claude-code@ega
  allow="$(gsh claude-code@ega "$EG_PROBE")" || true
  if [ "$(eg TCP_example.com "$free")" = OPEN ] &&
     [ "$(eg TCP_example.com "$allow")" = CLOSED ] && [ "$(eg TCP_9.9.9.9 "$allow")" = OPEN ]; then
    res check Policy "nerdctl enforces default: allow, and refuses the denied name (#266)" pass \
      "under the policy: $(echo "$allow" | one_line 160)"
  else
    res check Policy "nerdctl enforces default: allow, and refuses the denied name (#266)" fail \
      "under the policy: $(echo "$allow" | one_line 160); with no policy: $(echo "$free" | one_line 160)"
  fi
  run 300 brig rm claude-code@ega || true
  run 60 brig policy detach e2e-egallow claude-code -n ega || true

  # The table outlives its resolver and refuses new connections once the
  # resolver's heartbeat lapses, 15 s after the last beat. The address
  # example.com resolved to stays in the allow set for two minutes, so only
  # the heartbeat can close it.
  ip="$(eg DNS_example.com "$deny")"
  pid1="$(cat "$HOME/.brig/egress/brig-claude-code-egd.pid" 2> /dev/null || true)"
  before="no address"
  if [ -n "$ip" ] && [ "$ip" != NONE ]; then
    before="$(eg_tcp brig-claude-code-egd "$ip")"
  fi
  echo "resolver pid [$pid1]; $ip:443 before the kill: $before"
  after=""
  if [ -n "$pid1" ] && [ "$before" = OPEN ]; then
    kill -KILL "$pid1" || true
    while [ "$waited" -lt 60 ]; do
      after="$(eg_tcp brig-claude-code-egd "$ip")"
      [ "$after" != CLOSED ] || break
      sleep 3
      waited=$((waited + 3))
    done
  fi
  if [ "$before" = OPEN ] && [ "$after" = CLOSED ]; then
    res check Policy "A dead resolver fails closed (#266)" pass \
      "$ip:443 (example.com, allowed) opened; after a kill -9 of resolver $pid1 it was refused within $waited s"
  else
    res check Policy "A dead resolver fails closed (#266)" fail \
      "resolver [$pid1]; $ip:443 before the kill: [$before], after $waited s: [$after]"
  fi

  # NetworkStale: the next brig sh boots the sandbox again under a new
  # resolver.
  p="$(gsh claude-code@egd "$READ_MARK; $EG_PROBE")" || true
  pid2="$(cat "$HOME/.brig/egress/brig-claude-code-egd.pid" 2> /dev/null || true)"
  if [ -n "$m1" ] && echo "$p" | grep -qx NONE && [ -n "$pid2" ] && [ "$pid2" != "$pid1" ] &&
     kill -0 "$pid2" 2> /dev/null && [ "$(eg TCP_example.com "$p")" = OPEN ] && [ "$(eg TCP_9.9.9.9 "$p")" = CLOSED ]; then
    res check Policy "brig sh boots a sandbox whose resolver died again, under a new one (#266)" pass \
      "new guest (no marker), resolver $pid1 -> $pid2: $(echo "$p" | one_line 160)"
  else
    res check Policy "brig sh boots a sandbox whose resolver died again, under a new one (#266)" fail \
      "marker [$m1]; resolver [$pid1] -> [$pid2]; $(echo "$p" | one_line 200)"
  fi

  run 60 brig policy detach e2e-egdeny claude-code -n egd
  p="$(gsh claude-code@egd "$EG_PROBE")" || true
  tables="$(eg_tables)"
  recs="$(eg_records brig-claude-code-egd)"
  if [ "$(eg TCP_9.9.9.9 "$p")" = OPEN ] && [ "$(eg DNS_example.net "$p")" != NONE ] &&
     ! echo "$tables" | grep -qx 'brig_egress_claude-code-egd' && [ -z "$recs" ]; then
    res check Policy "Detaching the policy boots the sandbox without a table or resolver (#266)" pass \
      "$(echo "$p" | one_line 160)"
  else
    res check Policy "Detaching the policy boots the sandbox without a table or resolver (#266)" fail \
      "$(echo "$p" | one_line 160); tables: [$(echo "$tables" | one_line 120)]; records: [$recs]"
  fi

  run 300 brig rm claude-code@egd || true
  run 300 brig rm claude-code@egn || true
  tables="$(eg_tables)"
  procs="$(eg_resolvers)"
  # The lock file of each sandbox stays by design. Its records go.
  left="$(find "$HOME/.brig/egress" -maxdepth 1 \( -name '*.pid' -o -name '*.spec' -o -name '*.log' \) 2> /dev/null || true)"
  if [ -z "$tables" ] && [ -z "$procs" ] && [ -z "$left" ]; then
    res check Policy "No egress table, resolver or record is left after rm (#266)" pass \
      "nft list tables names no brig_egress_ table, and no resolver runs"
  else
    res check Policy "No egress table, resolver or record is left after rm (#266)" fail \
      "tables: [$(echo "$tables" | one_line 120)]; resolvers: [$(echo "$procs" | one_line 160)]; files: [$(echo "$left" | one_line 120)]"
  fi
}

# check_cosign_hang points DOCKER_CONFIG at a credsStore helper that never
# answers, the shape Docker Desktop leaves when it is installed and not
# running. cosign asks that helper for registry credentials and hangs.
# claude-code, because brig verifies only the images brig-sh signs.
check_cosign_hang() {
  local d="$OUT/e2e-dockercfg" out rc=0 t0 t1 left ls
  rm -rf "$d"
  mkdir -p "$d/bin"
  printf '{"credsStore": "e2ehang"}\n' > "$d/config.json"
  printf '#!/bin/sh\nexec sleep 600\n' > "$d/bin/docker-credential-e2ehang"
  chmod +x "$d/bin/docker-credential-e2ehang"
  t0="$(now)"
  out="$(PATH="$d/bin:$PATH" DOCKER_CONFIG="$d" BRIG_VERIFY=require \
    timeout --kill-after=10 300 brig run -d claude-code@cos 2>&1 < /dev/null)" || rc=$?
  t1="$(now)"
  echo "brig run claude-code@cos: exit $rc after $(dt "$t0" "$t1") s: $out"
  sleep 2
  left="$(pgrep -a -f 'docker-credential-e2ehang' || true)"
  ls="$(timeout 30 brig ls -q 2> /dev/null | grep -v '^(none' || true)"
  if [ "$rc" = 5 ] && echo "$out" | grep -q 'It waits on docker-credential-e2ehang' && [ -z "$left" ] && [ -z "$ls" ]; then
    res check Verify "A hung credential helper is named, and killed with cosign (#365)" pass \
      "exit 5 after $(dt "$t0" "$t1") s under BRIG_VERIFY=require, naming docker-credential-e2ehang; no helper process and no sandbox left"
  else
    res check Verify "A hung credential helper is named, and killed with cosign (#365)" fail \
      "exit $rc after $(dt "$t0" "$t1") s; helpers left: [$(echo "$left" | one_line 120)]; brig ls: [$ls]; $(echo "$out" | one_line 300)"
  fi
  pkill -f 'docker-credential-e2ehang' 2> /dev/null || true
  timeout 60 brig rm claude-code@cos > /dev/null 2>&1 || true
  rm -rf "$d"
}

# boot_try MODE REF [ENV...]: one brig run -d REF under BRIG_VERIFY=MODE, with
# ENV added. It prints what brig said, removes the sandbox and returns brig's
# exit status.
boot_try() {
  local mode=$1 ref=$2 out rc=0
  shift 2
  out="$(env BRIG_VERIFY="$mode" "$@" timeout --kill-after=10 900 brig run -d "$ref" 2>&1 < /dev/null)" || rc=$?
  printf '$ BRIG_VERIFY=%s %s brig run -d %s\n%s\n[exit %s]\n' "$mode" "$*" "$ref" "$out" "$rc" >&2
  timeout 120 brig rm "$ref" > /dev/null 2>&1 || true
  printf '%s\n' "$out"
  return "$rc"
}

RECORD_CHECKS=(
  "An intact runtime bundle boots with its kernel and initrd verified (#403)"
  "A changed kernel, or a record the release does not list, refuses in every mode (#403)"
  "A record brig cannot check boots under warn and refuses under require (#403)"
  "A cosign that does not answer is \"cannot check\", not a refusal (#403)"
)

# check_record_refusals changes the files in share/guest one at a time and
# boots against each: a changed kernel, a record the release's checksums.txt
# does not list, and a missing signature. The files are put back after each
# case and when the block ends, however it ends.
check_record_refusals() {
  local g="$BUNDLE_DIR/share/guest" keep="$OUT/guest-keep" k name out rc
  if [ ! -f "$REPO/internal/verify/record.go" ]; then
    for name in "${RECORD_CHECKS[@]}"; do
      res check Verify "$name" skip "brig at $(git -C "$REPO" rev-parse --short HEAD) does not check the runtime bundle's record"
    done
    return 0
  fi
  if [ ! -f "$g/SHA256SUMS" ]; then
    for name in "${RECORD_CHECKS[@]}"; do
      res check Verify "$name" skip "the installed bundle keeps no record"
    done
    return 0
  fi
  k=Image
  [ ! -f "$g/bzImage" ] || k=bzImage
  rm -rf "$keep"
  mkdir -p "$keep"
  cp -p "$g/$k" "$g/SHA256SUMS" "$g/checksums.txt.sig" "$keep/"
  # shellcheck disable=SC2064
  trap "cp -p '$keep/$k' '$keep/SHA256SUMS' '$keep/checksums.txt.sig' '$g/'" EXIT
  put_back() { cp -p "$keep/$k" "$keep/SHA256SUMS" "$keep/checksums.txt.sig" "$g/"; }

  local w0=0 r0=0 o_w0 o_r0
  o_w0="$(boot_try warn ubuntu@rec)" || w0=$?
  o_r0="$(boot_try require claude-code@rec)" || r0=$?
  if [ "$w0" = 0 ] && echo "$o_w0" | grep -q 'boot assets verified' &&
     [ "$r0" = 0 ] && echo "$o_r0" | grep -q 'image and boot assets verified'; then
    res check Verify "${RECORD_CHECKS[0]}" pass \
      "warn: exit 0, $(echo "$o_w0" | grep 'verified' | one_line 80). require, claude-code: exit 0, $(echo "$o_r0" | grep 'verified' | one_line 80)"
  else
    res check Verify "${RECORD_CHECKS[0]}" fail \
      "warn: exit $w0, $(echo "$o_w0" | one_line 200); require: exit $r0, $(echo "$o_r0" | one_line 200)"
  fi

  local w1=0 r1=0 u1=0 w9=0 o_w1 o_r1 o_u1 o_w9
  printf 'x' >> "$g/$k"
  o_w1="$(boot_try warn ubuntu@rec)" || w1=$?
  o_r1="$(boot_try require claude-code@rec)" || r1=$?
  put_back
  printf '\n' >> "$g/SHA256SUMS"
  o_u1="$(boot_try warn ubuntu@rec)" || u1=$?
  put_back
  o_w9="$(boot_try warn ubuntu@rec)" || w9=$?
  if [ "$w1" = 5 ] && [ "$r1" = 5 ] && [ "$u1" = 5 ] && [ "$w9" = 0 ] &&
     echo "$o_w1" | grep -q "are not the ones the Linux runtime bundle's" &&
     echo "$o_u1" | grep -q "are not the ones the Linux runtime bundle's" &&
     echo "$o_w9" | grep -q 'boot assets verified'; then
    res check Verify "${RECORD_CHECKS[1]}" pass \
      "one byte appended to $k: exit 5 under warn and require. SHA256SUMS changed: exit 5 under warn. The files put back boot again. $(echo "$o_w1" | grep refusing | one_line 200)"
  else
    res check Verify "${RECORD_CHECKS[1]}" fail \
      "kernel changed: warn exit $w1, require exit $r1; record changed: warn exit $u1; put back: warn exit $w9. $(echo "$o_w1 $o_r1 $o_u1" | one_line 300)"
  fi

  local w2=0 r2=0 o_w2 o_r2
  mv "$g/checksums.txt.sig" "$keep/sig.aside"
  o_w2="$(boot_try warn ubuntu@rec)" || w2=$?
  o_r2="$(boot_try require claude-code@rec)" || r2=$?
  rm -f "$keep/sig.aside"
  put_back
  if [ "$w2" = 0 ] && [ "$r2" = 5 ] &&
     echo "$o_w2" | grep -q "cannot check the Linux runtime bundle's kernel and initrd" &&
     ! echo "$o_w2" | grep -q 'boot assets verified'; then
    res check Verify "${RECORD_CHECKS[2]}" pass \
      "checksums.txt.sig moved away: warn boots and says $(echo "$o_w2" | grep 'cannot check' | one_line 120); require exits 5"
  else
    res check Verify "${RECORD_CHECKS[2]}" fail \
      "warn: exit $w2, $(echo "$o_w2" | one_line 200); require: exit $r2, $(echo "$o_r2" | one_line 200)"
  fi

  # A proxy that takes the connection and never answers holds cosign's calls
  # to Sigstore until brig's cosign timeout cuts it off. The registries stay
  # off the proxy, so only cosign waits.
  local hang w3=0 o_w3 t0 t1
  python3 -c '
import socket, sys
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", 18181))
s.listen(64)
held = []
while True:
    held.append(s.accept()[0])
' &
  hang=$!
  sleep 1
  t0="$(now)"
  o_w3="$(boot_try warn ubuntu@rec HTTPS_PROXY=http://127.0.0.1:18181 https_proxy=http://127.0.0.1:18181 \
    NO_PROXY=docker.io,registry-1.docker.io,auth.docker.io,production.cloudflare.docker.com,ghcr.io,pkg-containers.githubusercontent.com \
    no_proxy=docker.io,registry-1.docker.io,auth.docker.io,production.cloudflare.docker.com,ghcr.io,pkg-containers.githubusercontent.com)" || w3=$?
  t1="$(now)"
  kill "$hang" 2> /dev/null || true
  wait "$hang" 2> /dev/null || true
  if [ "$w3" = 0 ] && echo "$o_w3" | grep -q 'cosign did not answer within' &&
     echo "$o_w3" | grep -q "cannot check the Linux runtime bundle's kernel and initrd"; then
    res check Verify "${RECORD_CHECKS[3]}" pass \
      "Sigstore behind a proxy that never answers: warn boots after $(dt "$t0" "$t1") s, $(echo "$o_w3" | grep 'did not answer' | one_line 200)"
  else
    res check Verify "${RECORD_CHECKS[3]}" fail "warn: exit $w3 after $(dt "$t0" "$t1") s, $(echo "$o_w3" | one_line 300)"
  fi
}

# check_docker_fallback runs brig itself, outside the launcher, with docker
# alone on PATH, and through the launcher, where the bundle puts nerdctl.
check_docker_fallback() {
  local real="$BUNDLE_DIR/bin/brig" out ctl nd dk
  nd="$(env PATH=/usr/bin:/bin sh -c 'command -v nerdctl' || true)"
  dk="$(env PATH=/usr/bin:/bin sh -c 'command -v docker' || true)"
  if [ -z "$dk" ] || [ -n "$nd" ]; then
    res check Runtime "brig says when it falls back to docker (#30)" skip "needs docker and no nerdctl on /usr/bin:/bin; docker [$dk], nerdctl [$nd]"
    return 0
  fi
  out="$(env -u BRIG_RUNTIME -u BRIG_RUNTIME_BIN PATH=/usr/bin:/bin timeout 60 "$real" ls 2>&1 < /dev/null || true)"
  ctl="$(timeout 60 brig ls 2>&1 < /dev/null || true)"
  echo "docker alone: $out"
  echo "launcher: $ctl"
  if echo "$out" | grep -q 'nerdctl is not on PATH, so brig is driving docker' && ! echo "$ctl" | grep -q 'driving docker'; then
    res check Runtime "brig says when it falls back to docker (#30)" pass \
      "with $dk alone on PATH: $(echo "$out" | grep 'driving docker' | one_line 200). Through the launcher, with nerdctl: no note"
  else
    res check Runtime "brig says when it falls back to docker (#30)" fail \
      "docker alone: $(echo "$out" | one_line 200); launcher: $(echo "$ctl" | one_line 120)"
  fi
}

# ---------------------------------------------------------------- main

say "brig e2e $LEVEL, results in $RESULTS"
res host "$HOST"
res meta host_os linux
res meta level "$LEVEL"
res meta run_note "LEVEL=$LEVEL: $EXEC_N execs each way, $CLAUDE_N claude-code boots, $SEQ_N ubuntu cycles$([ "$LEVEL" = nightly ] && echo ", $PAR_N in parallel, $CHURN_N stop/start cycles")."
if [ -n "${GITHUB_RUN_ID:-}" ]; then
  res meta run_id "$GITHUB_RUN_ID"
  res meta run_url "${GITHUB_SERVER_URL:-https://github.com}/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID"
  res meta source "GitHub Actions run $GITHUB_RUN_ID, attempt ${GITHUB_RUN_ATTEMPT:-1} (${GITHUB_EVENT_NAME:-unknown})"
fi
block setup setup
if [ -f "$OUT/setup.ok" ]; then
  block version check_version
  block home gate_home
  block exec gate_exec
  block claude gate_claude
  block cpus gate_cpus
  block bundle-record check_bundle_record
  block nosudo gate_nosudo
  block cycles check_cycles
  block profiles check_profiles
  block homes check_rm_home
  block failed-boot check_failed_boot
  block ports check_ports
  block postures check_postures
  block shell check_shell_removed
  block symlink check_symlink
  block hostmount-symlink check_hostmount_symlink
  block brigd-lock check_brigd_lock
  block creds check_creds
  block agent-rm check_agent_rm
  block files-mode check_files_mode
  block policy check_policy_enforced
  block cosign check_cosign_hang
  block record check_record_refusals
  block fallback check_docker_fallback
  if [ "$LEVEL" = nightly ]; then
    block parallel check_parallel
    block churn check_churn
  fi
else
  say "setup failed, so no gate ran"
fi

build_args=()
[ -z "${E2E_BASELINE:-}" ] || build_args+=(--baseline "$E2E_BASELINE")
verdict="$(res build ${build_args[@]+"${build_args[@]}"} "$RESULTS")"
say "verdict: $verdict ($RESULTS)"
[ "$verdict" = go ]
