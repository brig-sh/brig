package wrap

import (
	"fmt"
	"strings"

	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/runtime"
)

// runtimeEgress is the one place a policy document becomes something a runtime
// takes.
//
// The two types have the same shape and are deliberately separate: a policy is
// a document brig reads, a RunSpec is a request brig makes, and the runtime
// adapters have no business knowing how a policy is spelled on disk. This
// function is the seam, so a rule class added to the document has exactly one
// place to be taught to travel.
func runtimeEgress(e policy.Egress) runtime.Egress {
	if e.Default == "" {
		return runtime.Egress{}
	}
	out := runtime.Egress{Default: string(e.Default)}
	for _, r := range e.Allow {
		out.Allow = append(out.Allow, runtime.Rule{Host: r.Host, CIDR: r.CIDR})
	}
	for _, r := range e.Deny {
		out.Deny = append(out.Deny, runtime.Rule{Host: r.Host, CIDR: r.CIDR})
	}
	return out
}

// refuseRoutes returns the refusal for a run bound to a policy with routes,
// or nil. brig policy check prints a route and the posture it plans for, and
// no boot serves one yet. A run that went ahead would give the agent neither
// the route nor the network the route needs, under a policy that names both.
func refuseRoutes(bound []policy.Policy) error {
	var with []string
	for _, p := range bound {
		var routes []string
		for _, r := range p.Spec.Routes {
			routes = append(routes, r.Name)
		}
		if len(routes) > 0 {
			with = append(with, fmt.Sprintf("%s (route %s)", p.Metadata.Name, strings.Join(routes, ", ")))
		}
	}
	if len(with) == 0 {
		return nil
	}
	return fmt.Errorf("policy %s declares routes, and this version of brig does not serve routes; "+
		"refusing the run rather than booting without them. Detach the policy, or remove its spec.routes",
		strings.Join(with, ", "))
}
