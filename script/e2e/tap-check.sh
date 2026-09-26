#!/bin/bash
# Reads brig's Homebrew tap the way a user's Homebrew does, and records the
# result for the e2e report. It boots nothing, so a hosted macOS runner does.
#
# `brew readall brig-sh/brig` fails for the whole tap while hull@main.rb has
# no URL for every platform Homebrew 7 checks (brig-sh/hull#88). Until that
# lands, a failure that names hull@main.rb is expected. Any other failure is
# not.
#
# Settings: LEVEL, E2E_OUT and E2E_RESULTS, as in canary-linux.sh.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
LEVEL="${LEVEL:-canary}"
OUT="${E2E_OUT:-${RUNNER_TEMP:-/tmp}/brig-e2e}"
RESULTS="${E2E_RESULTS:-$OUT/results.json}"
LOGS="$OUT/logs"
mkdir -p "$LOGS"
export E2E_RECORDS="$OUT/records.jsonl"
: > "$E2E_RECORDS"

res() { python3 "$HERE/results.py" "$@"; }
one_line() { tr '\n' ' ' | tr -s ' ' | cut -c1-"${1:-400}"; }
tmo() {
  local s=$1
  shift
  perl -e 'alarm shift @ARGV; exec @ARGV or die "exec: $!"' "$s" "$@"
}

res host tap
res meta host_os none
res meta level "$LEVEL"
res meta commit "$(git -C "$REPO" rev-parse HEAD)"
res meta run_note "brew tap and brew readall, with no VM."
if [ -n "${GITHUB_RUN_ID:-}" ]; then
  res meta run_id "$GITHUB_RUN_ID"
  res meta run_url "${GITHUB_SERVER_URL:-https://github.com}/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID"
  res meta source "GitHub Actions run $GITHUB_RUN_ID, attempt ${GITHUB_RUN_ATTEMPT:-1} (${GITHUB_EVENT_NAME:-unknown})"
fi

brew_version="$(brew --version | head -n 1)"
res meta host_detail "macOS $(sw_vers -productVersion), $brew_version. It taps brig-sh/brig and reads every cask in it."
echo "$brew_version"

rc=0
tmo 600 brew tap brig-sh/brig > "$LOGS/tap.log" 2>&1 || rc=$?
cat "$LOGS/tap.log"
if [ "$rc" != 0 ]; then
  res check Homebrew "brew tap brig-sh/brig" fail "exit $rc: $(one_line 400 < "$LOGS/tap.log")"
else
  res check Homebrew "brew tap brig-sh/brig" pass "exit 0 on $brew_version"
fi

rc=0
tmo 600 brew readall brig-sh/brig > "$LOGS/readall.log" 2>&1 || rc=$?
cat "$LOGS/readall.log"
said="$(grep -v '^$' "$LOGS/readall.log" | head -n 4 | one_line 400)"
if [ "$rc" = 0 ]; then
  res check Homebrew "brew readall brig-sh/brig" pass "exit 0 on $brew_version"
elif grep -q 'hull@main' "$LOGS/readall.log"; then
  res check Homebrew "brew readall brig-sh/brig" expected "exit $rc, naming hull@main.rb, until brig-sh/hull#88 merges: $said"
else
  res check Homebrew "brew readall brig-sh/brig" fail "exit $rc: $said"
fi

verdict="$(res build "$RESULTS")"
echo "verdict: $verdict ($RESULTS)"
[ "$verdict" = go ]
