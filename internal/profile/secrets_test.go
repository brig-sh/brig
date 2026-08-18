package profile

import (
	"encoding/json"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// A bare string keeps its meaning, so every profile written against the old
// schema -- and every documented example -- parses unchanged and stays
// required.
func TestBareStringSecretIsRequired(t *testing.T) {
	var list []SecretDecl
	if err := yaml.UnmarshalStrict([]byte(`["gh-token"]`), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list) != 1 || list[0].Name != "gh-token" {
		t.Fatalf("got %+v, want one entry named gh-token", list)
	}
	if !list[0].IsRequired() {
		t.Error("a bare string is not required, and it must be")
	}
	if list[0].Importable() {
		t.Error("a bare string declares a source, and it must not")
	}
}

func TestObjectFormCarriesRequiredAndSources(t *testing.T) {
	const doc = `
- name: claude-credentials
  required: false
  expiryField: expiresAt
  sources:
    - from: keychain
      service: Claude Code-credentials
    - from: file
      path: ~/.claude/.credentials.json
      hint: run ` + "`claude`" + ` on the host once to log in
`
	var list []SecretDecl
	if err := yaml.UnmarshalStrict([]byte(doc), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	d := list[0]
	if d.IsRequired() {
		t.Error("required: false did not take")
	}
	if d.ExpiryField != "expiresAt" {
		t.Errorf("expiryField = %q", d.ExpiryField)
	}
	if got := d.SourceList(); len(got) != 2 ||
		got[0].From != SourceKeychain || got[0].Service != "Claude Code-credentials" ||
		got[1].From != SourceFile || got[1].Path != "~/.claude/.credentials.json" {
		t.Errorf("sources = %+v", got)
	}
}

// The singular from: plus its locator is a one-element sources:, the same way
// ref: is a refs: of length one. One idiom, two uses.
func TestSingularFromIsAOneElementChain(t *testing.T) {
	var list []SecretDecl
	if err := yaml.UnmarshalStrict([]byte("- {name: gh-token, from: env, var: GH_TOKEN}"), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := list[0].SourceList()
	if len(got) != 1 || got[0].From != SourceEnv || got[0].Var != "GH_TOKEN" {
		t.Errorf("SourceList() = %+v", got)
	}
}

// UnmarshalStrict cannot see inside a type with its own UnmarshalJSON, so the
// element has to refuse unknown fields itself. Without this, `requred: false`
// decodes into nothing and the secret is silently required -- the exact
// failure mode strict decoding exists to prevent.
func TestUnknownFieldInASecretIsRefused(t *testing.T) {
	var list []SecretDecl
	err := yaml.UnmarshalStrict([]byte("- {name: gh-token, requred: false}"), &list)
	if err == nil {
		t.Fatal("a misspelled field was accepted")
	}
	if !strings.Contains(err.Error(), "requred") {
		t.Errorf("the error does not name the offending field: %v", err)
	}
}

// The dedupe key the importer reads each locator once by, and the string
// provenance records as `from:`.
func TestLocatorIdentifiesASource(t *testing.T) {
	a := Source{From: SourceKeychain, Service: "Claude Code-credentials"}
	b := Source{From: SourceKeychain, Service: "Claude Code-credentials", Hint: "different hint"}
	if a.Locator() != b.Locator() {
		t.Errorf("%q and %q are the same locator", a.Locator(), b.Locator())
	}
	if a.Locator() != "keychain:Claude Code-credentials" {
		t.Errorf("Locator() = %q", a.Locator())
	}
	if (Source{From: SourceFile, Path: "/x"}).Locator() == a.Locator() {
		t.Error("two different locators collide")
	}
}

// The two spellings are one type, so a bare string re-marshals as the object
// form and parses back identically -- `brig profile export --json` therefore
// normalises them into one. YAML export hands back the source bytes
// (profile.Export), so a hand-written file keeps its own spelling; this pins
// the JSON side rather than leaving it to be discovered.
func TestBareStringRoundTripsAsTheObjectForm(t *testing.T) {
	var list []SecretDecl
	if err := yaml.UnmarshalStrict([]byte(`["gh-token"]`), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != `[{"name":"gh-token"}]` {
		t.Errorf("marshalled as %s", out)
	}
	var back []SecretDecl
	if err := yaml.UnmarshalStrict(out, &back); err != nil {
		t.Fatalf("the normalised form does not parse back: %v", err)
	}
	if len(back) != 1 || back[0].Name != "gh-token" || !back[0].IsRequired() {
		t.Errorf("round trip gave %+v", back)
	}
}

// ref: IS a refs: of length one, so every caller walks one shape. The chain is
// how one variable gets a shell override and a store fallback, which
// BRIG_FORWARD_ENV cannot give it once a profile binds the name.
func TestRefListExpandsTheSingularRef(t *testing.T) {
	if got := (EnvBinding{Name: "GH_TOKEN", Ref: "secrets.gh-token"}).RefList(); len(got) != 1 || got[0] != "secrets.gh-token" {
		t.Errorf("RefList() = %v", got)
	}
	b := EnvBinding{Name: "GH_TOKEN", Refs: []string{"env.GH_TOKEN", "secrets.gh-token"}}
	if got := b.RefList(); len(got) != 2 || got[0] != "env.GH_TOKEN" {
		t.Errorf("RefList() = %v", got)
	}
	if got := (EnvBinding{Name: "X", Value: "literal"}).RefList(); got != nil {
		t.Errorf("RefList() = %v; want nil for a literal", got)
	}
}

// files: now parses, together with the delivery that honours it. The test that
// pinned its ABSENCE lived here until this commit, so that landing the schema
// had to be a deliberate deletion rather than something discovered -- a
// binding that parses and delivers nothing is the worst failure mode this
// feature has. See volumes_test.go for what replaced it.
func TestFilesParsesNowThatItIsDelivered(t *testing.T) {
	var p Profile
	doc := "files:\n  - ref: secrets.x\n    path: .config/x\n    mode: \"0600\"\n"
	if err := yaml.UnmarshalStrict([]byte(doc), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(p.Files) != 1 || p.Files[0].Path != ".config/x" {
		t.Errorf("got %+v", p.Files)
	}
}
