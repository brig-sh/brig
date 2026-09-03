package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
)

// emptyPath points PATH at a directory with no runtime in it, so LookPath finds
// neither hull nor nerdctl and DetectFor returns the not-found sentinel. This
// is the only detection failure env and ls are allowed to paper over.
func emptyPath(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("BRIG_RUNTIME_BIN", "")
}

// env touches the runtime for one line of its report, so with no runtime on
// PATH it degrades: exit 0, that line marked unavailable. But an unknown
// BRIG_RUNTIME is a mistake to fix, not a missing install, and env is the verb
// people run to diagnose exactly that -- so it must surface the same message
// run gives, with a non-zero (here non-nil) result, not swallow it as "no
// runtime found".
func TestEnvSurfacesUnknownRuntime(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	t.Setenv("BRIG_RUNTIME", "podman")

	err := run([]string{"env", "claude-code"})
	if err == nil {
		t.Fatal("brig env with an unknown BRIG_RUNTIME was accepted")
	}
	if want := `unknown BRIG_RUNTIME "podman" (want hull or nerdctl)`; !strings.Contains(err.Error(), want) {
		t.Errorf("env error = %v, want it to name the bad value: %q", err, want)
	}
}

// ls asks the runtime what exists, and with none installed the honest answer is
// nothing. An unknown BRIG_RUNTIME is a different thing: reading a typo as "you
// have no sandboxes" hides the mistake, so it fails with the value named,
// exactly as run does.
func TestLsSurfacesUnknownRuntime(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	t.Setenv("BRIG_RUNTIME", "podman")

	err := listSandboxes(nil, false)
	if err == nil {
		t.Fatal("brig ls with an unknown BRIG_RUNTIME was accepted")
	}
	if want := `unknown BRIG_RUNTIME "podman" (want hull or nerdctl)`; !strings.Contains(err.Error(), want) {
		t.Errorf("ls error = %v, want it to name the bad value: %q", err, want)
	}
}

// A profile whose runtimeBin points nowhere is a misconfiguration, not a
// missing runtime. env is the verb run to find that out, so it names the bad
// path rather than exiting 0 with the runtime line marked unavailable, which
// would blame the wrong cause.
func TestEnvSurfacesMissingRuntimeBin(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	t.Setenv("BRIG_RUNTIME", "hull")
	t.Setenv("BRIG_RUNTIME_BIN", "")
	missing := filepath.Join(t.TempDir(), "hull")
	body := "name: pinned\nimage: i\nguestHome: /home/pinned\nbinary: m\nmem: 1\ncpus: 1\n" +
		"runtimeBin: " + missing + "\n"
	if err := os.WriteFile(filepath.Join(dir, "pinned.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// run reloads profiles from BRIG_PROFILE_DIR, but load the file here too so a
	// failure to parse it surfaces as a test error rather than a missing profile.
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}

	err := run([]string{"env", "pinned"})
	if err == nil {
		t.Fatal("brig env with a missing runtimeBin was accepted")
	}
	if !strings.Contains(err.Error(), "runtimeBin") || !strings.Contains(err.Error(), missing) {
		t.Errorf("env error = %v, want it to name the bad runtimeBin %q", err, missing)
	}
}

// With genuinely no runtime on PATH, ls answers the question that was asked --
// there are no sandboxes -- and exits 0, rather than failing a read-only query
// with the error of the thing it would have queried. This is the one case the
// guard is allowed to swallow, and it keys off the sentinel to do it.
func TestLsWithoutARuntimeSaysNothingToList(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	t.Setenv("BRIG_RUNTIME", "")
	emptyPath(t)

	if err := listSandboxes(nil, false); err != nil {
		t.Errorf("ls with no runtime on PATH failed instead of listing nothing: %v", err)
	}
}

// info is the new spelling of env, so it answers without a runtime for the same
// reason: the runtime is one line of the report, and the person reading it is
// often the one whose runtime is what broke. A new verb that fails where the
// one it deprecates succeeds would make the recommended spelling the worse one.
func TestInfoReportsWithoutARuntime(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	emptyPath(t)

	if err := run([]string{"info", "claude-code"}); err != nil {
		t.Errorf("brig info with no runtime on PATH failed instead of reporting: %v", err)
	}
}

// And the exemption stays narrow for info too: an unknown BRIG_RUNTIME is a
// mistake to fix, not a missing install.
func TestInfoSurfacesUnknownRuntime(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	t.Setenv("BRIG_RUNTIME", "podman")

	err := run([]string{"info", "claude-code"})
	if err == nil {
		t.Fatal("brig info with an unknown BRIG_RUNTIME was accepted")
	}
	if want := `unknown BRIG_RUNTIME "podman" (want hull or nerdctl)`; !strings.Contains(err.Error(), want) {
		t.Errorf("info error = %v, want it to name the bad value: %q", err, want)
	}
}
