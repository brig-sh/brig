package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/exitcode"
	"github.com/brig-sh/brig/internal/redact"
)

func TestParseBundleArgs(t *testing.T) {
	cases := []struct {
		args []string
		want bundleOptions
	}{
		{nil, bundleOptions{}},
		{[]string{"claude"}, bundleOptions{agent: "claude"}},
		{[]string{"-o", "/tmp/x.zip"}, bundleOptions{out: "/tmp/x.zip"}},
		{[]string{"--output=/tmp/x.zip", "--include-logs", "codex"}, bundleOptions{agent: "codex", out: "/tmp/x.zip", includeLogs: true}},
	}
	for _, c := range cases {
		got, err := parseBundleArgs(c.args)
		if err != nil || got != c.want {
			t.Errorf("parseBundleArgs(%q) = %+v, %v; want %+v", c.args, got, err, c.want)
		}
	}
}

func TestParseBundleArgsRefuses(t *testing.T) {
	for _, args := range [][]string{
		{"-o"},
		{"--json"},
		{"--bogus"},
		{"claude", "codex"},
	} {
		_, err := parseBundleArgs(args)
		var ue *usageError
		if !errors.As(err, &ue) {
			t.Errorf("parseBundleArgs(%q) = %v, want a usage error", args, err)
		}
	}
}

// The global --json reaches doctor; the bundle has no JSON form and says so.
func TestGlobalJSONRefusesTheBundle(t *testing.T) {
	swap(t, &globalJSON, true)
	err := doctorCmd(&bytes.Buffer{}, []string{"bundle"})
	var ue *usageError
	if !errors.As(err, &ue) {
		t.Errorf("brig --json doctor bundle = %v, want a usage error", err)
	}
}

func TestBundlePath(t *testing.T) {
	now := time.Date(2026, 9, 27, 10, 4, 5, 0, time.UTC)
	dir := t.TempDir()
	t.Chdir(dir)

	got, err := bundlePath("", now)
	if want := filepath.Join(dir, "brig-diagnostics-20260927T100405Z.zip"); err != nil || got != want {
		t.Errorf("default: %q, %v; want %q", got, err, want)
	}
	got, err = bundlePath(dir, now)
	if want := filepath.Join(dir, "brig-diagnostics-20260927T100405Z.zip"); err != nil || got != want {
		t.Errorf("directory: %q, %v; want %q", got, err, want)
	}
	got, err = bundlePath("report.zip", now)
	if want := filepath.Join(dir, "report.zip"); err != nil || got != want {
		t.Errorf("file: %q, %v; want %q", got, err, want)
	}
}

// An existing file is refused and left alone.
func TestBundlePathRefusesAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "taken.zip")
	if err := os.WriteFile(path, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := bundlePath(path, time.Now())
	var ue *usageError
	if !errors.As(err, &ue) {
		t.Errorf("err = %v, want a usage error", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "mine" {
		t.Errorf("the existing file changed: %q", b)
	}
}

// A trailing slash names a directory; a missing one is refused, not created
// as a file.
func TestBundlePathRefusesAMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope") + "/"
	if _, err := bundlePath(missing, time.Now()); err == nil {
		t.Error("a missing directory was accepted")
	}
	if _, err := os.Stat(strings.TrimSuffix(missing, "/")); !os.IsNotExist(err) {
		t.Error("something was created")
	}
}

func TestWriteBundle(t *testing.T) {
	r := redact.New(redact.Host{}, redact.Allow{})
	path := filepath.Join(t.TempDir(), "b.zip")
	entries := []bundleEntry{
		{r.Text("MANIFEST.txt"), r.Text("hello\n")},
		{r.Text("doctor.json"), r.Text("{}\n")},
	}
	if err := writeBundle(path, entries, time.Now()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", info.Mode().Perm())
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if len(zr.File) != 2 || zr.File[0].Name != "MANIFEST.txt" {
		t.Errorf("entries %v", zr.File)
	}
	// No temporary file is left beside it.
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".brig-diagnostics-*"))
	if len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

// A file created at the path during collection is not clobbered by the
// rename.
func TestWriteBundleRefusesAFileCreatedWhileCollecting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "b.zip")
	if _, err := bundlePath(dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Another process creates the path.
	if err := os.WriteFile(path, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := redact.New(redact.Host{}, redact.Allow{})
	entries := []bundleEntry{{r.Text("MANIFEST.txt"), r.Text("hello\n")}}
	err := writeBundle(path, entries, time.Now())
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("err = %v, want a refusal naming %s", err, path)
	}
	if b, readErr := os.ReadFile(path); readErr != nil || string(b) != "mine" {
		t.Errorf("the existing file changed: %q, %v", b, readErr)
	}
	left, _ := filepath.Glob(filepath.Join(dir, ".brig-diagnostics-*"))
	if len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

func quickCollectors(t *testing.T) {
	swap(t, &bundleCollectorTimeout, 50*time.Millisecond)
	swap(t, &bundleBudget, 5*time.Second)
}

func TestRunCollectorsRecordsEachOutcome(t *testing.T) {
	quickCollectors(t)
	r := redact.New(redact.Host{}, redact.Allow{})
	// The prompt collectors get a long timeout so a GC pause under -race
	// cannot time them out; only "hung" hits the 50ms quickCollectors sets.
	cs := []collector{
		{name: "fine", timeout: 5 * time.Second, run: func(*redact.Redactor) ([]rawEntry, error) { return []rawEntry{{"a.txt", "a"}}, nil }},
		{name: "broken", timeout: 5 * time.Second, run: func(*redact.Redactor) ([]rawEntry, error) { return nil, errors.New("no\nway") }},
		{name: "declined", timeout: 5 * time.Second, run: func(*redact.Redactor) ([]rawEntry, error) { return nil, skipped("nothing to read") }},
		{name: "hung", run: func(*redact.Redactor) ([]rawEntry, error) { time.Sleep(time.Second); return nil, nil }},
		{name: "panicky", timeout: 5 * time.Second, run: func(*redact.Redactor) ([]rawEntry, error) { var m map[string]int; m["x"] = 1; return nil, nil }},
	}
	start := time.Now()
	entries, statuses := runCollectors(cs, r)
	if time.Since(start) > 900*time.Millisecond {
		t.Error("a hung collector held up the run")
	}
	if len(entries) != 1 || entries[0].name != "a.txt" {
		t.Errorf("entries %+v", entries)
	}
	want := map[string]string{
		"fine": "ok", "broken": "failed: no way", "declined": "skipped: nothing to read",
		"hung": "timed out after 50ms",
	}
	for _, s := range statuses {
		if w, ok := want[s.name]; ok && s.result != w {
			t.Errorf("%s: %q, want %q", s.name, s.result, w)
		}
		if s.name == "panicky" && !strings.HasPrefix(s.result, "failed: panicked") {
			t.Errorf("panicky: %q", s.result)
		}
	}
}

func TestRunCollectorsStopsAtTheBudget(t *testing.T) {
	swap(t, &bundleCollectorTimeout, 40*time.Millisecond)
	swap(t, &bundleBudget, 60*time.Millisecond)
	r := redact.New(redact.Host{}, redact.Allow{})
	slow := func(*redact.Redactor) ([]rawEntry, error) { time.Sleep(time.Second); return nil, nil }
	_, statuses := runCollectors([]collector{{name: "a", run: slow}, {name: "b", run: slow}, {name: "c", run: slow}}, r)
	if statuses[2].result != "skipped: run budget spent" {
		t.Errorf("third collector: %q", statuses[2].result)
	}
}

// A collector's error text can carry a raw value; finishBundle scrubs it
// before reading Kinds, so the manifest's "replaced:" line counts it.
func TestManifestScrubsStatusesBeforeCountingKinds(t *testing.T) {
	r := redact.New(redact.Host{Home: "/Users/zzfoo"}, redact.Allow{})
	raw := []rawEntry{{"a.txt", "fine"}}
	statuses := []collectorStatus{
		{"broken", "failed: open /Users/zzfoo/.brig/x: denied, mail bob@example.com"},
	}
	entries, err := finishBundle(r, raw, statuses, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	manifest := entries[0].body.String()
	if !strings.Contains(manifest, "replaced: email, path") {
		t.Errorf("manifest does not credit both kinds: %s", manifest)
	}
	for _, e := range entries {
		if strings.Contains(e.body.String(), "/Users/zzfoo") || strings.Contains(e.body.String(), "bob@example.com") {
			t.Errorf("%s kept a raw value: %s", e.name, e.body)
		}
	}
}

// A value registered by a later collector is removed from an earlier one's
// output, because the scrub runs after every collector.
func TestFinishScrubsAfterEveryCollector(t *testing.T) {
	r := redact.New(redact.Host{}, redact.Allow{})
	raw := []rawEntry{{"first.txt", "uses ACME_CANARY_TOKEN"}}
	r.Var("ACME_CANARY_TOKEN") // as a later collector would
	entries, err := finishBundle(r, raw, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.body.String(), "ACME_CANARY_TOKEN") {
			t.Errorf("%s kept the value: %s", e.name, e.body)
		}
	}
	if entries[0].name.String() != "MANIFEST.txt" {
		t.Errorf("first entry %s, want MANIFEST.txt", entries[0].name)
	}
}

// When the final check finds a leak, the bundle is not written and the
// command fails.
func TestALeakWritesNothing(t *testing.T) {
	healthyHost(t)
	swap(t, &bundleCheck, func(*redact.Redactor, []byte) error { return &redact.LeakError{Kind: "VAR"} })
	path := filepath.Join(t.TempDir(), "b.zip")
	err := bundleCmd(&bytes.Buffer{}, []string{"-o", path})
	if err == nil || exitCode(err) != exitcode.Failure {
		t.Errorf("err = %v (exit %d), want a failure", err, exitCode(err))
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("a bundle was written despite the leak")
	}
}

func TestBundleCmdPrintsTwoLines(t *testing.T) {
	healthyHost(t)
	var out bytes.Buffer
	path := filepath.Join(t.TempDir(), "b.zip")
	if err := bundleCmd(&out, []string{"-o", path}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 || lines[0] != path {
		t.Errorf("stdout %q", out.String())
	}
}

// A new file in a missing directory is refused before collection, naming the
// directory.
func TestBundlePathRefusesAFileInAMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	_, err := bundlePath(filepath.Join(missing, "b.zip"), time.Now())
	var ue *usageError
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), missing) {
		t.Errorf("err = %v, want a usage error naming %s", err, missing)
	}
}

// `brig doctor --json bundle` is the same request as `brig --json doctor
// bundle`, and gets the same usage error, not an unknown profile "bundle".
func TestLocalJSONRefusesTheBundle(t *testing.T) {
	err := doctorCmd(&bytes.Buffer{}, []string{"--json", "bundle"})
	var ue *usageError
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), "no --json form") {
		t.Errorf("brig doctor --json bundle = %v, want the bundle's --json usage error", err)
	}
	if got := exitCode(err); got != exitcode.Usage {
		t.Errorf("exit %d, want %d", got, exitcode.Usage)
	}
}

// The manifest states the log cap only when the bundle has a log.
func TestManifestNotesTheLogCap(t *testing.T) {
	r := redact.New(redact.Host{}, redact.Allow{})
	logCap := fmt.Sprintf("last %d lines", bundleLogTail)
	with, err := finishBundle(r, []rawEntry{{"logs/brig-a.log", "boot\n"}}, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(with[0].body.String(), logCap) {
		t.Errorf("the manifest does not note the log cap: %s", with[0].body)
	}
	without, err := finishBundle(r, []rawEntry{{"doctor.json", "{}"}}, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(without[0].body.String(), logCap) {
		t.Errorf("the manifest notes a cap on logs it does not carry: %s", without[0].body)
	}
}
