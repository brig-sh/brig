package runtime

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubRuntimeBin writes an executable that says something on stderr and exits
// with the status given, so a test drives a real exec.Command through the
// adapter rather than asserting on a flag somewhere above it.
func stubRuntimeBin(t *testing.T, said string, status int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hull")
	script := fmt.Sprintf("#!/bin/sh\ncat <<'SAID' >&2\n%s\nSAID\nexit %d\n", said, status)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The runtime's output is captured rather than passed through, so a boot that
// fails must still quote what the runtime said. Losing it would make a broken
// boot unreportable.
func TestRunQuotesWhatTheRuntimeSaidWhenItFails(t *testing.T) {
	h := &hull{bin: stubRuntimeBin(t, "pulling ghcr.io/x\nError: no space left on device", 1)}

	err := h.Run(RunSpec{Name: "brig-x", Image: "img", Hypervisor: "vz"})
	if err == nil {
		t.Fatal("a runtime that exited non-zero must fail the boot")
	}
	if !strings.Contains(err.Error(), "no space left on device") {
		t.Errorf("the failure does not carry what the runtime said: %v", err)
	}
}

// A boot that works says nothing at all: what the runtime printed on its way
// up is held and then dropped, which is the whole point of holding it.
func TestRunSaysNothingWhenItWorks(t *testing.T) {
	h := &hull{bin: stubRuntimeBin(t, "pulling ghcr.io/x", 0)}

	if err := h.Run(RunSpec{Name: "brig-x", Image: "img", Hypervisor: "vz"}); err != nil {
		t.Fatalf("boot: %v", err)
	}
}

// Under --verbose the caller hands the runtime a writer, and the output goes
// there as it happens rather than being held for a failure that may not come.
func TestRunStreamsTheRuntimeOutputWhenItIsAskedFor(t *testing.T) {
	h := &hull{bin: stubRuntimeBin(t, "pulling ghcr.io/x", 0)}

	var progress bytes.Buffer
	spec := RunSpec{Name: "brig-x", Image: "img", Hypervisor: "vz", Progress: &progress}
	if err := h.Run(spec); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if !strings.Contains(progress.String(), "pulling ghcr.io/x") {
		t.Errorf("the runtime's output did not reach the writer: %q", progress.String())
	}
}

// Streamed output is not quoted back on the error as well: the reader watched
// it happen, and repeating it at the bottom reads like a second failure.
func TestRunDoesNotQuoteOutputItAlreadyPrinted(t *testing.T) {
	h := &hull{bin: stubRuntimeBin(t, "Error: no space left on device", 1)}

	var progress bytes.Buffer
	spec := RunSpec{Name: "brig-x", Image: "img", Hypervisor: "vz", Progress: &progress}
	err := h.Run(spec)
	if err == nil {
		t.Fatal("a runtime that exited non-zero must fail the boot")
	}
	if strings.Contains(err.Error(), "no space left on device") {
		t.Errorf("the failure repeated output that was already on screen: %v", err)
	}
	if !strings.Contains(progress.String(), "no space left on device") {
		t.Errorf("the runtime's output did not reach the writer: %q", progress.String())
	}
}

// What is held is bounded, and it is the END that is kept: a tool that printed
// a progress bar and then died says why on its last lines.
func TestNarrationKeepsTheEndOfALongStream(t *testing.T) {
	n := narrate(nil)
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(n, "%s\n", strings.Repeat("x", 100))
	}
	fmt.Fprintln(n, "Error: the last word")

	got := n.explain(errors.New("boom")).Error()
	if !strings.Contains(got, "Error: the last word") {
		t.Errorf("the end of the stream was dropped: %q", got)
	}
	if len(got) > 4*narrationLimit {
		t.Errorf("the whole stream was held: %d bytes", len(got))
	}
}
