package v1alpha1

import (
	"fmt"
	"net/netip"
	"strings"
	"unicode"

	"sigs.k8s.io/yaml"
)

// Validate checks that a decoded policy is well formed, as written. It reads
// no store and no file.
//
// A route that names a preset is checked for what it sets, since the preset
// fills the rest and the presets are not part of this package. A route
// without one is checked in full, except for the defaults that expansion adds:
// mode, and Authorization in inject.headers.remove. Route.Validate checks a
// route after expansion.
func (p *Policy) Validate() error {
	switch p.APIVersion {
	case "":
		return fmt.Errorf("apiVersion is required, and must be %q", APIVersion)
	case APIVersion:
	default:
		return fmt.Errorf("apiVersion %q is not supported (this build knows %q)", p.APIVersion, APIVersion)
	}
	switch p.Kind {
	case "":
		return fmt.Errorf("kind is required, and must be %q", Kind)
	case Kind:
	default:
		return fmt.Errorf("kind %q is not supported (this build knows %q)", p.Kind, Kind)
	}
	if p.Metadata.Name == "" {
		return fmt.Errorf("metadata.name is required")
	}
	if err := CheckName(p.Metadata.Name); err != nil {
		return err
	}
	if strings.ContainsAny(p.Metadata.Description, "\r\n") {
		return fmt.Errorf("metadata.description is one line")
	}
	if p.Spec.Egress == nil && len(p.Spec.Routes) == 0 {
		return fmt.Errorf("a policy needs spec.egress, spec.routes or both")
	}
	if p.Spec.Egress != nil {
		if err := p.Spec.Egress.Validate(); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for i := range p.Spec.Routes {
		r := &p.Spec.Routes[i]
		if err := r.validateWritten(); err != nil {
			return err
		}
		if seen[r.Name] {
			return fmt.Errorf("route %q is declared twice", r.Name)
		}
		seen[r.Name] = true
	}
	return nil
}

// Validate checks that an egress rule set has a default, and that each rule
// names one destination in one form.
func (e *Egress) Validate() error {
	switch e.Default {
	case EgressAllow, EgressDeny:
	case "":
		return fmt.Errorf("egress.default is required (allow or deny)")
	default:
		return fmt.Errorf("egress.default %q is not allow or deny", e.Default)
	}
	for _, list := range [][]Rule{e.Allow, e.Deny} {
		for _, r := range list {
			if err := r.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

// Validate checks that a rule names exactly one destination, and that a CIDR
// parses. A malformed CIDR on a deny rule would otherwise fail open once an
// enforcer read it.
func (r Rule) Validate() error {
	switch {
	case r.Host == "" && r.CIDR == "":
		return fmt.Errorf("a rule needs host: or cidr:")
	case r.Host != "" && r.CIDR != "":
		return fmt.Errorf("a rule takes host: or cidr:, not both (got %q and %q)", r.Host, r.CIDR)
	}
	if r.CIDR != "" {
		if _, err := netip.ParsePrefix(prefixBits(r.CIDR)); err != nil {
			return fmt.Errorf("cidr %q is not a valid CIDR: %w", r.CIDR, err)
		}
	}
	// host: has no glob grammar of its own. The enforcer decides which
	// wildcard forms it honors, so only a value that no grammar can read is
	// refused here.
	for _, c := range r.Host {
		if unicode.IsSpace(c) || unicode.IsControl(c) {
			return fmt.Errorf("host %q contains whitespace or a control character", r.Host)
		}
	}
	return nil
}

// CheckName returns an error when name is not usable as a policy name. A route
// name also passes CheckRouteName.
//
// The character rule is a copy of the profile name grammar in internal/profile.
// It is copied because a package under api/ does not import internal/, and
// internal/policy.CheckName calls this function. A name uses lowercase
// letters, digits, dot, dash and underscore, and starts with a letter or
// digit. It must also read back as itself in YAML. The bare words no, true and
// 123 are inside the character set, and decode to a boolean or a number.
func CheckName(name string) error {
	if !safeName(name) {
		return fmt.Errorf("name %q may use only lowercase letters, digits, dot, "+
			"dash and underscore, and must start with a letter or digit", name)
	}
	var decoded any
	if err := yaml.Unmarshal([]byte(name), &decoded); err != nil {
		return fmt.Errorf("name %q does not read back as itself in YAML: %w", name, err)
	}
	if decoded != name {
		return fmt.Errorf("name %q reads as %#v when written unquoted in YAML, not as itself; "+
			"pick a different name", name, decoded)
	}
	return nil
}

// CheckRouteName returns an error when name is not usable as a route name.
//
// A route name is the path segment of the route URL, and it names the guest
// variable BRIG_ROUTE_<NAME>_TOKEN, with - read as _. So it uses lowercase
// letters, digits and dashes, and starts with a letter. A dot would end the
// variable name for the shell, and an underscore would let a_b and a-b name
// the same variable. The name also reads back as itself in YAML, as
// CheckName requires.
func CheckRouteName(name string) error {
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case (r >= '0' && r <= '9' || r == '-') && i > 0:
		default:
			return fmt.Errorf("route name %q may use only lowercase letters, digits and dashes, "+
				"and must start with a letter", name)
		}
	}
	return CheckName(name)
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

// prefixBits drops the leading zeros of the prefix length of a CIDR, which
// net.ParseCIDR accepts and netip.ParsePrefix refuses, so a rule that the
// older parser loaded still loads. A package under api/ does not import net.
func prefixBits(cidr string) string {
	addr, bits, ok := strings.Cut(cidr, "/")
	if !ok || len(bits) < 2 || strings.Trim(bits, "0123456789") != "" {
		return cidr
	}
	if trimmed := strings.TrimLeft(bits, "0"); trimmed != "" {
		return addr + "/" + trimmed
	}
	return addr + "/0"
}
