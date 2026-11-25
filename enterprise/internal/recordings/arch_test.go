// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package recordings

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	loginPath     = "github.com/brig-sh/brig/enterprise/internal/login"
	secretPrefix  = "github.com/brig-sh/brig/internal/secret"
	enterpriseMod = "github.com/brig-sh/brig/enterprise"
)

// The recorder needs the person's login token, and the store that holds it
// was built so that no code that starts a run could read it. That changed on
// purpose, for one caller. These tests keep the change as narrow as it was
// meant to be: one accessor, one package that calls it, and a recorder that
// can only be handed the token as a value.

// Only the non-test files of this package call login.AccessToken. A second
// caller, in any other verb or in the recorder, would be a second place a
// token could be read and sent from.
func TestOnlyTheRecordingsGlueCallsAccessToken(t *testing.T) {
	root := filepath.Join("..", "..") // enterprise/
	calls := 0
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
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		dir := filepath.Dir(path)
		// The accessor is defined in the login package; its own code is the
		// verbs' and not a caller of the exported function.
		if dir == filepath.Join(root, "internal", "login") {
			return nil
		}
		n, err := accessTokenReferences(path)
		if err != nil {
			return err
		}
		if dir == filepath.Join(root, "internal", "recordings") {
			calls += n
			return nil
		}
		if n > 0 {
			t.Errorf("%s calls login.AccessToken; only the non-test files of enterprise/internal/recordings may", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("enterprise/internal/recordings refers to login.AccessToken %d times, want exactly once (the accessToken seam)", calls)
	}
}

// accessTokenReferences counts the references in file to AccessToken of the
// login package, under whatever name the file imports it, and reports a dot
// import of it as a reference, since that hides the name.
func accessTokenReferences(file string) (int, error) {
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
	if err != nil {
		return 0, err
	}
	local := ""
	n := 0
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if p != loginPath {
			continue
		}
		switch {
		case imp.Name == nil:
			local = "login"
		case imp.Name.Name == ".":
			n++
		case imp.Name.Name != "_":
			local = imp.Name.Name
		}
	}
	if local == "" {
		return n, nil
	}
	ast.Inspect(f, func(node ast.Node) bool {
		if sel, ok := node.(*ast.SelectorExpr); ok && sel.Sel.Name == "AccessToken" {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == local {
				n++
			}
		}
		return true
	})
	return n, nil
}

// The recorder and the terminal relay cannot open the store, directly or
// through anything they import, in their code or in their tests: the token
// reaches the recorder as a value in a client, and it has no other way to it.
func TestTheRecorderCannotOpenTheLoginStore(t *testing.T) {
	for _, pkg := range []string{
		enterpriseMod + "/internal/recording/...",
		enterpriseMod + "/internal/ptyrelay/...",
	} {
		for _, goos := range []string{"darwin", "linux"} {
			cmd := exec.Command("go", "list", "-deps", "-test", pkg)
			cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=arm64")
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("go list -deps -test %s (GOOS=%s): %v", pkg, goos, err)
			}
			pkgs := strings.Fields(string(out))
			if len(pkgs) < 20 {
				t.Fatalf("go list -deps -test %s named only %d packages", pkg, len(pkgs))
			}
			for _, p := range pkgs {
				// The test binary of a package lists as "path [path.test]".
				if p == loginPath || strings.HasPrefix(p, secretPrefix) {
					t.Errorf("GOOS=%s: %s depends on %s, so it could open the login store", goos, pkg, p)
				}
			}
		}
	}
}
