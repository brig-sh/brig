package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The lines hull prints during a pull, in the format of its
// pkg/ociclient/progress.go. pullWatch reads these, so a change to that format
// shows up here.
const (
	hullPullLayer   = "pull: layer 1/2, 12.0 MB extracted (image 480.2 MB compressed)"
	hullPullLayer2  = "pull: layer 2/2, 1.0 GB extracted (image 480.2 MB compressed)"
	hullPullSummary = "pull: 2/2 layers, 1.1 GB extracted"
)

// fakeSpinner records what announce asked it to show.
type fakeSpinner struct {
	bytes.Buffer
	spun     []string
	spinning bool
	stops    int
}

func (f *fakeSpinner) Spin(what string) func() {
	f.spun = append(f.spun, what)
	f.spinning = true
	return func() {
		f.spinning = false
		f.stops++
	}
}

func TestAPullIsAnnouncedFromHullsOwnLines(t *testing.T) {
	scratchHome(t)
	said := strings.Join([]string{hullPullLayer, hullPullLayer2, hullPullSummary, "booted"}, "\n")
	h := &hull{bin: stubRuntimeBin(t, said, 0)}

	var notice bytes.Buffer
	if err := h.Run(RunSpec{Name: "brig-x", Image: "img", Hypervisor: "vz", Notice: &notice}); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if want := "brig: pulling img...\nbrig: img pulled\n"; notice.String() != want {
		t.Errorf("notice = %q, want %q", notice.String(), want)
	}
}

func TestABootFromTheStoreAnnouncesNothing(t *testing.T) {
	scratchHome(t)
	h := &hull{bin: stubRuntimeBin(t, "booted", 0)}

	var notice bytes.Buffer
	if err := h.Run(RunSpec{Name: "brig-x", Image: "img", Hypervisor: "vz", Notice: &notice}); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if notice.Len() != 0 {
		t.Errorf("a boot with no pull announced %q", notice.String())
	}
}

// The error says why the pull stopped, so the notice does not claim it ended.
func TestAFailedPullGetsNoEndLine(t *testing.T) {
	scratchHome(t)
	said := hullPullLayer + "\nError: failed to pull image img: no space left on device"
	h := &hull{bin: stubRuntimeBin(t, said, 1)}

	var notice bytes.Buffer
	err := h.Run(RunSpec{Name: "brig-x", Image: "img", Hypervisor: "vz", Notice: &notice})
	if err == nil || !strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("the failure does not carry what hull said: %v", err)
	}
	if want := "brig: pulling img...\n"; notice.String() != want {
		t.Errorf("notice = %q, want %q", notice.String(), want)
	}
}

func TestVerboseLeavesThePullToTheStream(t *testing.T) {
	scratchHome(t)
	h := &hull{bin: stubRuntimeBin(t, hullPullLayer+"\n"+hullPullSummary, 0)}

	var notice, progress bytes.Buffer
	spec := RunSpec{Name: "brig-x", Image: "img", Hypervisor: "vz", Notice: &notice, Progress: &progress}
	if err := h.Run(spec); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if notice.Len() != 0 {
		t.Errorf("the pull was announced beside the stream: %q", notice.String())
	}
	if !strings.Contains(progress.String(), hullPullSummary) {
		t.Errorf("the stream lost hull's pull lines: %q", progress.String())
	}
}

// The spinner stops at hull's summary line, before the boot that follows it.
func TestTheSpinnerStopsWhenThePullEnds(t *testing.T) {
	var s fakeSpinner
	p := &pullWatch{notice: &s, ref: "img"}

	_, _ = p.Write([]byte(hullPullLayer + "\n"))
	if !s.spinning {
		t.Fatal("the first progress line did not start the spinner")
	}
	_, _ = p.Write([]byte(hullPullLayer2 + "\n" + hullPullSummary + "\nbooted\n"))
	if s.spinning {
		t.Error("the spinner outlived hull's summary line")
	}
	p.finish(true)
	if len(s.spun) != 1 || s.spun[0] != "pulling img" || s.stops != 1 {
		t.Errorf("spun %q and stopped %d times, want one spin of \"pulling img\"", s.spun, s.stops)
	}
	if s.Len() != 0 {
		t.Errorf("a spinning notice also printed lines: %q", s.String())
	}
}

// On a terminal a failed pull clears its spinner and says nothing more: the
// error that follows says why it stopped.
func TestAFailedPullStopsTheSpinnerQuietly(t *testing.T) {
	var s fakeSpinner
	p := &pullWatch{notice: &s, ref: "img"}
	_, _ = p.Write([]byte(hullPullLayer + "\nError: failed to pull image img: EOF\n"))
	p.finish(false)
	if s.spinning || s.stops != 1 {
		t.Errorf("spinning %v after %d stops, want stopped once", s.spinning, s.stops)
	}
	if s.Len() != 0 {
		t.Errorf("a failed pull printed %q beside its spinner", s.String())
	}
}

// Pipe writes do not follow line boundaries.
func TestAProgressLineSplitAcrossWrites(t *testing.T) {
	var notice bytes.Buffer
	p := &pullWatch{notice: &notice, ref: "img"}
	for _, chunk := range []string{"boo", "ted\npu", "ll: lay", "er 1/2, 1 B\n", "pull: 2/2 la", "yers\n"} {
		_, _ = p.Write([]byte(chunk))
	}
	if want := "brig: pulling img...\nbrig: img pulled\n"; notice.String() != want {
		t.Errorf("notice = %q, want %q", notice.String(), want)
	}
}

func TestALongLineDoesNotGrowTheWatch(t *testing.T) {
	p := &pullWatch{ref: "img"}
	_, _ = p.Write(bytes.Repeat([]byte("x"), 1<<20))
	if len(p.line) > pullLineHead {
		t.Errorf("the watch kept %d bytes of one line", len(p.line))
	}
}

func TestTheKernelDownloadSpinsOnASpinner(t *testing.T) {
	h := &hull{bin: stubRuntimeBin(t, "fetched", 0)}
	var s fakeSpinner
	if err := h.pullAssets(t.TempDir(), &s, nil); err != nil {
		t.Fatalf("assets pull: %v", err)
	}
	if len(s.spun) != 1 || !strings.HasPrefix(s.spun[0], "downloading the kernel") || s.stops != 1 {
		t.Errorf("spun %q and stopped %d times", s.spun, s.stops)
	}
	if s.Len() != 0 {
		t.Errorf("a spinning notice also printed lines: %q", s.String())
	}
}

// oras can succeed and the record of the fetch still fail. That return stops
// the spinner too, or its frames draw over the error.
func TestAFailedFetchRecordStopsTheSpinner(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "oras")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := lookPath
	t.Cleanup(func() { lookPath = orig })
	lookPath = func(string) (string, error) { return stub, nil }

	var s fakeSpinner
	if err := orasPull(t.TempDir(), BootFetch{Ref: pinnedRef}, &s, nil); err == nil {
		t.Fatal("a pull that left no kernel to record succeeded")
	}
	if s.spinning || s.stops != 1 {
		t.Errorf("spinning %v after %d stops, want stopped once", s.spinning, s.stops)
	}
}
