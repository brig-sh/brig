package wrap

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Nothing in this package reaches into the workspace with a plain os.* call on
// a path (see workspaceRoot). The workspace is the guest's to rewrite, and a
// plain call resolves the path from scratch, following any link the guest
// planted on the way to a host path the guest cannot reach itself. Every such
// call in the package's non-test sources has to be on osPathAllowlist, so a new
// one cannot land without someone reading why its path is not the guest's.
//
// The scan reads every file whatever its build tags, so a call in a file for
// the other platform is seen too.
func TestNothingReachesTheWorkspaceWithAPlainOSCall(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var calls []osPathCall
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, scanOSPathCalls(fset, name, f)...)
	}
	for _, e := range checkOSPathCalls(calls, osPathAllowlist) {
		t.Error(e)
	}
}

// The scanner has to see a new plain call, or
// TestNothingReachesTheWorkspaceWithAPlainOSCall passes on a package that
// broke the rule. A seeded os.WriteFile is the regression the rule exists to
// stop: a write that follows a link the guest planted.
func TestTheOSScanCatchesAPlainWriteFile(t *testing.T) {
	const src = `package wrap

import "os"

func (c *Config) writeMarker() error {
	return os.WriteFile(c.Workspace+"/.brig-marker", nil, 0o600)
}
`
	calls := scanSource(t, "marker.go", src)
	errs := checkOSPathCalls(calls, nil)
	if len(errs) != 1 {
		t.Fatalf("got %d findings, want 1: %q", len(errs), errs)
	}
	for _, want := range []string{"marker.go", "Config.writeMarker", "os.WriteFile"} {
		if !strings.Contains(errs[0], want) {
			t.Errorf("finding %q does not name %s", errs[0], want)
		}
	}
}

// An import alias, a function value and a call inside a closure are the same
// call by other spellings. Each of them has to be found. A file can import os
// twice under two names, and a call through either one counts.
func TestTheOSScanSeesThroughOtherSpellings(t *testing.T) {
	const src = `package wrap

import (
	"os"
	sys "os"
)

var remove = sys.RemoveAll

func seed(dir string) {
	go func() { _ = sys.Symlink("/etc", dir+"/link") }()
}

func peek(p string) ([]byte, error) { return os.ReadFile(p) }
`
	calls := scanSource(t, "spell.go", src)
	errs := checkOSPathCalls(calls, nil)
	if len(errs) != 3 {
		t.Fatalf("got %d findings, want 3: %q", len(errs), errs)
	}
	joined := strings.Join(errs, "\n")
	for _, want := range []string{"os.RemoveAll", "os.Symlink", "seed", "peek calls os.ReadFile(p)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("findings %q do not name %s", errs, want)
		}
	}
}

// A dot import hides every os call behind a bare name the scanner cannot tell
// from a local function, so the import itself is the finding.
func TestTheOSScanRefusesADotImport(t *testing.T) {
	const src = `package wrap

import . "os"

func gone(p string) error { return Remove(p) }
`
	calls := scanSource(t, "dot.go", src)
	if errs := checkOSPathCalls(calls, nil); len(errs) != 1 {
		t.Fatalf("got %d findings, want 1: %q", len(errs), errs)
	}
}

// An allowlisted call passes only at the site the entry names. The same call
// moved into another function, or handed another path in the same function, is
// a new call nobody reviewed, and an entry with no call left behind it is a
// stale claim about the package.
func TestTheOSAllowlistIsKeyedBySite(t *testing.T) {
	const src = `package wrap

import "os"

func readIndex(path, backup string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return os.ReadFile(backup)
	}
	return b, nil
}

func readOther(path string) ([]byte, error) { return os.ReadFile(path) }
`
	calls := scanSource(t, "index.go", src)
	allow := []osPathAllow{
		{File: "index.go", Func: "readIndex", Call: "os.ReadFile", Arg: "path", Why: "test"},
		{File: "index.go", Func: "gone", Call: "os.Remove", Arg: "path", Why: "test"},
	}
	errs := checkOSPathCalls(calls, allow)
	if len(errs) != 3 {
		t.Fatalf("got %d findings, want 3: %q", len(errs), errs)
	}
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, "readOther") {
		t.Errorf("the call in readOther passed on readIndex's entry: %q", errs)
	}
	if !strings.Contains(joined, "readIndex calls os.ReadFile(backup)") {
		t.Errorf("the call on backup passed on the entry for path: %q", errs)
	}
	if strings.Contains(joined, "readIndex calls os.ReadFile(path)") {
		t.Errorf("the allowlisted call on path was reported: %q", errs)
	}
	if !strings.Contains(joined, "gone") {
		t.Errorf("the stale entry for gone was not reported: %q", errs)
	}
}

func scanSource(t *testing.T, name, src string) []osPathCall {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	return scanOSPathCalls(fset, name, f)
}

// osPathFuncs are the os functions that take a path and resolve it themselves,
// following every link on the way. The value is how many leading arguments are
// paths, which is how many an allowlist entry spells out.
var osPathFuncs = map[string]int{
	"Chdir": 1, "Chmod": 1, "Chown": 1, "Chtimes": 1, "CopyFS": 1, "Create": 1,
	"CreateTemp": 1, "DirFS": 1, "Lchown": 1, "Link": 2, "Lstat": 1, "Mkdir": 1,
	"MkdirAll": 1, "MkdirTemp": 1, "Open": 1, "OpenFile": 1, "OpenInRoot": 2,
	"OpenRoot": 1, "ReadDir": 1, "ReadFile": 1, "Readlink": 1, "Remove": 1,
	"RemoveAll": 1, "Rename": 2, "Stat": 1, "Symlink": 2, "Truncate": 1,
	"WriteFile": 1,
}

const dotImportOS = `import . "os"`

// osPathCall is one use of an osPathFuncs function in a source file.
type osPathCall struct {
	File string
	// Func is the enclosing function, "Recv.name" for a method, or "var x"
	// for a package-level initializer.
	Func string
	// Call is spelled os.Name whatever the file imports os as.
	Call string
	// Arg is the path arguments as the source writes them, comma-separated.
	// Empty when the function is taken as a value and not called.
	Arg  string
	Line int
}

// osPathAllow lets one call site through. The key is the file, the enclosing
// function, the os function and the path argument, so the same call moved to
// another function, or handed another path, is a new call and fails.
type osPathAllow struct {
	File, Func, Call, Arg string
	// Why is the reason the path is not the guest's to redirect.
	Why string
}

func (a osPathAllow) key() string { return a.File + "\x00" + a.Func + "\x00" + a.Call + "\x00" + a.Arg }

// scanOSPathCalls lists every call to an osPathFuncs function in f, and every
// place one is taken as a value, since a value is a call made somewhere the
// scan does not look.
func scanOSPathCalls(fset *token.FileSet, file string, f *ast.File) []osPathCall {
	var out []osPathCall
	// A set, since a file can import os twice under two names, and keeping
	// only the last name let every call through the first one pass unseen.
	osNames := map[string]bool{}
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if path != "os" {
			continue
		}
		name := "os"
		if imp.Name != nil {
			name = imp.Name.Name
		}
		switch name {
		case ".":
			out = append(out, osPathCall{File: file, Call: dotImportOS,
				Line: fset.Position(imp.Pos()).Line})
		case "_":
		default:
			osNames[name] = true
		}
	}
	if len(osNames) == 0 {
		return out
	}
	match := func(e ast.Expr) (string, bool) {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return "", false
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || !osNames[id.Name] {
			return "", false
		}
		_, ok = osPathFuncs[sel.Sel.Name]
		return sel.Sel.Name, ok
	}
	for _, decl := range f.Decls {
		fn := declName(decl)
		called := map[ast.Expr]bool{}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				name, ok := match(n.Fun)
				if !ok {
					return true
				}
				called[n.Fun] = true
				var args []string
				for i := 0; i < osPathFuncs[name] && i < len(n.Args); i++ {
					args = append(args, types.ExprString(n.Args[i]))
				}
				out = append(out, osPathCall{File: file, Func: fn, Call: "os." + name,
					Arg: strings.Join(args, ", "), Line: fset.Position(n.Pos()).Line})
			case *ast.SelectorExpr:
				if name, ok := match(n); ok && !called[n] {
					out = append(out, osPathCall{File: file, Func: fn, Call: "os." + name,
						Line: fset.Position(n.Pos()).Line})
				}
			}
			return true
		})
	}
	return out
}

func declName(decl ast.Decl) string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv == nil || len(d.Recv.List) == 0 {
			return d.Name.Name
		}
		recv := d.Recv.List[0].Type
		if star, ok := recv.(*ast.StarExpr); ok {
			recv = star.X
		}
		switch r := recv.(type) {
		case *ast.IndexExpr:
			recv = r.X
		case *ast.IndexListExpr:
			recv = r.X
		}
		return types.ExprString(recv) + "." + d.Name.Name
	case *ast.GenDecl:
		var names []string
		for _, spec := range d.Specs {
			if vs, ok := spec.(*ast.ValueSpec); ok {
				for _, n := range vs.Names {
					names = append(names, n.Name)
				}
			}
		}
		return d.Tok.String() + " " + strings.Join(names, ", ")
	}
	return ""
}

// checkOSPathCalls returns one message per call no entry lets through, and one
// per entry that matches no call. A stale entry is a reviewed reason for a
// call that is gone, and it lets the next call written at that site through
// unread.
func checkOSPathCalls(calls []osPathCall, allow []osPathAllow) []string {
	used := map[string]bool{}
	known := map[string]bool{}
	for _, a := range allow {
		known[a.key()] = true
	}
	var errs []string
	for _, c := range calls {
		k := osPathAllow{File: c.File, Func: c.Func, Call: c.Call, Arg: c.Arg}.key()
		if known[k] {
			used[k] = true
			continue
		}
		if c.Call == dotImportOS {
			errs = append(errs, fmt.Sprintf("%s:%d: %s hides every os call from this scan. "+
				"Import os by name", c.File, c.Line, dotImportOS))
			continue
		}
		what := fmt.Sprintf("calls %s(%s)", c.Call, c.Arg)
		if c.Arg == "" {
			what = "takes " + c.Call + " as a value"
		}
		errs = append(errs, fmt.Sprintf("%s:%d: %s %s on a path outside any os.Root. "+
			"Reach the workspace through workspaceRoot, or add an osPathAllowlist entry "+
			"saying why the guest cannot redirect this path",
			c.File, c.Line, c.Func, what))
	}
	for _, a := range allow {
		if !used[a.key()] {
			errs = append(errs, fmt.Sprintf("osPathAllowlist entry %s %s %s(%s) matches no "+
				"call. Drop it", a.File, a.Func, a.Call, a.Arg))
		}
	}
	sort.Strings(errs)
	return errs
}

// osPathAllowlist is every plain os call on a path the package makes, with the
// reason the guest cannot turn it into a read or write of a host path.
var osPathAllowlist = []osPathAllow{
	// Opening the workspace and the project. These run before a root exists,
	// or resolve the path afresh only to compare it against the handle, where
	// a poisoned answer can only refuse.
	{"rootio.go", "Config.openWorkspace", "os.MkdirAll", "c.Workspace",
		"checkWorkspacePathBeforeCreate has refused every link on the way, and no root exists yet"},
	{"rootio.go", "Config.checkWorkspacePathBeforeCreate", "os.Lstat", "p",
		"looks at a component without following it, and a link there is refused"},
	{"rootio.go", "Config.checkWorkspacePathBeforeCreate", "os.Readlink", "p",
		"reads the target of a refused link only to name it in the refusal"},
	{"rootio.go", "openPathHandle", "os.OpenRoot", "prefix",
		"the trusted prefix ends above the first directory the guest can write"},
	{"rootio.go", "openPathHandle", "os.Stat", "abs",
		"compared against the descended handle, so a redirected path refuses"},
	{"rootio.go", "heldDir.verifyStillOurs", "os.Stat", "r.dir",
		"compared against the held handle, so a redirected path refuses"},
	{"rootio.go", "resolveTrusted", "os.Stat", "dir",
		"stops at the first directory the guest can write, before looking inside it"},
	{"rootio.go", "resolveTrusted", "os.Lstat", "p",
		"p sits in a directory resolveTrusted just found the guest cannot write"},
	{"rootio.go", "resolveTrusted", "os.Readlink", "p",
		"p sits in a directory resolveTrusted just found the guest cannot write"},
	{"fdroot.go", "rootFromFile", "os.OpenRoot", "fmt.Sprintf(\"%s/%d\", dir, f.Fd())",
		"names a descriptor brig already holds, so no name is resolved again"},

	// brig's own state under ~/.brig. The guest home is a child of homes/ and
	// the guest sees nothing above it.
	{"index.go", "readIndex", "os.ReadFile", "path", "an index file in brig's state directory"},
	{"index.go", "writeIndex", "os.MkdirAll", "filepath.Dir(path)", "brig's state directory"},
	{"index.go", "writeIndex", "os.CreateTemp", "filepath.Dir(path)", "brig's state directory"},
	{"index.go", "writeIndex", "os.Remove", "tmp.Name()", "the temporary file writeIndex just made"},
	{"index.go", "writeIndex", "os.Rename", "tmp.Name(), path", "an index file in brig's state directory"},
	{"index.go", "dropLegacyWorkspaceIndex", "os.Remove", "path", "an index file in brig's state directory"},
	{"slugclaim.go", "migrateSlugClaims", "os.Stat", "path", "an index file in brig's state directory"},
	{"slugclaim.go", "migrateSlugClaims", "os.Remove", "old", "an index file in brig's state directory"},
	{"home.go", "removeHome", "os.OpenRoot", "dir",
		"the homes directory, the parent of every guest home and out of every guest's reach"},

	// The host's own files, which live outside the workspace by design. Every
	// write they feed goes through the workspace root.
	{"git.go", "openHostFile", "os.OpenFile", "path",
		"the host's gh hosts.yml, refused unless the open descriptor is a regular file"},
	{"config.go", "hostProjections", "os.Stat", "hostPath", "a directory under the host's own agent config"},
	{"workspace.go", "Config.seedHostConfig", "os.ReadDir", "seed.Host", "the host's own agent config"},
	{"workspace.go", "copyTree", "os.Lstat", "src", "a host source, and a link there is copied as a link"},
	{"workspace.go", "copyTree", "os.Readlink", "src", "a host source, copied as a link and never followed"},
	{"workspace.go", "copyTree", "os.ReadDir", "src", "a host source directory"},
	{"workspace.go", "copyTree", "os.Open", "src", "a host source file that Lstat found regular"},

	// Is a directory there, and which one. Nothing is read or written through
	// the answer, and a wrong one costs a notice, a restart or a trust key.
	{"home.go", "EphemeralHomeOf", "os.Lstat", "entry.Home",
		"existence only, and the delete that follows goes through removeHome's root"},
	{"home.go", "Config.reapOrphanHome", "os.Lstat", "c.Workspace",
		"existence only, and the delete that follows goes through removeHome's root"},
	{"home.go", "Config.createsEphemeralHome", "os.Lstat", "c.Workspace", "existence only"},
	{"home.go", "Config.ephemeralNotice", "os.Lstat", "c.Workspace", "existence only, to decide on a notice"},
	{"config.go", "Load", "os.Stat", "dir",
		"whether an older release's home is there, and the run still opens it through the descent"},
	{"config.go", "Config.slugMigrationNotice", "os.Stat", "oldWorkspace", "existence only, to decide on a notice"},
	{"config.go", "Config.mountProject", "os.Stat", "abs",
		"a friendly error for a missing project, which openHeldDir then descends into"},
	{"workspace.go", "Marker", "os.Stat", "real",
		"the inode for the stale-share marker, and a wrong one costs a restart"},
	{"workspace.go", "TrustKey", "os.Lstat", "filepath.Join(dir, \".git\")",
		"picks the trust key, which the guest can already choose by making a .git"},
	{"workspace.go", "isUserHome", "os.Stat", "home",
		"compared with the workspace by SameFile, and a failed or redirected stat answers true, which leaves the files alone"},
	{"workspace.go", "isUserHome", "os.Stat", "dir",
		"compared with the home directory by SameFile, and a failed or redirected stat answers true, which leaves the files alone"},
}
