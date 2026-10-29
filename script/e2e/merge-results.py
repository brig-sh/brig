#!/usr/bin/env python3
"""Merges one results.json per host into the results.json of the whole run.

    merge-results.py [--baseline FILE] [--expect linux,mac,tap] --out OUT results-*.json

The hosts are concatenated, and the run lists every host id. Gates and checks
stay keyed per host, so the page shows a column and a tab for each. A host
named in --expect whose file is missing, because its job crashed or never
uploaded, fails every gate that applies to it. It never shows as green.

The verdict is no-go when any gate fails on any host. The script prints it,
and exits 0 either way once OUT is written.
"""

import argparse
import importlib.util
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))


def load_results_module():
    spec = importlib.util.spec_from_file_location("results", os.path.join(HERE, "results.py"))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


R = load_results_module()


def missing_host(host_id, level, commit):
    """Returns the results of a host that uploaded none, so that it fails."""
    records = [
        {"kind": "meta", "key": "host", "value": host_id},
        {"kind": "meta", "key": "level", "value": level},
        {"kind": "meta", "key": "commit", "value": commit},
        {"kind": "meta", "key": "host_detail", "value": "This job uploaded no results.json. Its own log says why."},
        {"kind": "check", "group": "Harness", "name": "The job uploaded its results", "status": "fail",
         "evidence": "no results-%s.json among the downloaded artifacts" % host_id},
    ]
    data = R.build_host(records)
    for b in data["blockers"]:
        cell = b["results"]["now"][host_id]
        if cell["status"] == "fail":
            cell["note"] = "No results from this host."
    return data


def merge(parts):
    """Returns the results of every host in parts, as one run."""
    first = parts[0]
    data = {k: v for k, v in first.items() if k not in ("hosts", "facts", "runs", "blockers", "checks", "timings")}
    data["hosts"] = []
    data["facts"] = []
    data["blockers"] = [{k: v for k, v in g.items()} for g in R.GATES]
    for b in data["blockers"]:
        b["results"] = {"now": {}}
    data["checks"] = {"run": "now", "hosts": {}}
    run = {"id": "now", "label": "This run", "date": first["runs"][-1]["date"], "target": "", "hosts": [], "note": ""}
    targets, notes, metrics, labels = [], [], {}, set()
    for part in parts:
        now = next(r for r in part["runs"] if r["id"] == "now")
        for h in part["hosts"]:
            data["hosts"].append(h)
            run["hosts"].append(h["id"])
            short = h["short"]
            note = now.get("note", "").rstrip(".")
            prefix = "LEVEL=%s: " % part.get("level", "canary")
            if note.startswith(prefix):
                note = note[len(prefix):]
            if note:
                notes.append("%s: %s" % (short, note))
            for piece in (now.get("target") or "").split(" · "):
                if piece and piece not in targets:
                    targets.append(piece)
            cells = {b["id"]: b["results"]["now"].get(h["id"]) for b in part["blockers"]}
            for b in data["blockers"]:
                cell = cells.get(b["id"]) or R.gate_for_host(b, R.known_host(h["id"])["os"], {})
                b["results"]["now"][h["id"]] = cell
            data["checks"]["hosts"][h["id"]] = part["checks"]["hosts"].get(h["id"], [])
        for f in part["facts"]:
            if f["label"] not in labels:
                labels.add(f["label"])
                data["facts"].append(f)
        for m in (part.get("timings") or {}).get("metrics", []):
            merged = metrics.setdefault(m["name"], {"name": m["name"], "note": m.get("note", ""), "values": {}})
            merged["values"].update(m["values"])
    run["target"] = " · ".join(targets)
    run["note"] = "LEVEL=%s. %s" % (first.get("level", "canary"), "; ".join(notes) + ("." if notes else ""))
    data["runs"] = [run]
    if metrics:
        data["timings"] = {
            "unit": "s",
            "baseline": None,
            "current": "now",
            "flag_percent": 50,
            "metrics": [metrics[n] for n in R.METRICS if n in metrics],
        }
    return data


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("results", nargs="*", help="one results.json per host")
    ap.add_argument("--out", required=True, help="where to write the merged results.json")
    ap.add_argument("--baseline", help="an earlier merged results.json to compare with")
    ap.add_argument("--expect", default="", help="comma-separated host ids that should have results")
    args = ap.parse_args()

    parts = []
    for path in args.results:
        data = R.load(path)
        if data is None:
            print("merge-results: skipping %s, which is not a results.json" % path, file=sys.stderr)
            continue
        parts.append(data)
    seen = {h["id"] for p in parts for h in p["hosts"]}
    level = parts[0].get("level", "canary") if parts else "canary"
    commit = next((p.get("commit") for p in parts if p.get("commit")), "")
    missing = []
    for host_id in [h for h in args.expect.split(",") if h]:
        if host_id not in seen:
            print("merge-results: no results from %s" % host_id, file=sys.stderr)
            parts.append(missing_host(host_id, level, commit))
            missing.append(host_id)
    if not parts:
        sys.exit("merge-results: no results to merge")

    data = merge(parts)
    data["commit"] = commit
    data["missing_hosts"] = missing
    R.apply_baseline(data, R.load(args.baseline))
    verdict = R.finish(data)
    R.write(data, args.out)
    print(verdict)
    return 0


if __name__ == "__main__":
    sys.exit(main())
