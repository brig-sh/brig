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
func TestForgetSessionPrunesByName(t *testing.T) {
	isolateState(t)

	prod := mustLoad(t, Options{Name: "acme-corp-prod"})
	if err := prod.claimSlug(); err != nil {
		t.Fatalf("the first name could not claim: %v", err)
	}

	// A name never claimed is not an error: reset walks every brig sandbox.
	ForgetSession("brig-claude-code-neverseen")
	ForgetSession(prod.VMName)

	staging := mustLoad(t, Options{Name: "acme-corp-staging"})
	if err := staging.claimSlug(); err != nil {
		t.Errorf("reset did not release the claim: %v", err)
	}
}

// The claim index is bookkeeping, so an unusable one costs nothing: a corrupt
// file is treated as empty and the run proceeds, exactly as it did before the
// file existed.
func TestACorruptClaimIndexDoesNotBreakARun(t *testing.T) {
	dir := isolateState(t)
	path := filepath.Join(dir, sessionIndexName)
	if err := os.WriteFile(path, []byte("{not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := mustLoad(t, Options{Name: "acme-corp-prod"})
	if err := c.claimSlug(); err != nil {
		t.Errorf("a corrupt claim index failed the run: %v", err)
	}
}
