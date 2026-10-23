#!/bin/bash
# Run the network conformance suite (#264) against a real guest.
#
# It builds brig from this checkout and the probe for the guest, then boots
# three sandboxes in turn: no policy, a default: deny policy and a
# default: allow one. The record lands in
# docs/manual-tests/egress-conformance-<backend>-<runtime version>.md.
#
#   script/egress-conformance.sh [-image REF] [-record PATH]
#
# On macOS 15 or newer it boots hvi, with hull on PATH or BRIG_RUNTIME_BIN
# naming it. On Linux it boots nerdctl from the runtime bundle install.sh
# installs, with brig-ctl on PATH, and the record is filed under the bundle's
# version. With no runtime the run fails. It never skips, because
# docs/releasing.md reads the result. See test/conformance for the cases and
# the verdict rules.
set -euo pipefail
cd "$(dirname "$0")/.."

BIN="$(mktemp -d)"
trap 'rm -rf "$BIN"' EXIT

# The record names the brig version, so it has to be this checkout's. make
# build passes git's answers. -buildvcs=false stops the toolchain stamping its
# own over them: from a worktree nested inside another checkout it walks up
# and stamps the outer one's commit.
GOFLAGS=-buildvcs=false make -s build BINDIR="$BIN"

# The guest is of the host's own architecture. A shell under Rosetta says
# x86_64 on an arm64 Mac, so there the hardware is asked before uname.
if [ "$(uname -s)" = Darwin ] && [ "$(sysctl -i -n hw.optional.arm64)" = 1 ]; then
  arch=arm64
else
  case "$(uname -m)" in
    arm64 | aarch64) arch=arm64 ;;
    x86_64) arch=amd64 ;;
    *) echo "egress-conformance: no guest build for $(uname -m)" >&2; exit 1 ;;
  esac
fi

# On Linux brig reaches the bundle's containerd only with the environment the
# bundle's launchers set, and the brig built above runs without a launcher.
# brig-ctl prints that environment, and it is read here rather than copied,
# because it is what decides between the rootless containerd and the system
# one. A caller that set it already keeps their own.
#
# A user install puts only the brig launchers on PATH, so brig-ctl is looked
# for where install.sh puts it too: under the XDG data directory for a user,
# and under /var/lib/brig for root. brig-env.sh then puts the bundle's bin on
# PATH, which is where the runner finds brig-ctl for the bundle's pins.
if [ "$(uname -s)" = Linux ] && [ -z "${CONTAINERD_ADDRESS:-}" ]; then
  ctl=$(command -v brig-ctl || true)
  for c in "${XDG_DATA_HOME:-$HOME/.local/share}/brig/data/bin/brig-ctl" /var/lib/brig/data/bin/brig-ctl; do
    [ -n "$ctl" ] && break
    [ -x "$c" ] && ctl=$c
  done
  if [ -z "$ctl" ]; then
    echo "egress-conformance: no brig-ctl, so no runtime bundle to boot." >&2
    echo "Install it with install.sh, or set CONTAINERD_ADDRESS and BRIG_RUNTIME_BIN for a nerdctl of your own" >&2
    exit 1
  fi
  # Two steps, because eval returns its own status: a brig-ctl env that
  # failed inside it would not stop the script under set -e, and the suite
  # would run without the bundle's CONTAINERD_ADDRESS.
  envs=$("$ctl" env)
  eval "$envs"
fi
make -s netprobe NETPROBE_DIR="$BIN/netprobe"

go build -o "$BIN/conformance" ./test/conformance

"$BIN/conformance" -brig "$BIN/brig" -probe "$BIN/netprobe/linux-$arch/netprobe" "$@"
