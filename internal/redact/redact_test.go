package redact

import (
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// realDigest is a full-length digest, so the kept-digest cases exercise the
// length rule.
const realDigest = "sha256:d34db33fd34db33fd34db33fd34db33fd34db33fd34db33fd34db33fd34db33f"

func testRedactor() *Redactor {
	return New(
		Host{Home: "/home/zz-canary", User: "zz-canary", Hostname: "canary-box.local"},
		Allow{
			Names:    []string{"claude-code"},
			Vars:     []string{"ANTHROPIC_API_KEY"},
			Images:   []string{"ghcr.io/brig-sh/claude-code"},
			Domains:  []string{"github.com", ".anthropic.com"},
			Files:    []string{"sessions.json", "policies"},
			Prefixes: []netip.Prefix{netip.MustParsePrefix("198.18.0.0/15")},
		})
}

func TestAllowlistedValuesStayReadable(t *testing.T) {
	r := testRedactor()
	cases := map[string]string{
		r.Name(KindProfile, "claude-code"):         "claude-code",
		r.Var("ANTHROPIC_API_KEY"):                 "ANTHROPIC_API_KEY",
		r.Image("ghcr.io/brig-sh/claude-code:1.2"): "ghcr.io/brig-sh/claude-code:1.2",
		r.Domain("github.com"):                     "github.com",
		r.Domain("api.anthropic.com"):              "api.anthropic.com",
		r.IP("127.0.0.1"):                          "127.0.0.1",
		r.IP("198.18.0.4"):                         "198.18.0.4",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestIdentifyingValuesGetStablePlaceholders(t *testing.T) {
	r := testRedactor()
	if got := r.Var("ACME_CANARY_TOKEN"); got != "<VAR-1>" {
		t.Errorf("var: %q", got)
	}
	if got := r.Var("ACME_CANARY_TOKEN"); got != "<VAR-1>" {
		t.Errorf("second sight of the same var: %q, want the same placeholder", got)
	}
	if got := r.Var("OTHER_CANARY"); got != "<VAR-2>" {
		t.Errorf("second var: %q", got)
	}
	if got := r.Domain("git.acme-canary.io"); got != "<domain-1>" {
		t.Errorf("domain: %q", got)
	}
	if got := r.Name(KindProfile, "acme-canary"); got != "<profile-1>" {
		t.Errorf("profile: %q", got)
	}
	if got := r.IP("203.0.113.9"); got != "<ip-1>" {
		t.Errorf("ip: %q", got)
	}
}

func TestImageRefs(t *testing.T) {
	r := testRedactor()
	cases := []struct{ in, want string }{
		{"ghcr.io/acme-canary/agent:1.4.2", "ghcr.io/<image-1>:1.4.2"},
		{"registry.acme-canary.io/team/agent:latest", "<registry-1>/<image-2>:latest"},
		{"ghcr.io/acme-canary/agent:canary-prod", "ghcr.io/<image-1>:<tag-1>"},
		{"ghcr.io/acme-canary/agent@" + realDigest, "ghcr.io/<image-1>@" + realDigest},
		{"localhost:5000/agent:v2", "<registry-2>/<image-3>:v2"},
	}
	for _, c := range cases {
		if got := r.Image(c.in); got != c.want {
			t.Errorf("Image(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPaths(t *testing.T) {
	r := testRedactor()
	cases := []struct{ in, want string }{
		{"/home/zz-canary", "$HOME"},
		{"/home/zz-canary/.brig/sessions.json", "$HOME/.brig/sessions.json"},
		{"/home/zz-canary/.config/brig/acme-canary.yaml", "$HOME/.config/brig/<path-1>"},
		{"/home/zz-canary/.config/brig/policies", "$HOME/.config/brig/policies"},
		{"/home/zz-canary/code/acme-api", "$HOME/<path-2>"},
		{"/usr/local/bin/brig", "/usr/local/bin/brig"},
		{"/opt/homebrew/bin/hull", "/opt/homebrew/bin/hull"},
		{"/Volumes/acme/project", "<path-3>"},
		// A path that only shares a prefix with $HOME is not under it.
		{"/home/zz-canary-other/x", "<path-4>"},
	}
	for _, c := range cases {
		if got := r.Path(c.in); got != c.want {
			t.Errorf("Path(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := r.Workspace("/home/zz-canary/code/acme-api"); got != "<workspace-1>" {
		t.Errorf("Workspace = %q", got)
	}
}

func TestKindsListsWhatWasReplaced(t *testing.T) {
	r := testRedactor()
	r.Var("ACME_CANARY_TOKEN")
	r.Domain("git.acme-canary.io")
	r.Var("ANTHROPIC_API_KEY") // allowlisted, not a replacement
	if got, want := r.Kinds(), []string{"VAR", "domain"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Kinds = %v, want %v", got, want)
	}
}

// Concurrent registration from several goroutines is safe.
func TestConcurrentUseIsSafe(t *testing.T) {
	r := testRedactor()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(4)
		go func() { defer wg.Done(); r.Var("V" + string(rune('A'+i%26))) }()
		go func() { defer wg.Done(); r.Domain("h.example") }()
		go func() { defer wg.Done(); r.Text("mail zz@acme-canary.io on canary-box from 203.0.113.7") }()
		go func() { defer wg.Done(); _ = r.Check([]byte("on <host> from <ip-1>")) }()
	}
	wg.Wait()
}

// An allowlisted repository's tag and digest still go through their own
// rules.
func TestAllowlistedImageRepoStillRedactsTagAndDigest(t *testing.T) {
	r := testRedactor()
	if got, want := r.Image("ghcr.io/brig-sh/claude-code:acmecorp-prod"), "ghcr.io/brig-sh/claude-code:<tag-1>"; got != want {
		t.Errorf("Image with a non-version tag = %q, want %q", got, want)
	}
	if got, want := r.Image("ghcr.io/brig-sh/claude-code:1.2"), "ghcr.io/brig-sh/claude-code:1.2"; got != want {
		t.Errorf("Image with a version tag = %q, want %q", got, want)
	}
}

// Only a release spelling is a version; "1.2-acmebank-internal" gets a
// placeholder.
func TestImageVersionTagIsStrict(t *testing.T) {
	r := testRedactor()
	keep := []string{"1.4.2", "v2", "0.1.0-rc23", "1.0.0-beta.2", "1.2.3+abc1234"}
	for _, tag := range keep {
		ref := "ghcr.io/acme-canary/agent:" + tag
		if got, want := r.Image(ref), "ghcr.io/<image-1>:"+tag; got != want {
			t.Errorf("Image(%q) = %q, want %q (kept)", ref, got, want)
		}
	}
	replace := []string{"1.2-acmebank-internal", "2024.1_acme.prod", "canary-prod"}
	for i, tag := range replace {
		ref := "ghcr.io/acme-canary/agent:" + tag
		want := fmt.Sprintf("ghcr.io/<image-1>:<tag-%d>", i+1)
		if got := r.Image(ref); got != want {
			t.Errorf("Image(%q) = %q, want %q (placeholder)", ref, got, want)
		}
	}
}

// A digest is kept only when it matches digestShape.
func TestImageDigestShape(t *testing.T) {
	r := testRedactor()
	if got := r.Image("agent@acme-secret"); strings.Contains(got, "acme-secret") {
		t.Errorf("Image leaked a non-digest value after @: %q", got)
	}
	if got, want := r.Image("ghcr.io/acme-canary/agent@"+realDigest), "ghcr.io/<image-3>@"+realDigest; got != want {
		t.Errorf("Image with a real digest = %q, want %q", got, want)
	}
}

// placeholder registers nothing for an empty value.
func TestPlaceholderGuardsEmptyValue(t *testing.T) {
	r := testRedactor()
	r.Image("ghcr.io/")
	if _, ok := r.originals[""]; ok {
		t.Errorf("originals has an entry for the empty string")
	}
}

// Domain redacts the host and keeps the port.
func TestDomainKeepsPort(t *testing.T) {
	r := testRedactor()
	if got, want := r.Domain("git.acme.io:443"), "<domain-1>:443"; got != want {
		t.Errorf("Domain = %q, want %q", got, want)
	}
}

// IP fails closed: an address with a port or prefix width, a bracketed IPv6
// address, and text that does not parse all become placeholders.
func TestIPFailsClosed(t *testing.T) {
	r := testRedactor()
	if got, want := r.IP("203.0.113.9:443"), "<ip-1>:443"; got != want {
		t.Errorf("IP(addr:port) = %q, want %q", got, want)
	}
	if got, want := r.IP("203.0.113.9/24"), "<ip-1>/24"; got != want {
		t.Errorf("IP(CIDR) = %q, want %q", got, want)
	}
	if got := r.IP("[2001:db8::1]"); got == "[2001:db8::1]" || !strings.HasPrefix(got, "<ip-") {
		t.Errorf("IP(bracketed IPv6) = %q, want a placeholder", got)
	}
	if got := r.IP("not-an-ip"); !strings.HasPrefix(got, "<ip-") {
		t.Errorf("IP(unparseable) = %q, want a placeholder", got)
	}
	if got, want := r.IP("127.0.0.1:8080"), "127.0.0.1:8080"; got != want {
		t.Errorf("IP(loopback:port) = %q, want %q (unchanged)", got, want)
	}
}

// ip returns a non-address, such as a timestamp, unchanged.
func TestIPUnexportedStaysFailOpen(t *testing.T) {
	r := testRedactor()
	if got, want := r.ip("10:22:33"), "10:22:33"; got != want {
		t.Errorf("ip(%q) = %q, want %q (unchanged, not an address)", "10:22:33", got, want)
	}
}

// A path under an install prefix is kept only if every later segment is an
// install name, an allowlisted file or a version. Device and kernel paths are
// kept whole.
func TestSystemPathsKeepOnlyKnownLayouts(t *testing.T) {
	r := testRedactor()
	keep := []string{
		"/usr/local/bin/brig",
		"/opt/homebrew/bin/hull",
		"/opt/homebrew/Caskroom/brig/0.3.0/brig",
		"/dev/kvm",
	}
	for _, p := range keep {
		if got := r.Path(p); got != p {
			t.Errorf("Path(%q) = %q, want unchanged", p, got)
		}
	}
	replace := []struct{ in, want string }{
		{"/Applications/AcmeCorp VPN.app/x", "<path-1>"},
		{"/etc/acme-corp/cfg", "<path-2>"},
		{"/run/user/zz-canary/x", "<path-3>"},
		{"/nix/store/abc-acme-api", "<path-4>"},
	}
	for _, c := range replace {
		if got := r.Path(c.in); got != c.want {
			t.Errorf("Path(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Only a valid port is split off; otherwise the whole input is one value.
func TestDomainInvalidPortIsNotSplit(t *testing.T) {
	r := testRedactor()
	if got := r.Domain("acme.io:secret-env"); strings.Contains(got, "secret-env") {
		t.Errorf("Domain leaked text after a colon that was not a port: %q", got)
	}
	if got, want := r.Domain("git.acme.io:443"), "<domain-2>:443"; got != want {
		t.Errorf("Domain with a real port = %q, want %q", got, want)
	}
}

// A kept IPv6 address with a port keeps its brackets; a placeholder is not
// bracketed.
func TestIPv6WithPortKeepsBrackets(t *testing.T) {
	r := testRedactor()
	if got, want := r.IP("[::1]:8080"), "[::1]:8080"; got != want {
		t.Errorf("IP(loopback IPv6:port) = %q, want %q (unchanged, bracketed)", got, want)
	}
	if got, want := r.Domain("[::1]:8080"), "[::1]:8080"; got != want {
		t.Errorf("Domain(loopback IPv6:port) = %q, want %q (unchanged, bracketed)", got, want)
	}
	got := r.IP("[2001:db8::1]:443")
	if !strings.HasPrefix(got, "<ip-") || !strings.HasSuffix(got, ">:443") {
		t.Errorf("IP(redacted IPv6:port) = %q, want <ip-N>:443 (placeholder, unbracketed)", got)
	}
}

// Every branch of IP registers the address as written, not normalised, so
// Text finds the raw spelling.
func TestIPRegistersRawSpelling(t *testing.T) {
	r := testRedactor()
	r.IP("[2001:DB8::1]:443")
	if _, ok := r.originals["2001:DB8::1"]; !ok {
		t.Errorf("originals has no entry for the raw spelling %q", "2001:DB8::1")
	}
}

// An allowlisted repository's digest still goes through digestShape.
func TestImageDigestOnAllowlistedRepo(t *testing.T) {
	r := testRedactor()
	kept := "ghcr.io/brig-sh/claude-code@" + realDigest
	if got := r.Image(kept); got != kept {
		t.Errorf("Image(%q) = %q, want unchanged", kept, got)
	}
	if got := r.Image("ghcr.io/brig-sh/claude-code@acme"); strings.Contains(got, "acme") {
		t.Errorf("Image leaked a non-digest value on an allowlisted repo: %q", got)
	}
}

// An allowlisted domain matches case-insensitively.
func TestAllowDomainsMatchAnyCase(t *testing.T) {
	r := New(Host{}, Allow{Domains: []string{"GitHub.com", ".Anthropic.com"}})
	for _, h := range []string{"github.com", "GITHUB.COM", "api.anthropic.com", "API.Anthropic.COM"} {
		if got := r.Domain(h); got != h {
			t.Errorf("Domain(%q) = %q, want unchanged", h, got)
		}
	}
}

// Two spellings of one host name are one host, and get one placeholder.
func TestDomainPlaceholderIgnoresCase(t *testing.T) {
	r := testRedactor()
	a, b := r.Domain("Acme.io"), r.Domain("acme.io")
	if a != b || a != "<domain-1>" {
		t.Errorf("Domain(Acme.io) = %q, Domain(acme.io) = %q, want one <domain-1>", a, b)
	}
	if got := r.Domain("CANARY-BOX.LOCAL"); got != "<host>" {
		t.Errorf("Domain of the host name in another case = %q, want <host>", got)
	}
}

// A value seen under a second kind keeps its first placeholder, and Kinds
// lists both.
func TestKindsListsEveryKindAValueWasSeenAs(t *testing.T) {
	r := testRedactor()
	p := r.Name(KindProfile, "acme-canary")
	if got := r.Name(KindLabel, "acme-canary"); got != p {
		t.Errorf("second kind got %q, want the first placeholder %q", got, p)
	}
	if got, want := r.Kinds(), []string{"label", "profile"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Kinds = %v, want %v", got, want)
	}
}
