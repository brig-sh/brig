package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/wrap"
)

// fellBackRuntime is a runtime that drives a binary nobody named. It collects
// no telemetry, so `brig telemetry status` reaches it and calls nothing else.
type fellBackRuntime struct {
	runtime.Runtime
	note string
}

func (f *fellBackRuntime) Fallback() string { return f.note }

func withVerbosity(t *testing.T, v wrap.Verbosity) {
	t.Helper()
	prev := verbosity
	verbosity = v
	t.Cleanup(func() { verbosity = prev })
}

// A detected runtime that fell back says so on stderr at default verbosity.
// Driven through telemetry. The other verbs that detect a runtime have a test
// each below, so a call site that stops asking fails one of them.
func TestFallbackNotePrintedAtDefaultVerbosity(t *testing.T) {
	withVerbosity(t, wrap.Normal)
	const note = "nerdctl is not on PATH, so brig is driving docker (/usr/bin/docker)"
	withRuntime(t, &fellBackRuntime{note: note})

	var out bytes.Buffer
	stderr := captureStderr(t, func() {
		if err := telemetryCmd(&out, []string{"status"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(stderr, "brig: "+note) {
		t.Errorf("stderr does not carry the fallback note: %q", stderr)
	}
	if strings.Contains(out.String(), "docker") {
		t.Errorf("the note leaked into stdout, where a script reads: %q", out.String())
	}
}

// -q is a script asking for identifiers and errors. The note is neither.
func TestFallbackNoteDroppedUnderQuiet(t *testing.T) {
	withVerbosity(t, wrap.Quiet)
	stderr := captureStderr(t, func() {
		sayFallback(&fellBackRuntime{note: "nerdctl is not on PATH"})
	})
	if stderr != "" {
		t.Errorf("-q printed %q", stderr)
	}
}

// No note, no line: a runtime that found its first choice, or one that has no
// fallback to report at all, prints nothing.
func TestNoFallbackPrintsNothing(t *testing.T) {
	withVerbosity(t, wrap.Normal)
	for _, rt := range []runtime.Runtime{&fellBackRuntime{}, &quietRuntime{}, nil} {
		stderr := captureStderr(t, func() { sayFallback(rt) })
		if stderr != "" {
			t.Errorf("%T printed %q", rt, stderr)
		}
	}
}

const fellBackNote = "nerdctl is not on PATH, so brig is driving docker (/usr/bin/docker)"

// wantFallbackNote fails unless stderr carries the note, as warnf prints it.
func wantFallbackNote(t *testing.T, verb, stderr string) {
	t.Helper()
	if !strings.Contains(stderr, "brig: "+fellBackNote) {
		t.Errorf("%s did not print the fallback note: %q", verb, stderr)
	}
}

// fellBackDoctorRuntime is what doctor's runtime row reads, having fallen back.
type fellBackDoctorRuntime struct{ doctorRuntime }

func (fellBackDoctorRuntime) Fallback() string { return fellBackNote }

// brig doctor is where someone goes to ask what brig will drive. A doctor
// that reports docker in the runtime row without the note leaves out why.
func TestDoctorSaysWhenTheRuntimeFellBack(t *testing.T) {
	withVerbosity(t, wrap.Normal)
	healthyHost(t)
	rt := detectRuntime
	swap(t, &detectRuntime, func() (runtime.Runtime, error) {
		got, err := rt()
		if err != nil {
			return nil, err
		}
		return fellBackDoctorRuntime{got.(doctorRuntime)}, nil
	})
	stderr := captureStderr(t, func() { runtimeCheck(nil) })
	wantFallbackNote(t, "brig doctor", stderr)
}

// fellBackListRuntime has no sandboxes, which is all rm --all --dry-run asks.
type fellBackListRuntime struct{ fellBackRuntime }

func (fellBackListRuntime) List() ([]runtime.Instance, error) { return nil, nil }

// rm --all removes every sandbox on the runtime it detects, so the reader is
// told first when that runtime is docker taken in place of nerdctl.
func TestRemoveAllSaysWhenTheRuntimeFellBack(t *testing.T) {
	withVerbosity(t, wrap.Normal)
	withRuntime(t, &fellBackListRuntime{fellBackRuntime{note: fellBackNote}})
	stderr := captureStderr(t, func() {
		if err := removeAll("brig rm --all", nil, removeOpts{all: true, dryRun: true}); err != nil {
			t.Fatal(err)
		}
	})
	wantFallbackNote(t, "brig rm --all", stderr)
}

// brig logs resolves its runtime from the profile, not through the
// detectRuntime seam, so this drives the real nerdctl adapter against a PATH
// holding a docker and no nerdctl.
func TestLogsSaysWhenTheRuntimeFellBack(t *testing.T) {
	withVerbosity(t, wrap.Normal)
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("BRIG_RUNTIME", "nerdctl")
	t.Setenv("BRIG_RUNTIME_BIN", "")
	t.Setenv("BRIG_STATE_DIR", t.TempDir())
	t.Setenv("BRIG_WORKSPACE", t.TempDir())

	var rt runtime.Runtime
	stderr := captureStderr(t, func() {
		var err error
		if _, rt, _, err = resolveSandbox("claude-code"); err != nil {
			t.Fatal(err)
		}
	})
	if rt.Bin() != docker {
		t.Fatalf("logs resolved %q, want the docker on PATH %q", rt.Bin(), docker)
	}
	if !strings.Contains(stderr, "nerdctl is not on PATH") || !strings.Contains(stderr, docker) {
		t.Errorf("brig logs did not print the fallback note: %q", stderr)
	}
}
