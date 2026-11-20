package policy

import (
	"fmt"
	"net/url"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/egress"
)

// RefusePassthroughEgress refuses the passthrough routes of the policies ps
// bound to one run when the run's egress, merged as Merge does, would let the
// guest reach a route's upstream past the broker. A passthrough route
// forwards a credential that the guest holds, so its request rules bind only
// while the guest cannot send that credential upstream itself. It can with
// no spec.egress at all, under default: allow, with an allow host glob that
// covers the upstream and no deny that covers it, and with any allow cidr.
//
// A host deny under default: allow is not enough. A connection sent straight
// to an address asks for no name, so that deny is best effort. An allow cidr
// is refused whatever it holds, because the upstream's addresses are known
// only once the name resolves, at run time.
func RefusePassthroughEgress(ps []Policy) error {
	merged := Merge(ps)
	for _, p := range ps {
		for _, r := range p.Spec.Routes {
			if !r.Passthrough {
				continue
			}
			host, err := passthroughHost(r)
			if err != nil {
				return fmt.Errorf("policy %s: %w", p.Metadata.Name, err)
			}
			head := fmt.Sprintf("route %s of policy %s is passthrough, and the guest could reach %s past the broker",
				r.Name, p.Metadata.Name, host)
			switch {
			case merged.Default == "":
				return fmt.Errorf("%s: no policy bound to the run has spec.egress; bind one with egress "+
					"default: deny and no allow for %s", head, host)
			case merged.Default == v1alpha1.EgressAllow:
				return fmt.Errorf("%s: the egress default is allow, and a host deny under it is best effort, "+
					"since a connection sent straight to an address asks for no name; use egress default: deny", head)
			}
			if rule, ok := allowReach(merged, host); ok {
				return fmt.Errorf("%s: %s", head, reachReason(rule, ruleOwner(ps, rule), host))
			}
		}
	}
	return nil
}

// refuseOwnAllow refuses a policy whose own egress allows the upstream of one
// of its passthrough routes. That holds whatever else is bound with it, so it
// is refused when the document is read, not first at the run.
func refuseOwnAllow(p Policy) error {
	if p.Spec.Egress == nil {
		return nil
	}
	for _, r := range p.Spec.Routes {
		if !r.Passthrough {
			continue
		}
		host, err := passthroughHost(r)
		if err != nil {
			return err
		}
		if rule, ok := allowReach(*p.Spec.Egress, host); ok {
			return fmt.Errorf("route %s is passthrough, and the guest could reach %s past the broker: %s",
				r.Name, host, reachReason(rule, "", host))
		}
	}
	return nil
}

// passthroughHost is the host of the upstream of r, after its preset fills it.
func passthroughHost(r v1alpha1.Route) (string, error) {
	e, err := ExpandRoute(r)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(e.Upstream)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("route %s: upstream %q names no host", r.Name, e.Upstream)
	}
	return u.Hostname(), nil
}

// allowReach returns the allow rule of e that lets the guest reach host: any
// cidr, or a host glob that covers host when no deny glob does. Deny beats
// allow at both enforcers, so a covered deny leaves host refused.
func allowReach(e Egress, host string) (Rule, bool) {
	for _, rule := range e.Allow {
		if rule.CIDR != "" {
			return rule, true
		}
	}
	for _, rule := range e.Deny {
		if rule.Host != "" && egress.MatchHost(rule.Host, host) {
			return Rule{}, false
		}
	}
	for _, rule := range e.Allow {
		if rule.Host != "" && egress.MatchHost(rule.Host, host) {
			return rule, true
		}
	}
	return Rule{}, false
}

// reachReason says why rule lets the guest reach host, and what to change.
// owner names the policy that holds rule, or is "" for the policy at hand.
func reachReason(rule Rule, owner, host string) string {
	of := ""
	if owner != "" {
		of = " of policy " + owner
	}
	if rule.CIDR != "" {
		return fmt.Sprintf("allow cidr %s%s may hold an address of %s, and brig cannot resolve the name before "+
			"the run; remove the allow cidr, or the passthrough route", rule.CIDR, of, host)
	}
	return fmt.Sprintf("allow host %s%s covers it; remove the allow for %s, since the route reaches it through "+
		"the broker", rule.Host, of, host)
}

// ruleOwner names the first policy of ps whose egress allows rule.
func ruleOwner(ps []Policy, rule Rule) string {
	for _, p := range ps {
		if p.Spec.Egress == nil {
			continue
		}
		for _, r := range p.Spec.Egress.Allow {
			if r == rule {
				return p.Metadata.Name
			}
		}
	}
	return ""
}
