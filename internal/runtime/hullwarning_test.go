package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// warningHull is a hull whose `run` prints a pull line, a warning and a boot
// message on stderr, and exits with code.
func warningHull(t *testing.T, code string) *hull {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "hull")
	script := "#!/bin/sh\n" +
		"echo 'booting' >&2\n" +
		"echo 'warning: the image runs as uid 1000, and hvi joins the network gateway as that user. " +
		"It may not write to /x/gw.sock, so the guest is likely to boot with no network' >&2\n" +
		"echo 'started' >&2\n" +
		"exit " + code + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return &hull{bin: bin, linux: true}
}

// Negative: a boot that worked does not swallow hull's warning. hull on Linux
// warns and exits 0 when the image's user may not reach the gateway socket,
// and the guest comes up with no network. Held with the rest of hull's
// output, the warning was dropped, and nothing said why.
func TestHullBootSaysHullsWarning(t *testing.T) {
	var notice bytes.Buffer
	if err := warningHull(t, "0").boot(RunSpec{Notice: &notice}, []string{"run"}, nil); err != nil {
		t.Fatal(err)
	}
	got := notice.String()
	if !strings.Contains(got, "brig: hull: warning: the image runs as uid 1000") {
		t.Errorf("the warning was not said: %q", got)
	}
	// Only the warning: the rest is noise on a boot that worked.
	if strings.Contains(got, "booting") || strings.Contains(got, "started") {
		t.Errorf("more than the warning was said: %q", got)
	}
}

// A boot streamed live has shown the warning already, and a failed one quotes
// it on the error, so neither says it a second time.
func TestHullBootSaysAWarningOnce(t *testing.T) {
	var notice, live bytes.Buffer
	if err := warningHull(t, "0").boot(RunSpec{Notice: &notice, Progress: &live}, []string{"run"}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(notice.String(), "warning:") {
		t.Errorf("a streamed boot said the warning again: %q", notice.String())
	}
	notice.Reset()
	err := warningHull(t, "1").boot(RunSpec{Notice: &notice}, []string{"run"}, nil)
	if err == nil || !strings.Contains(err.Error(), "warning: the image runs as uid 1000") {
		t.Errorf("a failed boot did not quote the warning: %v", err)
	}
	if strings.Contains(notice.String(), "warning:") {
		t.Errorf("a failed boot said the warning beside its error: %q", notice.String())
	}
}
