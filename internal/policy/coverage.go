package policy

import (
	"fmt"

	"github.com/brig-sh/brig/internal/profile"
)

// NotEnforcedNote is what a command prints to say that binding a policy
// records an intention and nothing more: no part of brig reads these
// bindings when an agent runs, so an attached policy does not constrain
// anything yet. docs/policies.md says this outright, and the commands say
// it too -- the terminal is where someone acts, and a reader who never
// opens the docs would otherwise learn it from nowhere.
//
// A constant, and exported, so the sentence has one source. Two commands
// print it, and it is a claim about what brig does: the moment they
// disagree, one of them is lying about the tool it belongs to. Typed into
// each instead, nothing catches an edit that changes one and not the
// other.
const NotEnforcedNote = "note: no policy is enforced at runtime yet; this records the binding only"

// CheckCoverage reports whether an egress rule can be enforced against p at
// all: a shell or gui profile has no agent to hook a tool-call enforcer
// into, so no policy can bind it.
func CheckCoverage(p profile.Profile) error {
	switch {
	case p.IsShell():
		return fmt.Errorf("%s is kind: shell, which has no agent to hook an egress rule into", p.Name)
	case p.IsGUI():
		return fmt.Errorf("%s is kind: gui, which has no agent to hook an egress rule into", p.Name)
	}
	return nil
}
