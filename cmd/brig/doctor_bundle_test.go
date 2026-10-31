package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/runtime"
)

// bypassedBundle is healthyHost with this brig inside a runtime bundle's tree and
// the launcher's environment unset.
func bypassedBundle(t *testing.T) (envFile string) {
	t.Helper()
	healthyHost(t)
	t.Setenv("BRIG_BOOT_ASSETS", "")
	t.Setenv("BRIG_RUNTIME_BIN", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	prefix := t.TempDir()
	for _, dir := range []string{"bin", "etc"} {
		if err := os.Mkdir(filepath.Join(prefix, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	envFile = filepath.Join(prefix, "etc", "brig-env.sh")
	if err := os.WriteFile(envFile, []byte("export X=y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	swap(t, &doctorExecutable, func() (string, error) { return filepath.Join(prefix, "bin", "brig"), nil })
	return envFile
}

// The host in the report behind this row had every line at ok or at "run any
// agent once to fetch them", and then failed on CNI and on the initrd that
// fetch brought. The row names the launcher, and doctor stops offering the
// fetch: the assets it brings do not boot under the bundle's urunc.
func TestDoctorNamesABypassedBundleLauncher(t *testing.T) {
	envFile := bypassedBundle(t)

	checks := runDoctor(nil, nil)
	row := findCheck(t, checks, "bundle")
	if row.State != stateFail {
		t.Fatalf("bundle row state = %q, want fail", row.State)
	}
	for _, want := range []string{"export PATH=", envFile} {
		if !strings.Contains(row.Fix, want) {
			t.Errorf("bundle fix %q does not name %q", row.Fix, want)
		}
	}
	if err := doctorExit(checks); err != nil {
		t.Errorf("a bypassed launcher changed the exit status: %v", err)
	}
	boot := findCheck(t, checks, "boot")
	if strings.Contains(boot.Fix, "fetch") {
		t.Errorf("the boot row still offers the fetch: %q", boot.Fix)
	}
}

// The reporter's host had already fetched assets, and an "ok" beside the bundle
// row says they will do. The row stays ok, since the files are there, and says
// whose they are.
func TestDoctorSaysFetchedAssetsAreNotTheBundles(t *testing.T) {
	bypassedBundle(t)
	dir, _, _, err := runtime.BootAssetsDir(doctorRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	kernel, initrd := runtime.BootAssetNames()
	for _, name := range []string{kernel, initrd} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("stand-in\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	boot := findCheck(t, runDoctor(nil, nil), "boot")
	if boot.State != statePass {
		t.Fatalf("boot row state = %q, want ok", boot.State)
	}
	if !strings.Contains(boot.Finding, "not the runtime bundle's") {
		t.Errorf("the boot row does not say whose assets these are: %q", boot.Finding)
	}
}

// Through the launcher the same tree is healthy, and there is no row at all.
func TestDoctorNoBundleRowWithTheLauncherEnv(t *testing.T) {
	bypassedBundle(t)
	t.Setenv("BRIG_BOOT_ASSETS", t.TempDir())

	for _, c := range runDoctor(nil, nil) {
		if c.Name == "bundle" {
			t.Errorf("a bundle row on a host with the environment set: %+v", c)
		}
	}
}
