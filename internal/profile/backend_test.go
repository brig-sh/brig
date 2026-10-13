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

// teleportable: survives the parser, or a profile asking for sessions that own
// their storage would boot ordinary ones with a host home.
func TestParseKeepsTeleportable(t *testing.T) {
	got, err := Parse([]byte(`
name: p
image: img
guestHome: /root
kind: shell
binary: sh
mem: 1
cpus: 1
hypervisor: hvi
teleportable: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Teleportable {
		t.Fatalf("teleportable did not survive parsing: %+v", got)
	}
}

// A teleportable profile is held to the backend and the root it needs when it
// loads. Each refusal names the field to change, so the reader is sent to the
// line that contradicts the request rather than to a boot that fails.
func TestValidateRefusesATeleportableProfileItCannotKeep(t *testing.T) {
	ok := backendBase()
	ok.Teleportable, ok.Hypervisor = true, "hvi"
	for _, rt := range []string{"", "block"} {
		ok.RootfsType = rt
		if err := ok.Validate(); err != nil {
			t.Errorf("teleportable on hvi with rootfsType %q must be accepted: %v", rt, err)
		}
	}

	for _, c := range []struct {
		what string
		edit func(*Profile)
		want string
	}{
		{"no hypervisor, which is vz", func(p *Profile) { p.Hypervisor = "" }, "names none, which is vz"},
		{"vz", func(p *Profile) { p.Hypervisor = "vz" }, "says vz"},
		{"qemu", func(p *Profile) { p.Hypervisor = "qemu" }, "says qemu"},
		{"a shared root", func(p *Profile) { p.RootfsType = "virtiofs" }, "rootfsType: virtiofs"},
		{"a gui", func(p *Profile) { p.Kind = KindGUI; p.Binary = "" }, "kind: gui"},
	} {
		p := backendBase()
		p.Teleportable, p.Hypervisor = true, "hvi"
		c.edit(&p)
		err := p.Validate()
		if err == nil {
			t.Errorf("%s: a teleportable profile was accepted", c.what)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: refusal does not say %q: %v", c.what, c.want, err)
		}
	}

	// Off, none of it applies: the same profile on vz is an ordinary one.
	p := backendBase()
	p.Hypervisor = "vz"
	if err := p.Validate(); err != nil {
		t.Errorf("a profile that does not ask to be teleportable was refused: %v", err)
	}
}
