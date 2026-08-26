package wrap

import (
	"strings"
	"testing"
)

// The hvi backend needs Apple's in-kernel interrupt controller, which exists
// only on macOS 15 and newer. A host below that floor is refused here, before
// anything is started, with a message that names the version and the bypass --
// rather than the VMM dying at boot with an unnamed dyld failure (#4).
func TestPreflightHypervisorRefusesHVIBelowMacOS15(t *testing.T) {
	old := &Config{MacOSVersion: func() string { return "14.5" }}

	err := old.preflightHypervisor("hvi")
	if err == nil {
		t.Fatal("hvi on macOS 14.5 must be refused")
	}
	if !strings.Contains(err.Error(), "BRIG_HYPERVISOR=vz") {
		t.Errorf("message does not name the bypass: %v", err)
	}
	if !strings.Contains(err.Error(), "14.5") {
		t.Errorf("message does not name the host version: %v", err)
	}
	if !strings.Contains(err.Error(), "macOS 15") {
		t.Errorf("message does not name the floor: %v", err)
	}

	// vz has a console and no dependency on the in-kernel controller, so it is
	// the working bypass and must proceed on the same host.
	if err := old.preflightHypervisor("vz"); err != nil {
		t.Errorf("vz must proceed on macOS 14.5: %v", err)
	}

	// Once the host is new enough, hvi is exactly what the profile asked for.
	newHost := &Config{MacOSVersion: func() string { return "15.0" }}
	if err := newHost.preflightHypervisor("hvi"); err != nil {
		t.Errorf("hvi must proceed on macOS 15.0: %v", err)
	}

	// Off macOS the version reader says "", and there is nothing to refuse: the
	// Linux runtime ignores the hypervisor field a profile still carries.
	off := &Config{MacOSVersion: func() string { return "" }}
	if err := off.preflightHypervisor("hvi"); err != nil {
		t.Errorf("hvi must proceed off macOS: %v", err)
	}

	// A version brig cannot parse is not grounds to block the run: that would
	// swap the boot crash for a refusal just as opaque.
	garbled := &Config{MacOSVersion: func() string { return "sonoma" }}
	if err := garbled.preflightHypervisor("hvi"); err != nil {
		t.Errorf("hvi must proceed on an unparseable version: %v", err)
	}
}
