package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
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
	swap(t, &apparmorRestrictsUserNS, func() bool { return false })
	swap(t, &userNamespaces, func() error { return errors.New("unshare: Operation not permitted") })
	c := userNSCheck(nil)
	if c.State != stateFail || !strings.Contains(c.Finding, "not permitted") || !strings.Contains(c.Fix, "max_user_namespaces") {
		t.Errorf("%+v", c)
	}
}

// No unshare is no answer, not a refusal.
func TestUserNSCheckWithoutUnshare(t *testing.T) {
	swap(t, &apparmorRestrictsUserNS, func() bool { return false })
	swap(t, &userNamespaces, func() error { return fmt.Errorf("unshare: %w", exec.ErrNotFound) })
	if c := userNSCheck(nil); c.State != stateInfo {
		t.Errorf("%+v", c)
	}
}

// On Ubuntu 24.04 and later a probe from doctor fails on a host set up right, so
// the row asks for the profile of the rootlesskit a run starts, never the probe.
func TestUserNSCheckAsksAppArmorForTheProfile(t *testing.T) {
	swap(t, &apparmorRestrictsUserNS, func() bool { return true })
	swap(t, &userNamespaces, func() error {
		t.Error("probed a host where AppArmor restricts user namespaces")
		return nil
	})
	rt := bundleNerdctl(t)
	rootlesskit := filepath.Join(filepath.Dir(rt.bin), "rootlesskit")
	swap(t, &apparmorLoaded, filepath.Join(t.TempDir(), "unreadable"))
	swap(t, &apparmorDir, t.TempDir())
	if err := os.MkdirAll(filepath.Join(apparmorDir, "local"), 0o755); err != nil {
		t.Fatal(err)
	}
	other := "/var/lib/brig/data/bin/rootlesskit flags=(unconfined) { userns, }\n"
	if err := os.WriteFile(filepath.Join(apparmorDir, "var.lib.brig.data.bin.rootlesskit"), []byte(other), 0o644); err != nil {
		t.Fatal(err)
	}
	c := userNSCheck(rt)
	if c.State != stateFail || !strings.Contains(c.Finding, rootlesskit) || !strings.Contains(c.Fix, "install.sh") {
		t.Errorf("another tree's profile covered this one: %+v", c)
	}
	mine := rootlesskit + " flags=(unconfined) { userns, }\n"
	if err := os.WriteFile(filepath.Join(apparmorDir, "local", "mine"), []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := userNSCheck(rt); c.State != statePass {
		t.Errorf("with the profile: %+v", c)
	}
	t.Setenv("PATH", t.TempDir())
	if c := userNSCheck(nil); c.State != stateInfo {
		t.Errorf("no rootlesskit anywhere: %+v", c)
	}
}

// loadProfile adds a loaded profile to the kernel's list as securityfs shows it.
func loadProfile(t *testing.T, name, attach string) {
	t.Helper()
	dir, err := os.MkdirTemp(apparmorLoaded, "profile.")
	if err != nil {
		t.Fatal(err)
	}
	for file, value := range map[string]string{"name": name, "attach": attach} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(value+"\n"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
}

// A profile written to /etc/apparmor.d and never loaded covers nothing, and a
// profile for a longer path covers another program.
func TestAppArmorAsksTheLoadedProfiles(t *testing.T) {
	const rootlesskit = "/home/alex/.local/share/brig/bin/rootlesskit"
	swap(t, &apparmorLoaded, t.TempDir())
	swap(t, &apparmorDir, t.TempDir())
	written := rootlesskit + " flags=(unconfined) { userns, }\n"
	if err := os.WriteFile(filepath.Join(apparmorDir, "written"), []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}
	if apparmorNames(rootlesskit) {
		t.Error("a profile on disk that was never loaded covered rootlesskit")
	}
	loadProfile(t, rootlesskit+"-old", rootlesskit+"-old")
	if apparmorNames(rootlesskit) {
		t.Error("the profile of a longer path covered rootlesskit")
	}
	loadProfile(t, "brig-rootlesskit", rootlesskit)
	if !apparmorNames(rootlesskit) {
		t.Error("a loaded profile that attaches to rootlesskit did not cover it")
	}
	swap(t, &apparmorLoaded, t.TempDir())
	loadProfile(t, rootlesskit, rootlesskit)
	if !apparmorNames(rootlesskit) {
		t.Error("a loaded profile named by the path did not cover it")
	}
}

// Where the kernel's list cannot be read, a profile file has to name the whole
// path outside a comment.
func TestAppArmorFilesNameTheWholePath(t *testing.T) {
	const rootlesskit = "/opt/brig/bin/rootlesskit"
	swap(t, &apparmorLoaded, filepath.Join(t.TempDir(), "unreadable"))
	for content, want := range map[string]bool{
		"# " + rootlesskit + " flags=(unconfined) { userns, }\n":                 false,
		rootlesskit + ".bak flags=(unconfined) { userns, }\n":                    false,
		"/opt/brig/bin/rootlesskit-old flags=(unconfined) { userns, }\n":         false,
		rootlesskit + " flags=(unconfined) { userns, }\n":                        true,
		"profile brig-rootlesskit " + rootlesskit + " flags=(unconfined) {\n}\n": true,
		"\"" + rootlesskit + "\" flags=(unconfined) {\n}\n":                      true,
	} {
		swap(t, &apparmorDir, t.TempDir())
		if err := os.WriteFile(filepath.Join(apparmorDir, "profile"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := apparmorNames(rootlesskit); got != want {
			t.Errorf("%q covers rootlesskit: %v, want %v", content, got, want)
		}
	}
}

// AppArmor attaches by the resolved path. Through a symlinked nerdctl, the
// bundle's rootlesskit sits beside the target.
func TestRootlesskitPathFollowsASymlinkedNerdctl(t *testing.T) {
	rt := bundleNerdctl(t)
	rootlesskit := filepath.Join(filepath.Dir(rt.bin), "rootlesskit")
	for _, path := range []string{rt.bin, rootlesskit} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(t.TempDir(), "nerdctl")
	if err := os.Symlink(rt.bin, link); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(rootlesskit)
	if err != nil {
		t.Fatal(err)
	}
	if got := rootlesskitPath(nerdctlDoctorRuntime{doctorRuntime{bin: link}}); got != want {
		t.Errorf("rootlesskitPath through a link = %q, want %q", got, want)
	}
}

// A range already handed out is not offered again, to this user or another.
func TestSubIDFixOffersAFreeRange(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	uids := writeFile(t, "first:100000:65536\nsecond:165536:65536\n")
	gids := writeFile(t, "first:100000:65536\nthird:300000:1000\n")
	c := subIDCheck(uids, gids)
	if want := "--add-subuids 301000-366535 --add-subgids 301000-366535 " + me.Username; !strings.Contains(c.Fix, want) {
		t.Errorf("fix %q, want %q", c.Fix, want)
	}
	if got := nextSubIDStart(filepath.Join(t.TempDir(), "absent")); got != 100000 {
		t.Errorf("a host with no ranges starts at %d", got)
	}
}

// Root uses the node-wide install, which maps no subordinate IDs.
func TestLinuxHostRowsForRoot(t *testing.T) {
	swap(t, &hostOS, "linux")
	swap(t, &geteuid, func() int { return 0 })
	for _, c := range linuxHostRows(nerdctlDoctorRuntime{}, "") {
		switch c.Name {
		case "subids", "userns", "uidmap":
			t.Errorf("a %s row for root: %+v", c.Name, c)
		}
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

// A missing device is an unloaded module. A device this user cannot open is not,
// and the module advice would do nothing for it.
func TestDevicesCheckFitsTheFixToTheFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "vhost-vsock")
	c := devicesCheck(missing)
	if c.State != stateFail || !strings.Contains(c.Finding, missing) || !strings.Contains(c.Fix, "vhost_vsock") {
		t.Errorf("a missing device: %+v", c)
	}
	if os.Geteuid() != 0 {
		denied := writeFile(t, "")
		if err := os.Chmod(denied, 0); err != nil {
			t.Fatal(err)
		}
		c = devicesCheck(denied)
		if c.State != stateFail || strings.Contains(c.Fix, "modprobe") || !strings.Contains(c.Fix, "install.sh") {
			t.Errorf("a denied device: %+v", c)
		}
	}
	if c := devicesCheck(writeFile(t, "")); c.State != statePass {
		t.Errorf("an openable device: %+v", c)
	}
}

// The virtual line reports /dev/kvm, and a second line for one cause is what
// doctor exists to stop.
func TestLinuxHostRowsLeaveKVMToTheVirtualLine(t *testing.T) {
	swap(t, &hostOS, "linux")
	swap(t, &userNamespaces, func() error { return nil })
	for _, c := range linuxHostRows(nerdctlDoctorRuntime{}, "") {
		if strings.Contains(c.Finding, "/dev/kvm") {
			t.Errorf("a host row reports /dev/kvm: %+v", c)
		}
	}
}

// bundleNerdctl is a nerdctl in a runtime bundle's tree.
func bundleNerdctl(t *testing.T) nerdctlDoctorRuntime {
	t.Helper()
	prefix := t.TempDir()
	for _, dir := range []string{"bin", "etc"} {
		if err := os.Mkdir(filepath.Join(prefix, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(prefix, "etc", "brig-env.sh"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return nerdctlDoctorRuntime{doctorRuntime{bin: filepath.Join(prefix, "bin", "nerdctl")}}
}

// The tree says whether the tools are there. A host that names its own kernel or
// its own nerdctl still lacks them.
func TestToolsRowDependsOnTheBundleTree(t *testing.T) {
	swap(t, &hostOS, "linux")
	swap(t, &userNamespaces, func() error { return nil })
	has := func(rt nerdctlDoctorRuntime) bool {
		for _, c := range linuxHostRows(rt, "") {
			if c.Name == "tools" {
				return true
			}
		}
		return false
	}
	own := nerdctlDoctorRuntime{doctorRuntime{bin: filepath.Join(t.TempDir(), "nerdctl")}}
	t.Setenv("BRIG_BOOT_ASSETS", t.TempDir())
	t.Setenv("BRIG_RUNTIME_BIN", own.bin)
	if !has(own) {
		t.Error("setting BRIG_BOOT_ASSETS and BRIG_RUNTIME_BIN took the tools row away")
	}
	if has(bundleNerdctl(t)) {
		t.Error("a tools row for the bundle's own nerdctl")
	}
}

// The host in the report had no /opt/cni/bin/bridge and doctor said nothing, and
// its run failed with `needs CNI plugin "bridge"`.
func TestCNICheckFollowsWhereNerdctlLooks(t *testing.T) {
	withBridge := t.TempDir()
	for _, plugin := range cniPlugins {
		if err := os.WriteFile(filepath.Join(withBridge, plugin), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	empty := t.TempDir()
	half := t.TempDir()
	if err := os.WriteFile(filepath.Join(half, "bridge"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CNI_PATH", half)
	if c := cniCheck(); c.State != stateFail || !strings.Contains(c.Finding, "host-local") || strings.Contains(c.Finding, "bridge,") {
		t.Errorf("a half-installed directory: %+v", c)
	}

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
	t.Setenv("NERDCTL_TOML", writeFile(t, "cni_path = '"+withBridge+"'\n"))
	if c := cniCheck(); c.State != statePass {
		t.Errorf("a single-quoted cni_path: %+v", c)
	}

	// With no NERDCTL_TOML, a user's nerdctl reads its own configuration.
	swap(t, &geteuid, func() int { return 1000 })
	t.Setenv("NERDCTL_TOML", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "nerdctl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nerdctl", "nerdctl.toml"), []byte("cni_path = \""+withBridge+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := cniCheck(); c.State != statePass {
		t.Errorf("the user's own configuration: %+v", c)
	}
}

// With no setting, nerdctl takes the first of its default directories that
// exists. A rootless user's own come first. A later directory with every plugin
// does not rescue an earlier one that has only bridge, since nerdctl never
// looks there.
func TestCNICheckTakesNerdctlsFirstDefault(t *testing.T) {
	t.Setenv("CNI_PATH", "")
	t.Setenv("NERDCTL_TOML", filepath.Join(t.TempDir(), "absent.toml"))
	swap(t, &geteuid, func() int { return 1000 })
	home := t.TempDir()
	t.Setenv("HOME", home)

	full := filepath.Join(home, ".local/lib/cni")
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, plugin := range cniPlugins {
		if err := os.WriteFile(filepath.Join(full, plugin), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := defaultCNIDir(); got != full {
		t.Fatalf("defaultCNIDir = %q, want the user's %q", got, full)
	}
	if c := cniCheck(); c.State != statePass || !strings.Contains(c.Finding, full) {
		t.Errorf("the user's own plugins: %+v", c)
	}

	first := filepath.Join(home, "opt/cni/bin")
	if err := os.MkdirAll(first, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "bridge"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if c := cniCheck(); c.State != stateFail || !strings.Contains(c.Finding, first) || strings.Contains(c.Finding, full) {
		t.Errorf("a later full directory covered an earlier half one: %+v", c)
	}

	// Root does not look in a home.
	swap(t, &geteuid, func() int { return 0 })
	if got := defaultCNIDir(); strings.HasPrefix(got, home) {
		t.Errorf("root's default is in the home: %q", got)
	}
}

func TestToolsCheckNamesWhatIsMissing(t *testing.T) {
	fakeTools(t, 0, "oras")
	c := toolsCheck(true, "")
	if c.State != stateFail || !strings.Contains(c.Finding, "hvi not on PATH") || strings.Contains(c.Finding, "oras") ||
		!strings.HasSuffix(c.Finding, "needs it") {
		t.Errorf("%+v", c)
	}
	fakeTools(t, 0, "oras", "hvi")
	if c := toolsCheck(true, ""); c.State != statePass {
		t.Errorf("%+v", c)
	}
	// oras fetches boot assets, and a host that has them runs without it.
	fakeTools(t, 0, "hvi")
	if c := toolsCheck(false, ""); c.State != statePass {
		t.Errorf("oras asked for with the assets present: %+v", c)
	}
	if c := toolsCheck(true, ""); c.State != stateFail || !strings.Contains(c.Finding, "oras") {
		t.Errorf("oras not asked for with the assets missing: %+v", c)
	}
	fakeTools(t, 0)
	if c := toolsCheck(true, ""); c.State != stateFail || !strings.HasSuffix(c.Finding, "needs them") {
		t.Errorf("two tools missing: %+v", c)
	}
}

// The two hosts from the review. A run boots on hvi unless something names
// another monitor, so the host with hvi alone boots and the host with
// cloud-hypervisor alone does not.
func TestToolsCheckAsksForTheMonitorARunBootsOn(t *testing.T) {
	fakeTools(t, 0, "hvi")
	if c := toolsCheck(false, ""); c.State != statePass {
		t.Errorf("hvi on PATH, no cloud-hypervisor: %+v", c)
	}
	fakeTools(t, 0, "cloud-hypervisor")
	if c := toolsCheck(false, ""); c.State != stateFail || !strings.Contains(c.Finding, "hvi") {
		t.Errorf("cloud-hypervisor on PATH, no hvi: %+v", c)
	}
	if c := toolsCheck(false, "cloud-hypervisor"); c.State != statePass {
		t.Errorf("cloud-hypervisor asked for and on PATH: %+v", c)
	}
	if c := toolsCheck(false, "bhyve"); c.State != stateFail || !strings.Contains(c.Finding, "bhyve") {
		t.Errorf("a monitor urunc does not run: %+v", c)
	}
}

// Doctor reads BRIG_HYPERVISOR the way a run does: per agent first, then
// global, then the profile's own field.
func TestDoctorPassesTheRunsHypervisorToTheHostRows(t *testing.T) {
	healthyHost(t)
	var got string
	swap(t, &linuxHostChecks, func(_ runtime.Runtime, hv string) []check {
		got = hv
		return nil
	})
	t.Setenv("BRIG_HYPERVISOR", "firecracker")
	runDoctor(nil, nil)
	if got != "firecracker" {
		t.Errorf("BRIG_HYPERVISOR=firecracker reached the host rows as %q", got)
	}
	t.Setenv("BRIG_HYPERVISOR", "")
	runDoctor(&profile.Profile{Name: "demo", Hypervisor: "qemu"}, nil)
	if got != "qemu" {
		t.Errorf("the profile's qemu reached the host rows as %q", got)
	}
	t.Setenv("BRIG_DEMO_HYPERVISOR", "cloud-hypervisor")
	runDoctor(&profile.Profile{Name: "demo", Hypervisor: "qemu"}, nil)
	if got != "cloud-hypervisor" {
		t.Errorf("BRIG_DEMO_HYPERVISOR reached the host rows as %q", got)
	}
}

// None of these rows runs on a host they do not describe, and none reaches the
// exit status.
func TestLinuxHostRowsStayOffElsewhere(t *testing.T) {
	swap(t, &hostOS, "darwin")
	if rows := linuxHostRows(nil, ""); rows != nil {
		t.Errorf("rows on macOS: %+v", rows)
	}
	swap(t, &hostOS, "linux")
	if rows := linuxHostRows(doctorRuntime{}, ""); rows != nil {
		t.Errorf("rows for hull: %+v", rows)
	}
}

func TestLinuxHostRowsLeaveTheExitStatusAlone(t *testing.T) {
	swap(t, &hostOS, "linux")
	swap(t, &userNamespaces, func() error { return errors.New("no") })
	t.Setenv("PATH", t.TempDir())
	t.Setenv("BRIG_BOOT_ASSETS", "")
	if err := doctorExit(linuxHostRows(nerdctlDoctorRuntime{}, "")); err != nil {
		t.Errorf("a host row changed the exit status: %v", err)
	}
}

// nerdctlDoctorRuntime is doctorRuntime under the name a Linux host detects.
type nerdctlDoctorRuntime struct{ doctorRuntime }

func (nerdctlDoctorRuntime) Kind() string { return "nerdctl" }

func TestRuntimeRowSaysWhenItIsNotTheBundle(t *testing.T) {
	swap(t, &hostOS, "linux")
	own := nerdctlDoctorRuntime{doctorRuntime{bin: filepath.Join(t.TempDir(), "nerdctl")}}
	if got := notTheBundle(own); !strings.Contains(got, "not the runtime bundle") {
		t.Errorf("notTheBundle = %q", got)
	}
	// Naming it by hand does not make it the bundle's.
	t.Setenv("BRIG_RUNTIME_BIN", own.bin)
	if got := notTheBundle(own); !strings.Contains(got, "not the runtime bundle") {
		t.Errorf("with BRIG_RUNTIME_BIN set, notTheBundle = %q", got)
	}
}

// With or without the launcher, the bundle's nerdctl is the bundle's.
func TestRuntimeRowDefersToTheBundleLine(t *testing.T) {
	bypassedBundle(t)
	swap(t, &hostOS, "linux")
	if got := notTheBundle(bundleNerdctl(t)); got != "" {
		t.Errorf("the bundle's runtime was called foreign: %q", got)
	}
}
