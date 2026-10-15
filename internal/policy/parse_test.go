package policy

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseValidYAML(t *testing.T) {
	doc := `
apiVersion: brig.sh/v1alpha1
name: no-net
desc: the agent reaches nothing outbound
egress:
  default: deny
  allow:
    - host: api.anthropic.com
    - host: "*.githubusercontent.com"
`
	p, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Metadata.Name != "no-net" {
		t.Errorf("name = %q, want no-net", p.Metadata.Name)
	}
	if p.Spec.Egress.Default != "deny" {
		t.Errorf("egress.default = %q, want deny", p.Spec.Egress.Default)
	}
	if len(p.Spec.Egress.Allow) != 2 {
		t.Errorf("egress.allow has %d entries, want 2", len(p.Spec.Egress.Allow))
	}
}

func TestParseValidJSON(t *testing.T) {
	doc := `{
		"apiVersion": "brig.sh/v1alpha1",
		"name": "no-net",
		"egress": {"default": "deny", "allow": [{"host": "api.anthropic.com"}]}
	}`
	if _, err := Parse([]byte(doc)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

// TestParseRefusesUnknownField checks that an unrecognised field -- at the
// top level or nested under egress -- fails to parse rather than being
// silently dropped.
func TestParseRefusesUnknownField(t *testing.T) {
	cases := []string{
		`
apiVersion: brig.sh/v1alpha1
name: no-net
unexpected: true
egress:
  default: deny
`,
		`
apiVersion: brig.sh/v1alpha1
name: no-net
egress:
  unexpected: true
  default: deny
`,
	}
	for _, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("Parse(%q) should have refused the unrecognised field, got no error", doc)
		}
	}
}

func TestParseRefusesMisspelledField(t *testing.T) {
	doc := `
apiVersion: brig.sh/v1alpha1
name: no-net
dsc: a typo for desc
egress:
  default: deny
`
	if _, err := Parse([]byte(doc)); err == nil {
		t.Fatal("a misspelled field should be refused, not silently dropped")
	}
}

func TestParseSurfacesValidationErrors(t *testing.T) {
	doc := `
apiVersion: brig.sh/v1alpha1
name: no-net
egress:
  default: maybe
`
	_, err := Parse([]byte(doc))
	if err == nil {
		t.Fatal("expected an error for an unrecognised egress.default")
	}
	if !strings.Contains(err.Error(), "maybe") {
		t.Errorf("error %q does not name the bad value", err)
	}
}

const flatDoc = `
apiVersion: brig.sh/v1alpha1
name: locked-down
desc: only Anthropic's API and one internal range
egress:
  default: deny
  allow:
    - host: api.anthropic.com
    - cidr: 10.0.0.0/8
`

const envelopeDoc = `
apiVersion: brig.sh/v1alpha1
kind: Policy
metadata:
  name: locked-down
  description: only Anthropic's API and one internal range
spec:
  egress:
    default: deny
    allow:
      - host: api.anthropic.com
      - cidr: 10.0.0.0/8
`

// The flat form converts into the same value the envelope decodes to, so
// nothing above this package handles two shapes.
func TestParseConvertsTheFlatForm(t *testing.T) {
	flat, isFlat, err := parse([]byte(flatDoc))
	if err != nil {
		t.Fatalf("the flat form did not parse: %v", err)
	}
	if !isFlat {
		t.Error("a document with no kind: was not reported as the flat form")
	}
	envelope, isFlat, err := parse([]byte(envelopeDoc))
	if err != nil {
		t.Fatalf("the envelope did not parse: %v", err)
	}
	if isFlat {
		t.Error("the envelope was reported as the flat form")
	}
	if !reflect.DeepEqual(flat, envelope) {
		t.Errorf("the two forms differ:\nflat:     %+v\nenvelope: %+v", flat, envelope)
	}
}

func TestParseRefusesAMixOfTheTwoForms(t *testing.T) {
	for _, doc := range []string{
		// kind: makes it an envelope, where name: is unknown.
		"apiVersion: brig.sh/v1alpha1\nkind: Policy\nname: a\nspec: {egress: {default: deny}}\n",
		// No kind: makes it the flat form, where spec: is unknown.
		"apiVersion: brig.sh/v1alpha1\nname: a\nspec: {egress: {default: deny}}\n",
		// kind: with an empty value is an envelope, and is refused there.
		"apiVersion: brig.sh/v1alpha1\nkind: \"\"\nmetadata: {name: a}\nspec: {egress: {default: deny}}\n",
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("Parse accepted:\n%s", doc)
		}
	}
}

func TestLoadAllRecordsTheFlatForm(t *testing.T) {
	dir := t.TempDir()
	writePolicy(t, dir, "flat.yaml", strings.Replace(flatDoc, "locked-down", "flat", 1))
	writePolicy(t, dir, "envelope.yaml", strings.Replace(envelopeDoc, "locked-down", "envelope", 1))

	entries, err := LoadAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !entries["flat"].Flat {
		t.Error("the flat file was not recorded as the flat form")
	}
	if e, ok := entries["envelope"]; !ok || e.Flat {
		t.Errorf("the envelope file was not loaded as an envelope: %+v", e)
	}
}

// The flat form took desc: over several lines, which metadata.description
// refuses. Such a file loaded before the envelope, so it still loads.
func TestParseJoinsAMultiLineFlatDesc(t *testing.T) {
	doc := "apiVersion: brig.sh/v1alpha1\nname: no-net\ndesc: |\n  only Anthropic's API\n\n" +
		"  and one internal range\negress:\n  default: deny\n"
	p, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("a flat document with a multi-line desc did not load: %v", err)
	}
	if got, want := p.Metadata.Description, "only Anthropic's API and one internal range"; got != want {
		t.Errorf("description = %q, want %q", got, want)
	}
	// The envelope holds one line, as it always did.
	envelope := "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata:\n  name: no-net\n" +
		"  description: |\n    a\n    b\nspec:\n  egress:\n    default: deny\n"
	if _, err := Parse([]byte(envelope)); err == nil {
		t.Error("an envelope with a two-line description was accepted")
	}
}

// The flat form is decoded as strictly as the envelope: a lookalike key is
// refused there too.
func TestParseRefusesALookalikeKeyInTheFlatForm(t *testing.T) {
	for _, doc := range []string{
		"apiVersion: brig.sh/v1alpha1\nname: no-net\nName: other\negress:\n  default: deny\n",
		"apiVersion: brig.sh/v1alpha1\nname: no-net\negress:\n  default: deny\n  Default: allow\n",
		"apiVersion: brig.sh/v1alpha1\nname: no-net\negress:\n  default: deny\n---\nname: two\n",
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("Parse accepted:\n%s", doc)
		}
	}
}

// A file that ends in a bare --- loaded before the envelope, in the flat
// form. It still loads, and so does an envelope that ends the same way.
func TestParseLoadsAFileThatEndsInAMarker(t *testing.T) {
	for _, doc := range []string{flatDoc, envelopeDoc} {
		want, err := Parse([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		for _, tail := range []string{"---\n", "---\n# more later\n"} {
			p, err := Parse([]byte(doc + tail))
			if err != nil {
				t.Errorf("Parse refused a file that ends in %q: %v\n%s", tail, err, doc)
				continue
			}
			if !reflect.DeepEqual(p, want) {
				t.Errorf("with a trailing %q the policy is\n%+v\nwant\n%+v", tail, p, want)
			}
		}
	}
}

// The flat form was decoded through encoding/json, which matches a key to a
// field without regard to case, so a file that spells Egress: or Host: loaded
// before the envelope. It still loads, as the flat form, and converts to the
// same policy as its lower-case spelling. The envelope stays exact.
func TestParseLoadsAFlatDocumentInAnyCase(t *testing.T) {
	doc := "apiVersion: brig.sh/v1alpha1\nName: locked-down\nDesc: only Anthropic's API and one internal range\n" +
		"Egress:\n  Default: deny\n  Allow:\n    - Host: api.anthropic.com\n    - CIDR: 10.0.0.0/8\n"
	p, isFlat, err := parse([]byte(doc))
	if err != nil {
		t.Fatalf("a flat document in mixed case did not load: %v", err)
	}
	if !isFlat {
		t.Error("it was not reported as the flat form")
	}
	want, _, err := parse([]byte(flatDoc))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("the mixed-case document converts to\n%+v\nwant\n%+v", p, want)
	}
	envelope := strings.Replace(envelopeDoc, "    allow:", "    Allow:", 1)
	if _, err := Parse([]byte(envelope)); err == nil {
		t.Error("an envelope with Allow: was accepted")
	}
}
