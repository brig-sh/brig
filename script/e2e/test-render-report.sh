#!/bin/sh
# Check the e2e report tools without a runtime: results.py builds a
# results.json from a canary's records, and render-report.py renders it and the
# v0.3.0 release-gate example as a page, a fragment and a job summary. Both
# refuse a status the page has no chip for.
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

# A canary's records become a results.json. cpus is expected to fail on
# bundle rc9, so the verdict is still go.
cp "$td/records-canary.jsonl" "$T/records.jsonl"
verdict="$(results build "$T/canary.json")" || fail "results.py build failed"
[ "$verdict" = go ] || fail "the canary records gave $verdict, not go"
ok "records with every gate passing or expected build a go"

# The same records again, with the first results.json as the baseline.
verdict="$(results build --baseline "$T/canary.json" "$T/canary-2.json")" || fail "a build with a baseline failed"
python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
assert [r["id"] for r in d["runs"]] == ["base", "now"], d["runs"]
assert d["timings"]["baseline"] == "base"
m = {x["name"]: x["values"]["gha"] for x in d["timings"]["metrics"]}
assert m["ubuntu boot"] == [3.12, 3.12], m
assert d["blockers"][0]["results"]["base"]["gha"]["status"] == "pass"
' "$T/canary-2.json" || fail "the baseline did not reach runs, gates and timings"
ok "a baseline results.json becomes the first run and the timing baseline"

for f in "$td/results-v030-gate.json" "$T/canary.json" "$T/canary-2.json"; do
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
grep -q '^- \*\*Network: A published port answers from the host\*\*$' "$T/canary.md" \
  || fail "the summary does not list the failing check"
grep -q 'Connection reset by peer' "$T/canary.md" || fail "the summary drops the failing check's evidence"
ok "the summary has the verdict, the gates and the failing check with its evidence"

# A gate that fails, or does not run, is a no-go.
grep -v '"id": "nosudo"' "$td/records-canary.jsonl" > "$T/records.jsonl"
verdict="$(results build "$T/missing.json")" || fail "a build with a gate missing failed"
[ "$verdict" = no-go ] || fail "a gate that did not run gave $verdict"
python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
g = {b["id"]: b["results"]["now"]["gha"]["status"] for b in d["blockers"]}
assert g["nosudo"] == "skip", g
' "$T/missing.json" || fail "a gate that did not run is not recorded as skip"
cp "$td/records-canary.jsonl" "$T/records.jsonl"
results gate exec fail "brig sh: 85/100 correct (15 empty, 0 wrong)."
verdict="$(results build "$T/failed.json")" || fail "a build with a failing gate failed"
[ "$verdict" = no-go ] || fail "a failing gate gave $verdict"
ok "a gate that fails or does not run is a no-go"

# A status the page has no chip for is refused, by both tools.
python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
d["checks"]["hosts"]["gha"][0]["status"] = "passed"
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
