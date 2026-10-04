package egress

import "testing"

func mustParse(t *testing.T, def string, allow, deny []string) *Policy {
	t.Helper()
	p, err := Parse(def, allow, deny)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseRefusesWhatHullRefuses(t *testing.T) {
	for _, tc := range []struct {
		def   string
		allow []string
	}{
		{"", nil},
		{"maybe", nil},
		{"deny", []string{"example.com"}},
		{"deny", []string{"ip=1.2.3.4"}},
		{"deny", []string{"cidr=1.2.3.4"}},
		{"deny", []string{"host="}},
		{"deny", []string{"host=[a"}},
	} {
		if _, err := Parse(tc.def, tc.allow, nil); err == nil {
			t.Errorf("Parse(%q, %q) succeeded", tc.def, tc.allow)
		}
	}
}

func TestAllowsQuery(t *testing.T) {
	deny := mustParse(t, "deny",
		[]string{"host=api.anthropic.com", "host=*.github.com", "cidr=10.0.0.0/8"},
		[]string{"host=gist.github.com"})
	allow := mustParse(t, "allow", nil, []string{"host=example.com", "host=*.evil.test"})
	for _, tc := range []struct {
		p    *Policy
		name string
		want bool
	}{
		{deny, "api.anthropic.com", true},
		{deny, "API.Anthropic.com.", true},
		{deny, "anthropic.com", false},
		{deny, "codeload.github.com", true},
		// path.Match's * spans dots, as on hull's gateway.
		{deny, "a.b.github.com", true},
		// A glob does not cover the apex.
		{deny, "github.com", false},
		// A deny glob beats an allow glob.
		{deny, "gist.github.com", false},
		{deny, "example.com", false},
		{allow, "example.com", true},
		{allow, "anything.test", true},
	} {
		if got := tc.p.AllowsQuery(tc.name); got != tc.want {
			t.Errorf("%s policy: AllowsQuery(%q) = %v, want %v", tc.p.Default, tc.name, got, tc.want)
		}
	}
	if !allow.DeniesQuery("www.evil.test") || allow.DeniesQuery("evil.test") {
		t.Error("DeniesQuery does not follow the deny globs")
	}
}

func TestLiteralHosts(t *testing.T) {
	p := mustParse(t, "deny", []string{"host=a.com", "host=*.b.com", "host=c?.com", "cidr=1.2.3.0/24"}, nil)
	got := p.allow.literalHosts()
	if len(got) != 1 || got[0] != "a.com" {
		t.Errorf("literalHosts = %q, want [a.com]", got)
	}
}

// An IPv4-mapped prefix names IPv4 addresses. Dropped as IPv6, a deny rule
// written that way would vanish without an error.
func TestParseReadsIPv4MappedCIDRsAsIPv4(t *testing.T) {
	p := mustParse(t, "allow", nil, []string{"cidr=::ffff:10.0.0.0/104"})
	if got := p.deny.v4CIDRs(); len(got) != 1 || got[0].String() != "10.0.0.0/8" {
		t.Errorf("deny cidrs = %v", got)
	}
}
