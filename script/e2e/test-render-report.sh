#!/bin/sh
# Check the e2e report tools without a runtime. results.py builds each host's
# results.json from its records, merge-results.py joins the hosts, and
# render-report.py renders them and the v0.3.0 release-gate example as a page,
# a fragment and a job summary. All of them refuse what the page cannot show.
#
# Usage: script/e2e/test-render-report.sh

set -eu

here="$(cd "$(dirname "$0")" && pwd)"
td="$here/testdata"

skip() { echo "SKIP: $*"; exit 0; }
fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok - $*"; }

command -v python3 > /dev/null 2>&1 || skip "no python3"

T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT

render() { python3 "$here/render-report.py" "$@"; }
results() { E2E_RECORDS="$T/records.jsonl" python3 "$here/results.py" "$@"; }
merge() { python3 "$here/merge-results.py" "$@"; }

# A Linux host's records become a results.json. cpus is expected to fail on
# bundle rc9, and macOS's gate does not apply, so the verdict is still go.
cp "$td/records-canary.jsonl" "$T/records.jsonl"
verdict="$(results build "$T/canary.json")" || fail "results.py build failed"
[ "$verdict" = go ] || fail "the canary records gave $verdict, not go"
python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
g = {b["id"]: b["results"]["now"]["linux"]["status"] for b in d["blockers"]}
assert g["cpus"] == "expected" and g["gatekeeper"] == "na", g
' "$T/canary.json" || fail "the gates did not land per host"
ok "a Linux host's records build a go, with the macOS gate n/a"

# The same records again, with the first results.json as the baseline.
results build --baseline "$T/canary.json" "$T/canary-2.json" > /dev/null || fail "a build with a baseline failed"
python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
assert [r["id"] for r in d["runs"]] == ["base", "now"], d["runs"]
assert d["timings"]["baseline"] == "base"
m = {x["name"]: x["values"]["linux"] for x in d["timings"]["metrics"]}
assert m["ubuntu boot"] == [3.12, 3.12], m
assert d["blockers"][0]["results"]["base"]["linux"]["status"] == "pass"
' "$T/canary-2.json" || fail "the baseline did not reach runs, gates and timings"
ok "a baseline results.json becomes the first run and the timing baseline"

# A second host, then both merged, with a third expected and missing.
cp "$td/records-mac.jsonl" "$T/records.jsonl"
results build "$T/mac.json" > /dev/null || fail "the macOS records did not build"
verdict="$(merge --expect linux,mac --out "$T/merged.json" "$T/canary.json" "$T/mac.json")" \
  || fail "merge-results.py failed"
[ "$verdict" = go ] || fail "two hosts with no failing gate merged to $verdict"
python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
assert [h["id"] for h in d["hosts"]] == ["linux", "mac"], d["hosts"]
assert d["runs"][-1]["hosts"] == ["linux", "mac"]
gk = next(b for b in d["blockers"] if b["id"] == "gatekeeper")["results"]["now"]
assert gk == {"linux": {"status": "na", "note": "macOS only."}, "mac": gk["mac"]} and gk["mac"]["status"] == "skip", gk
assert set(d["checks"]["hosts"]) == {"linux", "mac"}
m = {x["name"]: x["values"] for x in d["timings"]["metrics"]}
assert set(m["ubuntu boot"]) == {"linux", "mac"}, m
labels = [f["label"] for f in d["facts"]]
assert labels.count("brig version") == 1 and "hull" in labels and "runtime bundle (Linux)" in labels, labels
' "$T/merged.json" || fail "the merge did not keep hosts, gates, checks and timings apart"
verdict="$(merge --expect linux,mac,tap --out "$T/missing.json" "$T/canary.json" "$T/mac.json" 2> "$T/merge.err")" \
  || fail "merge-results.py failed with a host missing"
[ "$verdict" = no-go ] || fail "a missing host merged to $verdict"
python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
assert "tap" in [h["id"] for h in d["hosts"]]
rows = d["checks"]["hosts"]["tap"]
assert rows[0]["status"] == "fail" and "no results-tap.json" in rows[0]["evidence"], rows
' "$T/missing.json" || fail "a missing host does not show as a failure"
ok "hosts merge into one run, and a missing host is a no-go"

for f in "$td/results-v030-gate.json" "$T/canary.json" "$T/canary-2.json" "$T/merged.json"; do
  name="$(basename "$f" .json)"
  render "$f" > "$T/$name.html" || fail "$name did not render"
  head -n 1 "$T/$name.html" | grep -q '^<!doctype html>' || fail "$name.html is not a whole document"
  sed -n '/<head>/,/<\/head>/p' "$T/$name.html" | grep -q '<title>' || fail "$name.html has no title in <head>"
  tail -n 1 "$T/$name.html" | grep -q '^</html>$' || fail "$name.html does not end the document"
  render --fragment "$f" > "$T/$name.frag.html" || fail "$name did not render as a fragment"
  if grep -q -E '<html|<head>|<body>' "$T/$name.frag.html"; then
    fail "$name's fragment carries a document wrapper"
  fi
  render --summary "$f" > "$T/$name.md" || fail "$name did not render as a summary"
  grep -q '^## ' "$T/$name.md" || fail "$name.md has no title"
  grep -q '^### Timings' "$T/$name.md" || fail "$name.md has no timings"
  ok "$name renders as a page, a fragment and a summary"
done

# The failing check's evidence carries "</script>". Inside the data block it
# must stay escaped, or the page's script would end there.
n="$(grep -c '</script>' "$T/canary.html")"
[ "$n" = 2 ] || fail "canary.html has $n </script> tags, not the template's 2"
ok "evidence cannot end the page's data block"

grep -q '^## brig e2e canary: Go$' "$T/canary.md" || fail "the summary does not lead with the verdict"
grep -q '^| --cpus sizes the guest | Expected fail |$' "$T/canary.md" || fail "the summary has no row for the cpus gate"
# The backticks are Markdown in the summary, not a command substitution.
# shellcheck disable=SC2016
grep -q -F 'runtime bundle (Linux) `v0.1.0-rc9 (install.sh pin)`' "$T/canary.md" || fail "the summary does not say which bundle ran"
grep -q '^- \*\*Network: A published port answers from the host\*\*$' "$T/canary.md" \
  || fail "the summary does not list the failing check"
grep -q 'Connection reset by peer' "$T/canary.md" || fail "the summary drops the failing check's evidence"
grep -q '^| --cpus sizes the guest | Expected fail | N/A |$' "$T/merged.md" || fail "the merged summary has no column per host"
ok "the summary has the verdict, the bundle, the gates and the failing check with its evidence"

# A gate that fails, or does not run, is a no-go.
grep -v '"id": "nosudo"' "$td/records-canary.jsonl" > "$T/records.jsonl"
verdict="$(results build "$T/unrecorded.json")" || fail "a build with a gate missing failed"
[ "$verdict" = no-go ] || fail "a gate that did not run gave $verdict"
python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
g = {b["id"]: b["results"]["now"]["linux"]["status"] for b in d["blockers"]}
assert g["nosudo"] == "fail", g
' "$T/unrecorded.json" || fail "a gate that did not run is not recorded as a failure"
cp "$td/records-canary.jsonl" "$T/records.jsonl"
results gate exec fail "brig sh: 85/100 correct (15 empty, 0 wrong)."
verdict="$(results build "$T/failed.json")" || fail "a build with a failing gate failed"
[ "$verdict" = no-go ] || fail "a failing gate gave $verdict"
ok "a gate that fails or does not run is a no-go"

# A status the page has no chip for is refused, by both tools.
python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
d["checks"]["hosts"]["linux"][0]["status"] = "passed"
json.dump(d, open(sys.argv[2], "w"))
' "$T/canary.json" "$T/bad.json"
if render "$T/bad.json" > "$T/bad.html" 2> "$T/bad.err"; then
  fail "a check with status passed rendered"
fi
grep -q "bad status 'passed'" "$T/bad.err" || fail "the refusal does not name the status: $(cat "$T/bad.err")"
if results check Install version passed evidence 2> "$T/bad.err"; then
  fail "results.py recorded a check with status passed"
fi
ok "a status with no chip is refused"

results at-least v0.1.0-rc10 v0.1.0-rc10 || fail "rc10 is not at least rc10"
results at-least v0.1.0 v0.1.0-rc10 || fail "v0.1.0 is not at least rc10"
if results at-least v0.1.0-rc9 v0.1.0-rc10; then fail "rc9 counts as at least rc10"; fi
ok "bundle versions compare with rc10 as the floor"
