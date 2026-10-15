package v1alpha1

import (
	"os/exec"
	"strings"
	"testing"
)

const thisPackage = "github.com/brig-sh/brig/api/policy/v1alpha1"

// refusedImports returns what this package may not link. direct are its own
// imports, and deps are every package it links. Its own code does no I/O, so
// it imports nothing that opens a file, a socket or a process, and it never
// reaches brig's internal/ tree. fmt and net/url link os, syscall and net
// themselves, so only the direct imports are held to those three.
func refusedImports(direct, deps []string) []string {
	var refused []string
	for _, imp := range direct {
		switch {
		case imp == "os", strings.HasPrefix(imp, "os/"), imp == "io/ioutil", imp == "syscall",
			imp == "net", imp == "net/http", strings.HasPrefix(imp, "net/http/"), imp == "log":
			refused = append(refused, "imports "+imp)
		}
	}
	for _, dep := range deps {
		switch {
		case strings.HasPrefix(dep, "github.com/brig-sh/brig/internal/"),
			dep == "net/http", dep == "os/exec", dep == "io/ioutil", dep == "log":
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

// An operator links this package without linking brig.
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
		{[]string{"os"}, nil},
		{[]string{"os/exec"}, nil},
		{[]string{"io/ioutil"}, nil},
		{[]string{"syscall"}, nil},
		{[]string{"net"}, nil},
		{[]string{"net/http"}, nil},
		{[]string{"log"}, nil},
		{nil, []string{"github.com/brig-sh/brig/internal/secret"}},
		{nil, []string{"net/http"}},
		{nil, []string{"os/exec"}},
		{nil, []string{"log"}},
	} {
		if got := refusedImports(c.direct, c.deps); len(got) == 0 {
			t.Errorf("imports %v and deps %v were not refused", c.direct, c.deps)
		}
	}
	// What fmt, net/url and the YAML decoder link is allowed.
	if got := refusedImports([]string{"fmt", "net/url", "net/netip", "reflect", "sigs.k8s.io/yaml"},
		[]string{"os", "syscall", "net", "io/fs"}); len(got) != 0 {
		t.Errorf("an allowed import was refused: %v", got)
	}
}
