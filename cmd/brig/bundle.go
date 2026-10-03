package main

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brig-sh/brig/internal/buildinfo"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/redact"
)

// brig doctor bundle zips the doctor report, host, runtime, sandboxes, user
// profiles and failed-boot logs for a public issue. Identifying values become
// placeholders; see internal/redact.
//
// Each collector runs under a timeout and records its own failure in the
// manifest; the zip is written regardless.

// bundleOptions is one invocation's request.
type bundleOptions struct {
	agent       string
	out         string
	includeLogs bool
}

func parseBundleArgs(args []string) (bundleOptions, error) {
	var o bundleOptions
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--include-logs":
			o.includeLogs = true
		case a == "-o" || a == "--output":
			if i+1 >= len(args) {
				return o, usagef("%s needs a path", a)
			}
			i++
			o.out = args[i]
		case strings.HasPrefix(a, "--output="):
			o.out = strings.TrimPrefix(a, "--output=")
		case a == "--json":
			return o, usagef("`brig doctor bundle` writes a zip and has no --json form")
		case strings.HasPrefix(a, "-"):
			return o, usagef("unknown flag %q for `brig doctor bundle` "+
				"(it takes an optional agent, -o <path> and --include-logs)", a)
		default:
			if o.agent != "" {
				return o, usagef("`brig doctor bundle` takes one agent, not both %q and %q", o.agent, a)
			}
			o.agent = a
		}
	}
	return o, nil
}

// rawEntry is a collector's output before the scrub.
type rawEntry struct{ name, body string }

// collector gathers one part of the bundle. It registers identifying values
// with r (r.Var, r.Image, r.Workspace) and returns raw text; the scrub runs
// after all collectors.
type collector struct {
	name string
	// timeout overrides bundleCollectorTimeout, for the log reads.
	timeout time.Duration
	run     func(r *redact.Redactor) ([]rawEntry, error)
}

// skipped is a collector declining to run, with the reason the manifest gives.
type skipped string

func (s skipped) Error() string { return string(s) }

type collectorStatus struct{ name, result string }

var (
	bundleCollectorTimeout = 10 * time.Second
	bundleBudget           = 60 * time.Second
	bundleNow              = time.Now
	// bundleCheck is the final check; a test seam to force a leak.
	bundleCheck = (*redact.Redactor).Check
)

// runCollectors runs each collector under its timeout and the run's budget. A
// collector that times out is abandoned, not killed (runtime.Runtime takes no
// context), and may keep registering values: the redactor is safe for
// concurrent use and finishBundle checks twice. Partial entries from a failed
// or skipped collector are kept and scrubbed like the rest.
func runCollectors(cs []collector, r *redact.Redactor) ([]rawEntry, []collectorStatus) {
	deadline := time.Now().Add(bundleBudget)
	var entries []rawEntry
	var statuses []collectorStatus
	for _, c := range cs {
		left := time.Until(deadline)
		if left <= 0 {
			statuses = append(statuses, collectorStatus{c.name, "skipped: run budget spent"})
			continue
		}
		timeout := c.timeout
		if timeout == 0 {
			timeout = bundleCollectorTimeout
		}
		timeout = min(timeout, left)
		type result struct {
			entries []rawEntry
			err     error
		}
		done := make(chan result, 1)
		go func() {
			defer func() {
				if p := recover(); p != nil {
					done <- result{err: fmt.Errorf("panicked: %v", p)}
				}
			}()
			es, err := c.run(r)
			done <- result{es, err}
		}()
		record := func(res result) {
			var sk skipped
			switch {
			case errors.As(res.err, &sk):
				statuses = append(statuses, collectorStatus{c.name, "skipped: " + string(sk)})
			case res.err != nil:
				statuses = append(statuses, collectorStatus{c.name, "failed: " + oneLine(res.err.Error())})
			default:
				statuses = append(statuses, collectorStatus{c.name, "ok"})
			}
			entries = append(entries, res.entries...)
		}
		select {
		case res := <-done:
			record(res)
		case <-time.After(timeout):
			// A result that lands on the deadline counts as a finish:
			// select picks at random when both are ready.
			select {
			case res := <-done:
				record(res)
			default:
				statuses = append(statuses, collectorStatus{c.name, "timed out after " + timeout.Round(time.Millisecond).String()})
			}
		}
	}
	return entries, statuses
}

// finishBundle scrubs every entry, puts the manifest first, and checks the
// result. A leak is scrubbed once more, since an abandoned collector may have
// registered a value late; a second leak refuses the bundle.
func finishBundle(r *redact.Redactor, raw []rawEntry, statuses []collectorStatus, now time.Time) ([]bundleEntry, error) {
	entries := make([]bundleEntry, 0, len(raw)+1)
	for _, e := range raw {
		entries = append(entries, bundleEntry{r.Text(e.name), r.Text(e.body)})
	}
	// Scrubbed before Kinds is read: a status carries error text, and
	// its values must count in the manifest's replaced line.
	scrubbed := make([]collectorStatus, len(statuses))
	for i, s := range statuses {
		scrubbed[i] = collectorStatus{r.Text(s.name).String(), r.Text(s.result).String()}
	}
	manifest := renderManifest(entries, scrubbed, r.Kinds(), now)
	entries = append([]bundleEntry{{r.Text("MANIFEST.txt"), r.Text(manifest)}}, entries...)
	for attempt := 0; ; attempt++ {
		err := checkEntries(r, entries)
		if err == nil {
			return entries, nil
		}
		if attempt == 1 {
			return nil, err
		}
		for i, e := range entries {
			entries[i] = bundleEntry{r.Text(e.name.String()), r.Text(e.body.String())}
		}
	}
}

func checkEntries(r *redact.Redactor, entries []bundleEntry) error {
	for _, e := range entries {
		if err := bundleCheck(r, []byte(e.name.String())); err != nil {
			return err
		}
		if err := bundleCheck(r, []byte(e.body.String())); err != nil {
			return err
		}
	}
	return nil
}

const manifestPreamble = `This bundle is meant to be attached to a public issue.

Identifying values are replaced with placeholders. A numbered one, such as
<workspace-1>, means the same value everywhere in this bundle and nothing
outside it; a fixed one, such as <mac> or <redacted:aws>, names only the kind
of value. No map back to the original values is kept.

Structured files are redacted field by field. Doctor's findings, logs, other
command output, the collector lines below and environment.json's runtimeError
are free text, redacted best-effort: registered values and common token
shapes are removed, but a secret in a format brig does not recognise, or a
path outside $HOME quoted by a runtime error, can survive. Read doctor.json,
any log, the collector lines and runtimeError before attaching them.
`

func renderManifest(entries []bundleEntry, statuses []collectorStatus, kinds []string, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "brig diagnostics bundle\ncreated: %s\nbrig: %s\n\n", now.UTC().Format(time.RFC3339), buildinfo.Read())
	b.WriteString(manifestPreamble)
	b.WriteString("\nfiles:\n  MANIFEST.txt\n")
	logs := false
	for _, e := range entries {
		fmt.Fprintf(&b, "  %s\n", e.name)
		logs = logs || strings.HasPrefix(e.name.String(), "logs/")
	}
	if logs {
		// Says the tail cut is brig's, not the runtime's.
		fmt.Fprintf(&b, "\nlogs: each is capped at its last %d lines\n", bundleLogTail)
	}
	b.WriteString("\ncollectors:\n")
	for _, s := range statuses {
		fmt.Fprintf(&b, "  %-12s %s\n", s.name, s.result)
	}
	replaced := "nothing"
	if len(kinds) > 0 {
		replaced = kindList(kinds)
	}
	fmt.Fprintf(&b, "\nreplaced: %s\n", replaced)
	return b.String()
}

// kindList is the replaced kinds as the manifest and summary print them.
// "token:github" becomes "github token": the scrub's assignment pattern would
// redact "github" in the manifest.
func kindList(kinds []string) string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		if shape, ok := strings.CutPrefix(k, "token:"); ok {
			k = shape + " token"
		}
		out[i] = k
	}
	return strings.Join(out, ", ")
}

// bundleJSON is indented JSON without HTML escaping: encoding/json would write
// the '<' of every placeholder as <.
func bundleJSON(v any) (string, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(v)
	return b.String(), err
}

// bundleSummary is the second line of output.
func bundleSummary(entries []bundleEntry, kinds []string, o bundleOptions) string {
	logs := 0
	for _, e := range entries {
		if strings.HasPrefix(e.name.String(), "logs/") {
			logs++
		}
	}
	var logNote string
	switch {
	case o.includeLogs:
		logNote = fmt.Sprintf("%d logs (--include-logs; read them before attaching)", logs)
	case logs > 0:
		logNote = fmt.Sprintf("%d failed-boot logs", logs)
	default:
		logNote = "no logs (no failed boot recorded; --include-logs adds them)"
	}
	replaced := "nothing replaced"
	if len(kinds) > 0 {
		replaced = "replaced " + kindList(kinds)
	}
	return fmt.Sprintf("%d files; %s; %s", len(entries), replaced, logNote)
}

// bundleHost, bundleAllow and bundleCollectors are filled in by
// bundle_collect.go; package variables so a test can narrow them.
var (
	bundleHost       = hostFacts
	bundleAllow      = allowlist
	bundleCollectors = defaultBundleCollectors
)

func bundleCmd(out io.Writer, args []string) error {
	o, err := parseBundleArgs(args)
	if err != nil {
		return err
	}
	now := bundleNow()
	path, err := bundlePath(o.out, now)
	if err != nil {
		return err
	}
	// Reloaded for the profiles check, as doctorCmd does. The agent is
	// resolved first, so an unknown name is exit 3.
	loadErr := profile.Load(profile.Dir())
	var agent *profile.Profile
	if o.agent != "" {
		p, ok := profile.Lookup(o.agent)
		if !ok {
			return notFoundf("unknown profile %q. `brig agent ls` lists them", o.agent)
		}
		agent = &p
	}
	r := redact.New(bundleHost(), bundleAllow())
	registerSettingPaths(r, agent)
	raw, statuses := runCollectors(bundleCollectors(o, agent, loadErr), r)
	entries, err := finishBundle(r, raw, statuses, now)
	if err != nil {
		var leak *redact.LeakError
		if errors.As(err, &leak) {
			return fmt.Errorf("the bundle still contained a %s value, so it was not written. "+
				"This is a bug in brig; please report it without the bundle", leak.Kind)
		}
		return err
	}
	if err := writeBundle(path, entries, now); err != nil {
		return fmt.Errorf("could not write %s: %w", path, err)
	}
	fmt.Fprintln(out, path)
	fmt.Fprintln(out, bundleSummary(entries, r.Kinds(), o))
	return nil
}

// bundleEntry is one file in the zip. Both halves are Clean, so nothing reaches
// the archive without passing the redactor.
type bundleEntry struct {
	name redact.Clean
	body redact.Clean
}

// bundlePath resolves -o: empty is the current directory, a directory gets
// the default name inside it, a new path is the file. An existing file is
// refused, not overwritten.
func bundlePath(out string, now time.Time) (string, error) {
	name := "brig-diagnostics-" + now.UTC().Format("20060102T150405Z") + ".zip"
	if out == "" {
		out = "."
	}
	info, err := os.Stat(out)
	switch {
	case err == nil && info.IsDir():
		return filepath.Abs(filepath.Join(out, name))
	case err == nil:
		return "", usagef("%s already exists; name a new file or a directory", out)
	case errors.Is(err, fs.ErrNotExist) && strings.HasSuffix(out, "/"):
		return "", usagef("directory %s does not exist", out)
	case errors.Is(err, fs.ErrNotExist):
		// Checked before collection, which can take most of a minute.
		dir := filepath.Dir(out)
		switch info, err := os.Stat(dir); {
		case errors.Is(err, fs.ErrNotExist), err == nil && !info.IsDir():
			return "", usagef("directory %s does not exist", dir)
		case err != nil:
			return "", err
		}
		return filepath.Abs(out)
	default:
		return "", err
	}
}

// writeBundle writes the zip to a temporary file in the target directory and
// renames it, so an interrupted run leaves no partial bundle. CreateTemp makes
// the file 0600.
func writeBundle(path string, entries []bundleEntry, modified time.Time) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".brig-diagnostics-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	zw := zip.NewWriter(tmp)
	for _, e := range entries {
		w, err := zw.CreateHeader(&zip.FileHeader{
			Name:     e.name.String(),
			Method:   zip.Deflate,
			Modified: modified,
		})
		if err != nil {
			_ = tmp.Close()
			return err
		}
		if _, err := io.WriteString(w, e.body.String()); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := zw.Close(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// path may have appeared during collection, and Rename overwrites:
	// check again just before it.
	switch _, err := os.Lstat(path); {
	case err == nil:
		return fmt.Errorf("%s was created while the bundle was collecting; not overwriting it", path)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	return os.Rename(tmp.Name(), path)
}
