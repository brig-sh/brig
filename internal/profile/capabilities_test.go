package profile

import (
	"strings"
	"testing"
)

const capBase = "name: kvmtest\nimage: i\nguestHome: /home/k\nbinary: k\nmem: 1\ncpus: 1\n"

// Nested virtualization is off unless a profile lists it. The shipped specs
// are the profiles most runs use, so one of them gaining the capability would
// hand every one of those runs a hypervisor of its own without anyone having
// asked for it. This test is what makes the default a rule.
func TestNoShippedProfileDeclaresACapability(t *testing.T) {
	reset(t)
	if err := Load(); err != nil {
		t.Fatalf("the embedded specs did not load: %v", err)
	}
	for _, p := range All() {
		if IsCustom(p.Name) {
			continue
		}
		if len(p.Capabilities) > 0 {
			t.Errorf("the shipped %s profile declares capabilities %v; every shipped "+
				"profile must boot without one", p.Name, p.Capabilities)
		}
		if p.Nested() {
			t.Errorf("the shipped %s profile asks for nested virtualization", p.Name)
		}
	}
}

// kvm is the one capability, and it parses.
func TestCapabilityKVMParses(t *testing.T) {
	p, err := Parse([]byte(capBase + "capabilities: [kvm]\n"))
	if err != nil {
		t.Fatalf("capabilities: [kvm] was refused: %v", err)
	}
	if !p.Nested() {
		t.Errorf("a profile listing kvm does not report Nested: %+v", p.Capabilities)
	}
}

// Absent is the default, and a profile that says nothing gets nothing.
func TestAProfileWithoutCapabilitiesIsNotNested(t *testing.T) {
	p, err := Parse([]byte(capBase))
	if err != nil {
		t.Fatal(err)
	}
	if p.Nested() || len(p.Capabilities) != 0 {
		t.Errorf("a profile that lists no capability reports %v, Nested=%v", p.Capabilities, p.Nested())
	}
	empty, err := Parse([]byte(capBase + "capabilities: []\n"))
	if err != nil {
		t.Fatalf("an empty capability list was refused: %v", err)
	}
	if empty.Nested() {
		t.Error("an empty capability list reports Nested")
	}
}

// A capability widens the boundary, so a name brig does not know is refused.
// A misspelling that parsed would read as a profile that
// asked for something, and a future capability must not be granted by a brig
// too old to know what it grants.
func TestAnUnknownCapabilityIsRefused(t *testing.T) {
	for _, bad := range []string{"KVM", "\" kvm\"", "nested", "gpu", "\"\""} {
		_, err := Parse([]byte(capBase + "capabilities: [" + bad + "]\n"))
		if err == nil {
			t.Errorf("capability %s was accepted", bad)
			continue
		}
		if !strings.Contains(err.Error(), "is not one of: kvm") {
			t.Errorf("capability %s: the refusal does not name the known list: %v", bad, err)
		}
	}
}

func TestADuplicateCapabilityIsRefused(t *testing.T) {
	_, err := Parse([]byte(capBase + "capabilities: [kvm, kvm]\n"))
	if err == nil {
		t.Fatal("kvm listed twice was accepted")
	}
	if !strings.Contains(err.Error(), "twice") {
		t.Errorf("the refusal does not say the entry repeats: %v", err)
	}
}

// The strict decoder is what catches a misspelled key. capability: in the
// singular would otherwise decode into nothing and boot a guest without the
// thing the file plainly asked for.
func TestAMisspelledCapabilitiesKeyIsRefused(t *testing.T) {
	if _, err := Parse([]byte(capBase + "capability: [kvm]\n")); err == nil {
		t.Error("capability: (singular) parsed, so a typo silently drops the request")
	}
}

// Every other slice is cloned, and this one widens the boundary: a caller
// appending kvm through a clone would change what every later caller in the
// process boots.
func TestCloneDoesNotShareCapabilities(t *testing.T) {
	p := Profile{Capabilities: []string{CapabilityKVM}}
	c := p.clone()
	c.Capabilities[0] = "CLOBBERED"
	if p.Capabilities[0] != CapabilityKVM {
		t.Error("a clone shares the original's capability list")
	}
}

// Export writes the file verbatim for a profile that has one; for one built in
// Go it marshals, and the capability has to survive that trip or an exported
// kvm profile imports back without it.
func TestCapabilitiesSurviveTheJSONExport(t *testing.T) {
	p, err := Parse([]byte(capBase + "capabilities: [kvm]\n"))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := ExportJSON(p)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(blob)
	if err != nil {
		t.Fatalf("the export does not parse back: %v\n%s", err, blob)
	}
	if !back.Nested() {
		t.Errorf("kvm was lost on the way through JSON:\n%s", blob)
	}
}

// The header is how someone writing a profile learns a field exists.
func TestExportHeaderDocumentsCapabilities(t *testing.T) {
	if !strings.Contains(exportHeader, "#   capabilities") {
		t.Error("the export header does not document capabilities")
	}
}
