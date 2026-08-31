package policy

import (
	"fmt"

	"github.com/brig-sh/brig/internal/profile"
)

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
