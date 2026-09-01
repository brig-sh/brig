package wrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The transcript in issue #26: two different session names whose slugs shorten
// to the same sandbox. The first claims it; the second must be refused rather
// than dropped into the same guest home with different credentials.
func TestACollidingSessionNameIsRefused(t *testing.T) {
	isolateState(t)

	prod := mustLoad(t, Options{Name: "acme-corp-prod"})
	if err := prod.claimSlug(); err != nil {
		t.Fatalf("the first name could not claim its own sandbox: %v", err)
	}

	staging := mustLoad(t, Options{Name: "acme-corp-staging"})
	if staging.VMName != prod.VMName {
		t.Fatalf("the two names did not collide: %q and %q -- pick names that do",
			prod.VMName, staging.VMName)
	}

	err := staging.claimSlug()
	if err == nil {
		t.Fatal("a name colliding with another was allowed to share the sandbox")
	}
	// The refusal has to name both sessions, so the reader knows what they are
	// up against and which other name to look for.
	if !strings.Contains(err.Error(), "acme-corp-prod") ||
		!strings.Contains(err.Error(), "acme-corp-staging") {
		t.Errorf("the refusal does not name both sessions: %v", err)
	}
}

// The ordinary repeat run: the same name returning to its own sandbox is not a
// collision and must be let through.
func TestTheSameNameKeepsItsOwnSandbox(t *testing.T) {
	isolateState(t)

	first := mustLoad(t, Options{Name: "acme-corp-prod"})
	if err := first.claimSlug(); err != nil {
		t.Fatalf("the first run could not claim: %v", err)
	}
	again := mustLoad(t, Options{Name: "acme-corp-prod"})
	if err := again.claimSlug(); err != nil {
		t.Errorf("a repeat run of the same name was refused its own sandbox: %v", err)
	}
}

// `brig rm` releases the claim, so the sandbox a different name was refused is
// free for it once the first is gone.
func TestRemoveReleasesTheClaim(t *testing.T) {
	isolateState(t)

	prod := mustLoad(t, Options{Name: "acme-corp-prod"})
	if err := prod.claimSlug(); err != nil {
		t.Fatalf("the first name could not claim: %v", err)
	}

	rt := &removingRuntime{}
	prod.Runtime = rt
	if err := prod.Remove(); err != nil {
		t.Fatalf("remove failed: %v", err)
	}

	staging := mustLoad(t, Options{Name: "acme-corp-staging"})
	if err := staging.claimSlug(); err != nil {
		t.Errorf("the sandbox was not released by rm: %v", err)
	}
}

// `brig reset` works from the instance list, so it releases by name without a
// Config -- the claim has to go on that path too, or a reset leaves every
// sandbox it removed still claimed.
func TestForgetSlugClaimPrunesByName(t *testing.T) {
	isolateState(t)

	prod := mustLoad(t, Options{Name: "acme-corp-prod"})
	if err := prod.claimSlug(); err != nil {
		t.Fatalf("the first name could not claim: %v", err)
	}

	// A name never claimed is not an error: reset walks every brig sandbox.
	ForgetSlugClaim("brig-claude-code-neverseen")
	ForgetSlugClaim(prod.VMName)

	staging := mustLoad(t, Options{Name: "acme-corp-staging"})
	if err := staging.claimSlug(); err != nil {
		t.Errorf("reset did not release the claim: %v", err)
	}
}

// The claims an older release wrote are carried across the rename rather than
// dropped: the old file's shape is this file's shape, so there is nothing to
// guess, and dropping them would turn the refusal off until every session had
// run again -- which is the window in which two agents share one home.
func TestTheOldClaimsAreCarriedAcrossTheRename(t *testing.T) {
	dir := isolateState(t)

	// sessions.json as the claim index had it: keyed by the sandbox name, with
	// the owning session name as the value.
	body := `{"brig-claude-code-acme-corp": "acme-corp-prod"}`
	if err := os.WriteFile(filepath.Join(dir, sessionIndexName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// The claim is still in force, which is the whole point of carrying it:
	// this name shortens to the sandbox acme-corp-prod owns.
	staging := mustLoad(t, Options{Name: "acme-corp-staging"})
	if staging.VMName != "brig-claude-code-acme-corp" {
		t.Fatalf("this case is written around brig-claude-code-acme-corp, and the "+
			"sandbox is %q", staging.VMName)
	}
	err := staging.claimSlug()
	if err == nil {
		t.Fatal("the carried claim did not refuse a colliding name")
	}
	if !strings.Contains(err.Error(), "acme-corp-prod") {
		t.Errorf("the refusal does not name the session that owns the sandbox: %v", err)
	}

	// Moved rather than copied: what is left under the old name is a file the
	// session index is about to write for itself.
	if got := readIndex[string](slugClaimIndexName)["brig-claude-code-acme-corp"]; got != "acme-corp-prod" {
		t.Errorf("the claim was carried over as %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, sessionIndexName)); !os.IsNotExist(err) {
		t.Errorf("the claims were left behind under the old name (%v)", err)
	}
}

// The guard on that migration: a current sessions.json is a map of session
// entries, whose values are objects rather than strings, so it cannot be read
// as claims. Nothing is carried over and nothing is deleted -- a session index
// mistaken for claims would cost every session in it its home.
func TestASessionIndexIsNotMistakenForClaims(t *testing.T) {
	dir := isolateState(t)

	path := filepath.Join(dir, sessionIndexName)
	body := `{"claude-code@rc23test":` +
		`{"home": "/private/tmp/ws-rc23", "sandbox": "brig-claude-code-rc23test"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	c := mustLoad(t, Options{Name: "acme-corp-prod"})
	if err := c.claimSlug(); err != nil {
		t.Fatalf("a run with a session index and no claims was refused: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the session index was deleted as though it held claims: %v", err)
	}
	if got := WorkspaceOfSandbox("brig-claude-code-rc23test"); got != "/private/tmp/ws-rc23" {
		t.Errorf("the session entry reads back as %q, want its home", got)
	}
	// The only claim is this run's own: nothing was carried over.
	claims := readIndex[string](slugClaimIndexName)
	if len(claims) != 1 || claims[c.VMName] != "acme-corp-prod" {
		t.Errorf("the claim index holds %v, want this run's claim alone", claims)
	}
}

// The claim index is bookkeeping, so an unusable one costs nothing: a corrupt
// file is treated as empty and the run proceeds, exactly as it did before the
// file existed.
func TestACorruptClaimIndexDoesNotBreakARun(t *testing.T) {
	dir := isolateState(t)
	path := filepath.Join(dir, slugClaimIndexName)
	if err := os.WriteFile(path, []byte("{not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := mustLoad(t, Options{Name: "acme-corp-prod"})
	if err := c.claimSlug(); err != nil {
		t.Errorf("a corrupt claim index failed the run: %v", err)
	}
}
