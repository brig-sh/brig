package policy

import (
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
)

func TestCheckCoverageAcceptsAnAgentProfile(t *testing.T) {
	p := profile.Profile{Name: "claude-code", Kind: profile.KindAgent}
	if err := CheckCoverage(p); err != nil {
		t.Errorf("an agent profile was refused: %v", err)
	}
}

// The default kind, with nothing set, is agent -- so an empty Kind is
// accepted too.
func TestCheckCoverageAcceptsTheDefaultKind(t *testing.T) {
	p := profile.Profile{Name: "claude-code"}
	if err := CheckCoverage(p); err != nil {
		t.Errorf("a profile with no kind set was refused: %v", err)
	}
}

func TestCheckCoverageRefusesAShellProfile(t *testing.T) {
	p := profile.Profile{Name: "ubuntu", Kind: profile.KindShell}
	err := CheckCoverage(p)
	if err == nil {
		t.Fatal("a shell profile was accepted")
	}
	if !strings.Contains(err.Error(), "ubuntu") || !strings.Contains(err.Error(), "shell") {
		t.Errorf("the error does not name the profile and its kind: %v", err)
	}
}

func TestCheckCoverageRefusesAGUIProfile(t *testing.T) {
	p := profile.Profile{Name: "desktop", Kind: profile.KindGUI}
	err := CheckCoverage(p)
	if err == nil {
		t.Fatal("a gui profile was accepted")
	}
	if !strings.Contains(err.Error(), "desktop") || !strings.Contains(err.Error(), "gui") {
		t.Errorf("the error does not name the profile and its kind: %v", err)
	}
}
