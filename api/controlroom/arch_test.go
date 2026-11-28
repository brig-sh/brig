package controlroom

import (
	"os/exec"
	"strings"
	"testing"
)

const thisPackage = "github.com/brig-sh/brig/api/controlroom"

// refusedImports returns what this package may not link. direct are its own
// imports, and deps are every package it links. It talks HTTP and reads
// files, but it starts no process and links nothing outside the standard
// library, so the warden links it without linking brig or its dependencies.
func refusedImports(direct, deps []string) []string {
	var refused []string
	for _, imp := range direct {
		if imp == "os/exec" {
			refused = append(refused, "imports "+imp)
		}
	}
	for _, dep := range deps {
		if dep == thisPackage {
			continue
		}
		// A standard library path has no dot in its first element.
		if first, _, _ := strings.Cut(dep, "/"); strings.Contains(first, ".") {
			refused = append(refused, "links "+dep)
		}
	}
	return refused
}

func goList(t *testing.T, args ...string) []string {
	t.Helper()
	out, err := exec.Command("go", append([]string{"list"}, args...)...).Output()
	if err != nil {
		t.Fatalf("go list %v: %v", args, err)
	}
	return strings.Fields(string(out))
}

// The warden links this package without linking brig.
func TestLinksNothingRefused(t *testing.T) {
	direct := goList(t, "-f", `{{join .Imports " "}}`, thisPackage)
	deps := goList(t, "-deps", "-f", "{{.ImportPath}}", thisPackage)
	if len(direct) == 0 || len(deps) <= len(direct) {
		t.Fatalf("go list read nothing useful: %d imports, %d deps", len(direct), len(deps))
	}
	for _, r := range refusedImports(direct, deps) {
		t.Errorf("%s %s", thisPackage, r)
	}
}

// The check above passes on an empty answer as well as on a clean one, so
// each rule is shown to refuse a planted import.
func TestRefusedImportsCatchesAPlantedImport(t *testing.T) {
	for _, c := range []struct {
		direct, deps []string
	}{
		{[]string{"os/exec"}, nil},
		{nil, []string{"github.com/brig-sh/brig/internal/secret"}},
		{nil, []string{"github.com/brig-sh/brig/api/policy/v1alpha1"}},
		{nil, []string{"golang.org/x/sys/unix"}},
	} {
		if got := refusedImports(c.direct, c.deps); len(got) == 0 {
			t.Errorf("imports %v and deps %v were not refused", c.direct, c.deps)
		}
	}
	if got := refusedImports([]string{"net/http", "os", "regexp"},
		[]string{thisPackage, "net/http", "os", "crypto/rand"}); len(got) != 0 {
		t.Errorf("an allowed import was refused: %v", got)
	}
}
