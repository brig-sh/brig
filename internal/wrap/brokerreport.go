package wrap

import (
	"fmt"
	"sort"
	"strings"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/injector"
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

// routeTransport is the transport this run's guest reaches its routes by.
func (c *Config) routeTransport() injector.Transport {
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
	case c.routeRefusal() != nil:
		parts = append(parts, "refused: brig.internal routing is not available on this runtime yet")
	case ok:
		parts = append(parts, fmt.Sprintf("brig-broker pid %d on 127.0.0.1:%d", st.PID, st.Port))
		if len(st.Pending) > 0 {
			parts = append(parts, "write-back pending: "+strings.Join(st.Pending, ", "))
		}
		if len(st.Failing) > 0 {
			parts = append(parts, "refresh failing: "+strings.Join(st.Failing, ", "))
		}
	default:
		parts = append(parts, "brig-broker not running; the run starts it")
	}
	parts = append(parts, c.routeStoreLine(c.checkRouteStore())...)
	return strings.Join(parts, "; ")
}

// InfoBroker is the broker as `brig info --json` reports it. It names
// routes and secrets, never a value or a token.
type InfoBroker struct {
	Transport string   `json:"transport"`
	Routes    []string `json:"routes"`
	Running   bool     `json:"running"`
	PID       int      `json:"pid,omitempty"`
	Port      int      `json:"port,omitempty"`
	Pending   []string `json:"pending,omitempty"`
	Failing   []string `json:"failing,omitempty"`
	// KeptOnHost are the secrets the routes inject, which stay on the
	// host.
	KeptOnHost []string `json:"keptOnHost,omitempty"`
	// Missing are the secrets the routes inject that brig's secret store
	// does not hold, and StoreError why the store could not be listed. The
	// run is refused for either, and Refused says why.
	Missing    []string `json:"missing,omitempty"`
	StoreError string   `json:"storeError,omitempty"`
	Refused    string   `json:"refused,omitempty"`
}

// infoBroker is the InfoBroker of this run, or nil for a run with no route.
func (c *Config) infoBroker() *InfoBroker {
	if len(c.routes) == 0 && c.routesErr == nil {
		return nil
	}
	b := &InfoBroker{Transport: string(c.routeTransport()), KeptOnHost: c.routeSkipped}
	for _, r := range c.routes {
		b.Routes = append(b.Routes, r.Name)
	}
	b.Missing, b.StoreError, b.Refused = c.routeStoreFacts(c.checkRouteStore())
	if err := c.routeRefusal(); err != nil {
		b.Refused = err.Error()
		return b
	}
	if st, ok := c.routeBroker().Status(); ok {
		b.Running, b.PID, b.Port, b.Pending, b.Failing = true, st.PID, st.Port, st.Pending, st.Failing
	}
	return b
}
