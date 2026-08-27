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

	got := (&nerdctl{bin: "/usr/local/bin/nerdctl"}).Isolation("")
	if got.Boundary != BoundaryVM {
		t.Errorf("the default shim is urunc, so the boundary is a microVM: %s", got.Line())
	}
	if !strings.Contains(got.Detail, uruncShim) {
		t.Errorf("the row does not name the shim that establishes it: %s", got.Line())
	}
}

// The case the row exists for. BRIG_CONTAINERD_RUNTIME=runc is a supported
// thing to ask for and it costs the kernel boundary, and until this row nothing
// said so: the block reported the same sandbox either way.
func TestNerdctlReportsASharedKernelWhenTheShimIsReplaced(t *testing.T) {
	t.Setenv("BRIG_CONTAINERD_RUNTIME", "runc")

	got := (&nerdctl{bin: "/usr/local/bin/nerdctl"}).Isolation("")
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

	got := (&nerdctl{bin: "/usr/local/bin/nerdctl"}).Isolation("")
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
	if got := d.Isolation("").Detail; !strings.HasPrefix(got, "docker over containerd") {
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
		got := (&hull{bin: "hull"}).Isolation(tc.asked)
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
