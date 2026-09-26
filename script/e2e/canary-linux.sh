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

# gsh REF TEXT: runs TEXT in the guest with `brig sh REF` and prints what came
# back, without CRs. brig sh needs a console on stdin and stdout, so it runs
# on a pty from script(1). A copy goes to stderr, which is the block's log.
gsh() {
  local ref=$1 text=$2 f out rc=0
  f="$(mktemp "$OUT/gsh.XXXXXX")"
  printf '#!/usr/bin/env bash\nexec brig sh %q %q\n' "$ref" "$text" > "$f"
  chmod +x "$f"
  out="$(timeout --kill-after=10 "${GSH_TIMEOUT:-300}" script -q -e -c "$f" /dev/null < /dev/null)" || rc=$?
  rm -f "$f"
  out="${out//$'\r'/}"
  printf '$ brig sh %s %q\n%s\n[exit %s]\n' "$ref" "$text" "$out" "$rc" >&2
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
  local pkgs=()
  command -v newuidmap > /dev/null || pkgs+=(uidmap)
  command -v setfacl > /dev/null || pkgs+=(acl)
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
  res meta target "brig ${head:0:7} · bundle $bundle"
  res fact "brig version" "$version"
  res fact "runtime bundle" "$bundle" "https://github.com/NOFireAI/brig-standalone-linux/releases/tag/$bundle"
  res fact urunc "${urunc:0:7}" "https://github.com/urunc-dev/urunc/commit/$urunc"
  res meta host_detail "$(nproc) vCPUs, $(free -g | awk '/^Mem:/ { print $2 }') GB, kernel $(uname -r), nested KVM. A rootless user install from install.sh at ${head:0:7}."

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

gate_cpus() {
  local want n0 n3 bundle rc=0
  want="$(timeout 30 brig agent show ubuntu --json | field cpus)"
  run 900 brig run -d ubuntu@cpu
  n0="$(gsh ubuntu@cpu nproc | tail -n 1)" || true
  run 300 brig rm ubuntu@cpu || true
  run 900 brig run -d ubuntu@cpu3 --cpus 3
  n3="$(gsh ubuntu@cpu3 nproc | tail -n 1)" || true
  run 300 brig rm ubuntu@cpu3 || true
  bundle="$(awk -F= '$1 == "BUNDLE_VERSION" { print $2 }' "$BUNDLE_DIR/pins.env")"
  local note="guest nproc=$n0 for the profile's cpus: $want, and nproc=$n3 for --cpus 3, on bundle $bundle."
  res at-least "$bundle" v0.1.0-rc10 || rc=$?
  if [ "$n0" = "$want" ] && [ "$n3" = 3 ]; then
    res gate cpus pass "$note"
  elif [ "$rc" = 1 ] && [ "$n0" = 1 ] && [ "$n3" = 1 ]; then
    res gate cpus expected "$note The fix ships in bundle rc10, and this gate is enforced from there."
  else
    res gate cpus fail "$note"
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
  warned="$(echo "$err" | grep 'is being restarted' || true)"
  if [ "$rc" = 0 ] && [ -n "$warned" ] && [ "$m3" = NONE ] && echo "$e3" | grep -q TCP_OK; then
    res check Network "--network shared restarts it, with the warning (#342)" pass "$(echo "$warned" | cut -c1-160)... New guest (no marker), egress: $(echo "$e3" | one_line 60)"
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

# ---------------------------------------------------------------- main

say "brig e2e $LEVEL, results in $RESULTS"
res host linux
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
  block nosudo gate_nosudo
  block cycles check_cycles
  block profiles check_profiles
  block homes check_rm_home
  block failed-boot check_failed_boot
  block ports check_ports
  block postures check_postures
  block shell check_shell_removed
  block symlink check_symlink
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
