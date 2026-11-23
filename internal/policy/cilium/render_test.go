package cilium

import (
	"bytes"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/policy"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// loadPolicy reads testdata/<name>.policy.yaml through policy.Parse, so the
// input is validated and route presets resolve as they do for brig.
func loadPolicy(t *testing.T, name string) *v1alpha1.Policy {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join("testdata", name+".policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := policy.Parse(blob)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return &p
}

// checkGolden compares got with testdata/<name>.cnp.yaml, and rewrites the
// file under -update.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name+".cnp.yaml")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file\n--- got:\n%s\n--- want:\n%s", name, got, want)
	}
}

func TestRenderGolden(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		// wantErr, when set, is part of the error Render must return. No
		// golden file exists for such a case.
		wantErr string
	}{
		{name: "deny-default", opts: Options{Namespace: "agents", PodSelector: map[string]string{"app": "claude"}}},
		{name: "allow-default"},
		{name: "deny-host-dropped"},
		{name: "pod-identity", opts: Options{PodIdentity: true}},
		{name: "cluster", opts: Options{Cluster: true, PodIdentity: true}},
		{name: "refused-glob", wantErr: "only '*' is a wildcard"},
		{name: "refused-host-deny", wantErr: "Cilium egressDeny has no toFQDNs"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := RenderYAML(loadPolicy(t, c.name), c.opts)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, c.name, got)
		})
	}
}

// A refused rule is named by its list and index, so the author can find it
// in a policy with many rules.
func TestRenderErrorsNameTheRule(t *testing.T) {
	for name, want := range map[string]string{
		"refused-glob":      `allow[0] host "api-?.example.com"`,
		"refused-host-deny": `deny[0] host "api.example.com" overlaps allow[0] host "*.example.com"`,
	} {
		_, err := Render(loadPolicy(t, name), Options{})
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%s: err = %v, want one starting %q", name, err, want)
		}
	}
}

// A deny glob that covers no allow host as a whole can still deny a name an
// allow host lets through. Dropping it would let that name out.
func TestRenderRefusesOverlappingDenyGlob(t *testing.T) {
	p := &v1alpha1.Policy{
		Metadata: v1alpha1.ObjectMeta{Name: "overlap"},
		Spec: v1alpha1.PolicySpec{Egress: &v1alpha1.Egress{
			Default: v1alpha1.EgressDeny,
			Allow:   []v1alpha1.Rule{{Host: "api.example.com"}},
			Deny:    []v1alpha1.Rule{{Host: "*.example.com"}},
		}},
	}
	_, err := Render(p, Options{})
	if err == nil || !strings.Contains(err.Error(), "deny[0]") {
		t.Fatalf("err = %v, want a refusal of deny[0]", err)
	}
}

// Under an allow default Cilium has nothing to leave out, so a deny host
// cannot be rendered at all.
func TestRenderRefusesHostDenyUnderAllowDefault(t *testing.T) {
	p := &v1alpha1.Policy{
		Metadata: v1alpha1.ObjectMeta{Name: "allow-host-deny"},
		Spec: v1alpha1.PolicySpec{Egress: &v1alpha1.Egress{
			Default: v1alpha1.EgressAllow,
			Deny:    []v1alpha1.Rule{{CIDR: "203.0.113.0/24"}, {Host: "evil.example.com"}},
		}},
	}
	_, err := Render(p, Options{})
	if err == nil || !strings.Contains(err.Error(), "deny[1] host") {
		t.Fatalf("err = %v, want a refusal of deny[1]", err)
	}
}

// A routes-only policy says nothing about egress, and an empty CNP would
// read as a statement that it does.
func TestRenderRefusesMissingEgress(t *testing.T) {
	p := &v1alpha1.Policy{
		Metadata: v1alpha1.ObjectMeta{Name: "routes-only"},
		Spec:     v1alpha1.PolicySpec{Routes: []v1alpha1.Route{{Name: "anthropic"}}},
	}
	if _, err := Render(p, Options{}); err == nil || !strings.Contains(err.Error(), "spec.egress") {
		t.Fatalf("err = %v, want a refusal that names spec.egress", err)
	}
	if _, err := Render(nil, Options{}); err == nil {
		t.Fatal("Render(nil) returned no error")
	}
}

// brig takes '_' in a policy name and Kubernetes does not. Without
// Options.Name the renderer must refuse rather than write an object kubectl
// rejects.
func TestRenderRefusesNonKubernetesName(t *testing.T) {
	p := &v1alpha1.Policy{
		Metadata: v1alpha1.ObjectMeta{Name: "claude_only"},
		Spec:     v1alpha1.PolicySpec{Egress: &v1alpha1.Egress{Default: v1alpha1.EgressDeny}},
	}
	if _, err := Render(p, Options{}); err == nil || !strings.Contains(err.Error(), `"claude_only"`) {
		t.Fatalf("err = %v, want a refusal that names claude_only", err)
	}
	np, err := Render(p, Options{Name: "claude-only"})
	if err != nil {
		t.Fatalf("with Options.Name: %v", err)
	}
	if np.Metadata.Name != "claude-only" {
		t.Fatalf("name = %q, want claude-only", np.Metadata.Name)
	}
}

// The policy must not change when the caller reuses its selector map.
func TestRenderDoesNotAliasSelector(t *testing.T) {
	p := &v1alpha1.Policy{
		Metadata: v1alpha1.ObjectMeta{Name: "alias"},
		Spec:     v1alpha1.PolicySpec{Egress: &v1alpha1.Egress{Default: v1alpha1.EgressDeny}},
	}
	sel := map[string]string{"app": "claude"}
	np, err := Render(p, Options{PodSelector: sel})
	if err != nil {
		t.Fatal(err)
	}
	sel["app"] = "other"
	sel["extra"] = "x"
	got := np.Spec.EndpointSelector.MatchLabels
	if len(got) != 1 || got["app"] != "claude" {
		t.Fatalf("selector = %v after the caller changed its map, want map[app:claude]", got)
	}
}

// TestRenderDenyHostUnderAllowCIDR checks that a deny host is refused when an
// allow CIDR may contain its addresses.
func TestRenderDenyHostUnderAllowCIDR(t *testing.T) {
	p := &v1alpha1.Policy{}
	p.Metadata.Name = "p"
	p.Spec.Egress = &v1alpha1.Egress{
		Default: v1alpha1.EgressDeny,
		Allow:   []v1alpha1.Rule{{CIDR: "0.0.0.0/0"}},
		Deny:    []v1alpha1.Rule{{Host: "evil.example.com"}},
	}
	_, err := Render(p, Options{})
	if err == nil || !strings.Contains(err.Error(), "allow[0] cidr") {
		t.Fatalf("err = %v, want one naming allow[0] cidr", err)
	}
}

// TestRenderLeadingGlobSpansLabels checks the patterns a leading glob becomes.
func TestRenderLeadingGlobSpansLabels(t *testing.T) {
	for in, want := range map[string][]string{
		"*.amazonaws.com": {"**.amazonaws.com", "*.amazonaws.com", "*.*.amazonaws.com", "*.*.*.amazonaws.com",
			"*.*.*.*.amazonaws.com", "*.*.*.*.*.amazonaws.com"},
		"a*.example.com": {"a*.example.com"},
		"*":              {"*"},
	} {
		if got := ciliumPatterns(in); !slices.Equal(got, want) {
			t.Errorf("ciliumPatterns(%q) = %q, want %q", in, got, want)
		}
	}
}

// cilium117Match reports whether name matches pattern as Cilium 1.17 reads a
// matchPattern: a bare "*" matches every name, and any other '*' matches a run
// of DNS characters within one label. "**." is then one label, not several:
// stg-1 dropped sts.us-east-2.amazonaws.com under "**.amazonaws.com" alone.
func cilium117Match(pattern, name string) bool {
	if pattern == "*" {
		return true
	}
	re := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, `[-a-zA-Z0-9_]*`) + "$"
	return regexp.MustCompile(re).MatchString(name)
}

// A leading glob reaches a name several labels deep on a Cilium release that
// reads "**." as one label, and still never the apex or a name that only ends
// in the same letters.
func TestRenderLeadingGlobOnCilium117(t *testing.T) {
	p := &v1alpha1.Policy{Spec: v1alpha1.PolicySpec{Egress: &v1alpha1.Egress{
		Default: v1alpha1.EgressDeny, Allow: []v1alpha1.Rule{{Host: "*.amazonaws.com"}},
	}}}
	np, err := Render(p, Options{Name: "glob"})
	if err != nil {
		t.Fatal(err)
	}
	var patterns []string
	for _, r := range np.Spec.Egress {
		for _, f := range r.ToFQDNs {
			patterns = append(patterns, f.MatchPattern)
		}
	}
	matches := func(name string) bool {
		return slices.ContainsFunc(patterns, func(pat string) bool { return cilium117Match(pat, name) })
	}
	for _, name := range []string{"sts.amazonaws.com", "sts.us-east-2.amazonaws.com",
		"bedrock-runtime.us-east-1.amazonaws.com", "bucket.s3.dualstack.us-east-1.amazonaws.com"} {
		if !matches(name) {
			t.Errorf("%s matches none of %q", name, patterns)
		}
	}
	for _, name := range []string{"amazonaws.com", "evilamazonaws.com", "amazonaws.com.evil.example"} {
		if matches(name) {
			t.Errorf("%s matches %q", name, patterns)
		}
	}
}

// A passthrough route holds no credential, and the pod still reaches the node
// broker for it, so the policy gets the broker rule as for any route.
func TestRenderPassthroughRouteReachesTheBroker(t *testing.T) {
	p, err := policy.Parse([]byte(`apiVersion: brig.sh/v1alpha1
kind: Policy
metadata:
  name: anthropic
spec:
  egress:
    default: deny
  routes:
    - name: anthropic
      preset: anthropic-api-key
      passthrough: true
      guest:
        env:
          ANTHROPIC_BASE_URL: "{route.url}"
`))
	if err != nil {
		t.Fatal(err)
	}
	np, err := Render(&p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	found := slices.ContainsFunc(np.Spec.Egress, func(r EgressRule) bool { return slices.Equal(r.ToCIDR, []string{BrokerCIDR}) })
	if !found {
		t.Errorf("no rule to the broker at %s in %+v", BrokerCIDR, np.Spec.Egress)
	}
}

// dnsNames returns the names and patterns of the DNS rule of np.
func dnsNames(t *testing.T, np *NetworkPolicy) []DNSRule {
	t.Helper()
	r := np.Spec.Egress[0]
	if len(r.ToEndpoints) == 0 || len(r.ToPorts) != 1 || r.ToPorts[0].Rules == nil {
		t.Fatalf("the first egress rule is not the DNS rule: %+v", r)
	}
	return r.ToPorts[0].Rules.DNS
}

// dnsAnswers reports whether a lookup of name passes the DNS rule as Cilium
// 1.17 reads it.
func dnsAnswers(rules []DNSRule, name string) bool {
	return slices.ContainsFunc(rules, func(r DNSRule) bool {
		if r.MatchName != "" {
			return r.MatchName == name
		}
		return cilium117Match(r.MatchPattern, name)
	})
}

// Negative: under a deny default the DNS proxy refuses a name the policy does
// not allow. A pattern of "*" let a guest carry data out in its lookups, as
// in <data>.attacker.example, which brig's own resolver refuses.
func TestRenderDenyDefaultLooksUpOnlyAllowedNames(t *testing.T) {
	np, err := Render(loadPolicy(t, "deny-default"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	rules := dnsNames(t, np)
	if slices.Contains(rules, DNSRule{MatchPattern: "*"}) {
		t.Fatalf("the DNS rule lets every name through: %+v", rules)
	}
	for _, name := range []string{"c2VjcmV0.attacker.example", "attacker.example",
		"api.anthropic.com.attacker.example", "evilgithubusercontent.com"} {
		if dnsAnswers(rules, name) {
			t.Errorf("a lookup of %s passes the DNS rule", name)
		}
	}
	for _, name := range []string{"api.anthropic.com", "raw.githubusercontent.com",
		"a.b.c.d.e.githubusercontent.com", "brig.internal"} {
		if !dnsAnswers(rules, name) {
			t.Errorf("a lookup of %s is refused", name)
		}
	}
}

// The resolver looks a name up under each search domain before it looks it
// up as written. The proxy refuses a lookup it has no pattern for, and musl
// waits on that refusal until it times out, so every allow host needs its
// search lookups answered.
func TestRenderDenyDefaultAnswersTheSearchLookups(t *testing.T) {
	np, err := Render(loadPolicy(t, "deny-default"), Options{DNSSearch: []string{"US-West-2.Compute.Internal."}})
	if err != nil {
		t.Fatal(err)
	}
	rules := dnsNames(t, np)
	for _, name := range []string{
		"api.anthropic.com.agents.svc.cluster.local",
		"api.anthropic.com.svc.cluster.local",
		"api.anthropic.com.cluster.local",
		"a.b.c.d.e.githubusercontent.com.agents.svc.cluster.local",
		"brig.internal.agents.svc.cluster.local",
		"db.agents.svc.cluster.local",
		"api.anthropic.com.us-west-2.compute.internal",
	} {
		if !dnsAnswers(rules, name) {
			t.Errorf("a search lookup of %s is refused", name)
		}
	}
	if dnsAnswers(rules, "c2VjcmV0.compute.internal.attacker.example") {
		t.Error("a name that only holds a search domain passes the DNS rule")
	}
}

// Under an allow default every name may be reached, so every lookup is
// answered.
func TestRenderAllowDefaultLooksUpEveryName(t *testing.T) {
	np, err := Render(loadPolicy(t, "allow-default"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := dnsNames(t, np); !slices.Equal(got, []DNSRule{{MatchPattern: "*"}}) {
		t.Errorf("DNS rule = %+v, want every name", got)
	}
}

// A search domain is a name. A glob there would answer lookups the policy
// does not allow.
func TestRenderRefusesAnUnusableSearchDomain(t *testing.T) {
	for _, d := range []string{"*.example.com", "a?.example.com", ""} {
		if _, err := Render(loadPolicy(t, "deny-default"), Options{DNSSearch: []string{d}}); err == nil {
			t.Errorf("search domain %q rendered", d)
		}
	}
}

// Negative: a CIDR is written as Cilium matches it. An IPv4-mapped deny kept
// as IPv6 matched no IPv4 address, so it denied nothing.
func TestRenderWritesCanonicalCIDRs(t *testing.T) {
	p := &v1alpha1.Policy{
		Metadata: v1alpha1.ObjectMeta{Name: "cidrs"},
		Spec: v1alpha1.PolicySpec{Egress: &v1alpha1.Egress{
			Default: v1alpha1.EgressDeny,
			Allow:   []v1alpha1.Rule{{CIDR: "10.1.2.3/8"}, {CIDR: "2001:db8::1/32"}},
			Deny:    []v1alpha1.Rule{{CIDR: "::ffff:10.0.0.0/104"}, {CIDR: "::ffff:192.168.1.7/120"}},
		}},
	}
	np, err := Render(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var allow, deny []string
	for _, r := range np.Spec.Egress[1:] {
		allow = append(allow, r.ToCIDR...)
	}
	for _, r := range np.Spec.EgressDeny {
		deny = append(deny, r.ToCIDR...)
	}
	if want := []string{"10.0.0.0/8", "2001:db8::/32"}; !slices.Equal(allow, want) {
		t.Errorf("allow CIDRs = %q, want %q", allow, want)
	}
	if want := []string{"10.0.0.0/8", "192.168.1.0/24"}; !slices.Equal(deny, want) {
		t.Errorf("deny CIDRs = %q, want %q", deny, want)
	}
}

// A CIDR that does not parse is refused by its rule, not written as it is.
func TestRenderRefusesABadCIDR(t *testing.T) {
	for name, eg := range map[string]*v1alpha1.Egress{
		"allow[0] cidr": {Default: v1alpha1.EgressDeny, Allow: []v1alpha1.Rule{{CIDR: "10.0.0.0/33"}}},
		"deny[0] cidr":  {Default: v1alpha1.EgressAllow, Deny: []v1alpha1.Rule{{CIDR: "not-a-cidr"}}},
	} {
		p := &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: "bad"}, Spec: v1alpha1.PolicySpec{Egress: eg}}
		if _, err := Render(p, Options{}); err == nil || !strings.HasPrefix(err.Error(), name) {
			t.Errorf("err = %v, want one starting %q", err, name)
		}
	}
}

// Negative: under --cluster a deny host is refused. The cluster rule lets
// through any pod or node, such as one at *.svc.cluster.local, so dropping
// the deny host would let it through.
func TestRenderRefusesADenyHostUnderCluster(t *testing.T) {
	for _, host := range []string{"*.svc.cluster.local", "evil.example.com"} {
		p := &v1alpha1.Policy{
			Metadata: v1alpha1.ObjectMeta{Name: "cluster-deny"},
			Spec: v1alpha1.PolicySpec{Egress: &v1alpha1.Egress{
				Default: v1alpha1.EgressDeny,
				Allow:   []v1alpha1.Rule{{Host: "api.example.com"}},
				Deny:    []v1alpha1.Rule{{Host: host}},
			}},
		}
		if _, err := Render(p, Options{Cluster: true}); err == nil || !strings.Contains(err.Error(), "--cluster") {
			t.Errorf("%s: err = %v, want a refusal that names --cluster", host, err)
		}
		if _, err := Render(p, Options{}); err != nil {
			t.Errorf("%s: refused without --cluster: %v", host, err)
		}
	}
}

// The broker address is an address or a CIDR, and anything else is refused
// before a rule is written for it.
func TestRenderBrokerAddress(t *testing.T) {
	p := loadPolicy(t, "deny-default")
	for in, want := range map[string]string{
		"169.254.171.9":      "169.254.171.9/32",
		"fd00::171:1":        "fd00::171:1/128",
		"::ffff:169.254.1.1": "169.254.1.1/32",
		"169.254.171.9/32":   "169.254.171.9/32",
		"fd00::171:1/128":    "fd00::171:1/128",
	} {
		np, err := Render(p, Options{BrokerCIDR: in})
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got := np.Spec.Egress[1].ToCIDR; !slices.Equal(got, []string{want}) {
			t.Errorf("%s: broker rule %q, want %q", in, got, want)
		}
	}
	// Negative: a range opened port 443 to all of it for every selected pod.
	for _, in := range []string{"not-an-address", "169.254.171.1/40", "brig.internal",
		"10.0.0.0/8", "169.254.171.0/24", "fd00::/64"} {
		if _, err := Render(p, Options{BrokerCIDR: in}); err == nil {
			t.Errorf("broker address %q rendered", in)
		}
		if err := (Options{BrokerCIDR: in}).Check(); err == nil {
			t.Errorf("Check passed broker address %q", in)
		}
	}
}

// A namespace that Kubernetes would refuse at apply time is refused here.
func TestRenderRefusesANonKubernetesNamespace(t *testing.T) {
	for _, ns := range []string{"Agents", "agents.prod", "-agents", "agents_", strings.Repeat("a", 64)} {
		if _, err := Render(loadPolicy(t, "deny-default"), Options{Namespace: ns}); err == nil {
			t.Errorf("namespace %q rendered", ns)
		}
	}
	if _, err := Render(loadPolicy(t, "deny-default"), Options{Namespace: "agents-1"}); err != nil {
		t.Errorf("a valid namespace was refused: %v", err)
	}
}

// The Pod Identity agent answers at an IPv6 address on an IPv6 cluster, and
// the rule opens both.
func TestRenderPodIdentityOpensBothAddresses(t *testing.T) {
	np, err := Render(loadPolicy(t, "pod-identity"), Options{PodIdentity: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{PodIdentityCIDR, PodIdentityCIDR6}
	if !slices.ContainsFunc(np.Spec.Egress, func(r EgressRule) bool { return slices.Equal(r.ToCIDR, want) }) {
		t.Errorf("no rule to %q in %+v", want, np.Spec.Egress)
	}
}

// Negative: under --cluster a deny CIDR is refused. Cilium matches pods and
// nodes by identity, so the deny could not take back what the cluster rule
// lets through.
func TestRenderRefusesADenyCIDRUnderCluster(t *testing.T) {
	p := &v1alpha1.Policy{
		Metadata: v1alpha1.ObjectMeta{Name: "cluster-deny-cidr"},
		Spec: v1alpha1.PolicySpec{Egress: &v1alpha1.Egress{
			Default: v1alpha1.EgressDeny,
			Allow:   []v1alpha1.Rule{{Host: "api.example.com"}},
			Deny:    []v1alpha1.Rule{{CIDR: "10.0.0.0/8"}},
		}},
	}
	if _, err := Render(p, Options{Cluster: true}); err == nil || !strings.Contains(err.Error(), "deny[0] cidr") {
		t.Errorf("err = %v, want a refusal of deny[0] cidr", err)
	}
	if _, err := Render(p, Options{}); err != nil {
		t.Errorf("refused without --cluster: %v", err)
	}
}

// A dropped deny host is reported, because it stays denied only while no
// other policy for the same pods allows it.
func TestRenderReportsADroppedDenyHost(t *testing.T) {
	np, err := Render(loadPolicy(t, "deny-host-dropped"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := np.DroppedDenyHosts(); !slices.Equal(got, []string{"evil.example.com"}) {
		t.Errorf("DroppedDenyHosts = %q, want evil.example.com", got)
	}
	np, err = Render(loadPolicy(t, "deny-default"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := np.DroppedDenyHosts(); len(got) != 0 {
		t.Errorf("DroppedDenyHosts = %q with no deny host", got)
	}
	out, err := RenderYAML(loadPolicy(t, "deny-host-dropped"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("evil.example.com")) {
		t.Errorf("the dropped host reached the object:\n%s", out)
	}
}

// clusterRanges is the address range of the pods and nodes of the test
// cluster: the VPC of an EKS cluster with ENI addressing.
var clusterRanges = []string{"10.2.0.0/16"}

// cidrPolicy returns a policy with the given default and CIDR rules.
func cidrPolicy(def v1alpha1.EgressDefault, allow, deny []string) *v1alpha1.Policy {
	eg := &v1alpha1.Egress{Default: def}
	for _, c := range allow {
		eg.Allow = append(eg.Allow, v1alpha1.Rule{CIDR: c})
	}
	for _, c := range deny {
		eg.Deny = append(eg.Deny, v1alpha1.Rule{CIDR: c})
	}
	return &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: "cidrs"}, Spec: v1alpha1.PolicySpec{Egress: eg}}
}

func TestClusterReach(t *testing.T) {
	given := resolved{cluster: []netip.Prefix{netip.MustParsePrefix("10.2.0.0/16"), netip.MustParsePrefix("100.64.0.0/16")}}
	for c, want := range map[string]reach{
		"0.0.0.0/0":      reachAll,
		"10.2.0.0/16":    reachPart,
		"10.0.0.0/8":     reachPart,
		"100.64.0.0/10":  reachPart,
		"10.2.145.91/32": reachPart,
		"1.1.1.1/32":     reachNone,
		"10.3.0.0/16":    reachNone,
	} {
		if got := given.clusterReach(c); got != want {
			t.Errorf("%s with two ranges: reach %d, want %d", c, got, want)
		}
	}
	one := resolved{cluster: []netip.Prefix{netip.MustParsePrefix("10.2.0.0/16")}}
	for c, want := range map[string]reach{
		"10.0.0.0/8":     reachAll,
		"10.2.0.0/16":    reachAll,
		"10.2.128.0/17":  reachPart,
		"192.168.0.0/16": reachNone,
	} {
		if got := one.clusterReach(c); got != want {
			t.Errorf("%s with one range: reach %d, want %d", c, got, want)
		}
	}
	var none resolved
	for c, want := range map[string]reach{
		"10.2.145.91/32": reachMaybe,
		"172.20.0.0/16":  reachMaybe,
		"fd00::/8":       reachMaybe,
		"1.1.1.1/32":     reachNone,
		"2606:4700::/32": reachNone,
	} {
		if got := none.clusterReach(c); got != want {
			t.Errorf("%s with no ranges: reach %d, want %d", c, got, want)
		}
	}
}

// Negative: a deny that covers the cluster's ranges also denies every pod and
// node. Cilium applies no CIDR rule to them, so the toCIDR rule alone let the
// pod reach them, while brig's enforcer refuses them.
func TestRenderDenyOfTheClusterDeniesPodsAndNodes(t *testing.T) {
	np, err := Render(cidrPolicy(v1alpha1.EgressAllow, nil, []string{"10.0.0.0/8"}), Options{ClusterCIDRs: clusterRanges})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(np.Spec.EgressDeny[0].ToCIDR, []string{"10.0.0.0/8"}) {
		t.Errorf("the deny CIDR is not rendered first: %+v", np.Spec.EgressDeny)
	}
	want := clusterDeny()
	if got := np.Spec.EgressDeny[1:]; !reflect.DeepEqual(got, want) {
		t.Errorf("cluster deny = %+v, want %+v", got, want)
	}
	// The pod selectors require the namespace label. Without it NotIn would
	// also match the world identity and deny every address outside.
	for _, sel := range want[0].ToEndpoints {
		if !slices.ContainsFunc(sel.MatchExpressions, func(r LabelSelectorRequirement) bool {
			return r.Key == "k8s:io.kubernetes.pod.namespace" && (r.Operator == "Exists" || r.Operator == "In")
		}) {
			t.Errorf("selector %+v does not require the namespace label", sel)
		}
	}
	// kube-dns stays reachable on 53 alone, for the DNS rule.
	var ports []PortProtocol
	for _, pr := range want[2].ToPorts {
		ports = append(ports, pr.Ports...)
	}
	for _, p := range ports {
		lo, _ := strconv.Atoi(p.Port)
		if lo <= 53 && 53 <= max(lo, p.EndPort) {
			t.Errorf("kube-dns port 53 is denied by %+v", p)
		}
	}
	if len(ports) != 2 || ports[0].Port != "1" || ports[1].EndPort != 65535 {
		t.Errorf("kube-dns is not denied on every other port: %+v", ports)
	}
}

// Negative: a deny of part of the cluster's ranges is refused. Cilium cannot
// tell those pods and nodes from the others by address.
func TestRenderRefusesADenyOfPartOfTheCluster(t *testing.T) {
	for _, c := range []string{"10.2.145.91/32", "10.2.128.0/17"} {
		_, err := Render(cidrPolicy(v1alpha1.EgressAllow, nil, []string{c}), Options{ClusterCIDRs: clusterRanges})
		if err == nil || !strings.HasPrefix(err.Error(), "deny[0] cidr") || !strings.Contains(err.Error(), "part of the cluster") {
			t.Errorf("%s: err = %v, want a refusal of deny[0]", c, err)
		}
	}
}

// Negative: without the cluster's ranges, a deny of a private range is refused
// under an allow default and under --cluster, where the pod reaches the
// cluster. A public range still renders, and so does a private one under a
// deny default without --cluster, where no rule opens the cluster.
func TestRenderAPrivateDenyNeedsTheClusterRanges(t *testing.T) {
	cases := []struct {
		def     v1alpha1.EgressDefault
		cluster bool
		cidr    string
		refused bool
	}{
		{v1alpha1.EgressAllow, false, "10.0.0.0/8", true},
		{v1alpha1.EgressAllow, false, "172.16.0.0/12", true},
		{v1alpha1.EgressAllow, false, "fd00::/8", true},
		{v1alpha1.EgressDeny, true, "192.168.0.0/16", true},
		{v1alpha1.EgressAllow, false, "1.1.1.1/32", false},
		{v1alpha1.EgressDeny, true, "1.1.1.1/32", false},
		{v1alpha1.EgressDeny, false, "10.0.0.0/8", false},
	}
	for _, c := range cases {
		p := cidrPolicy(c.def, []string{"203.0.113.0/24"}, []string{c.cidr})
		_, err := Render(p, Options{Cluster: c.cluster})
		if c.refused && (err == nil || !strings.Contains(err.Error(), "--cluster-cidr")) {
			t.Errorf("%s %s cluster=%v: err = %v, want a refusal naming --cluster-cidr", c.def, c.cidr, c.cluster, err)
		}
		if !c.refused && err != nil {
			t.Errorf("%s %s cluster=%v: refused: %v", c.def, c.cidr, c.cluster, err)
		}
	}
}

// Under --cluster with the cluster's ranges, a deny outside them renders as
// a CIDR rule, and a deny of all of them takes the cluster back.
func TestRenderDenyUnderClusterWithTheRanges(t *testing.T) {
	np, err := Render(cidrPolicy(v1alpha1.EgressDeny, nil, []string{"1.1.1.1/32", "10.0.0.0/8"}), Options{Cluster: true, ClusterCIDRs: clusterRanges})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(np.Spec.EgressDeny); got != 2+len(clusterDeny()) {
		t.Errorf("%d deny rules, want the two CIDRs and the cluster deny: %+v", got, np.Spec.EgressDeny)
	}
}

// An allow that covers the cluster's ranges also opens every pod and node,
// once. An allow of part of them leaves that part closed and says so.
func TestRenderAllowOfTheClusterOpensPodsAndNodes(t *testing.T) {
	np, err := Render(cidrPolicy(v1alpha1.EgressDeny, []string{"10.0.0.0/8", "0.0.0.0/0"}, nil), Options{ClusterCIDRs: clusterRanges})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range np.Spec.Egress {
		if slices.Equal(r.ToEntities, []string{"cluster"}) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d cluster rules, want 1: %+v", n, np.Spec.Egress)
	}
	if len(np.Warnings()) != 0 {
		t.Errorf("warnings for an allow of the whole cluster: %q", np.Warnings())
	}
	np, err = Render(cidrPolicy(v1alpha1.EgressDeny, []string{"10.2.145.91/32"}, nil), Options{ClusterCIDRs: clusterRanges})
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(np.Spec.Egress, func(r EgressRule) bool { return len(r.ToEntities) > 0 }) {
		t.Errorf("an allow of one pod opened the cluster: %+v", np.Spec.Egress)
	}
	if w := np.Warnings(); len(w) != 1 || !strings.Contains(w[0], "allow[0]") {
		t.Errorf("warnings = %q, want one for allow[0]", w)
	}
	np, err = Render(cidrPolicy(v1alpha1.EgressDeny, []string{"10.0.0.0/8"}, nil), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if w := np.Warnings(); len(w) != 1 || !strings.Contains(w[0], "--cluster-cidr") {
		t.Errorf("warnings = %q, want one naming --cluster-cidr", w)
	}
}

// A cluster range that is not a CIDR is refused.
func TestRenderRefusesABadClusterRange(t *testing.T) {
	if err := (Options{ClusterCIDRs: []string{"10.2.0.0"}}).Check(); err == nil {
		t.Error("a cluster range without a length passed")
	}
}

// Negative: an IPv6 deny under an allow default or --cluster is refused when
// --cluster-cidr names no IPv6 range. The pods and nodes of an EKS IPv6
// cluster take global addresses (2600:...), outside every fallback range, so
// the deny was written as a toCIDR rule that reaches none of them.
func TestRenderRefusesAnIPv6DenyWithoutTheClusterIPv6Ranges(t *testing.T) {
	cases := []struct {
		def     v1alpha1.EgressDefault
		cluster bool
		ranges  []string
		cidr    string
	}{
		{v1alpha1.EgressAllow, false, nil, "2600:1f14::/32"},
		{v1alpha1.EgressAllow, false, nil, "2000::/3"},
		{v1alpha1.EgressAllow, false, []string{"10.2.0.0/16"}, "2600:1f14::/32"},
		{v1alpha1.EgressDeny, true, []string{"10.2.0.0/16"}, "2000::/3"},
	}
	for _, c := range cases {
		_, err := Render(cidrPolicy(c.def, nil, []string{c.cidr}), Options{Cluster: c.cluster, ClusterCIDRs: c.ranges})
		if err == nil || !strings.Contains(err.Error(), "names no IPv6 range") {
			t.Errorf("%s %s cluster=%v ranges=%q: err = %v, want a refusal of the IPv6 deny", c.def, c.cidr, c.cluster, c.ranges, err)
		}
	}
	// A deny default without --cluster never reaches the cluster, so the deny
	// renders as before.
	if _, err := Render(cidrPolicy(v1alpha1.EgressDeny, []string{"203.0.113.0/24"}, []string{"2600:1f14::/32"}), Options{}); err != nil {
		t.Errorf("an IPv6 deny under a deny default was refused: %v", err)
	}
}

// With the cluster's IPv6 range, an IPv6 deny outside it renders as toCIDR,
// and one that covers it denies every pod and node.
func TestRenderAnIPv6DenyWithTheClusterIPv6Range(t *testing.T) {
	ranges := []string{"10.2.0.0/16", "2600:1f14:abc::/56"}
	np, err := Render(cidrPolicy(v1alpha1.EgressAllow, nil, []string{"2606:4700::/32"}), Options{ClusterCIDRs: ranges})
	if err != nil {
		t.Fatal(err)
	}
	if len(np.Spec.EgressDeny) != 1 {
		t.Errorf("a deny outside the cluster rendered %d rules, want 1: %+v", len(np.Spec.EgressDeny), np.Spec.EgressDeny)
	}
	np, err = Render(cidrPolicy(v1alpha1.EgressAllow, nil, []string{"2600:1f14::/32"}), Options{ClusterCIDRs: ranges})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(np.Spec.EgressDeny); got != 1+len(clusterDeny()) {
		t.Errorf("a deny of the cluster's IPv6 range rendered %d rules, want the CIDR and the cluster deny", got)
	}
}

// A CIDR is read against the cluster's ranges of its own family. On a
// dual-stack cluster, an IPv4 deny that covers the IPv4 range covers the
// cluster for IPv4, whatever the IPv6 range.
func TestClusterReachReadsEachFamilyApart(t *testing.T) {
	dual := resolved{cluster: []netip.Prefix{netip.MustParsePrefix("10.2.0.0/16"), netip.MustParsePrefix("2600:1f14:abc::/56")}}
	for c, want := range map[string]reach{
		"10.0.0.0/8":         reachAll,
		"2600:1f14::/32":     reachAll,
		"2600:1f14:abc::/64": reachPart,
		"192.168.0.0/16":     reachNone,
		"2606:4700::/32":     reachNone,
	} {
		if got := dual.clusterReach(c); got != want {
			t.Errorf("%s: reach %d, want %d", c, got, want)
		}
	}
}

// A private deny outside the given ranges renders, and stderr asks whether
// the cluster uses it: a range --cluster-cidr leaves out fails open.
func TestRenderWarnsOfAPrivateDenyOutsideTheClusterRanges(t *testing.T) {
	np, err := Render(cidrPolicy(v1alpha1.EgressAllow, nil, []string{"100.64.0.0/10"}), Options{ClusterCIDRs: clusterRanges})
	if err != nil {
		t.Fatal(err)
	}
	if w := np.Warnings(); len(w) != 1 || !strings.Contains(w[0], "outside --cluster-cidr") {
		t.Errorf("warnings = %q, want one for the private range outside --cluster-cidr", w)
	}
	np, err = Render(cidrPolicy(v1alpha1.EgressAllow, nil, []string{"1.1.1.1/32"}), Options{ClusterCIDRs: clusterRanges})
	if err != nil {
		t.Fatal(err)
	}
	if w := np.Warnings(); len(w) != 0 {
		t.Errorf("a public deny warned: %q", w)
	}
}
