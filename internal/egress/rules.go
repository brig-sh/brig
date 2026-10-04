// Package egress enforces an egress policy on the Linux container network.
//
// On macOS the rules go on hull's user-mode network gateway. On Linux the
// sandbox's traffic is routed by the kernel, so the same rules are split in
// two. nftables in the network namespace that routes the sandbox refuses what
// the policy does not allow. A small resolver answers the sandbox's DNS and
// adds the addresses in its answers to the nftables sets the host rules
// decide by. The resolver installs the table and keeps it in place, and the
// table refuses new connections once the resolver is gone.
//
// It exists because hull's gateway cannot serve a Linux guest yet. Once it
// can, the gateway is where these rules belong, and this package is meant to
// go.
//
// The rules mean what they mean on hull's gateway (internal/netgw in
// brig-sh/hull). A host rule is a path.Match glob on the name the guest asks
// for. Under a deny default the resolver refuses a name no allow glob matches,
// and a connection is let through only to an address a rule allows. Deny beats
// allow, and allow beats the default.
package egress

import (
	"fmt"
	"net/netip"
	"path"
	"strings"
)

// Policy is a parsed egress policy.
type Policy struct {
	// Default is "allow" or "deny".
	Default string
	allow   ruleSet
	deny    ruleSet
}

// ruleSet is one side of a policy: the CIDRs and the host globs.
type ruleSet struct {
	cidrs []netip.Prefix
	hosts []string
}

// Parse builds a policy from the rules in the spelling hull's gateway takes
// on its command line: "host=<glob>" or "cidr=<cidr>".
func Parse(def string, allow, deny []string) (*Policy, error) {
	switch def {
	case "allow", "deny":
	default:
		return nil, fmt.Errorf("egress default %q is not allow or deny", def)
	}
	p := &Policy{Default: def}
	var err error
	if p.allow, err = parseRules(allow); err != nil {
		return nil, err
	}
	if p.deny, err = parseRules(deny); err != nil {
		return nil, err
	}
	return p, nil
}

func parseRules(rules []string) (ruleSet, error) {
	var set ruleSet
	for _, rule := range rules {
		kind, value, ok := strings.Cut(rule, "=")
		if !ok {
			return ruleSet{}, fmt.Errorf("rule %q is not host=<glob> or cidr=<cidr>", rule)
		}
		switch kind {
		case "cidr":
			prefix, err := netip.ParsePrefix(value)
			if err != nil {
				return ruleSet{}, fmt.Errorf("rule %q: %q is not a CIDR", rule, value)
			}
			// An IPv4-mapped prefix names IPv4 addresses. Kept as IPv6, it
			// would match nothing, and a deny rule would vanish without an
			// error.
			if prefix.Addr().Is4In6() && prefix.Bits() >= 96 {
				prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
			}
			set.cidrs = append(set.cidrs, prefix.Masked())
		case "host":
			glob := normalizeName(value)
			if glob == "" {
				return ruleSet{}, fmt.Errorf("rule %q: the host glob is empty", rule)
			}
			if _, err := path.Match(glob, "example.com"); err != nil {
				return ruleSet{}, fmt.Errorf("rule %q: %v", rule, err)
			}
			set.hosts = append(set.hosts, glob)
		default:
			return ruleSet{}, fmt.Errorf("rule %q is not host=<glob> or cidr=<cidr>", rule)
		}
	}
	return set, nil
}

// normalizeName returns a name or a glob in lower case with no trailing dot.
func normalizeName(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

func (r ruleSet) matchesHost(name string) bool {
	for _, glob := range r.hosts {
		if ok, err := path.Match(glob, name); err == nil && ok {
			return true
		}
	}
	return false
}

// literalHosts returns the host rules that name exactly one host. Only these
// can be resolved before a guest asks for them.
func (r ruleSet) literalHosts() []string {
	var out []string
	for _, glob := range r.hosts {
		if !strings.ContainsAny(glob, "*?[") {
			out = append(out, glob)
		}
	}
	return out
}

// v4CIDRs returns the IPv4 prefixes of the set. The sandbox network carries no
// IPv6, so an IPv6 prefix has nothing to match.
func (r ruleSet) v4CIDRs() []netip.Prefix {
	var out []netip.Prefix
	for _, c := range r.cidrs {
		if c.Addr().Is4() {
			out = append(out, c)
		}
	}
	return out
}

// HasHostRules returns whether any rule names a host, which is what puts the
// resolver on the enforcement path.
func (p *Policy) HasHostRules() bool {
	return len(p.allow.hosts) > 0 || len(p.deny.hosts) > 0
}

// AllowsQuery returns whether the resolver answers a query for name.
//
// Under a deny default, a name no allow glob matches is refused. An address
// the guest never learns is one it cannot reach, so a direct connection, DNS
// over HTTPS and DNS over TLS fail on the filter. Under an allow default every
// query is answered, and a name a deny glob matches is pinned as denied.
func (p *Policy) AllowsQuery(name string) bool {
	if p.Default == "allow" {
		return true
	}
	name = normalizeName(name)
	if p.deny.matchesHost(name) {
		return false
	}
	return p.allow.matchesHost(name)
}

// DeniesQuery returns whether name matches a deny glob, so the addresses in
// its answer go in the deny set.
func (p *Policy) DeniesQuery(name string) bool {
	return p.deny.matchesHost(normalizeName(name))
}
