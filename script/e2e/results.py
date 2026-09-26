#!/usr/bin/env python3
"""Collects what canary-linux.sh finds and writes it as results.json.

The canary never writes JSON itself. Each result goes through one of the
record commands, which append a line to the file $E2E_RECORDS names. build
reads those lines and writes results.json in schema 1, the one
render-report.py renders.

    results.py gate ID STATUS NOTE
    results.py check GROUP NAME STATUS EVIDENCE
    results.py meta KEY VALUE
    results.py sample METRIC SECONDS
    results.py note METRIC TEXT
    results.py build [--baseline FILE] OUT
    results.py median FILE
    results.py at-least VERSION FLOOR
    results.py field FILE KEY[.KEY...]

build prints the verdict, go or no-go. at-least exits 0 when VERSION is
FLOOR or newer, 1 when it is older, and 2 when either is not a vX.Y.Z or
vX.Y.Z-rcN tag.
"""

import datetime
import json
import os
import re
import statistics
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

STATUSES = {"pass", "fail", "expected", "warn", "skip", "na", "running",
            "fixed", "review", "progress", "open", "filed"}

# A gate that is expected to fail, such as --cpus on a bundle older than its
# fix, does not stop a go. Anything else that is not a pass does.
GO_STATUSES = {"pass", "expected"}

HOST = "gha"


def link(text, url):
    return {"text": text, "url": url}


BRIG = "https://github.com/brig-sh/brig"
BUNDLE = "https://github.com/NOFireAI/brig-standalone-linux"

# One gate per release blocker the v0.3.0 stress runs found on Linux, in the
# order the canary runs them.
GATES = [
    {
        "id": "home",
        "name": "A forced image pull with DOCKER_CONFIG unset",
        "detail": "brig put the guest's HOME into the runtime's own environment. "
                  "Rootless nerdctl then read /root/.docker/config.json, and every image pull failed.",
        "fix": [link("#337", BRIG + "/issues/337"), link("#343", BRIG + "/pull/343")],
    },
    {
        "id": "exec",
        "name": "Short guest commands return their output",
        "detail": "The runtime's in-guest agent reported a command's exit before its output was "
                  "drained, so about 15 in 100 short commands came back empty with exit 0.",
        "fix": [link("bundle #10", BUNDLE + "/pull/10"), link("#356", BRIG + "/pull/356")],
    },
    {
        "id": "claude",
        "name": "claude-code boots without a .claude refusal",
        "detail": "brig reads a few answers from the guest before it hands claude-code a credential. "
                  "A lost answer read as a .claude mount that was not ephemeral, so brig refused.",
        "fix": [link("bundle rc9", BUNDLE + "/releases/tag/v0.1.0-rc9"), link("#354", BRIG + "/pull/354")],
    },
    {
        "id": "nosudo",
        "name": "A no-sudo user install stops before the download",
        "detail": "A user without sudo unpacked the whole bundle and then waited at a sudo prompt. "
                  "The installer should stop first and print the commands for root.",
        "fix": [link("bundle #7", BUNDLE + "/pull/7")],
    },
    {
        "id": "cpus",
        "name": "--cpus sizes the guest",
        "detail": "Bundle rc9 boots every guest with one vCPU, whatever the profile or --cpus asks. "
                  "The fix ships in bundle rc10.",
        "fix": [link("bundle #11", BUNDLE + "/pull/11")],
    },
]

# The timings the report shows, in this order, when the run measured them.
METRICS = ["ubuntu boot", "claude-code boot", "claude-code in parallel", "claude-code restart"]

VERSION_RE = re.compile(r"^v?(\d+)\.(\d+)\.(\d+)(?:-rc\.?(\d+))?$")


def records_path():
    path = os.environ.get("E2E_RECORDS")
    if not path:
        sys.exit("results.py: set E2E_RECORDS to the records file")
    return path


def append(record):
    with open(records_path(), "a", encoding="utf-8") as fh:
        fh.write(json.dumps(record, ensure_ascii=False) + "\n")


def read_records():
    path = records_path()
    if not os.path.exists(path):
        return []
    with open(path, encoding="utf-8") as fh:
        return [json.loads(line) for line in fh if line.strip()]


def version_key(v):
    """Returns a sortable key for vX.Y.Z or vX.Y.Z-rcN, or None for anything else."""
    m = VERSION_RE.match(v.strip())
    if not m:
        return None
    major, minor, patch, rc = m.groups()
    # A release sorts after every one of its release candidates.
    return (int(major), int(minor), int(patch), int(rc) if rc is not None else float("inf"))


def median(values):
    return round(statistics.median(values), 2) if values else None


def load_baseline(path):
    """Returns (data, run id) of a previous results.json, or (None, None)."""
    if not path or not os.path.exists(path):
        return None, None
    try:
        with open(path, encoding="utf-8") as fh:
            data = json.load(fh)
    except (OSError, ValueError) as err:
        print("results.py: ignoring the baseline %s: %s" % (path, err), file=sys.stderr)
        return None, None
    if data.get("schema") != 1:
        return None, None
    current = (data.get("timings") or {}).get("current") or (data.get("checks") or {}).get("run")
    return data, current


def build(out, baseline_path):
    records = read_records()
    meta = {}
    gates = {}
    checks = []
    samples = {}
    notes = {}
    for r in records:
        kind = r.get("kind")
        if kind == "meta":
            meta[r["key"]] = r["value"]
        elif kind == "gate":
            gates[r["id"]] = {"status": r["status"], "note": r["note"]}
        elif kind == "check":
            checks.append({k: r[k] for k in ("group", "name", "status", "evidence")})
        elif kind == "sample":
            samples.setdefault(r["metric"], []).append(r["seconds"])
        elif kind == "note":
            notes[r["metric"]] = r["text"]

    level = meta.get("level", "canary")
    commit = meta.get("commit", "")
    short = commit[:7] or "unknown"
    bundle = meta.get("bundle_version", "unknown")
    now = datetime.datetime.now(datetime.timezone.utc).replace(microsecond=0)
    date = now.strftime("%Y-%m-%d")

    base, base_run = load_baseline(baseline_path)

    runs = []
    if base:
        brun = next((r for r in base.get("runs", []) if r.get("id") == base_run), {})
        runs.append({
            "id": "base",
            "label": "Baseline",
            "date": brun.get("date", ""),
            "target": brun.get("target", ""),
            "hosts": [HOST],
            "note": "the last scheduled run that passed (%s)." % (base.get("source") or "no source"),
        })
    runs.append({
        "id": "now",
        "label": "This run",
        "date": date,
        "target": "brig %s · bundle %s" % (short, bundle),
        "hosts": [HOST],
        "note": meta.get("run_note", "LEVEL=%s." % level),
    })

    blockers = []
    for g in GATES:
        res = gates.get(g["id"], {"status": "skip", "note": "Did not run. See the logs."})
        results = {"now": {HOST: res}}
        if base:
            for b in base.get("blockers", []):
                prev = (b.get("results", {}).get(base_run) or {}).get(HOST)
                if b.get("id") == g["id"] and prev:
                    results["base"] = {HOST: prev}
        blockers.append(dict(g, results=results))

    failed = [b for b in blockers if b["results"]["now"][HOST]["status"] not in GO_STATUSES]
    expected = [b for b in blockers if b["results"]["now"][HOST]["status"] == "expected"]
    bad_checks = [c for c in checks if c["status"] == "fail"]
    go = not failed

    if go:
        headline = "No gate fails on brig %s" % short
        text = "No gate fails."
        if expected:
            text += " Expected to fail on bundle %s: %s." % (bundle, "; ".join(b["name"] for b in expected))
    else:
        headline = "%s of %d gates fail on brig %s" % (len(failed), len(blockers), short)
        text = "Failing or not run: %s." % "; ".join(b["name"] for b in failed)
    if bad_checks:
        text += " %d of %d checks %s." % (len(bad_checks), len(checks),
                                         "fails" if len(bad_checks) == 1 else "fail")
    else:
        text += " No check fails."

    facts = [{"label": "brig", "value": short}]
    if commit:
        facts[0]["url"] = "%s/commit/%s" % (BRIG, commit)
    if meta.get("brig_version"):
        facts.append({"label": "brig version", "value": meta["brig_version"]})
    facts.append({"label": "runtime bundle", "value": bundle})
    if bundle.startswith("v"):
        facts[-1]["url"] = "%s/releases/tag/%s" % (BUNDLE, bundle)
    if meta.get("urunc_ref"):
        ref = meta["urunc_ref"]
        fact = {"label": "urunc", "value": ref[:7]}
        if re.match(r"^[0-9a-f]{40}$", ref):
            fact["url"] = "https://github.com/urunc-dev/urunc/commit/" + ref
        facts.append(fact)
    facts.append({"label": "level", "value": level})
    if meta.get("run_url"):
        facts.append({"label": "workflow run", "value": meta.get("run_id", "run"), "url": meta["run_url"]})
    if base and base.get("source_url"):
        facts.append({"label": "baseline", "value": "previous scheduled run", "url": base["source_url"]})

    metrics = []
    base_values = {}
    if base:
        for m in (base.get("timings") or {}).get("metrics", []):
            pair = (m.get("values") or {}).get(HOST)
            if isinstance(pair, list) and len(pair) == 2:
                base_values[m.get("name")] = pair[1]
    for name in METRICS:
        cur = median(samples.get(name, []))
        if cur is None:
            continue
        note = notes.get(name) or "median of %d" % len(samples[name])
        metrics.append({"name": name, "note": note, "values": {HOST: [base_values.get(name), cur]}})

    data = {
        "schema": 1,
        "title": "brig e2e %s" % level,
        "eyebrow": "brig · e2e · %s" % level,
        "headline": headline,
        "summary": ("The %s run of script/e2e/canary-linux.sh on a GitHub-hosted ubuntu-24.04 runner. "
                    "It installs brig with this commit's install.sh as a rootless user, swaps in brig "
                    "and brigd built from %s, and boots real microVMs under nested KVM.") % (level, short),
        "generated": now.isoformat().replace("+00:00", "Z"),
        "source": meta.get("source", "canary-linux.sh, run by hand"),
        "verdict": {
            "status": "go" if go else "no-go",
            "label": "Go" if go else "No-go",
            "text": text,
        },
        "facts": facts,
        "runs": runs,
        "hosts": [{
            "id": HOST,
            "short": "ubuntu-24.04",
            "label": "GitHub-hosted ubuntu-24.04, x64",
            "detail": meta.get("host_detail", ""),
        }],
        "headings": {
            "blockers": ["Gates", "One gate per past release blocker", "Gate"],
            "checks": ["Checks", "Every check, with its evidence"],
        },
        "blockers": blockers,
        "checks": {"run": "now", "hosts": {HOST: checks}},
    }
    if meta.get("run_url"):
        data["source_url"] = meta["run_url"]
    if metrics:
        data["timings"] = {
            "unit": "s",
            "baseline": "base" if base else None,
            "current": "now",
            # Nested KVM on a shared runner is noisy, so only a large change is flagged.
            "flag_percent": 50,
            "metrics": metrics,
        }

    problems = check_schema(data)
    if problems:
        for p in problems:
            print("results.py: " + p, file=sys.stderr)
        sys.exit(1)
    with open(out, "w", encoding="utf-8") as fh:
        json.dump(data, fh, indent=2, ensure_ascii=False)
        fh.write("\n")
    print(data["verdict"]["status"])


def check_schema(data):
    """Returns what render-report.py would refuse in data."""
    import importlib.util
    spec = importlib.util.spec_from_file_location("render_report", os.path.join(HERE, "render-report.py"))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod.check(data)


def field(path, keys):
    with (sys.stdin if path == "-" else open(path, encoding="utf-8")) as fh:
        value = json.load(fh)
    for k in keys.split("."):
        value = value[k]
    return value


def main(argv):
    if len(argv) < 2:
        sys.exit(__doc__)
    cmd, args = argv[1], argv[2:]
    if cmd == "gate" and len(args) == 3:
        if args[0] not in {g["id"] for g in GATES}:
            sys.exit("results.py: unknown gate %s" % args[0])
        if args[1] not in STATUSES:
            sys.exit("results.py: bad status %s" % args[1])
        append({"kind": "gate", "id": args[0], "status": args[1], "note": args[2]})
    elif cmd == "check" and len(args) == 4:
        if args[2] not in STATUSES:
            sys.exit("results.py: bad status %s" % args[2])
        append({"kind": "check", "group": args[0], "name": args[1], "status": args[2], "evidence": args[3]})
    elif cmd == "meta" and len(args) == 2:
        append({"kind": "meta", "key": args[0], "value": args[1]})
    elif cmd == "sample" and len(args) == 2:
        append({"kind": "sample", "metric": args[0], "seconds": float(args[1])})
    elif cmd == "note" and len(args) == 2:
        append({"kind": "note", "metric": args[0], "text": args[1]})
    elif cmd == "build" and args:
        baseline = None
        if args[0] == "--baseline" and len(args) == 3:
            baseline, args = args[1], args[2:]
        if len(args) != 1:
            sys.exit(__doc__)
        build(args[0], baseline)
    elif cmd == "median" and len(args) == 1:
        with open(args[0], encoding="utf-8") as fh:
            values = [float(x) for x in fh.read().split()]
        m = median(values)
        print("" if m is None else "%.2f" % m)
    elif cmd == "at-least" and len(args) == 2:
        have, floor = version_key(args[0]), version_key(args[1])
        if have is None or floor is None:
            return 2
        return 0 if have >= floor else 1
    elif cmd == "field" and len(args) == 2:
        value = field(args[0], args[1])
        print(value if not isinstance(value, (dict, list)) else json.dumps(value))
    else:
        sys.exit(__doc__)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
