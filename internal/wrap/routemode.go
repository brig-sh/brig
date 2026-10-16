package wrap

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/routemode"
	"github.com/brig-sh/brig/internal/runtime"
)

// routeMode is how a run serves its routes in a route mode besides endpoint.
// A build that registers such a mode (see internal/routemode) adds one with
// registerRouteMode from an init function. The runtime diverts the host and
// port of each such route's upstream URL to the broker (see diverts).
type routeMode interface {
	// check returns why a run on rt and path, with settings env and network
	// net, cannot serve routes in the mode, or nil. The route mode table
	// has answered enforced for path.
	check(env Env, rt runtime.Runtime, path runtime.RunPath, net string) error
	// unavailable returns what the BROKER row adds after the served modes
	// when c's run path does not serve the mode, or "" when it does.
	unavailable(c *Config) string
	// register adds what the broker needs to serve c's routes in the mode to
	// s, and returns the setup the boot hands the runtime.
	register(c *Config, s *injector.Sandbox) ([]runtime.Setup, error)
	// forget removes what register left on the host for sandbox vmName.
	forget(vmName string) error
}

var (
	routeModesMu sync.RWMutex
	routeModes   = map[v1alpha1.RouteMode]routeMode{}
)

// registerRouteMode adds h as the run of mode m, and returns a function that
// removes it again, for a test.
func registerRouteMode(m v1alpha1.RouteMode, h routeMode) (unregister func()) {
	routeModesMu.Lock()
	defer routeModesMu.Unlock()
	routeModes[m] = h
	return func() {
		routeModesMu.Lock()
		defer routeModesMu.Unlock()
		delete(routeModes, m)
	}
}

// routeModeOf returns the run of mode m, or nil when this build has none.
func routeModeOf(m v1alpha1.RouteMode) routeMode {
	routeModesMu.RLock()
	defer routeModesMu.RUnlock()
	if !routemode.Served(m) {
		return nil
	}
	return routeModes[m]
}

// registeredModes returns the modes besides endpoint that this build runs, in
// name order.
func registeredModes() []v1alpha1.RouteMode {
	var out []v1alpha1.RouteMode
	for _, m := range routemode.Names() {
		if routeModeOf(m) != nil {
			out = append(out, m)
		}
	}
	return out
}

// otherModes returns the modes besides endpoint that this run's routes name,
// in name order, whether this build serves them or not.
func (c *Config) otherModes() []v1alpha1.RouteMode {
	var out []v1alpha1.RouteMode
	for _, r := range c.routes {
		if r.Mode != "" && r.Mode != v1alpha1.ModeEndpoint && !slices.Contains(out, r.Mode) {
			out = append(out, r.Mode)
		}
	}
	slices.Sort(out)
	return out
}

// modeRoutes are this run's routes in mode m.
func (c *Config) modeRoutes(m v1alpha1.RouteMode) []v1alpha1.Route {
	var out []v1alpha1.Route
	for _, r := range c.routes {
		if r.Mode == m {
			out = append(out, r)
		}
	}
	return out
}

// modeRefusal is why this run cannot serve its routes besides endpoint, or
// nil when it has none. It comes ahead of every other route check. The route
// mode table answers first, and the mode's own check then has the last word.
// A mode this build does not serve is refused on every run path.
func (c *Config) modeRefusal() error {
	for _, m := range c.otherModes() {
		var names []string
		for _, r := range c.modeRoutes(m) {
			names = append(names, fmt.Sprintf("%s (route %s)", c.routePolicy[r.Name], r.Name))
		}
		head := fmt.Sprintf("policy %s declares a route in mode %s", strings.Join(names, ", "), m)
		if c.Runtime == nil {
			why := "and there is no runtime here to serve it"
			if !routemode.Served(m) {
				why = "and " + routemode.NotInBuild(m)
			}
			return fmt.Errorf("%s, %s. Use mode: endpoint, or detach the policy", head, why)
		}
		if err := checkMode(m, c.env, c.Runtime, c.routePath(), c.Network.RuntimeNet()); err != nil {
			return fmt.Errorf("%s: %w", head, err)
		}
	}
	return nil
}

// checkMode returns why a run on rt and path, with settings env and network
// net, cannot serve a route in mode m besides endpoint, or nil.
func checkMode(m v1alpha1.RouteMode, env Env, rt runtime.Runtime, path runtime.RunPath, net string) error {
	if err := runtime.RequireRouteMode(m, path); err != nil {
		return err
	}
	h := routeModeOf(m)
	if h == nil {
		return &runtime.CapabilityError{Property: runtime.RouteModeProperty(m), Path: path,
			State: runtime.CannotEnforce, Why: routemode.NotInBuild(m), Remedy: "Use mode: endpoint, or detach the policy"}
	}
	return h.check(env, rt, path, net)
}

// RouteModeServed returns why a run of p on rt cannot serve a route in mode m,
// or nil. It is the check a boot makes, for brig policy check: the runtime
// and hypervisor the run would use, and the network a route takes.
func RouteModeServed(p profile.Profile, rt runtime.Runtime, m v1alpha1.RouteMode) error {
	env := NewEnv(p.Name, nil)
	// A route takes the isolated posture, unless the run is offline.
	net := NetIsolated.RuntimeNet()
	if env.String("NETWORK", p.Network) == string(NetOffline) {
		net = NetOffline.RuntimeNet()
	}
	path := runtime.RoutePath(rt, env.String("HYPERVISOR", p.Hypervisor))
	switch {
	case m == "" || m == v1alpha1.ModeEndpoint:
		return runtime.CheckBrigInternal(rt, path, net)
	default:
		return checkMode(m, env, rt, path, net)
	}
}

// modeServed returns whether this run's path serves mode m besides endpoint.
func (c *Config) modeServed(m v1alpha1.RouteMode) bool {
	return c.Runtime != nil && checkMode(m, c.env, c.Runtime, c.routePath(), c.Network.RuntimeNet()) == nil
}
