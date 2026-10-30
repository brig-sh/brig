package main

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSubIDCount(t *testing.T) {
	path := writeFile(t, "alex:100000:65536\nother:200000:65536\n1000:300000:1000\nbroken\nalex:x:y\n")
	if got := subIDCount(path, "alex", "1000"); got != 66536 {
		t.Errorf("subIDCount = %d, want the ranges by name and by uid", got)
	}
	if got := subIDCount(path, "nobody", "9"); got != 0 {
		t.Errorf("subIDCount gave a stranger %d", got)
	}
	if got := subIDCount(filepath.Join(t.TempDir(), "absent"), "alex", "1000"); got != 0 {
		t.Errorf("subIDCount of no file = %d", got)
	}
}

// The first error a report from a real host hit, and doctor read ok.
func TestSubIDCheckFailsOnAShortRange(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	enough := writeFile(t, me.Username+":100000:65536\n")
	short := writeFile(t, me.Username+":100000:1000\n")

	if c := subIDCheck(enough, enough); c.State != statePass {
		t.Errorf("a full range: %+v", c)
	}
	for name, files := range map[string][2]string{"uids": {short, enough}, "gids": {enough, short}} {
		c := subIDCheck(files[0], files[1])
		if c.State != stateFail || !strings.Contains(c.Fix, "usermod") {
			t.Errorf("short %s: %+v", name, c)
		}
	}
}

func TestUserNSCheckNamesTheRefusal(t *testing.T) {
	swap(t, &userNamespaces, func() error { return errors.New("unshare: Operation not permitted") })
	c := userNSCheck()
	if c.State != stateFail || !strings.Contains(c.Finding, "not permitted") {
		t.Errorf("%+v", c)
	}
}

// fakeTools puts executables on an otherwise empty PATH.
func fakeTools(t *testing.T, mode os.FileMode, names ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o755|mode); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

// Present is not enough: a plain copy cannot write the map, and rootlesskit then
// fails with an error about the map and not about the binary.
func TestUidmapCheckRefusesAPlainBinary(t *testing.T) {
	swap(t, &fileCapsOf, func(string) bool { return false })

	fakeTools(t, 0, "newuidmap", "newgidmap")
	if c := uidmapCheck(); c.State != stateFail || !strings.Contains(c.Finding, "setuid") {
		t.Errorf("plain binaries: %+v", c)
	}
	fakeTools(t, os.ModeSetuid, "newuidmap")
	if c := uidmapCheck(); c.State != stateFail || !strings.Contains(c.Finding, "newgidmap") {
		t.Errorf("no newgidmap: %+v", c)
	}
	fakeTools(t, os.ModeSetuid, "newuidmap", "newgidmap")
	if c := uidmapCheck(); c.State != statePass {
		t.Errorf("setuid binaries: %+v", c)
	}
	swap(t, &fileCapsOf, func(string) bool { return true })
	fakeTools(t, 0, "newuidmap", "newgidmap")
	if c := uidmapCheck(); c.State != statePass {
		t.Errorf("binaries with file capabilities: %+v", c)
	}
}

func TestDevicesCheckNamesTheDevice(t *testing.T) {
	ok := writeFile(t, "")
	missing := filepath.Join(t.TempDir(), "vhost-vsock")
	c := devicesCheck(ok, missing)
	if c.State != stateFail || !strings.Contains(c.Finding, missing) || !strings.Contains(c.Fix, "vhost_vsock") {
		t.Errorf("%+v", c)
	}
	if c := devicesCheck(ok); c.State != statePass {
		t.Errorf("an openable device: %+v", c)
	}
}

// The host in the report had no /opt/cni/bin/bridge and doctor said nothing, and
// its run failed with `needs CNI plugin "bridge"`.
func TestCNICheckFollowsWhereNerdctlLooks(t *testing.T) {
	withBridge := t.TempDir()
	if err := os.WriteFile(filepath.Join(withBridge, "bridge"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()

	t.Setenv("CNI_PATH", empty)
	if c := cniCheck(); c.State != stateFail || !strings.Contains(c.Finding, empty) {
		t.Errorf("empty CNI_PATH: %+v", c)
	}
	t.Setenv("CNI_PATH", withBridge)
	if c := cniCheck(); c.State != statePass {
		t.Errorf("CNI_PATH with the plugin: %+v", c)
	}

	t.Setenv("CNI_PATH", "")
	t.Setenv("NERDCTL_TOML", writeFile(t, "debug = false\ncni_path = \""+withBridge+"\"\n"))
	if c := cniCheck(); c.State != statePass {
		t.Errorf("the configuration's cni_path: %+v", c)
	}
	t.Setenv("NERDCTL_TOML", writeFile(t, "cni_path = \""+empty+"\"\n"))
	if c := cniCheck(); c.State != stateFail || !strings.Contains(c.Fix, "install.sh") {
		t.Errorf("a cni_path with no plugin: %+v", c)
	}
}

func TestToolsCheckNamesWhatIsMissing(t *testing.T) {
	fakeTools(t, 0, "oras")
	c := toolsCheck()
	if c.State != stateFail || !strings.Contains(c.Finding, "cloud-hypervisor") || strings.Contains(c.Finding, "oras") {
		t.Errorf("%+v", c)
	}
	fakeTools(t, 0, "oras", "cloud-hypervisor")
	if c := toolsCheck(); c.State != statePass {
		t.Errorf("%+v", c)
	}
}

// None of these rows runs on a host they do not describe, and none reaches the
// exit status.
func TestLinuxHostRowsStayOffElsewhere(t *testing.T) {
	swap(t, &hostOS, "darwin")
	if rows := linuxHostRows(nil); rows != nil {
		t.Errorf("rows on macOS: %+v", rows)
	}
	swap(t, &hostOS, "linux")
	if rows := linuxHostRows(doctorRuntime{}); rows != nil {
		t.Errorf("rows for hull: %+v", rows)
	}
}

func TestLinuxHostRowsLeaveTheExitStatusAlone(t *testing.T) {
	swap(t, &hostOS, "linux")
	swap(t, &userNamespaces, func() error { return errors.New("no") })
	t.Setenv("PATH", t.TempDir())
	t.Setenv("BRIG_BOOT_ASSETS", "")
	if err := doctorExit(linuxHostRows(nerdctlDoctorRuntime{})); err != nil {
		t.Errorf("a host row changed the exit status: %v", err)
	}
}

// nerdctlDoctorRuntime is doctorRuntime under the name a Linux host detects.
type nerdctlDoctorRuntime struct{ doctorRuntime }

func (nerdctlDoctorRuntime) Kind() string { return "nerdctl" }

func TestRuntimeRowSaysWhenItIsNotTheBundle(t *testing.T) {
	swap(t, &hostOS, "linux")
	t.Setenv("BRIG_RUNTIME_BIN", "")
	if got := notTheBundle(nerdctlDoctorRuntime{}); !strings.Contains(got, "not the runtime bundle") {
		t.Errorf("notTheBundle = %q", got)
	}
	t.Setenv("BRIG_RUNTIME_BIN", "/somewhere/nerdctl")
	if got := notTheBundle(nerdctlDoctorRuntime{}); got != "" {
		t.Errorf("the launcher's runtime was called foreign: %q", got)
	}
}

// Inside the bundle's tree the runtime may be the bundle's own, and the bundle
// line already says what is wrong.
func TestRuntimeRowDefersToTheBundleLine(t *testing.T) {
	bypassedBundle(t)
	swap(t, &hostOS, "linux")
	if got := notTheBundle(nerdctlDoctorRuntime{}); got != "" {
		t.Errorf("a bypassed launcher's runtime was called foreign: %q", got)
	}
}
