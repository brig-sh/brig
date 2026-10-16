package wrap

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/runtime"
)

// routeCredential is one variable a route hands the guest a session token
// in, and the route it is for.
type routeCredential struct {
	Name  string
	Route string
}

// routeCredentials are the variables this run's routes hand the guest a
// session token in, by name and never by value: each guest.env name bound to
// {session.token}, and the token variable a git route's credential helper
// reads.
func (c *Config) routeCredentials() []routeCredential {
	var out []routeCredential
	for _, r := range c.routes {
		if r.Guest == nil {
			continue
		}
		var names []string
		for name, v := range r.Guest.Env {
			if v == v1alpha1.PlaceholderSessionToken {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			out = append(out, routeCredential{name, r.Name})
		}
		if r.Guest.Git != nil && len(r.Guest.Git.InsteadOf) > 0 {
			out = append(out, routeCredential{injector.TokenVar(r.Name), r.Name})
		}
	}
	return out
}

// routeTransport is the transport this run's guest reaches its routes by. A
// run whose routes are all in modes besides endpoint reaches them by the
// divert.
func (c *Config) routeTransport() injector.Transport {
	if len(c.routes) > 0 && len(c.endpointRoutes()) == 0 {
		return injector.TransportDivert
	}
	return injector.TransportBrigInternal
}

// brokerLine is the BROKER row: how the guest reaches the broker, which
// routes it serves this sandbox, whether it is running, and what it keeps on
// the host. Empty for a run with no route.
func (c *Config) brokerLine() string {
	if len(c.routes) == 0 && c.routesErr == nil {
		return ""
	}
	if c.routesErr != nil {
		return "refused: " + c.routesErr.Error()
	}
	var names []string
	for _, r := range c.routes {
		names = append(names, r.Name)
	}
	parts := []string{fmt.Sprintf("transport %s, routes %s", c.routeTransport(), strings.Join(names, ", "))}
	switch st, ok := c.routeBroker().Status(); {
	case c.modeRefusal() != nil:
		parts = append(parts, "refused: a route is in a mode this run cannot serve; brig info --json says why")
	case c.routeRefusal() != nil:
		parts = append(parts, "refused: this run cannot route brig.internal to the broker; brig info --json says why")
	case ok:
		parts = append(parts, fmt.Sprintf("brig-broker pid %d on 127.0.0.1:%d", st.PID, st.Port))
		if len(st.Pending) > 0 {
			parts = append(parts, "write-back pending: "+strings.Join(st.Pending, ", "))
		}
		if len(st.Failing) > 0 {
			parts = append(parts, "refresh failing: "+strings.Join(st.Failing, ", "))
		}
		if len(st.Recovering) > 0 {
			parts = append(parts, "refresh recovering: "+strings.Join(st.Recovering, ", "))
		}
		// The count is this sandbox's: another sandbox's leaks are not
		// this run's to report.
		if n := st.Placeholder[c.VMName]; n > 0 {
			parts = append(parts, fmt.Sprintf("placeholder refusals: %d", n))
		}
	case st.PID != 0:
		parts = append(parts, fmt.Sprintf("brig-broker pid %d is not answering on its control socket; "+
			"see %s", st.PID, injector.LogName))
	default:
		parts = append(parts, "brig-broker not running; the run starts it")
	}
	parts = append(parts, c.routeStoreLine(c.checkRouteStore())...)
	parts = append(parts, c.modesLine())
	return strings.Join(parts, "; ")
}

// servedModes are the route modes this run's path serves, never nil. An
// endpoint is served only where endpointAnswer says so, and another mode
// only where its own check holds.
func (c *Config) servedModes() []string {
	out := []string{}
	if c.Runtime == nil {
		return out
	}
	for _, m := range runtime.ServedModes(c.routePath()) {
		switch {
		case m == v1alpha1.ModeEndpoint:
			if c.endpointAnswer().State != runtime.Enforced {
				continue
			}
		case !c.modeServed(m):
			continue
		}
		out = append(out, string(m))
	}
	return out
}

// endpointAnswer is the endpoint row of the route mode table for this run's
// path. Where a guest reaches brig.internal and the table answers enforced,
// CheckBrigInternal has the last word.
func (c *Config) endpointAnswer() runtime.ModeRecord {
	r := runtime.ModeAnswer(v1alpha1.ModeEndpoint, c.routePath())
	if r.State != runtime.Enforced {
		return r
	}
	var ce *runtime.CapabilityError
	if err := c.endpointCheck(); errors.As(err, &ce) {
		r.State, r.Why = ce.State, ce.Why
	}
	return r
}

// modesLine is the part of the BROKER row that names the modes this run path
// serves, and what each route mode besides endpoint adds when it is not
// served.
func (c *Config) modesLine() string {
	modes := strings.Join(c.servedModes(), ", ")
	if modes == "" {
		modes = "none"
	}
	line := "modes " + modes
	for _, m := range registeredModes() {
		if more := routeModeOf(m).unavailable(c); more != "" {
			line += "; " + more
		}
	}
	return line
}

// unavailableModes names each route mode this build serves somewhere and
// this run's path does not, with the reason: the row of the route mode
// table, or the check that refuses it.
func (c *Config) unavailableModes() map[string]string {
	out := map[string]string{}
	for _, m := range append([]v1alpha1.RouteMode{v1alpha1.ModeEndpoint}, registeredModes()...) {
		var r runtime.ModeRecord
		switch {
		case c.Runtime == nil:
			r = runtime.ModeRecord{State: runtime.CannotEnforce, Why: "there is no runtime here"}
		case m == v1alpha1.ModeEndpoint:
			r = c.endpointAnswer()
		default:
			r = runtime.ModeAnswer(m, c.routePath())
			var ce *runtime.CapabilityError
			if err := checkMode(m, c.env, c.Runtime, c.routePath(), c.Network.RuntimeNet()); errors.As(err, &ce) &&
				r.State == runtime.Enforced {
				r.State, r.Why = ce.State, ce.Why
			}
		}
		if r.State != runtime.Enforced {
			out[string(m)] = r.Line()
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// InfoBroker is the broker as `brig info --json` reports it. It names
// routes and secrets, never a value or a token. Running is whether the
// broker answers, and PID is set also for one whose process is alive and
// does not.
type InfoBroker struct {
	Transport string   `json:"transport"`
	Routes    []string `json:"routes"`
	Running   bool     `json:"running"`
	PID       int      `json:"pid,omitempty"`
	Port      int      `json:"port,omitempty"`
	Pending   []string `json:"pending,omitempty"`
	Failing   []string `json:"failing,omitempty"`
	// Recovering are the secrets with a renewal journal not yet settled.
	Recovering []string `json:"recovering,omitempty"`
	// Placeholder is the number of this sandbox's requests the broker
	// refused for a session token outside its carrier.
	Placeholder int64 `json:"placeholder,omitempty"`
	// KeptOnHost are the secrets the routes inject, which stay on the
	// host.
	KeptOnHost []string `json:"keptOnHost,omitempty"`
	// Missing are the secrets the routes inject that brig's secret store
	// does not hold, and StoreError why the store could not be listed. The
	// run is refused for either, and Refused says why.
	Missing    []string `json:"missing,omitempty"`
	StoreError string   `json:"storeError,omitempty"`
	Refused    string   `json:"refused,omitempty"`
	// Modes are the route modes this run path serves: endpoint on hull on
	// hvi and nerdctl on urunc when brig.internal is routed there, and none
	// elsewhere.
	Modes []string `json:"modes"`
	// Unavailable names each route mode this build serves that this run
	// path does not, with the reason.
	Unavailable map[string]string `json:"unavailable,omitempty"`
}

// infoBroker is the InfoBroker of this run, or nil for a run with no route.
func (c *Config) infoBroker() *InfoBroker {
	if len(c.routes) == 0 && c.routesErr == nil {
		return nil
	}
	b := &InfoBroker{Transport: string(c.routeTransport()), KeptOnHost: c.routeSkipped,
		Modes: c.servedModes(), Unavailable: c.unavailableModes()}
	for _, r := range c.routes {
		b.Routes = append(b.Routes, r.Name)
	}
	b.Missing, b.StoreError, b.Refused = c.routeStoreFacts(c.checkRouteStore())
	if err := c.routeRefusal(); err != nil {
		b.Refused = err.Error()
		return b
	}
	switch st, ok := c.routeBroker().Status(); {
	case ok:
		b.Running, b.PID, b.Port, b.Pending, b.Failing = true, st.PID, st.Port, st.Pending, st.Failing
		b.Recovering, b.Placeholder = st.Recovering, st.Placeholder[c.VMName]
	case st.PID != 0:
		// Alive and not answering, which the BROKER row names too. The
		// pid tells it from no broker, which the next run starts.
		b.PID = st.PID
	}
	return b
}
