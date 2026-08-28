// Package policy holds the policy document as data: a named set of rules.
//
// A Policy carries no field naming how a rule gets enforced. brig is the
// open-source binary, so every field here is public the moment it merges.
//
// The only rule class is egress.
package policy

import (
	"fmt"
	"net"
	"unicode"
)

// APIVersion is the only apiVersion a Policy document may declare. Parse
// refuses anything else, rather than guessing what an unrecognised version
// means.
const APIVersion = "brig.sh/v1alpha1"

// Policy is one named set of rules, as data.
type Policy struct {
	// APIVersion pins the document shape. See APIVersion.
	APIVersion string `json:"apiVersion"`
	// Name is the policy's identifier. It wins over the filename the
	// document was read from.
	Name string `json:"name"`
	// Desc is a one-line description of the policy.
	Desc string `json:"desc,omitempty"`
	// Egress is the policy's outbound rule set.
	Egress Egress `json:"egress,omitempty"`
}

// Egress is what the agent may reach outbound.
type Egress struct {
	// Default is "allow" or "deny", applied to anything not named below.
	Default string `json:"default"`
	// Allow lists the exceptions to a "deny" default.
	Allow []Rule `json:"allow,omitempty"`
	// Deny always wins over Allow and over Default: a host named here is
	// refused regardless, and no other settings source restores it.
	Deny []Rule `json:"deny,omitempty"`
}

// Rule is one host, by exactly one of the two spellings a host can take.
type Rule struct {
	// Host is a glob, e.g. "*.githubusercontent.com".
	Host string `json:"host,omitempty"`
	// CIDR is a network range, e.g. "10.0.0.0/8".
	CIDR string `json:"cidr,omitempty"`
}

// Validate checks a policy is well-formed.
func (p Policy) Validate() error {
	switch p.APIVersion {
	case "":
		return fmt.Errorf("apiVersion is required, and must be %q", APIVersion)
	case APIVersion:
	default:
		return fmt.Errorf("apiVersion %q is not supported (this build knows %q)", p.APIVersion, APIVersion)
	}
	if p.Name == "" {
		return fmt.Errorf("name is required")
	}
	// Name has to be safe as a bare word in a shell command and in a file
	// path.
	if !safeName(p.Name) {
		return fmt.Errorf("name %q may use only lowercase letters, digits, dot, "+
			"dash and underscore, and must start with a letter or digit", p.Name)
	}
	switch p.Egress.Default {
	case "allow", "deny":
	case "":
		return fmt.Errorf("egress.default is required (allow or deny)")
	default:
		return fmt.Errorf("egress.default %q is not allow or deny", p.Egress.Default)
	}
	for _, list := range [][]Rule{p.Egress.Allow, p.Egress.Deny} {
		for _, r := range list {
			if err := r.validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

// validate checks that a Rule names exactly one host, in exactly one form,
// and that a cidr: parses -- a malformed cidr on a deny rule would fail
// open instead of closed once enforced.
func (r Rule) validate() error {
	switch {
	case r.Host == "" && r.CIDR == "":
		return fmt.Errorf("a rule needs host: or cidr:")
	case r.Host != "" && r.CIDR != "":
		return fmt.Errorf("a rule takes host: or cidr:, not both (got %q and %q)", r.Host, r.CIDR)
	}
	if r.CIDR != "" {
		if _, _, err := net.ParseCIDR(r.CIDR); err != nil {
			return fmt.Errorf("cidr %q is not a valid CIDR: %w", r.CIDR, err)
		}
	}
	// host: has no pinned glob grammar yet -- which wildcard forms an
	// enforcer honours is enforcer-specific, and this package does not
	// know how a rule gets enforced. Reject only what is unambiguously
	// wrong however it ends up read: whitespace or a control character.
	for _, c := range r.Host {
		if unicode.IsSpace(c) || unicode.IsControl(c) {
			return fmt.Errorf("host %q contains whitespace or a control character", r.Host)
		}
	}
	return nil
}

func safeName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '.' || r == '-' || r == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}
