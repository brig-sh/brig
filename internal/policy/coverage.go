package policy

import (
	"fmt"

	"github.com/brig-sh/brig/internal/profile"
)

// EnforcementNote is what a command prints to say where a binding is acted on
// and where it is still only recorded. A run on the hvi backend reads these
// bindings and puts the rules on the network gateway it gives that sandbox;
// every other backend takes its network from somewhere brig does not filter,
// and refuses the boot rather than pretending otherwise. docs/policies.md says
// this at length, and the commands say it too -- the terminal is where someone
// acts, and a reader who never opens the docs would otherwise learn it from
// nowhere.
//
// A constant, and exported, so the sentence has one source. Two commands print
// it, and it is a claim about what brig does: the moment they disagree, one of
// them is lying about the tool it belongs to. Typed into each instead, nothing
// catches an edit that changes one and not the other.
const EnforcementNote = "note: enforced on the hvi backend, which gives the sandbox a network of " +
	"its own; a run on any other backend is refused rather than left unenforced"

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
