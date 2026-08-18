package hostsrc_test

import (
	"os"
	"os/exec"
	"path/filepath"
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
	// cmd/brigd is on this list because it calls BuildEnv per ensure, in a
	// process that may have no GUI session at all.
	//
	// cmd/brig is NOT, and cannot be: `brig secret import` is wired there, so
	// the binary that carries the import verb necessarily links the package
	// that does the importing. Go's dependency granularity is the package, so
	// there is nothing left for `go list -deps` to prove about it -- which is
	// why the file-level check below takes over that half of the guarantee. It
	// is the weaker of the two and it is the strongest available.
	//
	// Note what this still does not prove. Until the next release removes
	// creds.ReadHost, a run DOES read another application's keychain when the
	// profile declares hostCredential:, so the guarantee is "no NEW host source
	// reaches the run path", not yet "a run reads no host source". Say so in
	// the PR body rather than letting the test's name overclaim.
	for _, pkg := range []string{
		"github.com/brig-sh/brig/internal/wrap",
		"github.com/brig-sh/brig/internal/creds",
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

// The other half of the guarantee, for the one package the dependency test
// cannot cover. cmd/brig links this package because `brig secret import` lives
// there; what must stay true is that the IMPORT VERB is the only thing in it
// that reaches a host source.
//
// So: exactly one file may name the package. A well-meaning "auto-import when
// the secret is missing" added to the run wiring -- main.go, or wherever the
// sandbox environment is built -- lands in some other file and fails here,
// which is the same regression the dependency test catches everywhere else.
func TestOnlyTheImportVerbReachesTheHostFromTheCLI(t *testing.T) {
	const allowed = "secretimport.go"
	entries, err := os.ReadDir(filepath.Join("..", "..", "cmd", "brig"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || name == allowed ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		blob, err := os.ReadFile(filepath.Join("..", "..", "cmd", "brig", name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(blob), "brig/internal/hostsrc") {
			t.Errorf("cmd/brig/%s reads a host source; only %s may", name, allowed)
		}
	}
}
