#!/usr/bin/env python3
"""Collects what the e2e scripts find and writes it as results.json.

A host script never writes JSON itself. Each result goes through one of the
record commands, which append a line to the file $E2E_RECORDS names. build
reads those lines and writes one host's results.json in schema 1, the one
render-report.py renders. merge-results.py joins several of them.

    results.py host ID
    results.py meta KEY VALUE
    results.py fact LABEL VALUE [URL]
    results.py gate ID STATUS NOTE
    results.py check GROUP NAME STATUS EVIDENCE
    results.py sample METRIC SECONDS
    results.py note METRIC TEXT
    results.py build [--baseline FILE] OUT
    results.py at-least VERSION FLOOR
    results.py field FILE KEY[.KEY...]

build prints the verdict, go or no-go. at-least exits 0 when VERSION is
FLOOR or newer, 1 when it is older, and 2 when either is not a vX.Y.Z or
vX.Y.Z-rcN tag. field reads FILE, or stdin for -.
"""

import datetime
import importlib.util
import json
import os
import re
import statistics
import sys

HERE = os.path.dirname(os.path.abspath(__file__))

STATUSES = {"pass", "fail", "expected", "warn", "skip", "na", "running",
            "fixed", "review", "progress", "open", "filed"}

# A gate that fails as expected, such as --cpus on a bundle older than its
# fix, does not stop a go. Neither does one a host skips or has no use for.
GO_STATUSES = {"pass", "expected", "skip", "na"}

# The hosts the workflow runs, by the prefix of their id: linux, and
# linux-rc10 for a leg with another runtime bundle. os decides which gates
# apply to a host.
HOSTS = {
    "linux": {"short": "Linux", "label": "GitHub-hosted ubuntu-24.04, x64", "os": "linux"},
    "mac": {"short": "macOS", "label": "Self-hosted macOS, arm64, SIP enabled", "os": "macos"},
    "tap": {"short": "Homebrew tap", "label": "GitHub-hosted macos-15, Homebrew only", "os": "none"},
}
NOT_HERE = {
    "linux": "Linux only.",
    "macos": "macOS only.",
}


def link(text, url):
    return {"text": text, "url": url}


BRIG = "https://github.com/brig-sh/brig"
BUNDLE = "https://github.com/NOFireAI/brig-standalone-linux"

# One gate per release blocker the v0.3.0 stress runs found, in the order
# the scripts run them.
GATES = [
    {
        "id": "home",
        "os": "linux",
        "name": "A forced image pull with DOCKER_CONFIG unset",
        "detail": "brig put the guest's HOME into the runtime's own environment. "
                  "Rootless nerdctl then read /root/.docker/config.json, and every image pull failed.",
        "fix": [link("#337", BRIG + "/issues/337"), link("#343", BRIG + "/pull/343")],
    },
    {
        "id": "exec",
        "os": "linux",
        "name": "Short guest commands return their output",
        "detail": "The runtime's in-guest agent reported a command's exit before its output was "
                  "drained, so about 15 in 100 short commands came back empty with exit 0.",
        "fix": [link("bundle #10", BUNDLE + "/pull/10"), link("#356", BRIG + "/pull/356")],
    },
    {
        "id": "claude",
        "os": "linux",
        "name": "claude-code boots without a .claude refusal",
        "detail": "brig reads a few answers from the guest before it hands claude-code a credential. "
                  "A lost answer read as a .claude mount that was not ephemeral, so brig refused.",
        "fix": [link("bundle rc9", BUNDLE + "/releases/tag/v0.1.0-rc9"), link("#354", BRIG + "/pull/354")],
    },
    {
        "id": "nosudo",
        "os": "linux",
        "name": "A no-sudo user install stops before the download",
        "detail": "A user without sudo unpacked the whole bundle and then waited at a sudo prompt. "
                  "The installer should stop first and print the commands for root.",
        "fix": [link("bundle #7", BUNDLE + "/pull/7")],
    },
    {
        "id": "cpus",
        "os": "linux",
        "name": "--cpus sizes the guest",
        "detail": "Bundle rc9 boots every guest with one vCPU, whatever the profile or --cpus asks. "
                  "The fix ships in bundle rc10.",
        "fix": [link("bundle #11", BUNDLE + "/pull/11")],
    },
    {
        "id": "gatekeeper",
        "os": "macos",
        "name": "A quarantined channel build answers at once",
        "detail": "An ad-hoc signed brig@main waited silently for a Gatekeeper prompt that a "
                  "headless session cannot show. Channel builds are notarized now.",
        "fix": [link("#331", BRIG + "/pull/331")],
    },
]
GATE_IDS = {g["id"] for g in GATES}

# The timings the report shows, in this order, when a run measured them.
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


def read_records(path):
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


def known_host(host_id):
    """Returns the defaults for a host id, matched on its prefix."""
    for prefix, known in HOSTS.items():
        if host_id == prefix or host_id.startswith(prefix + "-"):
            return known
    return {"short": host_id, "label": host_id, "os": "none"}


def host_entry(host_id, meta):
    known = known_host(host_id)
    return {
        "id": host_id,
        "short": meta.get("host_short", known["short"]),
        "label": meta.get("host_label", known["label"]),
        "detail": meta.get("host_detail", ""),
    }


def host_os(host_id, meta):
    return meta.get("host_os", known_host(host_id)["os"])


def gate_for_host(gate, os_name, recorded):
    """Returns the result a host gets for a gate: what it recorded, n/a, or a failure."""
    if gate["os"] != os_name:
        note = NOT_HERE.get(gate["os"], "Not on this host.")
        if os_name == "none":
            note = "This job boots no VM."
        return {"status": "na", "note": note}
    if gate["id"] in recorded:
        return recorded[gate["id"]]
    return {"status": "fail", "note": "Did not run: the block stopped before it recorded this gate. See the logs."}


def build_host(records):
    """Returns one host's results, in schema 1, from its records."""
    meta, facts, gates, checks, samples, notes = {}, [], {}, [], {}, {}
    for r in records:
        kind = r.get("kind")
        if kind == "meta":
            meta[r["key"]] = r["value"]
        elif kind == "fact":
            facts.append({k: v for k, v in (("label", r["label"]), ("value", r["value"]),
                                            ("url", r.get("url"))) if v})
        elif kind == "gate":
            gates[r["id"]] = {"status": r["status"], "note": r["note"]}
        elif kind == "check":
            checks.append({k: r[k] for k in ("group", "name", "status", "evidence")})
        elif kind == "sample":
            samples.setdefault(r["metric"], []).append(r["seconds"])
        elif kind == "note":
            notes[r["metric"]] = r["text"]

    host_id = meta.get("host", "linux")
    os_name = host_os(host_id, meta)
    level = meta.get("level", "canary")
    commit = meta.get("commit", "")
    short = commit[:7] or "unknown"
    now = datetime.datetime.now(datetime.timezone.utc).replace(microsecond=0)

    head = []
    if commit:
        head.append({"label": "brig", "value": short, "url": "%s/commit/%s" % (BRIG, commit)})
    head.append({"label": "level", "value": level})
    if meta.get("run_url"):
        head.append({"label": "workflow run", "value": meta.get("run_id", "run"), "url": meta["run_url"]})

    metrics = []
    for name in METRICS:
        cur = median(samples.get(name, []))
        if cur is not None:
            note = notes.get(name) or "median of %d" % len(samples[name])
            metrics.append({"name": name, "note": note, "values": {host_id: [None, cur]}})

    data = {
        "schema": 1,
        "title": "brig e2e %s" % level,
        "eyebrow": "brig · e2e · %s" % level,
        "level": level,
        "commit": commit,
        "generated": now.isoformat().replace("+00:00", "Z"),
        "source": meta.get("source", "script/e2e, run by hand"),
        "facts": head + facts,
        "runs": [{
            "id": "now",
            "label": "This run",
            "date": now.strftime("%Y-%m-%d"),
            "target": meta.get("target", "brig %s" % short),
            "hosts": [host_id],
            "note": meta.get("run_note", ""),
        }],
        "hosts": [host_entry(host_id, meta)],
        "headings": {
            "blockers": ["Gates", "One gate per past release blocker", "Gate"],
            "checks": ["Checks", "Every check, with its evidence"],
        },
        "blockers": [dict(g, results={"now": {host_id: gate_for_host(g, os_name, gates)}})
                     for g in GATES],
        "checks": {"run": "now", "hosts": {host_id: checks}},
    }
    if meta.get("run_url"):
        data["source_url"] = meta["run_url"]
    if metrics:
        data["timings"] = {
            "unit": "s",
            "baseline": None,
            "current": "now",
            # Nested KVM and shared runners are noisy, so only a large change is flagged.
            "flag_percent": 50,
            "metrics": metrics,
        }
    return data


def load(path):
    """Returns a results.json in schema 1, or None."""
    if not path or not os.path.exists(path):
        return None
    try:
        with open(path, encoding="utf-8") as fh:
            data = json.load(fh)
    except (OSError, ValueError) as err:
        print("results.py: ignoring %s: %s" % (path, err), file=sys.stderr)
        return None
    return data if data.get("schema") == 1 else None


def apply_baseline(data, base):
    """Adds an earlier run's results as the "base" run, for every host both have."""
    if not base:
        return
    base_run = (base.get("timings") or {}).get("current") or (base.get("checks") or {}).get("run")
    brun = next((r for r in base.get("runs", []) if r.get("id") == base_run), {})
    hosts = [h for h in data["runs"][-1]["hosts"] if h in brun.get("hosts", [])]
    if not hosts:
        return
    data["runs"].insert(0, {
        "id": "base",
        "label": "Baseline",
        "date": brun.get("date", ""),
        "target": brun.get("target", ""),
        "hosts": hosts,
        "note": "the last scheduled run that passed (%s)." % (base.get("source") or "no source"),
    })
    for b in data["blockers"]:
        prev = next((x for x in base.get("blockers", []) if x.get("id") == b["id"]), None)
        cells = ((prev or {}).get("results") or {}).get(base_run) or {}
        kept = {h: cells[h] for h in hosts if h in cells}
        if kept:
            b["results"]["base"] = kept
    base_values = {}
    for m in (base.get("timings") or {}).get("metrics", []):
        for h, pair in (m.get("values") or {}).items():
            if isinstance(pair, list) and len(pair) == 2:
                base_values[(m.get("name"), h)] = pair[1]
    timings = data.get("timings")
    if timings:
        timings["baseline"] = "base"
        for m in timings["metrics"]:
            for h, pair in m["values"].items():
                pair[0] = base_values.get((m["name"], h))
    if base.get("source_url"):
        data["facts"].append({"label": "baseline", "value": "previous scheduled run", "url": base["source_url"]})


def finish(data):
    """Sets the verdict, headline and summary over every host, checks the schema, and returns go or no-go."""
    host_ids = data["runs"][-1]["hosts"]
    shorts = {h["id"]: h["short"] for h in data["hosts"]}
    failed, expected, skipped = [], [], []
    for b in data["blockers"]:
        for h in host_ids:
            status = (b["results"].get("now") or {}).get(h, {}).get("status")
            where = "%s on %s" % (b["name"], shorts.get(h, h))
            if status not in GO_STATUSES:
                failed.append(where)
            elif status == "expected":
                expected.append(where)
            elif status == "skip":
                skipped.append(where)
    # A host with no results of its own, such as a crashed job, is a no-go even
    # when no gate applies to it.
    for h in data.get("missing_hosts", []):
        failed.append("no results from %s" % shorts.get(h, h))
    bad_checks = sum(1 for rows in data["checks"]["hosts"].values() for c in rows if c["status"] == "fail")
    all_checks = sum(len(rows) for rows in data["checks"]["hosts"].values())
    short = (data.get("commit") or "")[:7] or "unknown"
    go = not failed

    if go:
        data["headline"] = "No gate fails on brig %s" % short
        text = "No gate fails."
    else:
        data["headline"] = "%d gate %s fail on brig %s" % (
            len(failed), "result" if len(failed) == 1 else "results", short)
        text = "Failing, or did not run: %s." % "; ".join(failed)
    if expected:
        text += " Expected to fail: %s." % "; ".join(expected)
    if skipped:
        text += " Skipped: %s." % "; ".join(skipped)
    if bad_checks:
        text += " %d of %d checks %s." % (bad_checks, all_checks, "fails" if bad_checks == 1 else "fail")
    else:
        text += " No check fails."
    data["verdict"] = {"status": "go" if go else "no-go", "label": "Go" if go else "No-go", "text": text}
    names = [h["short"] for h in data["hosts"]]
    hosts = names[0] if len(names) == 1 else ", ".join(names[:-1]) + " and " + names[-1]
    data["summary"] = ("The %s run of the real-runtime checks in script/e2e/, against brig %s, on %s. "
                       "The Linux and macOS jobs install brig the way a user does and boot real microVMs."
                       % (data.get("level", "canary"), short, hosts))
    problems = check_schema(data)
    if problems:
        for p in problems:
            print("results.py: " + p, file=sys.stderr)
        sys.exit(1)
    return data["verdict"]["status"]


def check_schema(data):
    """Returns what render-report.py would refuse in data."""
    spec = importlib.util.spec_from_file_location("render_report", os.path.join(HERE, "render-report.py"))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod.check(data)


def write(data, out):
    with open(out, "w", encoding="utf-8") as fh:
        json.dump(data, fh, indent=2, ensure_ascii=False)
        fh.write("\n")


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
    if cmd == "host" and len(args) == 1:
        append({"kind": "meta", "key": "host", "value": args[0]})
    elif cmd == "meta" and len(args) == 2:
        append({"kind": "meta", "key": args[0], "value": args[1]})
    elif cmd == "fact" and len(args) in (2, 3):
        append({"kind": "fact", "label": args[0], "value": args[1], "url": args[2] if len(args) == 3 else ""})
    elif cmd == "gate" and len(args) == 3:
        if args[0] not in GATE_IDS:
            sys.exit("results.py: unknown gate %s" % args[0])
        if args[1] not in STATUSES:
            sys.exit("results.py: bad status %s" % args[1])
        append({"kind": "gate", "id": args[0], "status": args[1], "note": args[2]})
    elif cmd == "check" and len(args) == 4:
        if args[2] not in STATUSES:
            sys.exit("results.py: bad status %s" % args[2])
        append({"kind": "check", "group": args[0], "name": args[1], "status": args[2], "evidence": args[3]})
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
        data = build_host(read_records(records_path()))
        apply_baseline(data, load(baseline))
        verdict = finish(data)
        write(data, args[0])
        print(verdict)
    elif cmd == "at-least" and len(args) == 2:
        have, floor = version_key(args[0]), version_key(args[1])
        if have is None or floor is None:
            return 2
        return 0 if have >= floor else 1
    elif cmd == "field" and len(args) == 2:
        value = field(args[0], args[1])
        print(json.dumps(value) if isinstance(value, (dict, list, bool)) or value is None else value)
    else:
        sys.exit(__doc__)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
