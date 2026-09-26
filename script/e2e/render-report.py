#!/usr/bin/env python3
"""Renders a results.json from the real-runtime tests into one HTML page.

The same template serves two places: a published page, which wraps the markup
in its own document (--fragment), and the page a CI job uploads, which needs a
whole document (the default). --summary writes Markdown for a CI job summary
instead: the verdict, the gates, every failing check and the timings.

    render-report.py results.json > report.html
    render-report.py --fragment results.json > report-fragment.html
    render-report.py --summary results.json >> "$GITHUB_STEP_SUMMARY"

Schema 1, as check() enforces it:

    title, eyebrow, headline, summary, generated, source, source_url
    verdict   {status: go|no-go|pending, label, text}
    facts[]   {label, value, url}
    runs[]    {id, label, date, target, hosts[], note}
    hosts[]   {id, short, label, detail}
    blockers[] {id, name, detail, fix[] {text, url},
                results {run id: {host id: {status, note}}}}
    checks    {run, hosts {host id: [{group, name, status, evidence}]}}
    timings   {unit, baseline, current, flag_percent,
               metrics[] {name, note, values {host id: [baseline, current]}}}
    findings[] {severity, title, component, status, detail, links[]}
    open[]    {title, status, detail, links[]}
    headings  {blockers: [label, title, column], checks|open: [label, title]},
              all optional

Every status is one of STATUSES below. baseline and current are run ids, or
null when there is no such run.
"""

import argparse
import html
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
TEMPLATE = os.path.join(HERE, "report-template.html")

STATUSES = {"pass", "fail", "expected", "warn", "skip", "na", "running",
            "fixed", "review", "progress", "open", "filed"}
VERDICTS = {"go", "no-go", "pending"}

# The chip labels the page shows, so the summary reads the same.
LABELS = {
    "pass": "Pass", "fail": "Fail", "expected": "Expected fail",
    "warn": "Needs a look", "skip": "Not tested", "na": "N/A",
    "running": "Running", "fixed": "Fixed", "review": "In review",
    "progress": "In progress", "open": "Not filed", "filed": "Filed",
}

# The page starts its body here. Everything above it goes in <head>.
BODY_START = '<div class="wrap" id="app">'


def check(data):
    """Returns a list of problems with data, empty when it can be rendered."""
    problems = []
    if data.get("schema") != 1:
        problems.append("schema must be 1")
    for key in ("title", "runs", "hosts"):
        if not data.get(key):
            problems.append("missing " + key)
    verdict = data.get("verdict")
    if verdict and verdict.get("status") not in VERDICTS:
        problems.append("verdict: bad status %r" % verdict.get("status"))
    hosts = {h.get("id") for h in data.get("hosts", [])}
    runs = {r.get("id") for r in data.get("runs", [])}
    for r in data.get("runs", []):
        for h in r.get("hosts", []):
            if h not in hosts:
                problems.append("run %s names unknown host %s" % (r.get("id"), h))
    for b in data.get("blockers", []):
        for run_id, per_host in b.get("results", {}).items():
            if run_id not in runs:
                problems.append("blocker %s names unknown run %s" % (b.get("id"), run_id))
            for h, res in per_host.items():
                if res.get("status") not in STATUSES:
                    problems.append("blocker %s/%s/%s: bad status %r" % (b.get("id"), run_id, h, res.get("status")))
    checks = data.get("checks") or {}
    if checks.get("run") and checks["run"] not in runs:
        problems.append("checks name unknown run %s" % checks["run"])
    for h, rows in checks.get("hosts", {}).items():
        if h not in hosts:
            problems.append("checks name unknown host %s" % h)
        for row in rows:
            if row.get("status") not in STATUSES:
                problems.append("check %s/%s: bad status %r" % (h, row.get("name"), row.get("status")))
    timings = data.get("timings") or {}
    for key in ("baseline", "current"):
        if timings.get(key) and timings[key] not in runs:
            problems.append("timings %s names unknown run %s" % (key, timings[key]))
    for m in timings.get("metrics", []):
        for h, pair in m.get("values", {}).items():
            if not isinstance(pair, list) or len(pair) != 2:
                problems.append("timing %s/%s: values must be [baseline, current]" % (m.get("name"), h))
    for f in data.get("findings", []):
        if f.get("status") not in STATUSES:
            problems.append("finding %r: bad status %r" % (f.get("title"), f.get("status")))
    for o in data.get("open", []):
        if o.get("status") not in STATUSES:
            problems.append("open item %r: bad status %r" % (o.get("title"), o.get("status")))
    return problems


def render(data, fragment):
    with open(TEMPLATE, encoding="utf-8") as fh:
        page = fh.read()
    # "</" would end the <script> block early, so it is escaped inside JSON.
    payload = json.dumps(data, ensure_ascii=False, separators=(",", ":")).replace("</", "<\\/")
    page = page.replace("__REPORT_TITLE__", html.escape(data["title"]), 1)
    page = page.replace("__REPORT_DATA__", payload, 1)
    if fragment:
        return page
    head, sep, body = page.partition(BODY_START)
    if not sep:
        raise ValueError("the template has no " + BODY_START)
    return ("<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n"
            "<meta name=\"viewport\" content=\"width=device-width, initial-scale=1, viewport-fit=cover\">\n"
            + head.rstrip() + "\n</head>\n<body>\n" + sep + body.rstrip() + "\n</body>\n</html>\n")


def md_cell(text):
    """Returns text safe for one Markdown table cell."""
    return " ".join(str(text if text is not None else "").split()).replace("|", "\\|")


def md_status(status):
    label = LABELS.get(status, status or "N/A")
    return "**%s**" % label if status == "fail" else label


def md_link(text, url):
    return "[%s](%s)" % (text, url) if url else text


def host_short(data, host_id):
    for h in data.get("hosts", []):
        if h.get("id") == host_id:
            return h.get("short") or host_id
    return host_id


def run_label(data, run_id):
    for r in data.get("runs", []):
        if r.get("id") == run_id:
            return r.get("label") or run_id
    return run_id


def summary(data):
    """Returns the Markdown a CI job writes to its step summary."""
    out = []
    verdict = data.get("verdict") or {}
    title = data.get("title", "")
    if verdict.get("label"):
        title += ": " + verdict["label"]
    out.append("## " + title)
    out.append("")
    if data.get("headline"):
        out.append("**%s**" % data["headline"])
        out.append("")
    if verdict.get("text"):
        out.append(verdict["text"])
        out.append("")
    facts = [md_link("%s `%s`" % (f.get("label"), f.get("value")), f.get("url"))
             for f in data.get("facts", [])]
    if facts:
        out.append(" · ".join(facts))
        out.append("")

    # One column per run and host, in the order the page shows them.
    cols = [(r["id"], h) for r in data.get("runs", []) for h in r.get("hosts", [])]
    blockers = data.get("blockers", [])
    if blockers and cols:
        labels = data.get("headings", {}).get("blockers") or ["", "Gates"]
        out.append("### " + labels[1])
        out.append("")
        multi = len({h for _, h in cols}) > 1
        one_run = len({r for r, _ in cols}) == 1

        def col_label(r, h):
            if multi and one_run:
                return host_short(data, h)
            if multi:
                return "%s, %s" % (run_label(data, r), host_short(data, h))
            return run_label(data, r)
        head = ["Gate"] + [col_label(r, h) for r, h in cols]
        out.append("| " + " | ".join(md_cell(c) for c in head) + " |")
        out.append("|" + "---|" * len(head))
        notes = []
        for b in blockers:
            row = [md_cell(b.get("name"))]
            for r, h in cols:
                res = (b.get("results", {}).get(r) or {}).get(h)
                if not res:
                    row.append("--")
                    continue
                row.append(md_status(res.get("status")))
                if res.get("note") and r == cols[-1][0] and res.get("status") != "na":
                    where = " (%s)" % host_short(data, h) if multi else ""
                    notes.append("- **%s**%s: %s" % (b.get("name"), where, res["note"]))
            out.append("| " + " | ".join(row) + " |")
        out.append("")
        if notes:
            out.append("Notes (%s):" % run_label(data, cols[-1][0]))
            out.append("")
            out.extend(notes)
            out.append("")

    checks = (data.get("checks") or {}).get("hosts", {})
    if checks:
        failing = [(h, row) for h, rows in checks.items() for row in rows if row.get("status") == "fail"]
        looks = [(h, row) for h, rows in checks.items() for row in rows if row.get("status") == "warn"]
        total = sum(len(rows) for rows in checks.values())

        def listing(rows):
            for h, row in rows:
                where = " (%s)" % host_short(data, h) if len(checks) > 1 else ""
                out.append("- **%s: %s**%s" % (row.get("group", ""), row.get("name"), where))
                if row.get("evidence"):
                    out.append("  ```")
                    out.extend("  " + line for line in str(row["evidence"]).splitlines())
                    out.append("  ```")

        out.append("### Failing checks")
        out.append("")
        if not failing:
            out.append("None of the %d checks fails." % total)
        else:
            out.append("%d of the %d checks %s." % (len(failing), total,
                                                    "fails" if len(failing) == 1 else "fail"))
            out.append("")
            listing(failing)
        out.append("")
        if looks:
            out.append("### Checks that need a look")
            out.append("")
            listing(looks)
            out.append("")

    timings = data.get("timings") or {}
    metrics = timings.get("metrics", [])
    if metrics:
        unit = timings.get("unit", "")
        base = run_label(data, timings["baseline"]) if timings.get("baseline") else "Baseline"
        now = run_label(data, timings["current"]) if timings.get("current") else "Now"
        out.append("### Timings")
        out.append("")
        out.append("| Metric | Host | %s | %s | Change |" % (md_cell(base), md_cell(now)))
        out.append("|---|---|---|---|---|")
        for m in metrics:
            for h, (b, n) in m.get("values", {}).items():
                change = "--"
                if b and n is not None:
                    pct = (n - b) / b * 100
                    change = "0%" if abs(pct) < 0.5 else "%+.0f%%" % pct
                cells = [md_cell(m.get("name")), md_cell(host_short(data, h)),
                         "--" if b is None else "%.2f %s" % (b, unit),
                         "--" if n is None else "%.2f %s" % (n, unit), change]
                out.append("| " + " | ".join(cells) + " |")
        notes = ["- %s: %s" % (m.get("name"), m["note"]) for m in metrics if m.get("note")]
        if notes:
            out.append("")
            out.extend(notes)
        out.append("")
    return "\n".join(out)


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("results", help="results.json to render")
    mode = ap.add_mutually_exclusive_group()
    mode.add_argument("--fragment", action="store_true",
                      help="emit the page without <html>/<head>/<body>, for a host that wraps it")
    mode.add_argument("--summary", action="store_true",
                      help="emit Markdown for a CI job summary instead of HTML")
    args = ap.parse_args()
    with open(args.results, encoding="utf-8") as fh:
        data = json.load(fh)
    problems = check(data)
    if problems:
        for p in problems:
            print("render-report: " + p, file=sys.stderr)
        return 1
    if args.summary:
        sys.stdout.write(summary(data))
    else:
        sys.stdout.write(render(data, args.fragment))
    return 0


if __name__ == "__main__":
    sys.exit(main())
