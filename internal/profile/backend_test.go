package profile

import (
	"strings"
	"testing"
)

func backendBase() Profile {
	return Profile{Name: "p", Image: "img", GuestHome: "/root", Binary: "sh", Mem: 1, CPUs: 1}
}

// A typo in a backend name is caught against the profile that carries it,
// rather than travelling to a boot that fails naming a backend nobody meant.
func TestValidateRejectsAnUnknownHypervisor(t *testing.T) {
	for _, hv := range []string{"", "vz", "hvi", "qemu"} {
		p := backendBase()
		p.Hypervisor = hv
		if err := p.Validate(); err != nil {
			t.Errorf("hypervisor %q must be accepted: %v", hv, err)
		}
	}

	p := backendBase()
	p.Hypervisor = "hvii"
	err := p.Validate()
	if err == nil {
		t.Fatal("expected an unknown hypervisor to be refused")
	}
	if !strings.Contains(err.Error(), "hvi") {
		t.Errorf("error does not list the accepted backends: %v", err)
	}
}

func TestValidateRejectsAnUnknownRootfsType(t *testing.T) {
	for _, rt := range []string{"", "block", "virtiofs", "9pfs"} {
		p := backendBase()
		p.RootfsType = rt
		if err := p.Validate(); err != nil {
			t.Errorf("rootfsType %q must be accepted: %v", rt, err)
		}
	}

	p := backendBase()
	p.RootfsType = "ext4"
	if err := p.Validate(); err == nil {
		t.Fatal("expected an unknown rootfsType to be refused")
	}
}

// The new fields must survive a round trip through the parser, or a profile
// naming a backend would silently boot on the default one.
func TestParseKeepsTheBackendFields(t *testing.T) {
	got, err := Parse([]byte(`
name: p
image: img
guestHome: /root
kind: shell
binary: sh
mem: 1
cpus: 1
hypervisor: hvi
runtimeBin: /opt/hull
rootfsType: block
genericBoot: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Hypervisor != "hvi" || got.RuntimeBin != "/opt/hull" ||
		got.RootfsType != "block" || !got.GenericBoot {
		t.Fatalf("backend fields did not survive parsing: %+v", got)
	}
}
