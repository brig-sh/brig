package wrap

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
)

// policyBoundToSession binds the no-net policy to one session, the way
// `brig policy attach no-net <agent> -n <session>` records it. The document
// comes from writeTestPolicy; only the attachment record is written here,
// which is the shape loadWithPolicy cannot set up because it binds inline.
func policyBoundToSession(t *testing.T, agent, session string) {
	t.Helper()
	dir := t.TempDir()
	writeTestPolicy(t, dir, "no-net")
	rec := "sessions:\n  " + agent + ":\n    " + session + ":\n      - no-net\n"
	if err := os.WriteFile(filepath.Join(dir, "attachments.yaml"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIG_POLICY_DIR", dir)
}

// `attach -n` refuses a name it would have to rewrite, so a session row can
// only be filed under `foo`; --name sanitises instead, so all three of these
// start the session `foo`. Each must therefore reach `foo`'s policy, and its
// egress: one that resolved without reaching the rules would be the same
// failure a layer down.
func TestSessionPolicyFollowsTheSlug(t *testing.T) {
	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("no claude-code profile")
	}
	for _, name := range []string{"foo", "Foo", "FOO"} {
		policyBoundToSession(t, p.Name, "foo")
		c, err := Load(p, Options{Name: name}, nil)
		if err != nil {
			t.Errorf("--name %q was refused: %v", name, err)
			continue
		}
		// The premise: all three name one session. If this ever stops holding,
		// the rest of the test is asserting nothing.
		if c.Slug != "foo" {
			t.Fatalf("--name %q starts session %q, want foo", name, c.Slug)
		}
		if !slices.Contains(c.Policies, "no-net") {
			t.Errorf("--name %q carries %v, want the no-net attached to session foo",
				name, c.Policies)
		}
		if c.Egress.Default != "deny" {
			t.Errorf("--name %q carries egress default %q, want deny", name, c.Egress.Default)
		}
	}
}

// The other direction, so the lookup cannot be one that matches too much: a
// session with nothing attached carries nothing.
func TestUnboundSessionCarriesNoPolicy(t *testing.T) {
	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("no claude-code profile")
	}
	policyBoundToSession(t, p.Name, "foo")
	for _, name := range []string{"bar", "Bar"} {
		c, err := Load(p, Options{Name: name}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Policies) != 0 {
			t.Errorf("session %q carries %v, want nothing: only foo is bound", name, c.Policies)
		}
		if c.Egress.Default != "" {
			t.Errorf("session %q carries egress default %q, want none", name, c.Egress.Default)
		}
	}
}

// The ordinary case, which is every run before anyone attaches anything: no
// session, and nothing bound.
func TestARunWithNoSessionIsUnaffected(t *testing.T) {
	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("no claude-code profile")
	}
	policyBoundToSession(t, p.Name, "foo")
	c, err := Load(p, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Slug != "" {
		t.Fatalf("a run with no --name got session %q", c.Slug)
	}
	if len(c.Policies) != 0 {
		t.Errorf("a run with no session carries %v, want nothing", c.Policies)
	}
}
