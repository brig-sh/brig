package policy

import "testing"

func validPolicy() Policy {
	return Policy{
		APIVersion: APIVersion,
		Name:       "no-net",
		Egress: Egress{
			Default: "deny",
			Allow:   []Rule{{Host: "api.anthropic.com"}},
		},
	}
}

func TestValidatePolicyOK(t *testing.T) {
	if err := validPolicy().Validate(); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
}

func TestValidatePolicyRejections(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Policy)
	}{
		{"missing apiVersion", func(p *Policy) { p.APIVersion = "" }},
		{"unsupported apiVersion", func(p *Policy) { p.APIVersion = "brig.sh/v2" }},
		{"missing name", func(p *Policy) { p.Name = "" }},
		{"uppercase name", func(p *Policy) { p.Name = "No-Net" }},
		{"name starting with a dash", func(p *Policy) { p.Name = "-no-net" }},
		{"missing egress.default", func(p *Policy) { p.Egress.Default = "" }},
		{"bad egress.default", func(p *Policy) { p.Egress.Default = "maybe" }},
		{"allow rule with neither host nor cidr", func(p *Policy) {
			p.Egress.Allow = []Rule{{}}
		}},
		{"allow rule with both host and cidr", func(p *Policy) {
			p.Egress.Allow = []Rule{{Host: "example.com", CIDR: "10.0.0.0/8"}}
		}},
		{"deny rule with neither host nor cidr", func(p *Policy) {
			p.Egress.Deny = []Rule{{}}
		}},
		{"cidr with an octet missing", func(p *Policy) {
			p.Egress.Deny = []Rule{{CIDR: "10.0.0/8"}}
		}},
		{"cidr with no prefix length", func(p *Policy) {
			p.Egress.Allow = []Rule{{CIDR: "10.0.0.0"}}
		}},
		{"cidr that is not an address at all", func(p *Policy) {
			p.Egress.Allow = []Rule{{CIDR: "not-a-cidr"}}
		}},
		{"host with a space", func(p *Policy) {
			p.Egress.Allow = []Rule{{Host: "exa mple.com"}}
		}},
		{"host with a tab", func(p *Policy) {
			p.Egress.Allow = []Rule{{Host: "example.com\t"}}
		}},
		{"host with a newline", func(p *Policy) {
			p.Egress.Allow = []Rule{{Host: "example.com\n"}}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := validPolicy()
			c.edit(&p)
			if err := p.Validate(); err == nil {
				t.Fatalf("expected an error, got none")
			}
		})
	}
}

func TestValidatePolicyAcceptsCIDR(t *testing.T) {
	p := validPolicy()
	p.Egress.Allow = []Rule{{CIDR: "10.0.0.0/8"}}
	if err := p.Validate(); err != nil {
		t.Fatalf("a cidr: rule should parse and validate: %v", err)
	}
}

func TestValidatePolicyAcceptsDenyList(t *testing.T) {
	p := validPolicy()
	p.Egress.Deny = []Rule{{Host: "evil.example.com"}}
	if err := p.Validate(); err != nil {
		t.Fatalf("a deny: entry should validate alongside allow: %v", err)
	}
}

// host: has no pinned glob grammar yet (see Rule.validate), so a rule is
// judged only on whitespace/control characters, not on where a wildcard
// sits within it.
func TestValidatePolicyDoesNotJudgeGlobShape(t *testing.T) {
	for _, host := range []string{"*", "*.githubusercontent.com", "example.*", "ex*ample.com"} {
		p := validPolicy()
		p.Egress.Allow = []Rule{{Host: host}}
		if err := p.Validate(); err != nil {
			t.Errorf("Validate() rejected host %q: %v", host, err)
		}
	}
}

// CheckName is exported so a caller building a path from a name -- before
// any document exists to run Validate against -- can reject an unsafe one
// first. A name that would escape the directory it is joined into is
// exactly the case that matters.
func TestCheckName(t *testing.T) {
	for _, name := range []string{"no-net", "a", "a.b-c_d", "9lives"} {
		if err := CheckName(name); err != nil {
			t.Errorf("CheckName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{"", "No-Net", "-no-net", "../escape", "a/b", "."} {
		if err := CheckName(name); err == nil {
			t.Errorf("CheckName(%q) = nil, want an error", name)
		}
	}
}

// Every string here is inside safeName's own charset, and fails only
// because YAML resolves the whole token to a bool, null, or a number.
func TestCheckNameRefusesAmbiguousNames(t *testing.T) {
	ambiguous := []string{
		"no", "yes", "true", "false", "on", "off", "null", // bare booleans and null
		"123", "007", "00", "-0", // plain integers
		"1.5", "0.0", "3.14", "1e10", // floats and exponents
		"0x10", "0o17", "0b101", // hex, octal and binary
		"1_000",  // digit-group separator
		"y", "n", // single-letter booleans, distinct from yes/no
	}
	for _, name := range ambiguous {
		if err := CheckName(name); err == nil {
			t.Errorf("CheckName(%q) = nil, want an error: YAML reads this as something else", name)
		}
	}
	// Only a whole-token match resolves this way. A word merely containing
	// one of the above, or a date-shaped name, is read back as itself.
	unambiguous := []string{"no-net", "yes-please", "off-hours", "0x10-backup", "2024-01-01"}
	for _, name := range unambiguous {
		if err := CheckName(name); err != nil {
			t.Errorf("CheckName(%q) = %v, want nil", name, err)
		}
	}
}
