package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
)

func TestMergeOfNothingIsUnfiltered(t *testing.T) {
	if got := Merge(nil); got.Default != "" {
		t.Errorf("no policies produced a rule set: %+v", got)
	}
}

// One deny among a set of allows makes the whole run deny-by-default. The
// alternative is that adding a restrictive policy to a permissive one leaves
// the permissive one in charge, which is the wrong direction to be wrong in.
func TestMergeTakesTheStrictestDefault(t *testing.T) {
	permissive := Policy{Egress: Egress{Default: "allow"}}
	restrictive := Policy{Egress: Egress{Default: "deny"}}

	for _, tt := range []struct {
		name string
		in   []Policy
		want string
	}{
		{"allow alone", []Policy{permissive}, "allow"},
		{"deny alone", []Policy{restrictive}, "deny"},
		{"deny after allow", []Policy{permissive, restrictive}, "deny"},
		{"allow after deny", []Policy{restrictive, permissive}, "deny"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Merge(tt.in).Default; got != tt.want {
				t.Errorf("default = %q, want %q", got, tt.want)
			}
		})
	}
}

// Attaching is granting: a rule in either policy is a rule of the run's. Worth
// a test rather than a comment, because it is the half of the merge that makes
// a run reach something one of its policies alone would have denied.
func TestMergeUnionsTheRules(t *testing.T) {
	got := Merge([]Policy{
		{Egress: Egress{Default: "deny", Allow: []Rule{{Host: "a.example"}}}},
		{Egress: Egress{Default: "deny", Allow: []Rule{{Host: "b.example"}},
			Deny: []Rule{{CIDR: "10.0.0.0/8"}}}},
	})
	if len(got.Allow) != 2 {
		t.Errorf("allow rules were not unioned: %+v", got.Allow)
	}
	if len(got.Deny) != 1 || got.Deny[0].CIDR != "10.0.0.0/8" {
		t.Errorf("deny rules were not carried: %+v", got.Deny)
	}
}

// Two policies naming the same host are one rule, not two: a reader comparing
// what brig reports against what they wrote should not have to account for the
// repetition.
func TestMergeDropsARuleTwoPoliciesShare(t *testing.T) {
	shared := Rule{Host: "api.anthropic.com"}
	got := Merge([]Policy{
		{Egress: Egress{Default: "deny", Allow: []Rule{shared, {Host: "a.example"}}}},
		{Egress: Egress{Default: "deny", Allow: []Rule{shared}}},
	})
	if len(got.Allow) != 2 {
		t.Errorf("a rule two policies share was kept twice: %+v", got.Allow)
	}
}

// A profile with nothing bound resolves to nothing, which is every run before
// someone attaches a policy.
func TestResolveIsEmptyWithNothingBound(t *testing.T) {
	dir := t.TempDir()
	egress, names, err := Resolve(profile.Profile{Name: "p"}, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if egress.Default != "" || len(names) != 0 {
		t.Errorf("an unbound profile resolved to %+v / %v", egress, names)
	}
}

func TestResolveMergesWhatIsBound(t *testing.T) {
	dir := t.TempDir()
	writeEgressPolicy(t, dir, "base", "deny", "api.anthropic.com")
	writeEgressPolicy(t, dir, "extra", "deny", "registry.npmjs.org")

	p := profile.Profile{Name: "claude-code", Policy: []string{"base", "extra"}}
	egress, names, err := Resolve(p, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if egress.Default != "deny" {
		t.Errorf("default = %q, want deny", egress.Default)
	}
	if len(egress.Allow) != 2 {
		t.Errorf("both policies' rules should apply: %+v", egress.Allow)
	}
	if len(names) != 2 {
		t.Errorf("names = %v, want both", names)
	}
}

// All three bindings reach a boot, not only the inline one. A policy attached
// to a session is the narrowest of them and the easiest to drop on the way
// through, and dropping it means a run someone deliberately restricted comes
// up unrestricted.
func TestResolveReadsEveryBinding(t *testing.T) {
	dir := t.TempDir()
	writeEgressPolicy(t, dir, "inline", "allow", "a.example")
	writeEgressPolicy(t, dir, "on-profile", "allow", "b.example")
	writeEgressPolicy(t, dir, "on-session", "deny", "c.example")

	a, err := LoadAttachments(dir)
	if err != nil {
		t.Fatal(err)
	}
	a.AttachToProfile("on-profile", "claude-code")
	a.AttachToSession("on-session", "claude-code", "review")
	if err := a.Save(dir); err != nil {
		t.Fatal(err)
	}

	p := profile.Profile{Name: "claude-code", Policy: []string{"inline"}}
	egress, names, err := Resolve(p, "review", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 {
		t.Fatalf("names = %v, want all three bindings", names)
	}
	if len(egress.Allow) != 3 {
		t.Errorf("a binding's rules were dropped: %+v", egress.Allow)
	}
	// The session policy is the only deny, and one deny makes the run
	// deny-by-default.
	if egress.Default != "deny" {
		t.Errorf("default = %q, want deny: the session policy denies", egress.Default)
	}

	// And a run of another session gets only the two that are not that
	// session's, or an attachment would leak across sessions.
	_, other, err := Resolve(p, "elsewhere", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 2 {
		t.Errorf("session names = %v, want only the inline and profile bindings", other)
	}
}

// A bound name that does not load fails the resolution rather than being
// skipped. Carrying on would boot a sandbox reported as covered by a policy
// and enforcing less than was asked for, which is the failure this whole path
// exists to prevent.
func TestResolveRefusesAPolicyThatDoesNotLoad(t *testing.T) {
	dir := t.TempDir()
	p := profile.Profile{Name: "claude-code", Policy: []string{"gone"}}

	_, _, err := Resolve(p, "", dir)
	if err == nil {
		t.Fatal("a binding to a policy that does not exist was accepted")
	}
	if !strings.Contains(err.Error(), "gone") {
		t.Errorf("the error does not name the policy: %v", err)
	}
}

// One unparseable document in the policy directory must not refuse a boot whose
// own policy loaded. LoadAll reports the bad file and returns the rest; treating
// that report as fatal let an editor backup renamed to .yaml, or a half-written
// file, break every policy-bound run -- while `brig policy ls` and `brig policy
// check` kept working and gave no hint where it came from.
func TestResolveIgnoresAnUnrelatedBadDocument(t *testing.T) {
	dir := t.TempDir()
	writeEgressPolicy(t, dir, "mine", "deny", "api.anthropic.com")
	if err := os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte("{not: [yaml"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := profile.Profile{Name: "claude-code", Policy: []string{"mine"}}
	egress, names, err := Resolve(p, "", dir)
	if err != nil {
		t.Fatalf("a bad file nothing is bound to refused the run: %v", err)
	}
	if egress.Default != "deny" || len(names) != 1 {
		t.Errorf("the bound policy did not survive: %+v / %v", egress, names)
	}
}

// A bound name that is itself the unparseable file still fails the run, and the
// parse error travels with the refusal -- that document is the likeliest reason
// the name did not turn up, and dropping it leaves the user hunting.
func TestResolveExplainsWhenTheBoundPolicyIsTheBrokenOne(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mine.yaml"), []byte("{not: [yaml"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := profile.Profile{Name: "claude-code", Policy: []string{"mine"}}
	_, _, err := Resolve(p, "", dir)
	if err == nil {
		t.Fatal("a run bound to an unreadable policy was allowed")
	}
	if !strings.Contains(err.Error(), "mine") {
		t.Errorf("the error does not name the policy: %v", err)
	}
	if !strings.Contains(err.Error(), "mine.yaml") {
		t.Errorf("the error does not carry the reason the document did not load: %v", err)
	}
}

func writeEgressPolicy(t *testing.T, dir, name, def, allow string) {
	t.Helper()
	doc := "apiVersion: " + APIVersion + "\nname: " + name +
		"\negress:\n  default: " + def + "\n  allow:\n    - host: " + allow + "\n"
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}
