package runtime

import (
	"strings"
	"testing"
)

// fallbackNote detects the nerdctl runtime under pref and returns what it
// says about falling back. BRIG_RUNTIME is pinned because darwin defaults to
// hull, and PATH is the caller's temp dir so the host's own nerdctl or docker
// never decides the answer.
func fallbackNote(t *testing.T, pref Preference) (Runtime, string) {
	t.Helper()
	t.Setenv("BRIG_RUNTIME", "nerdctl")
	rt, err := DetectFor(pref)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := rt.(FallbackReporter)
	if !ok {
		t.Fatalf("%T does not report a fallback", rt)
	}
	return rt, f.Fallback()
}

// With no nerdctl on PATH the adapter drives docker. That used to happen with
// no output at all, so a user who installed brig for a microVM boundary got
// docker and nothing told them (#30).
func TestNerdctlFallbackToDockerIsReported(t *testing.T) {
	dir := t.TempDir()
	docker := executableFile(t, dir, "docker")
	t.Setenv("PATH", dir)
	t.Setenv("BRIG_RUNTIME_BIN", "")

	rt, note := fallbackNote(t, Preference{})
	if rt.Bin() != docker {
		t.Fatalf("Bin() = %q, want the docker on PATH %q", rt.Bin(), docker)
	}
	if note == "" {
		t.Fatal("docker was taken in place of nerdctl and nothing says so")
	}
	for _, want := range []string{"nerdctl", docker, "BRIG_RUNTIME_BIN"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note does not name %q: %s", want, note)
		}
	}
	if strings.Contains(note, "\n") {
		t.Errorf("the note is more than one line: %q", note)
	}
}

// docker sits on PATH beside nerdctl here, so a note keyed off "docker is
// installed" rather than "docker was taken" fails this.
func TestNerdctlFoundSaysNothing(t *testing.T) {
	dir := t.TempDir()
	nerdctlBin := executableFile(t, dir, "nerdctl")
	executableFile(t, dir, "docker")
	t.Setenv("PATH", dir)
	t.Setenv("BRIG_RUNTIME_BIN", "")

	rt, note := fallbackNote(t, Preference{})
	if rt.Bin() != nerdctlBin {
		t.Fatalf("Bin() = %q, want nerdctl %q", rt.Bin(), nerdctlBin)
	}
	if note != "" {
		t.Errorf("nerdctl was found, yet: %s", note)
	}
}

// A docker someone named is a choice, not a fallback. PATH has no nerdctl, so
// a note keyed off the binary being docker fails this.
func TestDockerNamedBySettingSaysNothing(t *testing.T) {
	dir := t.TempDir()
	docker := executableFile(t, dir, "docker")
	t.Setenv("PATH", dir)

	t.Run("BRIG_RUNTIME_BIN", func(t *testing.T) {
		t.Setenv("BRIG_RUNTIME_BIN", "docker")
		rt, note := fallbackNote(t, Preference{})
		if rt.Bin() != docker {
			t.Fatalf("Bin() = %q, want %q", rt.Bin(), docker)
		}
		if note != "" {
			t.Errorf("BRIG_RUNTIME_BIN named docker, yet: %s", note)
		}
	})
	t.Run("runtimeBin", func(t *testing.T) {
		t.Setenv("BRIG_RUNTIME_BIN", "")
		rt, note := fallbackNote(t, Preference{Bin: docker})
		if rt.Bin() != docker {
			t.Fatalf("Bin() = %q, want %q", rt.Bin(), docker)
		}
		if note != "" {
			t.Errorf("the profile's runtimeBin named docker, yet: %s", note)
		}
	})
}
