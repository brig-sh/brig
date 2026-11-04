package wrap

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/verify"
)

// bundleExecutable makes this run look like a brig inside a runtime bundle's
// tree, and returns the environment file of that tree.
func bundleExecutable(t *testing.T) string {
	t.Helper()
	t.Setenv("BRIG_RUNTIME_BIN", "")
	t.Setenv("BRIG_BOOT_ASSETS", "")
	prefix := t.TempDir()
	if err := os.Mkdir(filepath.Join(prefix, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(prefix, "etc", "brig-env.sh")
	if err := os.WriteFile(file, []byte("export X=y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	was := selfExecutable
	selfExecutable = func() (string, error) { return filepath.Join(prefix, "bin", "brig"), nil }
	t.Cleanup(func() { selfExecutable = was })
	return file
}

// The run that follows this warning fails on the CNI plugins or in the guest,
// and neither failure names the launcher. So it comes first, before the fetch of
// boot assets that do not fit the bundle.
func TestEnsureRunningWarnsOfABypassedBundleLauncher(t *testing.T) {
	file := bundleExecutable(t)
	rr := &resolvingRuntime{assets: givenAssets(t), kind: "nerdctl"}
	c := resolvingConfig(t, verify.Warn, rr)

	_ = c.EnsureRunning(creds.Set{})

	if !strings.Contains(rr.atResolve, file) {
		t.Errorf("the warning was not out before the boot assets were resolved:\n%s", rr.atResolve)
	}
	if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, "export PATH=") {
		t.Errorf("the warning does not give the line to type:\n%s", said)
	}
}

// A runtime that cannot reach the bundle's containerd stops the run at its first
// question, and that error does not name the launcher.
func TestEnsureRunningWarnsBeforeTheRuntimeFails(t *testing.T) {
	bundleExecutable(t)
	rr := &resolvingRuntime{assets: givenAssets(t), kind: "nerdctl"}
	c := resolvingConfig(t, verify.Warn, rr)
	c.Runtime = &unreachableRuntime{rr}

	if err := c.EnsureRunning(creds.Set{}); err == nil {
		t.Fatal("the run went on past a runtime that cannot answer")
	}

	if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, "export PATH=") {
		t.Errorf("the run stopped without the warning:\n%s", said)
	}
}

type unreachableRuntime struct{ *resolvingRuntime }

func (*unreachableRuntime) Running(string) (bool, error) {
	return false, errors.New("cannot connect to containerd")
}

// brigd takes this path too, and has a launcher of its own to name.
func TestEnsureRunningWarnsInTheWordsOfBrigd(t *testing.T) {
	file := bundleExecutable(t)
	selfExecutable = func() (string, error) {
		return filepath.Join(filepath.Dir(filepath.Dir(file)), "bin", "brigd"), nil
	}
	rr := &resolvingRuntime{assets: givenAssets(t), kind: "nerdctl"}
	c := resolvingConfig(t, verify.Warn, rr)

	_ = c.EnsureRunning(creds.Set{})

	said := c.Err.(*bytes.Buffer).String()
	for _, want := range []string{"this brigd is inside", "run brigd from"} {
		if !strings.Contains(said, want) {
			t.Errorf("the warning is missing %q:\n%s", want, said)
		}
	}
	if strings.Contains(said, "this brig is") {
		t.Errorf("the warning calls brigd brig:\n%s", said)
	}
}

// Through the launcher, the run says nothing about it.
func TestEnsureRunningQuietWithTheBundleEnv(t *testing.T) {
	bundleExecutable(t)
	t.Setenv("BRIG_RUNTIME_BIN", "/somewhere/nerdctl")
	rr := &resolvingRuntime{assets: givenAssets(t), kind: "nerdctl"}
	c := resolvingConfig(t, verify.Warn, rr)

	_ = c.EnsureRunning(creds.Set{})

	if said := c.Err.(*bytes.Buffer).String(); strings.Contains(said, "runtime bundle") {
		t.Errorf("warned on a run with the environment set:\n%s", said)
	}
}
