package cli

import (
	"os"

	"github.com/brig-sh/brig/internal/buildinfo"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/wrap"
)

// AttachedRun describes an attached run to a build's Attach hook.
type AttachedRun struct {
	Sandbox    string // the sandbox's name
	Ref        string // the agent, or agent@name, as the run names it
	Profile    string
	Runtime    string // the runtime's kind
	Version    string // brig's version
	Cols, Rows int    // the terminal's size now
}

// ErrRelayDeclined is what a relay returns, before it has run anything, to say
// the run goes the way it would with no relay: the runtime hands the process
// over as it does for any run.
var ErrRelayDeclined = runtime.ErrRelayDeclined

// attachHook is the hook the build set, nil for a build that sets none.
var attachHook func(AttachedRun) func(argv, env []string) (int, error)

// relayRun asks the build's hook, once, whether it runs this run's attached
// exec. The hook decides from what is local to this machine and touches
// neither the network nor the disk: the relay it returns is called by the
// runtime only after the exec's command is built, and may still decline. When it returns a function, relayRun sets it on cfg and reports true,
// and the caller takes the attached path with it. With no hook, a runtime
// other than hull, or a nil answer, it changes nothing and reports false, and
// the run goes the way it always has.
//
// Only hull is asked: it is the one runtime whose Attach honours a relay, so
// a hook is never handed a run it could not take.
func relayRun(cfg *wrap.Config, t profile.Profile) bool {
	if attachHook == nil || cfg.Runtime == nil || cfg.Runtime.Kind() != "hull" {
		return false
	}
	ref := t.Name
	if cfg.RawName != "" {
		ref += "@" + cfg.RawName
	}
	cols, rows := terminalSize()
	relay := attachHook(AttachedRun{
		Sandbox: cfg.VMName, Ref: ref, Profile: t.Name,
		Runtime: cfg.Runtime.Kind(), Version: buildinfo.Read().Version,
		Cols: cols, Rows: rows,
	})
	if relay == nil {
		return false
	}
	cfg.Relay = relay
	return true
}

// terminalSize is the size of the terminal brig writes to, else the one it
// reads from, else 80 by 24.
func terminalSize() (cols, rows int) {
	for _, f := range []*os.File{os.Stdout, os.Stdin} {
		if c, r := wrap.TerminalSize(f); c > 0 && r > 0 {
			return c, r
		}
	}
	return 80, 24
}
