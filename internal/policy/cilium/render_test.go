package cilium

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
			Deny:    []v1alpha1.Rule{{CIDR: "10.0.0.0/8"}, {Host: "evil.example.com"}},
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
