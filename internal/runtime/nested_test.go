package runtime

import (
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
)

// recordHull writes a stand-in hull whose `inspect` runs body.
func recordHull(t *testing.T, body string) string {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("shell stand-in is not portable to windows")
	}
	bin := filepath.Join(t.TempDir(), "hull")
	script := "#!/bin/sh\nif [ \"$1\" = inspect ]; then\n" + body + "\nfi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// hull's instance record says whether a running guest holds EL2. hull leaves
// the field out when it is false, and a hull that predates it never boots a
// nested guest, so an absent field is not nested.
func TestRunningNestedVirtReadsTheRecord(t *testing.T) {
	for _, tc := range []struct {
		name, record string
		want         bool
	}{
		{"nested", `{"id":"brig-s","status":"running","nestedVirt":true}`, true},
		{"false", `{"id":"brig-s","status":"running","nestedVirt":false}`, false},
		{"absent", `{"id":"brig-s","status":"running","backend":"hvi"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := recordHull(t, "printf '%s\\n' '"+tc.record+"'")
			got, err := (&hull{bin: bin}).RunningNestedVirt("brig-s")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("RunningNestedVirt = %v, want %v for %s", got, tc.want, tc.record)
			}
		})
	}
}

// A hull that could not be asked is an error, never "not nested": the caller
// refuses to join on it, and a false here would join a guest that may hold EL2
// under an envelope saying nothing about it.
func TestRunningNestedVirtFailsWhenHullCannotSay(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"exit", `printf 'error: store locked\n' >&2; exit 1`},
		{"garbage", `printf 'not a record\n'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := recordHull(t, tc.body)
			_, err := (&hull{bin: bin}).RunningNestedVirt("brig-s")
			if err == nil {
				t.Fatalf("%s: an unanswered inspect read as an answer", tc.name)
			}
			if errors.Is(err, ErrSandboxGone) {
				t.Errorf("%s: a failed question read as a sandbox that is gone: %v", tc.name, err)
			}
		})
	}
}

// A sandbox that exited between the caller seeing it running and asking how it
// booted is gone, which is an answer: the caller boots a new one.
func TestRunningNestedVirtSaysWhenTheSandboxIsGone(t *testing.T) {
	bin := recordHull(t, `printf 'error: instance not found: brig-s\n' >&2; exit 1`)
	_, err := (&hull{bin: bin}).RunningNestedVirt("brig-s")
	if !errors.Is(err, ErrSandboxGone) {
		t.Errorf("an instance hull cannot find: %v, want ErrSandboxGone", err)
	}
}

func TestOnlyHullInspectsNestedVirt(t *testing.T) {
	var _ NestedInspector = &hull{}
	var rt Runtime = &nerdctl{bin: "nerdctl"}
	if _, ok := rt.(NestedInspector); ok {
		t.Error("nerdctl claims to know whether a guest is nested, which it never boots")
	}
}
