package login_test

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/secret/login"
)

// The login token is a credential for brig's own control plane, not one a
// sandbox is given. If a run-path package could open its keychain service, a
// change there could hand the token to a guest. Go's internal rule already
// keeps run-path code off the keyring package; this asserts the one remaining
// door, the login package itself, stays shut to them as well.
//
// cmd/brig is Brig Community, which has no brig login, so it has no reason
// to link the store either. Brig Enterprise links it from enterprise/.
func TestRunPathCannotOpenTheLoginStore(t *testing.T) {
	const login = "github.com/brig-sh/brig/internal/secret/login"
	for _, pkg := range []string{
		"github.com/brig-sh/brig/internal/wrap",
		"github.com/brig-sh/brig/internal/creds",
		"github.com/brig-sh/brig/internal/injector",
		"github.com/brig-sh/brig/cmd/brigd",
		"github.com/brig-sh/brig/cmd/brig-broker",
		"github.com/brig-sh/brig/cmd/brig",
	} {
		out, err := goList("-deps", pkg)
		if err != nil {
			t.Fatalf("go list -deps %s: %v", pkg, err)
		}
		if slices.Contains(strings.Split(string(out), "\n"), login) {
			t.Errorf("%s depends on %s: a run can now reach the login token", pkg, login)
		}
	}
}

// The dependency test above sees packages, and internal/secret is on the run
// path: it may link this package's keyring, but it must not name this
// package's service. A keyring.Open with the service spelt out would reach
// the token without importing this package, so no code outside it may hold
// the name.
func TestOnlyTheLoginStoreNamesItsService(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	here, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			if n := e.Name(); path != root && (strings.HasPrefix(n, ".") || n == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if filepath.Dir(abs) == here {
			return nil
		}
		blob, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(blob), login.Service) {
			t.Errorf("%s names %s; only internal/secret/login may", path, login.Service)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Brig Enterprise must link this package for brig login, and the dependency
// test above sees only the core module. The file is the unit instead: in the
// whole tree, only the login verb's file may import the store, so no other
// verb, and none of the code that starts a run, can read the token.
func TestOnlyTheLoginVerbReachesTheLoginStore(t *testing.T) {
	allowed := filepath.Join("enterprise", "internal", "login", "login.go")
	root := filepath.Join("..", "..", "..")
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			if n := e.Name(); path != root && (strings.HasPrefix(n, ".") || n == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || rel == allowed ||
			filepath.Dir(rel) == filepath.Join("internal", "secret", "login") {
			return nil
		}
		blob, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(blob), `"github.com/brig-sh/brig/internal/secret/login"`) {
			t.Errorf("%s reaches the login store; only %s may", rel, allowed)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// goList runs go list with args for each GOOS brig ships for, and returns the
// outputs joined: a file built for one OS can add an import the other lacks.
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
