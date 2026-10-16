package wrap

import (
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/runtime"
)

// diverts is the divert of this run: brig.internal, for a guest that reaches
// an endpoint route there, to the sandbox's divert socket. The socket path
// depends on the sandbox alone, so a run that finds the sandbox up compares
// the same divert its gateway was started with.
func (c *Config) diverts() runtime.Divert {
	if !c.brigInternal() {
		return runtime.Divert{}
	}
	dir, err := injector.Dir()
	if err != nil {
		return runtime.Divert{}
	}
	sock, err := injector.DivertSocket(dir, c.VMName)
	if err != nil {
		return runtime.Divert{}
	}
	return runtime.Divert{Socket: sock, BrigInternal: true}
}

// runtimeEgress is the egress the runtime is handed: the rules of the bound
// policies, and the divert of brig.internal.
func (c *Config) runtimeEgress() runtime.Egress {
	e := runtimeEgress(c.Egress)
	e.Divert = c.diverts()
	return e
}
