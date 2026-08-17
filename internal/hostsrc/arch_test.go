package hostsrc_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The central guarantee of the credential-import design is that a RUN reads no
// host source. It cannot be tested behaviourally: if internal/wrap does not
// reference this package, there is nothing to instrument. So assert the
// dependency instead.
//
// This fails the moment someone adds a well-meaning "auto-import when the
// secret is missing" convenience to the run path -- which is exactly the
// regression that would silently restore the behaviour the design removes, and
// would do it while every other test passed.
func TestTheRunPathCannotReachTheImporter(t *testing.T) {
	// cmd/brig is on this list because it is where the run verb is wired, and
	// so where a well-meaning "auto-import when the secret is missing" would
	// most naturally be added -- checking internal/wrap alone would let exactly
	// that through. cmd/brigd for the same reason: it calls BuildEnv per ensure
	// in a process that may have no GUI session at all.
	//
	// Note what this still does not prove. Until the next release removes
	// creds.ReadHost, a run DOES read another application's keychain when the
	// profile declares hostCredential:, so the guarantee is "no NEW host source
	// reaches the run path", not yet "a run reads no host source". Say so in
	// the PR body rather than letting the test's name overclaim.
	for _, pkg := range []string{
		"github.com/brig-sh/brig/internal/wrap",
		"github.com/brig-sh/brig/internal/creds",
		"github.com/brig-sh/brig/cmd/brig",
		"github.com/brig-sh/brig/cmd/brigd",
	} {
		out, err := exec.Command("go", "list", "-deps", pkg).Output()
		if err != nil {
			t.Fatalf("go list -deps %s: %v", pkg, err)
		}
		if strings.Contains(string(out), "brig/internal/hostsrc") {
			t.Errorf("%s depends on internal/hostsrc: a run can now reach a host source", pkg)
		}
	}
}
