#!/bin/bash
# Run the network conformance suite (#264) against a real guest.
#
# It builds brig from this checkout and the probe for the guest, then boots
# three sandboxes in turn on hvi: no policy, a default: deny policy and a
# default: allow one. The record lands in
# docs/manual-tests/egress-conformance-<backend>-<runtime version>.md.
#
#   script/egress-conformance.sh [-image REF] [-record PATH]
#
# macOS 15 or newer with hull on PATH, or BRIG_RUNTIME_BIN naming it. With no
# runtime the run fails. It never skips, because docs/releasing.md reads the
# result. See test/conformance for the cases and the verdict rules.
set -euo pipefail
cd "$(dirname "$0")/.."

BIN="$(mktemp -d)"
trap 'rm -rf "$BIN"' EXIT

# The record names the brig version, so it has to be this checkout's. make
# build passes git's answers. -buildvcs=false stops the toolchain stamping its
# own over them: from a worktree nested inside another checkout it walks up
# and stamps the outer one's commit.
GOFLAGS=-buildvcs=false make -s build BINDIR="$BIN"

# hvi runs a guest of the host's own architecture. A shell under Rosetta
# says x86_64 on an arm64 Mac, so the hardware is asked before uname.
if [ "$(sysctl -i -n hw.optional.arm64)" = 1 ]; then
  arch=arm64
else
  case "$(uname -m)" in
    arm64 | aarch64) arch=arm64 ;;
    x86_64) arch=amd64 ;;
    *) echo "egress-conformance: no guest build for $(uname -m)" >&2; exit 1 ;;
  esac
fi
make -s netprobe NETPROBE_DIR="$BIN/netprobe"

go build -o "$BIN/conformance" ./test/conformance

"$BIN/conformance" -brig "$BIN/brig" -probe "$BIN/netprobe/linux-$arch/netprobe" "$@"
