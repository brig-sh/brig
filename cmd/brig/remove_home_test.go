package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/runtime"
)

// ephemeralHost is removeHost with no BRIG_WORKSPACE, and an index that
// records home as the ephemeral guest home of the faker session. It returns
// home, created with a file in it.
func ephemeralHost(t *testing.T, rt runtime.Runtime, home func(state string) string) string {
	t.Helper()
	removeHost(t, rt)
	t.Setenv("BRIG_WORKSPACE", "")
	if err := os.Unsetenv("BRIG_WORKSPACE"); err != nil {
		t.Fatal(err)
	}
	dir := home(os.Getenv("BRIG_STATE_DIR"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-state"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	index := `{"faker": {"home": "` + dir + `", "sandbox": "brig-faker", "ephemeral": true}}`
	if err := os.WriteFile(filepath.Join(os.Getenv("BRIG_STATE_DIR"), "sessions.json"),
		[]byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func homeUnderState(state string) string { return filepath.Join(state, "homes", "brig-faker") }

func TestRemoveRefDeletesTheEphemeralHome(t *testing.T) {
	rt := twoSandboxes()
	home := ephemeralHost(t, rt, homeUnderState)
	var err error
	stderr := captureStderr(t, func() {
		_, err = captureStdout(t, func() error { return run([]string{"rm", "faker"}) })
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "removed faker and its guest home " + home; !strings.Contains(stderr, want) {
		t.Errorf("stderr does not say %q:\n%s", want, stderr)
	}
	if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the ephemeral home is still there: %v", err)
	}
}

func TestRemoveRefDryRunNamesTheEphemeralHome(t *testing.T) {
	rt := twoSandboxes()
	home := ephemeralHost(t, rt, homeUnderState)
	out, err := captureStdout(t, func() error { return run([]string{"rm", "faker", "--dry-run"}) })
	if err != nil {
		t.Fatal(err)
	}
	if want := "would remove faker (sandbox brig-faker) and its guest home " + home; !strings.Contains(out, want) {
		t.Errorf("stdout does not say %q:\n%s", want, out)
	}
	if len(rt.removed) != 0 {
		t.Errorf("--dry-run removed %v", rt.removed)
	}
	if _, err := os.Stat(home); err != nil {
		t.Errorf("--dry-run deleted the home: %v", err)
	}
}

// The sandbox goes, the home cannot, and rm says so with exit 1: a recorded
// home outside ~/.brig/homes is refused by the delete.
func TestRemoveRefExitsOneWhenTheHomeStays(t *testing.T) {
	rt := twoSandboxes()
	outside := t.TempDir()
	home := ephemeralHost(t, rt, func(string) string { return filepath.Join(outside, "home") })
	_, err := captureStdout(t, func() error { return run([]string{"rm", "faker"}) })
	if err == nil {
		t.Fatal("rm reported success with the home left behind")
	}
	if got := exitCode(err); got != 1 {
		t.Errorf("exit %d, want 1: %v", got, err)
	}
	if !strings.Contains(err.Error(), "removed faker") || !strings.Contains(err.Error(), home) {
		t.Errorf("the error does not name the removal and the home: %v", err)
	}
	if len(rt.removed) != 1 || rt.removed[0] != "brig-faker" {
		t.Errorf("removed %v, want brig-faker", rt.removed)
	}
	if _, err := os.Stat(home); err != nil {
		t.Errorf("a home outside ~/.brig/homes was deleted: %v", err)
	}
}

// goneRuntime has no sandboxes, and when asked about one says it has none,
// or fails with err: a runtime after a sandbox was removed outside brig.
type goneRuntime struct {
	removeRuntime
	err error
}

func (r *goneRuntime) Exists(string) (bool, error) { return false, r.err }

// rm of a session whose sandbox the runtime reports missing forgets the
// session and leaves the guest home brig created for the next run of the ref
// to delete. The boot notice said rm deletes that home, so rm names it and
// says what will. --dry-run says the same. A home the run named and a project
// stay as on any rm, and rm says so without promising that a run deletes
// them. A runtime that cannot say keeps the session, so the home is still the
// session's and nothing is said about it.
func TestRemoveRefNamesTheHomeOfAForgottenSession(t *testing.T) {
	for _, tt := range []struct {
		name     string
		args     []string
		err      error
		named    bool
		project  bool
		wantNote bool
	}{
		{"forgotten", []string{"rm", "faker"}, nil, false, false, true},
		{"dry run", []string{"rm", "faker", "--dry-run"}, nil, false, false, true},
		{"named home", []string{"rm", "faker"}, nil, true, false, true},
		{"with a project", []string{"rm", "faker"}, nil, false, true, true},
		{"runtime cannot say", []string{"rm", "faker"}, errors.New("inspect: store locked"), false, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rt := &goneRuntime{err: tt.err}
			at := homeUnderState
			if tt.named {
				outside := t.TempDir()
				at = func(string) string { return filepath.Join(outside, "home") }
			}
			home := ephemeralHost(t, rt, at)
			entry := map[string]any{"home": home, "sandbox": "brig-faker", "ephemeral": !tt.named}
			project := ""
			if tt.project {
				project = t.TempDir()
				entry["project"] = project
			}
			blob, err := json.Marshal(map[string]any{"faker": entry})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(os.Getenv("BRIG_STATE_DIR"), "sessions.json"), blob, 0o600); err != nil {
				t.Fatal(err)
			}
			stderr := captureStderr(t, func() {
				_, err = captureStdout(t, func() error { return run(tt.args) })
			})
			if got := exitCode(err); got != exitNotFound {
				t.Fatalf("exit %d, want %d: %v", got, exitNotFound, err)
			}
			stays := "the guest home " + home + " stays on the host"
			deletes := stays + "; the next run of faker deletes it"
			switch {
			case !tt.wantNote:
				if strings.Contains(stderr, stays) {
					t.Errorf("stderr names the home of a session rm kept:\n%s", stderr)
				}
			case tt.named:
				if !strings.Contains(stderr, stays) || strings.Contains(stderr, deletes) {
					t.Errorf("stderr does not say %q alone:\n%s", stays, stderr)
				}
			default:
				if !strings.Contains(stderr, deletes) {
					t.Errorf("stderr does not say %q:\n%s", deletes, stderr)
				}
			}
			if keptProject := "the project " + project + " stays on the host"; tt.project &&
				strings.Contains(stderr, keptProject) != tt.wantNote {
				t.Errorf("stderr says %q: %v, want %v:\n%s", keptProject, !tt.wantNote, tt.wantNote, stderr)
			}
			if _, statErr := os.Stat(filepath.Join(home, "agent-state")); statErr != nil {
				t.Errorf("rm deleted the guest home of a sandbox it did not remove: %v", statErr)
			}
			if len(rt.removed) != 0 {
				t.Errorf("rm removed %v, which the runtime does not have", rt.removed)
			}
		})
	}
}
