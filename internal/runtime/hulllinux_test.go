package runtime

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// linuxHull is the adapter as newHull builds it on a Linux host.
func linuxHull() *hull { return &hull{bin: "hull", linux: true} }

// A policy on hull on Linux is refused, and BRIG_HYPERVISOR=hvi does not get
// it past: the hvi row is the macOS gateway's, and nothing on Linux puts the
// rules anywhere yet. The refusal names the Linux path, so the remedy is the
// one that works there.
func TestLinuxHullRefusesAPolicyOnEveryMonitor(t *testing.T) {
	for _, hv := range []string{"", "vz", "hvi", "cloud-hypervisor"} {
		spec := RunSpec{Name: "brig-s", Image: "img", Net: "isolated", Hypervisor: hv,
			Egress: Egress{Default: "deny"}}
		err := linuxHull().CanRun(spec)
		var ce *CapabilityError
		if !errors.As(err, &ce) {
			t.Fatalf("hypervisor %q: a policy was not refused with a CapabilityError: %v", hv, err)
		}
		if ce.Path != linuxHullPath || ce.State != CannotEnforce || ce.Property != EgressPolicy {
			t.Errorf("hypervisor %q: refused as %s on %s (%s), want %s on %s (%s)", hv,
				ce.Property, ce.Path, ce.State, EgressPolicy, linuxHullPath, CannotEnforce)
		}
		if strings.Contains(err.Error(), "BRIG_HYPERVISOR=hvi") {
			t.Errorf("hypervisor %q: the refusal sends a Linux user to hvi: %v", hv, err)
		}
	}
}

// brig.internal, and any diverted destination, are refused the same way: a
// connection the broker should take would be routed directly instead.
func TestLinuxHullRefusesADivert(t *testing.T) {
	for name, d := range map[string]Divert{
		"brig.internal": {BrigInternal: true, Socket: "/tmp/divert.sock"},
		"a route":       {Routes: []DivertRoute{{Host: "169.254.170.2", Port: 80, Mode: v1alpha1.ModeEndpoint}}, Socket: "/tmp/divert.sock"},
	} {
		spec := RunSpec{Name: "brig-s", Image: "img", Net: "isolated", Hypervisor: "hvi", Egress: Egress{Divert: d}}
		var ce *CapabilityError
		if err := linuxHull().CanRun(spec); !errors.As(err, &ce) || ce.Path != linuxHullPath {
			t.Errorf("%s: not refused on %s: %v", name, linuxHullPath, err)
		}
	}
}

// The rest of what hull on Linux cannot do yet is refused rather than booted
// without: a window, a network of the sandbox's own, a published port, and a
// monitor or a rootfs mode it would not boot.
//
// Each is checked for its own reason, so a case refused by an earlier check
// does not pass for the wrong one. The host is the overlay host with no
// mke2fs, so the snapshotter of the developer's own containerd does not
// refuse the overlay case first.
func TestLinuxHullRefusesWhatItCannotRunYet(t *testing.T) {
	overlayHost(t)
	lookMke2fs = func() bool { return false }
	for name, c := range map[string]struct {
		spec RunSpec
		want string
	}{
		"gui":              {RunSpec{GUI: true}, "cannot display on Linux"},
		"isolated":         {RunSpec{Net: "isolated"}, "--network isolated"},
		"publish":          {RunSpec{Net: "shared", Publish: []Publication{{HostPort: 3000, GuestPort: 3000}}}, "cannot publish a port yet"},
		"offline publish":  {RunSpec{Net: "none", Publish: []Publication{{HostPort: 3000, GuestPort: 3000}}}, "--network offline"},
		"unknown monitor":  {RunSpec{GenericBoot: true, Hypervisor: "xen"}, "not one urunc runs on Linux"},
		"unknown rootfs":   {RunSpec{GenericBoot: true, RootfsMode: "tmpfs"}, "is not a rootfs mode"},
		"overlay, no host": {RunSpec{GenericBoot: true, RootfsMode: RootfsModeOverlayBlock}, "urunc formats the disks"},
	} {
		c.spec.Name, c.spec.Image = "brig-s", "img"
		err := linuxHull().CanRun(c.spec)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want a refusal naming %q", name, err, c.want)
		}
	}
}

// The ISOLATION row and the boot ask hull on Linux what CanRun and doctor
// ask: whether urunc can build an overlay root here. They asked the macOS
// question, so with mke2fs on PATH and no Homebrew, doctor said the overlay
// root was available, and the run told the host to brew install e2fsprogs.
func TestHullOnLinuxBootsTheRootfsModeDoctorReports(t *testing.T) {
	overlayHost(t)
	hullMke2fs = "/nonexistent/homebrew/sbin/mke2fs"
	t.Setenv("BRIG_BOOT_ASSETS", "")
	h := linuxHull()
	if got := OverlayRootFor(h); !got.Available {
		t.Fatalf("doctor: %+v, want available", got)
	}
	given := BootAssets{Kernel: "/given/" + bootKernelName(), Initrd: "/given/" + bootInitrdName}
	locate := func() (string, error) { return t.TempDir(), nil }
	// Asked for, and the default where the host can build it.
	for _, asked := range []string{RootfsModeOverlayBlock, ""} {
		spec := RunSpec{Name: "brig-s", Image: "img", GenericBoot: true, BootAssets: given,
			Hypervisor: "hvi", RootfsMode: asked}
		if err := h.CanRun(spec); err != nil {
			t.Fatalf("%q: CanRun refused what doctor reports available: %v", asked, err)
		}
		if row := h.Isolation(spec).Detail; !strings.Contains(row, "rootfs "+RootfsModeOverlayBlock) {
			t.Errorf("%q: the ISOLATION row is %q, want the overlay root", asked, row)
		}
		_, env, err := runArgsFor(h.rootfsMode, spec, "hvi", "shared", "", "", locate, nil)
		if err != nil {
			t.Errorf("%q: the boot refused what doctor reports available: %v", asked, err)
			continue
		}
		if !slices.Contains(env, "HULL_ROOTFS_MODE="+RootfsModeOverlayBlock) {
			t.Errorf("%q: the boot hands no overlay root: %q", asked, env)
		}
	}
}

// The ISOLATION row of hull on Linux is the urunc row, whole. Its hypervisor
// is the monitor the guest boots on, hvi without one or with vz, and urunc's
// default where brig names none, as on a boot that brings its own image. Its
// rootfs is the mode brig hands urunc or urunc's default, never hull's macOS
// default or a --rootfs-type hull on Linux does not pass. Each row is the one
// nerdctl prints for the same run, so the two Linux paths cannot drift apart.
func TestLinuxHullIsolationIsTheUruncRow(t *testing.T) {
	overlayHost(t)
	h, n := linuxHull(), &nerdctl{bin: "nerdctl"}
	row := func(rest string) string { return "hull over containerd, " + uruncShim + ", " + rest }
	check := func(host string, cases []struct {
		spec RunSpec
		want string
	}) {
		for _, c := range cases {
			got := h.Isolation(c.spec).Detail
			if got != c.want {
				t.Errorf("%s, %+v: the ISOLATION row is %q, want %q", host, c.spec, got, c.want)
			}
			if same := "hull" + strings.TrimPrefix(n.Isolation(c.spec).Detail, "nerdctl"); got != same {
				t.Errorf("%s, %+v: the row is %q, and nerdctl's for the same run is %q", host, c.spec, got, same)
			}
			// brig plan's RUNTIME row names the same monitor, as nerdctl's does.
			b := h.Backend(c.spec)
			if !strings.Contains(got+",", "hypervisor "+orDefault(b, "urunc default")+",") {
				t.Errorf("%s, %+v: Backend says %q, the row is %q", host, c.spec, b, got)
			}
			if nb := n.Backend(c.spec); b != nb {
				t.Errorf("%s, %+v: Backend says %q, nerdctl's for the same run %q", host, c.spec, b, nb)
			}
		}
	}
	check("overlay host", []struct {
		spec RunSpec
		want string
	}{
		{RunSpec{GenericBoot: true}, row("hypervisor hvi, rootfs overlay-block")},
		{RunSpec{GenericBoot: true, Hypervisor: "vz"}, row("hypervisor hvi, rootfs overlay-block")},
		{RunSpec{GenericBoot: true, Hypervisor: "firecracker"}, row("hypervisor firecracker, rootfs urunc default")},
		{RunSpec{GenericBoot: true, RootfsMode: RootfsModeVirtiofs}, row("hypervisor hvi, rootfs urunc default")},
		// CanRun refuses it; the row names what was asked, not a default.
		{RunSpec{GenericBoot: true, Hypervisor: "krun"}, row("hypervisor krun, rootfs urunc default")},
		// brig names no monitor and no root for an image that brings its own.
		{RunSpec{}, row("hypervisor urunc default")},
		{RunSpec{Hypervisor: "vz"}, row("hypervisor urunc default")},
	})
	lookMke2fs = func() bool { return false }
	check("no mke2fs", []struct {
		spec RunSpec
		want string
	}{
		{RunSpec{GenericBoot: true}, row("hypervisor hvi, rootfs urunc default")},
		{RunSpec{GenericBoot: true, RootfsType: "block"}, row("hypervisor hvi, rootfs urunc default")},
	})
	// brig plan asks a runtime for its backend only through BackendNamer.
	if _, ok := Runtime(h).(BackendNamer); !ok {
		t.Error("hull is not a BackendNamer, so brig plan would name the hypervisor asked, not the monitor")
	}
	if path := RoutePath(h, ""); path.Backend != uruncShim {
		t.Errorf("RoutePath is %+v, want the shim the row names", path)
	}
}

// On macOS hull boots the hypervisor asked for, so brig plan names that one,
// as it did before hull named a backend: only hull on Linux maps vz to hvi.
func TestMacOSHullNamesTheHypervisorAsked(t *testing.T) {
	h := &hull{bin: "hull"}
	for _, hv := range []string{"", "vz", "hvi"} {
		for _, generic := range []bool{false, true} {
			if b := h.Backend(RunSpec{Hypervisor: hv, GenericBoot: generic}); b != hv {
				t.Errorf("%q, generic boot %v: Backend says %q, want the hypervisor asked", hv, generic, b)
			}
		}
	}
}

// What hull on Linux can run, it runs: no policy, on the shared bridge or
// offline, a monitor urunc runs or none.
func TestLinuxHullAcceptsAPlainRun(t *testing.T) {
	for _, net := range []string{"", "shared", "none"} {
		for _, hv := range []string{"", "vz", "hvi", "cloud-hypervisor"} {
			spec := RunSpec{Name: "brig-s", Image: "img", Net: net, Hypervisor: hv, GenericBoot: true}
			if err := linuxHull().CanRun(spec); err != nil {
				t.Errorf("net %q, hypervisor %q: %v", net, hv, err)
			}
		}
	}
	// Offline satisfies every rule set, here as on every other path.
	if err := linuxHull().CanRun(RunSpec{Name: "brig-s", Image: "img", Net: "none",
		Egress: Egress{Default: "deny"}}); err != nil {
		t.Errorf("an offline run under a policy: %v", err)
	}
}

// The route mode table answers for hull on Linux under its own path, so a
// route never reads as served there, whatever BRIG_HYPERVISOR says.
func TestLinuxHullServesNoRouteMode(t *testing.T) {
	for _, hv := range []string{"", "hvi"} {
		path := RoutePath(linuxHull(), hv)
		if path != linuxHullPath {
			t.Errorf("hypervisor %q: route path %s, want %s", hv, path, linuxHullPath)
		}
		if modes := ServedModes(path); len(modes) != 0 {
			t.Errorf("hypervisor %q: hull on Linux serves %v", hv, modes)
		}
	}
	// The macOS hull keeps its path.
	if path := RoutePath(&hull{bin: "hull"}, "hvi"); path != (RunPath{"hull", "hvi"}) {
		t.Errorf("macOS hull on hvi: route path %s", path)
	}
}

// brig doctor asks hull on Linux what its run asks: urunc's host check, not
// the macOS mke2fs from Homebrew. A Linux host with mke2fs on PATH and no
// Homebrew reads available, and one without mke2fs is not told to run brew.
func TestDoctorAsksHullOnLinuxTheUruncOverlayRoot(t *testing.T) {
	overlayHost(t)
	hullMke2fs = "/nonexistent/homebrew/sbin/mke2fs"

	if got := OverlayRootFor(linuxHull()); !got.Available {
		t.Errorf("mke2fs on PATH, no Homebrew: %+v, want available", got)
	}
	if got := OverlayRootFor(&hull{bin: "hull"}); got.Available {
		t.Errorf("the macOS hull no longer asks for its own mke2fs: %+v", got)
	}

	lookMke2fs = func() bool { return false }
	got := OverlayRootFor(linuxHull())
	if got.Available || strings.Contains(got.Why, "brew") || !strings.Contains(got.Why, "e2fsprogs") {
		t.Errorf("no mke2fs: %+v, want unavailable with urunc's fix and no brew", got)
	}
}

// The monitor half, as doctor reports it and the run decides it: urunc's
// monitors, not hull's macOS backends. vz, which macOS alone has, keeps the
// bundle's hvi, as on the run.
func TestDoctorAsksHullOnLinuxTheUruncMonitor(t *testing.T) {
	overlayHost(t)
	for _, c := range []struct {
		hv   string
		want bool
	}{
		{"", true}, {"vz", true}, {"hvi", true}, {"cloud-hypervisor", true}, {"qemu", true},
		{"firecracker", false}, {"xen", false},
	} {
		ok, why := OverlayMonitorFor(linuxHull(), c.hv)
		if ok != c.want || (!ok && why == "") {
			t.Errorf("%q: %v %q, want %v with a reason when false", c.hv, ok, why, c.want)
		}
		mode, _ := linuxHullRootfsMode(RunSpec{GenericBoot: true, Hypervisor: c.hv})
		if (mode == RootfsModeOverlayBlock) != ok {
			t.Errorf("%q: doctor says %v and the run picks %q", c.hv, ok, mode)
		}
	}
}
