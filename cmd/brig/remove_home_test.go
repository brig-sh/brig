package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ephemeralHost is removeHost with no BRIG_WORKSPACE, and an index that
// records home as the ephemeral guest home of the faker session. It returns
// home, created with a file in it.
func ephemeralHost(t *testing.T, rt *removeRuntime, home func(state string) string) string {
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
