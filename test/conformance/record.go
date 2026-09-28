package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var versionRE = regexp.MustCompile(`^v?[0-9]+(\.[0-9A-Za-z-]+)+$`)

// versionOf picks the version out of a runtime's --version line: the first
// word after the name that reads as one. hull prints "hull v0.1.0-rc29
// (...)". The record's file name is built from it, so nothing else is let in.
func versionOf(line string) string {
	f := strings.Fields(line)
	for i := 1; i < len(f); i++ {
		if versionRE.MatchString(f[i]) {
			return f[i]
		}
	}
	return ""
}

// recordPath is where the record for a backend and runtime version lives.
// One file per pair, so a new runtime version gets a new record and the old
// one stays as the evidence for its release.
func recordPath(backend, runtimeLine string) string {
	v := versionOf(runtimeLine)
	if v == "" {
		return ""
	}
	return fmt.Sprintf("docs/manual-tests/egress-conformance-%s-%s.md", backend, v)
}

func (r *report) counts() string {
	n := map[verdict]int{}
	for _, row := range r.rows {
		n[row.verdict]++
	}
	var parts []string
	for _, v := range []verdict{pass, knownGap, noRouteVerdict, unproven, fail} {
		if n[v] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n[v], v))
		}
	}
	return strings.Join(parts, ", ")
}

// scrub takes the host's address out of a line, so a record says where the
// host service was without naming the network it ran on. The address goes on
// its own too, since curl writes "HOST port N".
func (r *report) scrub(s string) string {
	// The suite's own paths are temporary and name the host's user, so a
	// record carries neither.
	if r.brigBin != "" {
		s = strings.ReplaceAll(s, r.brigBin, "brig")
	}
	if r.root != "" {
		s = strings.ReplaceAll(s, r.root, "{root}")
	}
	if r.svc != "" {
		s = strings.ReplaceAll(s, r.svc, hostSvc)
	}
	if r.host != "" {
		s = strings.ReplaceAll(s, r.host, "{host}")
	}
	return s
}

func cell(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}

func (o observation) short() string {
	switch {
	case o.invalid != "":
		return "nothing measured"
	case o.outcome == "":
		return "not run"
	}
	return string(o.outcome)
}

// full is the observation as the record's console block shows it. What the
// guest printed stays next to why it measured nothing, since it often holds
// the reason.
func (o observation) full() string {
	switch {
	case o.invalid == "":
		return o.line
	case o.line != "":
		return "nothing measured: " + o.invalid + "\n" + o.line
	}
	return "nothing measured: " + o.invalid
}

func bootName(p string) string {
	if p == noPolicy {
		return "no policy"
	}
	return p
}

// markdown is the record of one run, written for docs/manual-tests/.
func (r *report) markdown() string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	result := "PASS"
	if r.failed() {
		result = "FAIL"
	}
	w("# Egress conformance: %s, %s", r.backend, orNone(versionOf(r.runtime)))
	w("")
	w("Written by `script/egress-conformance.sh`. To change it, run the suite again.")
	w("")
	w("| | |")
	w("| --- | --- |")
	w("| backend | `%s` |", r.backend)
	w("| runtime | `%s` |", orNone(r.runtime))
	w("| brig | `%s` |", orNone(r.brig))
	w("| date | %s |", r.date.UTC().Format("2006-01-02 15:04 UTC"))
	w("| image | `%s` |", r.image)
	if d := r.digests(); d != "" {
		w("| image digest | %s |", d)
	}
	w("| hvi | `%s` |", orNone(r.hvi))
	w("| boot assets | `%s` |", orNone(r.assets))
	if c := r.counts(); c != "" {
		w("| result | **%s**: %s |", result, c)
	} else {
		w("| result | **%s** |", result)
	}
	w("")
	if len(r.problems) > 0 {
		w("## Why the run failed")
		w("")
		for _, p := range r.problems {
			w("- %s", r.scrub(p))
		}
		w("")
	}
	if len(r.rows) > 0 {
		w("## Cases")
		w("")
		w("A denied case passes only on the failure its policy causes. The column")
		w("with no policy is the same command on a guest with no policy. A denied")
		w("case is unproven when that guest did not get to the target either, so")
		w("the run says nothing about whether the policy blocks it. Only a case")
		w("that says why its target may be shut can be unproven. Any other fails.")
		w("")
		w("| case | what the guest tried | expected | with the policy | with no policy | verdict |")
		w("| --- | --- | --- | --- | --- | --- |")
		for _, row := range r.rows {
			want := row.c.want.String()
			if row.c.want == expectDeny {
				want = string(row.c.mode)
			}
			v := string(row.verdict)
			if row.why != "" {
				v += ": " + row.why
			}
			w("| `%s` | %s | %s | %s | %s | %s |", row.c.id, cell(row.c.title), want,
				row.with.short(), row.without.short(), cell(r.scrub(v)))
		}
		w("")
	}
	for _, p := range []string{noPolicy, denyPolicy, allowPolicy} {
		bt, ok := r.boots[p]
		if !ok {
			continue
		}
		w("## Boot: %s", bootName(p))
		w("")
		if bt.err != "" {
			w("The boot failed: %s", r.scrub(bt.err))
			w("")
		}
		if p != noPolicy {
			w("```yaml")
			b.WriteString(policies[p])
			w("```")
			w("")
		}
		w("- allowed controls reached their targets first and last: %s", yesNo(bt.controlsOK))
		w("- connections the host listener accepted: %d", bt.accepted)
		for _, n := range bt.notes {
			w("- %s", r.scrub(n))
		}
		w("")
		w("```console")
		for _, row := range r.rows {
			if p != noPolicy && row.c.policy != p {
				continue
			}
			o := row.with
			if p == noPolicy {
				o = row.without
			}
			w("$ %s", row.c.command())
			w("%s", r.scrub(o.full()))
		}
		w("```")
		w("")
	}
	return b.String()
}

// digests lists the image digests the boots ran, once each. Boots that ran
// different digests would show more than one.
func (r *report) digests() string {
	var seen []string
	for _, p := range []string{noPolicy, denyPolicy, allowPolicy} {
		bt, ok := r.boots[p]
		if !ok || bt.digest == "" || slices.Contains(seen, bt.digest) {
			continue
		}
		seen = append(seen, bt.digest)
	}
	for i, d := range seen {
		seen[i] = "`" + d + "`"
	}
	return strings.Join(seen, ", ")
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
