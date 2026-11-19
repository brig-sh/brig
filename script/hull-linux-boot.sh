#!/bin/sh
# Boot real hull sandboxes on Linux, for .github/workflows/hull-linux.yml.
#
# script/smoke.sh stubs the runtime, so ci.yml never shows that hull on Linux
# boots. This script does, on a host with KVM and a systemd user session. Each
# check is a subcommand, so the workflow shows which one failed:
#
#   setup <bindir>  install the runtime bundle rootless at the RUNTIME_VERSION
#                   install.sh pins, unless this user already has it, and put
#                   the brig, brigd and brig-broker in <bindir> behind the
#                   bundle's launcher
#   doctor          brig doctor ubuntu, with no line marked !!
#   offline         boot --network offline, run a command, reach nothing
#   isolated        boot on the profile's isolated network, reach three hosts
#   policy          a deny-by-default policy: one host reached, two refused
#   rm              brig rm leaves no sandbox and no gateway behind
#   cleanup         remove whatever this run left, by its name prefix
#
# Every sandbox is a session of the ubuntu profile. Its image runs as root, and
# hull on Linux refuses an image whose USER is a name.
set -eu

say() { printf 'hull-linux: %s\n' "$*" >&2; }
fail() {
  say "FAIL: $*"
  exit 1
}

# The checkout's root, where install.sh is.
TOP=$(cd "$(dirname "$0")/.." && pwd)

# Every sandbox, policy and gateway of this run carries RUN in its name, so
# cleanup removes this run's leftovers and nothing else.
RUN="${BRIG_CI_PREFIX:-ci${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-1}}"

ALLOWED_HOST=example.com
DENIED_HOST=github.com
DENIED_IP=1.1.1.1

# The runner's service starts no login session, so nothing sets
# XDG_RUNTIME_DIR. systemctl --user and the bundle's containerd socket both
# live under it.
XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export XDG_RUNTIME_DIR

# Where a user install of the bundle puts its tree and its launchers. These
# are the paths install.sh's install_runtime uses.
BUNDLE="${XDG_DATA_HOME:-$HOME/.local/share}/brig/data"
LAUNCHERS="$HOME/.local/bin"
PATH="$LAUNCHERS:$PATH"
export PATH

BRIG_RUNTIME=hull
DO_NOT_TRACK=1
# Per run, so a check finds only what this run made. The gateway directory
# holds the socket and pid file of every gateway this run starts.
BRIG_POLICY_DIR="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/brig-policies-$RUN"
BRIG_GATEWAY_DIR="$HOME/.brig/ci/$RUN"
export BRIG_RUNTIME DO_NOT_TRACK BRIG_POLICY_DIR BRIG_GATEWAY_DIR
# The ubuntu profile forwards GH_TOKEN into the guest when it is set. These
# guests get no credential. The launcher works out BRIG_RUNTIME_BIN itself.
unset GH_TOKEN GITHUB_TOKEN BRIG_RUNTIME_BIN BRIG_RUNTIME_VERSION

ref() { printf 'ubuntu@%s-%s' "$RUN" "$1"; }

# hull, with the environment the bundle's launcher gives brig.
hull() {
  (
    # shellcheck disable=SC1091
    . "$BUNDLE/etc/brig-env.sh"
    "$BUNDLE/bin/hull" "$@"
  )
}

# run_refs prints the refs of this run that brig lists.
run_refs() {
  _all=$(brig -q ls) || fail "brig ls failed"
  printf '%s\n' "$_all" | grep -F "ubuntu@$RUN-" || true
}

show_logs() {
  say "brig logs $1:"
  brig logs "$1" --tail 100 || true
  say "brig logs --gateway $1:"
  brig logs --gateway "$1" || true
}

# boot <ref> [flag...] starts the sandbox detached.
boot() {
  _ref=$1
  shift
  say "+ brig run -d --no-project $* $_ref"
  _out=$(brig run -d --no-project "$@" "$_ref" < /dev/null) \
    || { show_logs "$_ref"; fail "brig run -d $_ref failed"; }
  printf '%s\n' "$_out"
  case "$(run_refs)" in
    *"$_ref"*) ;;
    *) fail "$_ref booted, and brig ls does not list it" ;;
  esac
}

# The probe runs in the guest. ubuntu:latest has no curl, so it opens the
# connection with bash's /dev/tcp. It prints a marker either way. A missing
# marker means brig sh did not run it, which is a failure of its own.
# shellcheck disable=SC2016
PROBE='timeout 10 getent hosts "$1" || echo "dns: $1 did not resolve"
if timeout 20 bash -c "exec 3<>/dev/tcp/$1/$2"; then
  echo probe=reached
else
  echo "probe=refused rc=$?"
fi'

# probe <ref> <host> <port> sets PROBE_RESULT to reached or refused.
probe() {
  say "+ brig sh $1: connect to $2:$3"
  _out=$(brig sh "$1" -c "$PROBE" probe "$2" "$3" < /dev/null 2>&1) || true
  printf '%s\n' "$_out" | sed 's/^/    /'
  case "$_out" in
    *probe=reached*) PROBE_RESULT=reached ;;
    *probe=refused*) PROBE_RESULT=refused ;;
    *)
      show_logs "$1"
      fail "brig sh $1 did not run the probe"
      ;;
  esac
}

expect_reached() {
  probe "$@"
  [ "$PROBE_RESULT" = reached ] || { show_logs "$1"; fail "$1 did not reach $2:$3"; }
}

expect_refused() {
  probe "$@"
  [ "$PROBE_RESULT" = refused ] || { show_logs "$1"; fail "$1 reached $2:$3"; }
}

# leftovers prints what this run left behind: sandboxes brig lists, hull
# instances, gateway sockets and pid files, and gateway processes. A brig ls
# that fails prints a line too, so it never reads as nothing left.
leftovers() {
  (run_refs) || echo "brig ls failed, so its sandboxes are unknown"
  if [ -x "$BUNDLE/bin/hull" ]; then
    _ps=$(hull ps 2>&1) || say "hull ps failed: $_ps"
    printf '%s\n' "$_ps" | grep -F -- "$RUN-" || true
  fi
  if [ -d "$BRIG_GATEWAY_DIR" ]; then
    find "$BRIG_GATEWAY_DIR" \( -name '*.sock' -o -name '*.pid' -o -name '*.api' \) -print
  fi
  pgrep -af -- "$BRIG_GATEWAY_DIR/" || true
}

cmd_setup() {
  [ $# -eq 1 ] || fail "usage: $0 setup <directory with brig, brigd and brig-broker>"
  _bin=$1
  for _b in brig brigd brig-broker; do
    [ -x "$_bin/$_b" ] || fail "no $_b in $_bin"
  done

  [ -d "$XDG_RUNTIME_DIR" ] \
    || fail "no $XDG_RUNTIME_DIR: enable lingering for $(id -un) (loginctl enable-linger)"

  _want=$(sed -n 's/^RUNTIME_VERSION=//p' "$TOP/install.sh")
  [ -n "$_want" ] || fail "install.sh pins no RUNTIME_VERSION"
  _have=""
  if [ -f "$BUNDLE/pins.env" ]; then
    _have=$(awk -F= '$1 == "BUNDLE_VERSION" {print $2}' "$BUNDLE/pins.env")
  fi
  if [ "${_have#v}" = "${_want#v}" ] && [ -x "$LAUNCHERS/brig" ]; then
    say "the runtime bundle $_want is installed in $BUNDLE"
  else
    say "installing the runtime bundle $_want for $(id -un) (installed: ${_have:-none})"
    BRIG_INSTALL_ROOTLESS=1 sh "$TOP/install.sh"
  fi

  [ -x "$BUNDLE/bin/hull" ] \
    || fail "the runtime bundle $_want carries no hull. hull on Linux needs v0.1.0-rc22 or later"

  # A host reboot, or a stop by hand, leaves the unit enabled and not running.
  if ! systemctl --user is-active --quiet brig-containerd.service; then
    say "brig-containerd.service is not running; starting it"
    systemctl --user start brig-containerd.service \
      || { systemctl --user --no-pager status brig-containerd.service || true; fail "brig-containerd.service did not start"; }
  fi

  # install.sh puts a brig release here. This build replaces it, so the
  # launcher runs the code under test.
  for _b in brig brigd brig-broker; do
    install -m 0755 "$_bin/$_b" "$BUNDLE/bin/$_b"
  done

  _path=$(command -v brig) || fail "no brig on PATH"
  [ "$_path" = "$LAUNCHERS/brig" ] || fail "brig on PATH is $_path, not the launcher in $LAUNCHERS"
  _sha=$(git -C "$TOP" rev-parse HEAD)
  _ver=$(brig version --json) || fail "brig version failed"
  case "$_ver" in
    *"$_sha"*) ;;
    *)
      printf '%s\n' "$_ver"
      fail "the launcher does not run the build of $_sha"
      ;;
  esac
  brig version
  hull --version
}

cmd_doctor() {
  say "+ brig doctor ubuntu"
  _rc=0
  _out=$(brig doctor ubuntu 2>&1) || _rc=$?
  printf '%s\n' "$_out"
  [ "$_rc" -eq 0 ] || fail "brig doctor exited $_rc"
  case "$_out" in
    *"  ok  runtime   hull "*) ;;
    *) fail "brig doctor does not report hull as the runtime" ;;
  esac
  # doctor exits 0 on most findings, so a line marked !! fails here.
  if printf '%s\n' "$_out" | grep -q '^  !!  '; then
    fail "brig doctor marked a check !!"
  fi
}

cmd_offline() {
  _r=$(ref off)
  boot "$_r" --network offline
  say "+ brig sh $_r: uname -r"
  _guest=$(brig sh "$_r" uname -r < /dev/null) || { show_logs "$_r"; fail "brig sh $_r failed"; }
  printf '    %s\n' "$_guest"
  # A guest on the host's kernel is a container, and brig promises a microVM.
  [ -n "$_guest" ] || fail "brig sh $_r printed no kernel release"
  [ "$_guest" != "$(uname -r)" ] || fail "the guest runs the host's kernel $_guest"
  expect_refused "$_r" "$DENIED_IP" 443
  expect_refused "$_r" "$ALLOWED_HOST" 443
}

cmd_isolated() {
  _r=$(ref iso)
  boot "$_r"
  # An isolated sandbox gets a gateway of its own.
  [ -n "$(find "$BRIG_GATEWAY_DIR" -name 'sandbox-*.sock' 2> /dev/null)" ] \
    || fail "$_r booted, and no isolated gateway socket is in $BRIG_GATEWAY_DIR"
  # The policy check refuses these same targets. They have to be reachable
  # without a policy for that refusal to mean anything.
  expect_reached "$_r" "$ALLOWED_HOST" 443
  expect_reached "$_r" "$DENIED_HOST" 443
  expect_reached "$_r" "$DENIED_IP" 443
}

cmd_policy() {
  _r=$(ref pol)
  _label="$RUN-pol"
  mkdir -p "$BRIG_POLICY_DIR"
  say "+ brig policy create ci-deny"
  brig policy create ci-deny << EOF
apiVersion: brig.sh/v1alpha1
kind: Policy
metadata:
  name: ci-deny
  description: hull on Linux CI, deny by default with one host allowed
spec:
  egress:
    default: deny
    allow:
      - host: $ALLOWED_HOST
EOF
  say "+ brig policy attach ci-deny ubuntu -n $_label"
  brig policy attach ci-deny ubuntu -n "$_label"
  say "+ brig policy check ubuntu -n $_label"
  _rc=0
  brig policy check ubuntu -n "$_label" || _rc=$?
  [ "$_rc" -ne 7 ] || fail "brig refuses a policy on hull on Linux (exit 7); brig#675 lifts the refusal"
  [ "$_rc" -eq 0 ] || fail "brig policy check exited $_rc"
  boot "$_r"
  expect_reached "$_r" "$ALLOWED_HOST" 443
  # A refused name is the DNS half of the policy. The address is the other
  # half: a guest that skips DNS must be refused too.
  expect_refused "$_r" "$DENIED_HOST" 443
  expect_refused "$_r" "$DENIED_IP" 443
}

cmd_rm() {
  _refs=$(run_refs)
  [ -n "$_refs" ] || fail "brig lists no sandbox of this run, so brig rm has nothing to remove"
  for _r in $_refs; do
    say "+ brig rm $_r"
    brig rm "$_r" < /dev/null || fail "brig rm $_r failed"
  done
  # A gateway process can take a moment to exit after brig rm returns, so
  # the check waits up to 10 seconds.
  _left=""
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    _left=$(leftovers)
    [ -n "$_left" ] || break
    sleep 1
  done
  if [ -n "$_left" ]; then
    printf '%s\n' "$_left"
    fail "brig rm left the above behind"
  fi
  say "no sandbox, hull instance or gateway of $RUN is left"
}

# cleanup removes what this run left, whatever failed before it. It carries on
# past a failure and fails at the end if something is still there.
cmd_cleanup() {
  set +e
  for _r in $(run_refs); do
    say "+ brig rm $_r"
    brig rm "$_r" < /dev/null
  done
  if [ -x "$BUNDLE/bin/hull" ]; then
    _ps=$(hull ps 2>&1)
    for _vm in $(printf '%s\n' "$_ps" | tr -s ' \t' '\n' | grep -F -- "$RUN-" | sort -u); do
      say "+ hull rm --force $_vm"
      hull rm --force "$_vm"
    done
  fi
  pkill -f -- "$BRIG_GATEWAY_DIR/"
  sleep 1
  rm -rf "$BRIG_GATEWAY_DIR" "$BRIG_POLICY_DIR"
  _left=$(leftovers)
  set -e
  if [ -n "$_left" ]; then
    printf '%s\n' "$_left"
    fail "cleanup could not remove the above"
  fi
  say "nothing of $RUN is left"
}

[ $# -ge 1 ] || fail "usage: $0 setup|doctor|offline|isolated|policy|rm|cleanup"
_cmd=$1
shift
case "$_cmd" in
  setup) cmd_setup "$@" ;;
  doctor) cmd_doctor ;;
  offline) cmd_offline ;;
  isolated) cmd_isolated ;;
  policy) cmd_policy ;;
  rm) cmd_rm ;;
  cleanup) cmd_cleanup ;;
  *) fail "unknown check $_cmd" ;;
esac
