package policy

import (
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
	if p.Name != "no-net" {
		t.Errorf("name = %q, want no-net", p.Name)
	}
	if p.Egress.Default != "deny" {
		t.Errorf("egress.default = %q, want deny", p.Egress.Default)
	}
	if len(p.Egress.Allow) != 2 {
		t.Errorf("egress.allow has %d entries, want 2", len(p.Egress.Allow))
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
