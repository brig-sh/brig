package wrap

import (
	"net/netip"
	"net/url"
	"slices"
	"strconv"

	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/runtime"
)

// diverts is the divert of this run, to the sandbox's divert socket: for
// each route in a mode besides endpoint that this build serves, the host and
// port of its upstream URL, whose connections the mode's divert handler in
// the broker gets; and brig.internal for a guest that reaches an endpoint
// route there. The socket path depends on the sandbox alone, so a run that
// finds the sandbox up compares the same divert its gateway was started
// with.
func (c *Config) diverts() runtime.Divert {
	var routes []runtime.DivertRoute
	for _, m := range c.otherModes() {
		if routeModeOf(m) == nil {
			continue
		}
		for _, r := range c.modeRoutes(m) {
			u, err := url.Parse(r.Upstream)
			if err != nil {
				continue
			}
			port := 443
			if p, err := strconv.Atoi(u.Port()); err == nil {
				port = p
			}
			routes = append(routes, runtime.DivertRoute{Host: u.Hostname(), Port: port, Mode: m})
		}
	}
	internal := c.brigInternal()
	if len(routes) == 0 && !internal {
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
	return runtime.Divert{Routes: routes, Socket: sock, BrigInternal: internal}
}

// runtimeEgress is the egress the runtime is handed: the rules of the bound
// policies, and the divert.
//
// Under a deny default, the host of each diverted route is allowed too. The
// divert learns a host's addresses from the answers the guest gets, so a
// refused name would leave nothing to divert. A deny rule that names the
// host still wins.
func (c *Config) runtimeEgress() runtime.Egress {
	e := runtimeEgress(c.Egress)
	e.Divert = c.diverts()
	if e.Default == "deny" {
		e.Allow = allowDiverted(e.Allow, e.Divert)
	}
	return e
}

// allowDiverted returns allow with a rule for the host of each route of d
// that it lacks: a host rule for a name, and a /32 for an IPv4 address.
func allowDiverted(allow []runtime.Rule, d runtime.Divert) []runtime.Rule {
	for _, r := range d.Routes {
		rule := runtime.Rule{Host: r.Host}
		if a, err := netip.ParseAddr(r.Host); err == nil {
			rule = runtime.Rule{CIDR: netip.PrefixFrom(a, a.BitLen()).String()}
		}
		if !slices.Contains(allow, rule) {
			allow = append(allow, rule)
		}
	}
	return allow
}
