package profile

import (
	"strings"
	"testing"
)

// bindingBase is a profile that parses, so each case adds only the lines it is
// actually about. Shared with validate_test.go.
const bindingBase = "name: x\nimage: i\nguestHome: /home/x\nbinary: x\nmem: 1\ncpus: 1\n"

// The grammar is a namespace and a name, so that "get a variable into the
// guest" has exactly one spelling with a pluggable source -- which is what
// lets forward: be deprecated rather than kept alongside it forever.
func TestParseRefAcceptsBothNamespaces(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Ref
	}{
		{"secrets.gh_token", Ref{Namespace: NamespaceSecrets, Name: "gh_token"}},
		{"env.GH_TOKEN", Ref{Namespace: NamespaceEnv, Name: "GH_TOKEN"}},
	} {
		got, err := ParseRef(tc.in)
		if err != nil {
			t.Fatalf("ParseRef(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ParseRef(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
		if got.String() != tc.in {
			t.Errorf("String() = %q, want %q", got.String(), tc.in)
		}
	}
}

// An unknown namespace names the valid ones: the point of a grammar is that a
// typo tells you the alphabet.
func TestParseRefRejectsUnknownNamespace(t *testing.T) {
	_, err := ParseRef("vault.token")
	if err == nil {
		t.Fatal("an unknown namespace was accepted")
	}
	for _, want := range []string{"vault", "secrets", "env"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// A bare word is the mistake someone makes coming from forward:, so it earns a
// better error than "unknown namespace".
func TestParseRefRejectsRefWithNoNamespace(t *testing.T) {
	for _, in := range []string{"gh_token", "", "secrets.", ".gh_token"} {
		if _, err := ParseRef(in); err == nil {
			t.Errorf("ParseRef(%q) was accepted", in)
		}
	}
}

// A secret name holds no dot (see secret.ValidName), so the split is
// unambiguous -- but an env name is the caller's, and one with a dot in it must
// not silently lose its tail.
func TestParseRefSplitsOnTheFirstDotOnly(t *testing.T) {
	got, err := ParseRef("env.SOME.VAR")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "SOME.VAR" {
		t.Errorf("Name = %q, want %q", got.Name, "SOME.VAR")
	}
}

// Resolved is how every consumer asks "where does this value come from", so it
// has to distinguish a literal from a ref without making the caller re-parse.
func TestResolvedDistinguishesLiteralsFromRefs(t *testing.T) {
	if _, ok, err := (EnvBinding{Name: "V", Value: "x"}).Resolved(); ok || err != nil {
		t.Errorf("a literal reported ok=%v err=%v, want false/nil", ok, err)
	}
	r, ok, err := (EnvBinding{Name: "V", Ref: "secrets.a"}).Resolved()
	if err != nil || !ok || r.Namespace != NamespaceSecrets || r.Name != "a" {
		t.Errorf("Resolved() = %+v, %v, %v", r, ok, err)
	}
	if _, ok, err := (EnvBinding{Name: "V", Ref: "nope"}).Resolved(); ok || err == nil {
		t.Errorf("a malformed ref reported ok=%v err=%v", ok, err)
	}
}

// The new keys parse. Validation of what they may say is B1's; this only
// establishes that the struct decodes.
func TestSecretsAndEnvDecode(t *testing.T) {
	p, err := Parse([]byte(bindingBase + `secrets:
  - gh_token
env:
  - name: GH_TOKEN
    ref: secrets.gh_token
  - name: MODE
    value: fast
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Secrets) != 1 || p.Secrets[0].Name != "gh_token" {
		t.Fatalf("Secrets = %v", p.Secrets)
	}
	if len(p.Env) != 2 || p.Env[0].Ref != "secrets.gh_token" || p.Env[1].Value != "fast" {
		t.Fatalf("Env = %+v", p.Env)
	}
}

// files: arrives in part 2. Until it does, the strict decoder rejecting it is
// the honest failure -- a files: binding that parsed and delivered nothing
// would be a secret the profile believes is in the guest and is not.
func TestFilesIsNotYetAKey(t *testing.T) {
	if _, err := Parse([]byte(bindingBase + "files:\n  - name: k\n    ref: secrets.k\n")); err == nil {
		t.Error("files: parsed, but nothing delivers it yet")
	}
}

// clone is what keeps two profiles from sharing a backing array, and a new
// slice field that misses it is a bug that only shows up under mutation.
func TestCloneCopiesTheNewSlices(t *testing.T) {
	p := Profile{Secrets: []SecretDecl{{Name: "a"}}, Env: []EnvBinding{{Name: "V", Ref: "env.V"}}}
	c := p.clone()
	c.Secrets[0].Name = "b"
	c.Env[0].Name = "W"
	if p.Secrets[0].Name != "a" || p.Env[0].Name != "V" {
		t.Errorf("clone shares its backing arrays: %+v", p)
	}
}
