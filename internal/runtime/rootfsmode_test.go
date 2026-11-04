package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// overlayHost makes this host one that can build an overlay root, under hull
// and under urunc, until the test ends. TestMain makes it one that cannot.
func overlayHost(t *testing.T) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mke2fs")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	hull, look := hullMke2fs, lookMke2fs
	t.Cleanup(func() { hullMke2fs, lookMke2fs = hull, look })
	hullMke2fs = bin
	lookMke2fs = func() bool { return true }
	for _, name := range []string{"CONTAINERD_SNAPSHOTTER", "BRIG_CONTAINERD_RUNTIME", "HULL_ROOTFS_MODE", "HULL_HVI_ROOTFS_UPPER"} {
		t.Setenv(name, "")
	}
	t.Setenv("NERDCTL_TOML", filepath.Join(t.TempDir(), "none.toml"))
}

// The default, the opt-in and the opt-out of the overlay root, decided in one
// place for every runtime.
func TestRootfsModeFor(t *testing.T) {
	up := OverlayRoot{Available: true}
	down := OverlayRoot{Why: "install e2fsprogs"}
	generic := func(mode, rootfsType string) RunSpec {
		return RunSpec{GenericBoot: true, RootfsMode: mode, RootfsType: rootfsType}
	}
	for _, c := range []struct {
		name    string
		spec    RunSpec
		monitor bool
		host    OverlayRoot
		want    string
		refused string // a word the refusal carries, "" for none
	}{
		{"default on a host that builds it", generic("", ""), true, up, RootfsModeOverlayBlock, ""},
		{"default over a virtiofs root", generic("", "virtiofs"), true, up, RootfsModeOverlayBlock, ""},
		{"default on a host that cannot", generic("", ""), true, down, "", ""},
		{"default on a monitor without it", generic("", ""), false, up, "", ""},
		{"default with a block root", generic("", "block"), true, up, "", ""},
		{"default with a 9p root", generic("", "9pfs"), true, up, "", ""},
		{"default on an image that is a guest", RunSpec{}, true, up, "", ""},
		{"asked on a host that builds it", generic(RootfsModeOverlayBlock, ""), true, up, RootfsModeOverlayBlock, ""},
		{"asked leaves the monitor to the runtime", generic(RootfsModeOverlayBlock, ""), false, up, RootfsModeOverlayBlock, ""},
		{"asked on a host that cannot", generic(RootfsModeOverlayBlock, ""), true, down, "", "install e2fsprogs"},
		{"asked with a block root", generic(RootfsModeOverlayBlock, "block"), true, up, "", "rootfs type block"},
		{"asked on an image that is a guest", RunSpec{RootfsMode: RootfsModeOverlayBlock}, true, up, "", "genericBoot"},
		{"opted out", generic(RootfsModeVirtiofs, ""), true, up, "", ""},
		{"opted out on an image that is a guest", RunSpec{RootfsMode: RootfsModeVirtiofs}, true, up, "", ""},
		{"no such mode", generic("overlay", ""), true, up, "", "use overlay-block or virtiofs"},
	} {
		got, err := rootfsModeFor(c.spec, c.monitor, c.host)
		switch {
		case c.refused == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.refused != "" && (err == nil || !strings.Contains(err.Error(), c.refused)):
			t.Errorf("%s: want a refusal naming %q, got %v", c.name, c.refused, err)
		case got != c.want:
			t.Errorf("%s: mode %q, want %q", c.name, got, c.want)
		}
	}
}

// hull runs one mke2fs, so that file is the whole of what it needs.
func TestHullOverlayRoot(t *testing.T) {
	if got := hullOverlayRoot(); got.Available || !strings.Contains(got.Why, "brew install e2fsprogs") {
		t.Errorf("no mke2fs: %+v, want unavailable with the fix", got)
	}
	overlayHost(t)
	if got := hullOverlayRoot(); !got.Available {
		t.Errorf("mke2fs there: %+v", got)
	}
	if err := os.Chmod(hullMke2fs, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := hullOverlayRoot(); got.Available {
		t.Errorf("an mke2fs nobody can run counts as there")
	}
	if got := OverlayRootFor(&hull{bin: "hull"}); got.Available {
		t.Errorf("OverlayRootFor does not ask hull's probe: %+v", got)
	}
}

// urunc needs nerdctl, its own shim, the overlayfs snapshotter and mke2fs.
// Each one missing is named.
func TestNerdctlOverlayRoot(t *testing.T) {
	overlayHost(t)
	n := &nerdctl{bin: "/usr/local/bin/nerdctl"}
	if got := n.overlayRoot(); !got.Available {
		t.Fatalf("everything there: %+v", got)
	}
	if got := OverlayRootFor(n); !got.Available {
		t.Errorf("OverlayRootFor does not ask nerdctl's probe: %+v", got)
	}
	if got := (&nerdctl{bin: "/usr/bin/docker"}).overlayRoot(); got.Available || !strings.Contains(got.Why, "docker") {
		t.Errorf("docker: %+v", got)
	}

	t.Setenv("BRIG_CONTAINERD_RUNTIME", "io.containerd.kata.v2")
	if got := n.overlayRoot(); got.Available || !strings.Contains(got.Why, "kata") {
		t.Errorf("another shim: %+v", got)
	}
	t.Setenv("BRIG_CONTAINERD_RUNTIME", "io.containerd.urunc.v2")
	if got := n.overlayRoot(); !got.Available {
		t.Errorf("urunc named outright: %+v", got)
	}

	t.Setenv("CONTAINERD_SNAPSHOTTER", "devmapper")
	if got := n.overlayRoot(); got.Available || !strings.Contains(got.Why, "devmapper (CONTAINERD_SNAPSHOTTER)") {
		t.Errorf("devmapper: %+v", got)
	}
	t.Setenv("CONTAINERD_SNAPSHOTTER", "")

	lookMke2fs = func() bool { return false }
	if got := n.overlayRoot(); got.Available || !strings.Contains(got.Why, "e2fsprogs") {
		t.Errorf("no mke2fs: %+v", got)
	}
}

// nerdctl takes the snapshotter from CONTAINERD_SNAPSHOTTER, then from its
// nerdctl.toml, then uses overlayfs.
func TestContainerdSnapshotter(t *testing.T) {
	overlayHost(t)
	toml := filepath.Join(t.TempDir(), "nerdctl.toml")
	t.Setenv("NERDCTL_TOML", toml)

	if got, from := containerdSnapshotter(); got != "overlayfs" || from != "nerdctl's default" {
		t.Errorf("no file, no variable: %q from %q", got, from)
	}
	for body, want := range map[string]string{
		"namespace = \"brig\"\nsnapshotter = \"devmapper\"\n":          "devmapper",
		"snapshotter = 'blockfile' # the pool\n":                       "blockfile",
		"snapshotter=overlayfs\n":                                      "overlayfs",
		"debug = false\n[cni]\nsnapshotter = \"devmapper\"\n":          "overlayfs",
		"# snapshotter = \"devmapper\"\nsnapshotter = \"overlayfs\"\n": "overlayfs",
	} {
		if err := os.WriteFile(toml, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, _ := containerdSnapshotter(); got != want {
			t.Errorf("%q: %q, want %q", body, got, want)
		}
	}
	if err := os.WriteFile(toml, []byte("snapshotter = \"devmapper\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, from := containerdSnapshotter(); from != toml {
		t.Errorf("the answer names %q, want the file", from)
	}
	t.Setenv("CONTAINERD_SNAPSHOTTER", "overlayfs")
	if got, from := containerdSnapshotter(); got != "overlayfs" || from != "CONTAINERD_SNAPSHOTTER" {
		t.Errorf("the variable does not win over the file: %q from %q", got, from)
	}
}

// The monitor half of the default, as doctor reports it and the run decides it.
func TestOverlayMonitorFor(t *testing.T) {
	overlayHost(t)
	h, n := &hull{}, &nerdctl{bin: "/usr/local/bin/nerdctl"}
	for _, c := range []struct {
		rt   Runtime
		hv   string
		want bool
	}{
		{h, "", true}, {h, "vz", true}, {h, "hvi", true}, {h, "qemu", false},
		{n, "", true}, {n, "hvi", true}, {n, "cloud-hypervisor", true}, {n, "firecracker", false},
	} {
		if ok, why := OverlayMonitorFor(c.rt, c.hv); ok != c.want || (!ok && why == "") {
			t.Errorf("%T %q: %v %q, want %v with a reason when false", c.rt, c.hv, ok, why, c.want)
		}
	}
	t.Setenv("HULL_ROOTFS_MODE", "overlay-block")
	if ok, why := OverlayMonitorFor(h, "vz"); ok || !strings.Contains(why, "HULL_ROOTFS_MODE") {
		t.Errorf("HULL_ROOTFS_MODE set: %v %q", ok, why)
	}
	t.Setenv("HULL_ROOTFS_MODE", "")
	t.Setenv("HULL_HVI_ROOTFS_UPPER", "/x")
	if ok, why := OverlayMonitorFor(h, "hvi"); ok || !strings.Contains(why, "HULL_HVI_ROOTFS_UPPER") {
		t.Errorf("HULL_HVI_ROOTFS_UPPER set: %v %q", ok, why)
	}
}
