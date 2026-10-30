package main

import (
	"strings"
	"testing"
)

// nerdctlBootRuntime is doctorRuntime under the name a Linux host detects.
type nerdctlBootRuntime struct{ doctorRuntime }

func (nerdctlBootRuntime) Kind() string { return "nerdctl" }

// The fetch brings an init that cannot mount the root urunc shares, and a run
// that took this advice failed in the guest with "exec sleep failed".
func TestDoctorBootDoesNotOfferTheFetchOnNerdctl(t *testing.T) {
	healthyHost(t)
	t.Setenv("BRIG_BOOT_ASSETS", "")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	rt := nerdctlBootRuntime{doctorRuntime{bin: "/nowhere/nerdctl"}}

	c := bootCheck(rt)
	if c.State != stateFail {
		t.Fatalf("boot state = %q, want fail", c.State)
	}
	if strings.Contains(c.Fix, "fetch") {
		t.Errorf("the boot line still offers the fetch on nerdctl: %q", c.Fix)
	}
	if !strings.Contains(c.Fix, "install.sh") {
		t.Errorf("the boot line does not name the installer: %q", c.Fix)
	}
}
