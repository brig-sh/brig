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
