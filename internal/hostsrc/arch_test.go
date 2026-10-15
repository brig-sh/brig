package hostsrc_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	// This proves only that the run path cannot reach the importer. The claim
	// itself, that a run reads no host source, is TestTheRunPathReadsNoKeychain
	// below: the keychain was read through a second door, creds.ReadHost behind
	// the profile's hostCredential: field, which never touched this package.
	for _, pkg := range runPathPackages {
		out, err := goList("-deps", pkg)
		if err != nil {
			t.Fatalf("go list -deps %s: %v", pkg, err)
		}
		if strings.Contains(string(out), "brig/internal/hostsrc") {
			t.Errorf("%s depends on internal/hostsrc: a run can now reach a host source", pkg)
		}
	}
}

// The run path delivers what the store holds and never calls a provider.
// internal/renew sends refresh tokens to a token endpoint, and a refresh
// rotates a pair the host's own agent may share. So no run-path package
// links it, and neither does cmd/brig: the CLI has no verb that refreshes.
func TestTheRunPathCannotReachTheRefresher(t *testing.T) {
	const refresher = "github.com/brig-sh/brig/internal/renew"
	for _, pkg := range append(slices.Clone(runPathPackages), "github.com/brig-sh/brig/cmd/brig") {
		out, err := goList("-deps", pkg)
		if err != nil {
			t.Fatalf("go list -deps %s: %v", pkg, err)
		}
		if slices.Contains(strings.Split(string(out), "\n"), refresher) {
			t.Errorf("%s depends on %s: a run can now refresh a credential", pkg, refresher)
		}
	}
}

// goList runs go list with args for each GOOS brig ships for, and returns the
// outputs joined. CI runs on Linux alone, and a file built only for macOS, or
// only for Linux, can add an import the other build does not have.
func goList(args ...string) ([]byte, error) {
	var all []byte
	for _, goos := range []string{"darwin", "linux"} {
		cmd := exec.Command("go", append([]string{"list"}, args...)...)
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=arm64")
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("GOOS=%s: %w", goos, err)
		}
		all = append(all, out...)
	}
	return all, nil
}

// runPathPackages are the packages a run is built from. cmd/brig is not among
// them: `brig secret import` is wired there, so it necessarily links the
// importer, and the file-level tests below take over its half of the guarantee.
// internal/injector is brig's side of the credential broker: it starts
// brig-broker and asks it for tokens, on every run with a route.
var runPathPackages = []string{
	"github.com/brig-sh/brig/internal/wrap",
	"github.com/brig-sh/brig/internal/creds",
	"github.com/brig-sh/brig/internal/injector",
	"github.com/brig-sh/brig/cmd/brigd",
}

// brokerBinary is brig-broker, the one process that refreshes a credential
// and signs session tokens. It is not on the run path, because it links the
// refresher. It reads brig's own store and no host source.
const brokerBinary = "github.com/brig-sh/brig/cmd/brig-broker"

// The host key that signs session tokens lives in brig-broker alone, and
// internal/broker is the package that signs with it. A run asks brig-broker
// for a token over its socket, so no run-path package, and not the CLI,
// links the signer: a run that could mint its own tokens would not need the
// broker to agree.
func TestTheRunPathCannotMintTokens(t *testing.T) {
	const signer = "github.com/brig-sh/brig/internal/broker"
	for _, pkg := range append(slices.Clone(runPathPackages), "github.com/brig-sh/brig/cmd/brig") {
		out, err := goList("-deps", pkg)
		if err != nil {
			t.Fatalf("go list -deps %s: %v", pkg, err)
		}
		if slices.Contains(strings.Split(string(out), "\n"), signer) {
			t.Errorf("%s depends on %s: a run can now mint a session token", pkg, signer)
		}
	}
}

// brig-broker reads the credential a route injects from brig's own store,
// on every request. It must not reach a host source: a route names a secret
// in the store, never the agent's own keychain item.
func TestTheBrokerCannotReachTheImporter(t *testing.T) {
	out, err := goList("-deps", brokerBinary)
	if err != nil {
		t.Fatalf("go list -deps %s: %v", brokerBinary, err)
	}
	deps := strings.Split(string(out), "\n")
	if slices.Contains(deps, "github.com/brig-sh/brig/internal/hostsrc") {
		t.Errorf("%s depends on internal/hostsrc: the broker can now read a host source", brokerBinary)
	}
	// The proof the other tests lean on: brig-broker does link the
	// refresher, so a check that found nothing there would prove nothing.
	if !slices.Contains(deps, "github.com/brig-sh/brig/internal/renew") {
		t.Errorf("%s does not link internal/renew; this test checks the wrong package", brokerBinary)
	}
}

// A run reads no host source. The dependency test above cannot say so on its
// own: hostCredential: used to read the macOS keychain through a `security`
// call inside internal/creds, a package the run path is allowed to link, so a
// second door stood open with the first one shut. The field is removed, and
// this pins the removal: no non-test source in any package of this module
// that a run-path package links names the keychain tool. Every linked package,
// not just the three named above, or a new internal package that shells out
// and is imported by wrap would pass both tests. The importer is where that
// call belongs, and internal/secret -- brig's own store -- is the one keychain
// a run may open.
func TestTheRunPathReadsNoKeychain(t *testing.T) {
	const module = "github.com/brig-sh/brig/"
	const allowed = module + "internal/secret"
	seen := map[string]bool{}
	for _, pkg := range runPathPackages {
		out, err := goList("-deps", "-f", "{{.ImportPath}} {{.Dir}}", pkg)
		if err != nil {
			t.Fatalf("go list -deps %s: %v", pkg, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			path, dir, ok := strings.Cut(line, " ")
			if !ok || !strings.HasPrefix(path, module) || path == allowed || seen[path] {
				continue
			}
			seen[path] = true
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
					continue
				}
				blob, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				for _, marker := range []string{"find-generic-password", "/usr/bin/security"} {
					if strings.Contains(string(blob), marker) {
						t.Errorf("%s/%s names %q: a run reads a host keychain", path, name, marker)
					}
				}
			}
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
