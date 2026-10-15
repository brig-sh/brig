package v1alpha1

import (
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const envelopeDoc = `
apiVersion: brig.sh/v1alpha1
kind: Policy
metadata:
  name: locked-down
  description: only Anthropic's API and one internal range
  labels:
    team: platform
spec:
  egress:
    default: deny
    allow:
      - host: api.anthropic.com
      - cidr: 10.0.0.0/8
    deny:
      - host: telemetry.example.com
`

func TestDecodeEnvelope(t *testing.T) {
	p, err := Decode([]byte(envelopeDoc))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if p.Metadata.Name != "locked-down" || p.Metadata.Labels["team"] != "platform" {
		t.Errorf("metadata = %+v", p.Metadata)
	}
	if p.Spec.Egress == nil || p.Spec.Egress.Default != EgressDeny || len(p.Spec.Egress.Allow) != 2 {
		t.Errorf("spec.egress = %+v", p.Spec.Egress)
	}
}

// A document printed by `brig policy show` is meant to be saved and read back,
// so the marshaled form decodes into the same value.
func TestEnvelopeRoundTrips(t *testing.T) {
	p, err := Decode([]byte(envelopeDoc))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := yaml.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Decode(blob)
	if err != nil {
		t.Fatalf("the marshaled form does not decode: %v\n%s", err, blob)
	}
	if !reflect.DeepEqual(p, again) {
		t.Errorf("round trip changed the policy:\n%+v\n%+v", p, again)
	}
}

func TestDecodeRefusesUnknownField(t *testing.T) {
	for _, c := range []struct{ name, doc string }{
		{"top level", "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: a}\n" +
			"spec: {egress: {default: deny}}\nstatus: {}\n"},
		{"metadata", "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: a, namespace: x}\n" +
			"spec: {egress: {default: deny}}\n"},
		{"spec", "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: a}\n" +
			"spec: {egress: {default: deny}, engine: x}\n"},
		{"spec.egress", "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: a}\n" +
			"spec: {egress: {default: deny, mode: strict}}\n"},
		{"a rule", "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: a}\n" +
			"spec: {egress: {default: deny, allow: [{host: a.example, port: 443}]}}\n"},
		{"the flat form's desc", "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: a}\n" +
			"desc: x\nspec: {egress: {default: deny}}\n"},
		{"a key given twice", "apiVersion: brig.sh/v1alpha1\nkind: Policy\nkind: Policy\n" +
			"metadata: {name: a}\nspec: {egress: {default: deny}}\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Decode([]byte(c.doc)); err == nil {
				t.Errorf("Decode accepted:\n%s", c.doc)
			}
		})
	}
}

func validEnvelope() Policy {
	return Policy{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: Kind},
		Metadata: ObjectMeta{Name: "no-net"},
		Spec: PolicySpec{Egress: &Egress{
			Default: EgressDeny,
			Allow:   []Rule{{Host: "api.anthropic.com"}},
		}},
	}
}

func TestPolicyValidate(t *testing.T) {
	p := validEnvelope()
	if err := p.Validate(); err != nil {
		t.Fatalf("a valid policy was refused: %v", err)
	}
	for _, c := range []struct {
		name string
		edit func(*Policy)
		want string
	}{
		{"missing apiVersion", func(p *Policy) { p.APIVersion = "" }, "apiVersion is required"},
		{"another apiVersion", func(p *Policy) { p.APIVersion = "brig.sh/v1" }, "not supported"},
		{"missing kind", func(p *Policy) { p.Kind = "" }, "kind is required"},
		{"another kind", func(p *Policy) { p.Kind = "NetworkPolicy" }, "not supported"},
		{"missing name", func(p *Policy) { p.Metadata.Name = "" }, "metadata.name is required"},
		{"uppercase name", func(p *Policy) { p.Metadata.Name = "No-Net" }, "lowercase"},
		{"name that escapes a directory", func(p *Policy) { p.Metadata.Name = "../x" }, "lowercase"},
		{"name YAML reads as a boolean", func(p *Policy) { p.Metadata.Name = "no" }, "reads as"},
		{"two-line description", func(p *Policy) { p.Metadata.Description = "a\nb" }, "one line"},
		{"no egress block", func(p *Policy) { p.Spec.Egress = nil }, "spec.egress is required"},
		{"missing default", func(p *Policy) { p.Spec.Egress.Default = "" }, "egress.default is required"},
		{"bad default", func(p *Policy) { p.Spec.Egress.Default = "maybe" }, "maybe"},
		{"rule with neither form", func(p *Policy) { p.Spec.Egress.Deny = []Rule{{}} }, "needs host"},
		{"rule with both forms", func(p *Policy) {
			p.Spec.Egress.Allow = []Rule{{Host: "a.example", CIDR: "10.0.0.0/8"}}
		}, "not both"},
		{"cidr an octet short", func(p *Policy) {
			p.Spec.Egress.Deny = []Rule{{CIDR: "10.0.0/8"}}
		}, "not a valid CIDR"},
		{"host with a space", func(p *Policy) {
			p.Spec.Egress.Allow = []Rule{{Host: "exa mple.com"}}
		}, "whitespace"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := validEnvelope()
			c.edit(&p)
			err := p.Validate()
			if err == nil {
				t.Fatal("Validate accepted it")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not say %q", err, c.want)
			}
		})
	}
}

// encoding/json, under sigs.k8s.io/yaml, matches a key to a field without
// regard to case and folds ſ to s and the Kelvin sign to k. Each of these keys
// would set the field it resembles, and win over the real key beside it.
func TestDecodeRefusesALookalikeKey(t *testing.T) {
	for _, c := range []struct{ name, doc, want string }{
		{"hoſt in a rule", "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: a}\n" +
			"spec: {egress: {default: deny, allow: [{host: a.example, hoſt: b.example}]}}\n",
			`unknown field "spec.egress.allow[0].hoſt"`},
		{"ſpec", "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: a}\n" +
			"ſpec: {egress: {default: allow}}\nspec: {egress: {default: deny}}\n", `unknown field "ſpec"`},
		{"Spec beside spec", "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: a}\n" +
			"spec: {egress: {default: deny}}\nSpec: {egress: {default: allow}}\n", `unknown field "Spec"`},
		{"Spec alone", "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: a}\n" +
			"Spec: {egress: {default: allow}}\n", `unknown field "Spec"`},
		{"Kelvin sign in kind", "apiVersion: brig.sh/v1alpha1\nKind: Policy\nkind: Policy\n" +
			"metadata: {name: a}\nspec: {egress: {default: deny}}\n", "unknown field"},
		{"NAME in metadata", "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: a, NAME: b}\n" +
			"spec: {egress: {default: deny}}\n", `unknown field "metadata.NAME"`},
		{"a JSON key given twice", `{"apiVersion": "brig.sh/v1alpha1", "kind": "Policy",
			"metadata": {"name": "a"}, "spec": {"egress": {"default": "deny", "default": "allow"}}}`, "default"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, err := Decode([]byte(c.doc))
			if err == nil {
				t.Fatalf("Decode accepted it as %+v", p)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not say %q", err, c.want)
			}
		})
	}
}

// yaml.UnmarshalStrict reads the first document of a stream and drops the
// rest, so a second policy after --- would be ignored without a word.
func TestDecodeRefusesASecondDocument(t *testing.T) {
	first := "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: a}\nspec: {egress: {default: deny}}\n"
	for _, doc := range []string{
		first + "---\napiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: b}\nspec: {egress: {default: allow}}\n",
		first + "---\n---\nmetadata: {name: b}\n",
		// The decode would read the empty first document and drop this one.
		"---\n---\n" + first,
	} {
		if _, err := Decode([]byte(doc)); err == nil || !strings.Contains(err.Error(), "more than one YAML document") {
			t.Errorf("Decode = %v, want the second document refused:\n%s", err, doc)
		}
	}
	// A marker before the only document is not a second one.
	if _, err := Decode([]byte("---\n" + first)); err != nil {
		t.Errorf("a leading --- was refused: %v", err)
	}
}

// A trailing --- with nothing after it, or only comments, starts a document
// that holds nothing, and dropping it loses nothing. Older versions loaded
// such a file, so it is not counted as a second document.
func TestDecodeIgnoresAnEmptyTrailingDocument(t *testing.T) {
	first := "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: a}\nspec: {egress: {default: deny}}\n"
	for _, tail := range []string{
		"---\n",
		"---",
		"--- \n\n  \n",
		"---\n# the next policy goes here\n\n",
		"---\n---\n",
		"...\n---\n",
	} {
		p, err := Decode([]byte(first + tail))
		if err != nil {
			t.Errorf("Decode refused a trailing %q: %v", tail, err)
			continue
		}
		if p.Metadata.Name != "a" {
			t.Errorf("trailing %q: name = %q, want a", tail, p.Metadata.Name)
		}
	}
}

// netip.ParsePrefix refuses a prefix length with a leading zero, which
// net.ParseCIDR accepted, so such a rule is read the way it was before.
func TestRuleAcceptsWhatTheOlderCIDRParserAccepted(t *testing.T) {
	for _, cidr := range []string{"10.0.0.0/08", "10.0.0.0/8", "10.0.0.1/8", "::/0", "1.2.3.4/00"} {
		if err := (Rule{CIDR: cidr}).Validate(); err != nil {
			t.Errorf("cidr %q was refused: %v", cidr, err)
		}
	}
	for _, cidr := range []string{"10.0.0/8", "10.0.0.0/33", "10.0.0.0/8x", "fe80::1%en0/64"} {
		if err := (Rule{CIDR: cidr}).Validate(); err == nil {
			t.Errorf("cidr %q was accepted", cidr)
		}
	}
}
