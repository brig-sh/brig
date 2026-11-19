package policy

import (
	"strings"
	"testing"
)

// passthroughDoc is a policy with the docs' passthrough route on the
// anthropic-api-key preset, and egress as given (YAML under spec:, or "").
func passthroughDoc(name, egress string) string {
	return `apiVersion: brig.sh/v1alpha1
kind: Policy
metadata:
  name: ` + name + `
spec:
` + egress + `  routes:
    - name: anthropic
      preset: anthropic-api-key
      passthrough: true
      request:
        body:
          toolUse:
            deny: [Bash]
`
}

// egressDoc is a policy with egress alone.
func egressDoc(name, egress string) string {
	return "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata:\n  name: " + name + "\nspec:\n" + egress
}

func mustParse(t *testing.T, doc string) Policy {
	t.Helper()
	p, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, doc)
	}
	return p
}

func wantErr(t *testing.T, err error, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("got no error, want one with %q", parts)
	}
	for _, part := range parts {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("error %q does not hold %q", err, part)
		}
	}
}

// The starter from `brig policy create` allows api.anthropic.com. With the
// passthrough route added, the guest could send its key there directly, so
// the document is refused when it is read, and the refusal says what to drop.
func TestStarterWithAPassthroughRouteIsRefused(t *testing.T) {
	doc := passthroughDoc("starter", "  egress:\n    default: deny\n    allow:\n      - host: api.anthropic.com\n")
	_, err := Parse([]byte(doc))
	wantErr(t, err, "route anthropic is passthrough", "allow host api.anthropic.com",
		"remove the allow for api.anthropic.com", "through the broker")
}

// A glob reaches the upstream as well as the name does.
func TestAnAllowGlobOverAPassthroughUpstreamIsRefused(t *testing.T) {
	doc := passthroughDoc("glob", "  egress:\n    default: deny\n    allow:\n      - host: \"*.anthropic.com\"\n")
	_, err := Parse([]byte(doc))
	wantErr(t, err, "allow host *.anthropic.com", "remove the allow for api.anthropic.com")
}

// The upstream's addresses are known only at run time, so any allow cidr is
// refused, and the refusal says why.
func TestAnAllowCIDRWithAPassthroughRouteIsRefused(t *testing.T) {
	doc := passthroughDoc("cidr", "  egress:\n    default: deny\n    allow:\n      - cidr: 10.0.0.0/8\n")
	_, err := Parse([]byte(doc))
	wantErr(t, err, "allow cidr 10.0.0.0/8", "cannot resolve the name before the run")
}

// A deny default with no allow for the upstream leaves the broker the only
// way to it, and the run is accepted.
func TestADenyDefaultWithoutTheAllowIsAccepted(t *testing.T) {
	for _, egress := range []string{
		"  egress:\n    default: deny\n",
		"  egress:\n    default: deny\n    allow:\n      - host: github.com\n",
		// Deny beats allow at both enforcers.
		"  egress:\n    default: deny\n    allow:\n      - host: \"*.anthropic.com\"\n    deny:\n      - host: api.anthropic.com\n",
	} {
		p := mustParse(t, passthroughDoc("checked", egress))
		if err := RefusePassthroughEgress([]Policy{p}); err != nil {
			t.Errorf("egress\n%s\nwas refused: %v", egress, err)
		}
	}
}

// Policy B allows the host that policy A's passthrough route reaches. Each
// reads alone, and the run that binds both is refused, naming B.
func TestAMergedAllowOfThePassthroughUpstreamIsRefused(t *testing.T) {
	a := mustParse(t, passthroughDoc("a", "  egress:\n    default: deny\n"))
	b := mustParse(t, egressDoc("b", "  egress:\n    default: deny\n    allow:\n      - host: api.anthropic.com\n"))
	err := RefusePassthroughEgress([]Policy{a, b})
	wantErr(t, err, "route anthropic of policy a is passthrough", "allow host api.anthropic.com of policy b",
		"remove the allow for api.anthropic.com")
}

// With no egress bound, the guest reaches every host.
func TestAPassthroughRouteWithNoEgressIsRefused(t *testing.T) {
	p := mustParse(t, passthroughDoc("bare", ""))
	wantErr(t, RefusePassthroughEgress([]Policy{p}), "no policy bound to the run has spec.egress",
		"default: deny")
}

// Under an allow default a host deny is best effort: a connection sent
// straight to the upstream's address asks for no name.
func TestAPassthroughRouteUnderAnAllowDefaultIsRefused(t *testing.T) {
	for _, egress := range []string{
		"  egress:\n    default: allow\n",
		"  egress:\n    default: allow\n    deny:\n      - host: api.anthropic.com\n",
	} {
		p := mustParse(t, passthroughDoc("open", egress))
		wantErr(t, RefusePassthroughEgress([]Policy{p}), "the egress default is allow", "best effort")
	}
	// One deny default among the bound policies makes the run's default deny.
	a := mustParse(t, passthroughDoc("a", "  egress:\n    default: allow\n"))
	b := mustParse(t, egressDoc("b", "  egress:\n    default: deny\n"))
	if err := RefusePassthroughEgress([]Policy{a, b}); err != nil {
		t.Errorf("a run whose merged default is deny was refused: %v", err)
	}
}

// A route that holds the credential is not held to the rule: the guest has
// no key to send past the broker.
func TestACredentialRouteKeepsItsAllow(t *testing.T) {
	doc := egressDoc("cred", "  egress:\n    default: allow\n    allow:\n      - host: api.anthropic.com\n") +
		"  routes:\n    - name: anthropic\n      preset: anthropic-api-key\n      credential:\n        secretRef:\n          name: anthropic-key\n"
	p := mustParse(t, doc)
	if err := RefusePassthroughEgress([]Policy{p}); err != nil {
		t.Errorf("a credential route was refused: %v", err)
	}
}
