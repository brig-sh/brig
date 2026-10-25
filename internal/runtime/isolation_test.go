package runtime

import (
	"strings"
	"testing"
)

// The default is the whole promise: containerd hands the container to the
// urunc shim, so the guest on Linux has a kernel of its own the way the macOS
// guest does.
func TestNerdctlReportsAMicroVMOnTheUruncShim(t *testing.T) {
	t.Setenv("BRIG_CONTAINERD_RUNTIME", "")

	got := (&nerdctl{bin: "/usr/local/bin/nerdctl"}).Isolation(RunSpec{})
	if got.Boundary != BoundaryVM {
		t.Errorf("the default shim is urunc, so the boundary is a microVM: %s", got.Line())
	}
	if !strings.Contains(got.Detail, uruncShim) {
		t.Errorf("the row does not name the shim that establishes it: %s", got.Line())
	}
}

// brig now refuses to boot runc, but the row still names it: `brig info`
// resolves the shim without booting, so a reader who set
// BRIG_CONTAINERD_RUNTIME=runc still sees what it would be. It costs the kernel
// boundary, and until this row nothing said so: the block reported the same
// sandbox either way.
func TestNerdctlReportsASharedKernelWhenTheShimIsReplaced(t *testing.T) {
	t.Setenv("BRIG_CONTAINERD_RUNTIME", "runc")

	got := (&nerdctl{bin: "/usr/local/bin/nerdctl"}).Isolation(RunSpec{})
	if got.Boundary != BoundaryContainer {
		t.Errorf("runc shares the host kernel, so this is a container: %s", got.Line())
	}
	if !strings.Contains(got.Detail, "shares the host kernel") {
		t.Errorf("the row does not say what the replacement costs: %s", got.Line())
	}
	if strings.Contains(got.Line(), string(BoundaryVM)) {
		t.Errorf("the row claimed a microVM the shim does not boot: %s", got.Line())
	}
}

// A shim brig does not know may well boot a VM -- kata does -- and brig cannot
// establish that from a name. The row says so rather than picking the answer
// the reader would rather have.
func TestNerdctlWillNotGuessAtAnUnknownShim(t *testing.T) {
	t.Setenv("BRIG_CONTAINERD_RUNTIME", "io.containerd.kata.v2")

	got := (&nerdctl{bin: "/usr/local/bin/nerdctl"}).Isolation(RunSpec{})
	if got.Boundary != BoundaryUnknown {
		t.Errorf("an unrecognised shim is not established either way: %s", got.Line())
	}
	if !strings.Contains(got.Detail, "io.containerd.kata.v2") {
		t.Errorf("the row does not name the shim it cannot place: %s", got.Line())
	}
	if !strings.Contains(got.Detail, "cannot tell") {
		t.Errorf("the row does not say brig cannot tell: %s", got.Line())
	}
}

// One adapter drives nerdctl and docker both, and it used to report "nerdctl"
// for either. A row about a runtime the reader has not installed is a row they
// cannot check.
func TestDockerIsReportedAsDocker(t *testing.T) {
	t.Setenv("BRIG_CONTAINERD_RUNTIME", "")

	d := &nerdctl{bin: "/usr/bin/docker"}
	if got := d.Kind(); got != "docker" {
		t.Errorf("Kind() = %q, want docker", got)
	}
	if got := d.Isolation(RunSpec{}).Detail; !strings.HasPrefix(got, "docker over containerd") {
		t.Errorf("the isolation row does not name docker: %s", got)
	}
	if got := (&nerdctl{bin: "/usr/local/bin/nerdctl"}).Kind(); got != "nerdctl" {
		t.Errorf("Kind() = %q, want nerdctl", got)
	}
}

// Every hull backend is a hypervisor, so the boundary does not turn on which
// one -- but which one is named, because it decides what that VM can do. An
// unset backend reports the one that would boot rather than a blank.
func TestHullReportsAMicroVMAndTheBackendUnderIt(t *testing.T) {
	for _, tc := range []struct{ asked, want string }{
		{"", "vz"},
		{"vz", "vz"},
		{"hvi", "hvi"},
		{"qemu", "qemu"},
	} {
		got := (&hull{bin: "hull"}).Isolation(RunSpec{Hypervisor: tc.asked})
		if got.Boundary != BoundaryVM {
			t.Errorf("hull on %q is a microVM: %s", tc.asked, got.Line())
		}
		if !strings.Contains(got.Detail, tc.want+" backend") {
			t.Errorf("hull on %q does not name the %s backend: %s", tc.asked, tc.want, got.Line())
		}
	}
}

// The row reads as one sentence in the envelope's own shape: the boundary,
// then the detail in parentheses, the way the network and sandbox rows do.
func TestIsolationLineIsTheBoundaryThenTheDetail(t *testing.T) {
	got := Isolation{BoundaryVM, "hull, vz backend"}.Line()
	if got != "microVM (hull, vz backend)" {
		t.Errorf("Line() = %q", got)
	}
}

// A path to runc or crun is the same plain container as the bare name, because
// nerdctl runs it as the runc shim's binary. The row says so and names the path
// that was set, rather than calling the boundary unknown.
func TestNerdctlReportsASharedKernelForAPathToRuncOrCrun(t *testing.T) {
	for _, shim := range []string{"/usr/bin/runc", "/usr/bin/crun", "./crun"} {
		t.Setenv("BRIG_CONTAINERD_RUNTIME", shim)

		got := (&nerdctl{bin: "/usr/local/bin/nerdctl"}).Isolation(RunSpec{})
		if got.Boundary != BoundaryContainer {
			t.Errorf("%s shares the host kernel, so this is a container: %s", shim, got.Line())
		}
		if !strings.Contains(got.Detail, shim+": the guest shares the host kernel") {
			t.Errorf("the row does not name the path and what it costs: %s", got.Line())
		}
	}
}

// The runc shim's own binary, by name or by path, boots on io.containerd.runc.v2
// as the bare runtimes do. It passed the refusal and created a container on the
// runc shim (#468), so CanRun must refuse it and Run must not reach the runtime.
func TestNerdctlRefusesTheRuncShimBinaryByNameOrPath(t *testing.T) {
	for _, shim := range []string{
		"containerd-shim-runc-v2",
		"containerd-shim-runc-v1",
		"/home/u/.local/share/brig/data/bin/containerd-shim-runc-v2",
		"./containerd-shim-runc-v2",
	} {
		t.Setenv("BRIG_CONTAINERD_RUNTIME", shim)
		n := &nerdctl{bin: stubRuntimeBin(t, "STUB RAN", 0)}

		got := n.Isolation(RunSpec{})
		if got.Boundary != BoundaryContainer {
			t.Errorf("%s boots the runc shim, so this is a container: %s", shim, got.Line())
		}
		if !strings.Contains(got.Detail, shim+": the guest shares the host kernel") {
			t.Errorf("the row does not name the value and what it costs: %s", got.Line())
		}
		if err := n.CanRun(RunSpec{Name: "brig-x", Image: "img"}); err == nil {
			t.Errorf("%s: CanRun let the runc shim through", shim)
		}
		err := n.Run(RunSpec{Name: "brig-x", Image: "img"})
		if err == nil || strings.Contains(err.Error(), "STUB RAN") {
			t.Errorf("%s: Run reached the runtime instead of refusing: %v", shim, err)
		}
	}
}

// The refusal of the runc shim's binary is keyed on its file name, and the
// microVM shim's binary sits one letter away from it. A urunc binary added to
// the refused set, or a bare containerd-shim-urunc-v2 mistaken for the runc
// shim, would stop brig booting the one shim it exists for. brig cannot see
// nerdctl's PATH, so it does not credit the name with a microVM either: the row
// stays unknown, and CanRun lets it through.
func TestNerdctlKeepsTheUruncShimBinaryAllowedByNameOrPath(t *testing.T) {
	for _, shim := range []string{
		"containerd-shim-urunc-v2",
		"/usr/local/bin/containerd-shim-urunc-v2",
		"./containerd-shim-urunc-v2",
	} {
		t.Setenv("BRIG_CONTAINERD_RUNTIME", shim)
		n := &nerdctl{bin: stubRuntimeBin(t, "STUB RAN", 0)}

		if err := n.CanRun(RunSpec{Name: "brig-x", Image: "img"}); err != nil {
			t.Errorf("%s: CanRun refused the urunc shim binary: %v", shim, err)
		}
		if got := n.Isolation(RunSpec{}); got.Boundary != BoundaryUnknown {
			t.Errorf("%s is neither the runc shim nor provably a microVM: %s", shim, got.Line())
		}
	}
}

// containerd starts a shim by the last two dot-separated parts of the runtime
// name (BinaryName in containerd's pkg/shim), so the rest of the name says
// nothing about what boots. Each name is placed by the shim it resolves to. A
// runc shim under any prefix is a container and refused before the runtime
// runs. urunc's v2 shim alone is a microVM. A shim brig cannot place -- crun
// ships none, so io.containerd.crun.v2 is whatever containerd-shim-crun-v2 is
// -- stays unknown and is allowed, as does a dotted name outside
// io.containerd. that resolves to urunc, since nerdctl may find a binary of
// that name first.
func TestNerdctlPlacesAShimNameByTheShimContainerdStarts(t *testing.T) {
	for _, tc := range []struct {
		shim string
		want Boundary
	}{
		{"io.containerd.runc.v2", BoundaryContainer},
		{"io.containerd.runc.v1", BoundaryContainer},
		{"x.runc.v2", BoundaryContainer},
		{"runc.v2", BoundaryContainer},
		{"io.containerd.foo.runc.v2", BoundaryContainer},
		{"io.containerd.urunc.runc.v2", BoundaryContainer},
		{"io.containerd.urunc.v2", BoundaryVM},
		{"io.containerd.foo.urunc.v2", BoundaryVM},
		{"io.containerd.urunc.v3", BoundaryUnknown},
		{"x.urunc.v2", BoundaryUnknown},
		{"io.containerd.crun.v2", BoundaryUnknown},
		{"io.containerd.kata.v2", BoundaryUnknown},
		// nerdctl's prefix is io.containerd. with its dot, at the start. These
		// two are looked up on PATH first, like x.urunc.v2.
		{"io.containerdx.urunc.v2", BoundaryUnknown},
		{"x.io.containerd.urunc.v2", BoundaryUnknown},
		// containerd refuses a relative name with a slash before it derives a
		// shim, so neither of these starts the shim its last parts name.
		{"io.containerd.x/y.urunc.v2", BoundaryUnknown},
		{"io.containerd.x/y.runc.v2", BoundaryUnknown},
	} {
		t.Setenv("BRIG_CONTAINERD_RUNTIME", tc.shim)
		n := &nerdctl{bin: stubRuntimeBin(t, "STUB RAN", 0)}

		got := n.Isolation(RunSpec{})
		if got.Boundary != tc.want {
			t.Errorf("%s: the row says %s, want %s", tc.shim, got.Line(), tc.want)
		}
		if !strings.Contains(got.Detail, tc.shim) {
			t.Errorf("%s: the row does not name the value that was set: %s", tc.shim, got.Line())
		}
		err := n.CanRun(RunSpec{Name: "brig-x", Image: "img"})
		if refused, want := err != nil, tc.want == BoundaryContainer; refused != want {
			t.Errorf("%s: CanRun refused = %v, want %v (%v)", tc.shim, refused, want, err)
		}
		if tc.want != BoundaryContainer {
			continue
		}
		err = n.Run(RunSpec{Name: "brig-x", Image: "img"})
		if err == nil || strings.Contains(err.Error(), "STUB RAN") {
			t.Errorf("%s: Run reached the runtime instead of refusing: %v", tc.shim, err)
		}
	}
}

// io.containerd.urunc.runc.v2 starts containerd-shim-runc-v2. The row matched
// the io.containerd.urunc. prefix and called that a microVM, which is the one
// mistake the row must not make: the stronger boundary over a plain container.
func TestNerdctlDoesNotCallARuncShimUnderAUruncPrefixAMicroVM(t *testing.T) {
	t.Setenv("BRIG_CONTAINERD_RUNTIME", "io.containerd.urunc.runc.v2")

	n := &nerdctl{bin: "/usr/local/bin/nerdctl"}
	if got := n.Isolation(RunSpec{}); got.Boundary != BoundaryContainer {
		t.Errorf("the name resolves to the runc shim, so this is a container: %s", got.Line())
	}
	if err := n.CanRun(RunSpec{Name: "brig-x", Image: "img"}); err == nil {
		t.Error("CanRun let a runc shim through under a urunc prefix")
	}
}

// The row names only a hypervisor brig hands urunc. Outside a generic boot brig
// passes none, so urunc picks, and a macOS backend setting that leaked into the
// spec is not one either: nerdctl has no flag for it.
func TestNerdctlNamesNoHypervisorItDidNotPass(t *testing.T) {
	t.Setenv("BRIG_CONTAINERD_RUNTIME", "")

	got := (&nerdctl{bin: "/usr/local/bin/nerdctl"}).Isolation(RunSpec{Hypervisor: "hvi"}).Line()
	for _, never := range []string{monitorCloudHypervisor, "hvi"} {
		if strings.Contains(got, never) {
			t.Errorf("the row names %s, which brig did not pass: %s", never, got)
		}
	}
	if !strings.Contains(got, "hypervisor urunc default") {
		t.Errorf("the row does not say urunc picks the hypervisor: %s", got)
	}
}

// docker cannot carry the annotation, so brig refuses the generic boot there
// and passes no monitor. The row must not name the one a nerdctl boot passes.
func TestDockerNamesNoHypervisorOnAGenericBoot(t *testing.T) {
	t.Setenv("BRIG_CONTAINERD_RUNTIME", "")

	got := (&nerdctl{bin: "/usr/bin/docker"}).Isolation(RunSpec{GenericBoot: true}).Line()
	if strings.Contains(got, monitorCloudHypervisor) {
		t.Errorf("the row names a monitor docker is never handed: %s", got)
	}
}

// RootfsType has no nerdctl flag, so it never reaches urunc. Naming it here
// reports a setting the run ignores.
func TestNerdctlNamesNoRootfsType(t *testing.T) {
	t.Setenv("BRIG_CONTAINERD_RUNTIME", "")

	got := (&nerdctl{bin: "/usr/local/bin/nerdctl"}).Isolation(RunSpec{RootfsType: "block"}).Line()
	if strings.Contains(got, "rootfs") || strings.Contains(got, "block") {
		t.Errorf("the row names a rootfs type nerdctl ignores: %s", got)
	}
}

// The variable is named only when it is the reason for the shim. On a default
// install it is unset, and naming it there sends the reader after a setting
// nobody made.
func TestNerdctlNamesNoSettingWhenUnset(t *testing.T) {
	t.Setenv("BRIG_CONTAINERD_RUNTIME", "")

	got := (&nerdctl{bin: "/usr/local/bin/nerdctl"}).Isolation(RunSpec{}).Line()
	if strings.Contains(got, "BRIG_CONTAINERD_RUNTIME") {
		t.Errorf("the row names a setting that is not set: %s", got)
	}
}

// Set, the variable is what decided the boundary, so the row names it with the
// value. A shared-kernel shim takes no hypervisor, so none is named.
func TestNerdctlNamesTheSettingBehindTheShim(t *testing.T) {
	for _, shim := range []string{"runc", uruncShim, "io.containerd.kata.v2"} {
		t.Setenv("BRIG_CONTAINERD_RUNTIME", shim)

		got := (&nerdctl{bin: "/usr/local/bin/nerdctl"}).Isolation(RunSpec{GenericBoot: true}).Line()
		if !strings.Contains(got, "BRIG_CONTAINERD_RUNTIME="+shim) {
			t.Errorf("the row does not name the setting behind %s: %s", shim, got)
		}
		if shim != uruncShim && strings.Contains(got, "hypervisor") {
			t.Errorf("the row names a hypervisor for %s, which is not urunc: %s", shim, got)
		}
	}
}

// On a generic boot brig tells urunc which monitor to use, so the row names
// that one: the same value runArgs puts in the annotation.
func TestNerdctlNamesTheMonitorItPassesOnAGenericBoot(t *testing.T) {
	t.Setenv("BRIG_CONTAINERD_RUNTIME", "")

	n := &nerdctl{bin: "/usr/local/bin/nerdctl"}
	got := n.Isolation(RunSpec{GenericBoot: true}).Line()
	if !strings.Contains(got, "hypervisor "+monitorCloudHypervisor) {
		t.Errorf("the row does not name the monitor brig passes: %s", got)
	}
	if m := n.monitor(RunSpec{GenericBoot: true}); m != monitorCloudHypervisor {
		t.Errorf("monitor() = %q, want %q, the value the annotation carries", m, monitorCloudHypervisor)
	}
}

// hull takes --rootfs-type, so the row names the value brig passes. With none
// passed, hull picks per backend, and the row says so without guessing which.
func TestHullNamesTheRootfsTypeItPasses(t *testing.T) {
	got := (&hull{bin: "hull"}).Isolation(RunSpec{Hypervisor: "hvi", RootfsType: "block"}).Line()
	if !strings.Contains(got, "rootfs block") {
		t.Errorf("the row does not name the rootfs type brig passes: %s", got)
	}

	got = (&hull{bin: "hull"}).Isolation(RunSpec{}).Line()
	if !strings.Contains(got, "rootfs hull default") {
		t.Errorf("the row does not say hull picks the rootfs: %s", got)
	}
	for _, never := range []string{"block", "virtiofs", "9pfs"} {
		if strings.Contains(got, never) {
			t.Errorf("the row names %s, which brig did not pass: %s", never, got)
		}
	}
}
