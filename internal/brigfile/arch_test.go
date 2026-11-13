package brigfile

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const thisPackage = "github.com/brig-sh/brig/internal/brigfile"

// refusedImports returns what this package may not link. Loading a Brigfile
// reads files and nothing else: it runs no subprocess, opens no socket,
// reaches no runtime and reads no credential. internal/profile links
// internal/secret and os/exec for reasons of its own, so those two are held
// to the direct imports only.
func refusedImports(direct, deps []string) []string {
	var refused []string
	for _, imp := range direct {
		switch {
		case imp == "os/exec", imp == "net", strings.HasPrefix(imp, "net/http"),
			imp == "github.com/brig-sh/brig/internal/secret":
			refused = append(refused, "imports "+imp)
		}
	}
	for _, dep := range deps {
		for _, pkg := range []string{"wrap", "hostsrc", "creds", "renew", "runtime", "verify", "broker"} {
			if dep == "github.com/brig-sh/brig/internal/"+pkg {
				refused = append(refused, "links "+dep)
			}
		}
		if dep == "net/http" {
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

func TestLoadingLinksNothingThatRunsOrFetches(t *testing.T) {
	direct := goList(t, "-f", `{{join .Imports " "}}`, thisPackage)
	deps := goList(t, "-deps", "-f", "{{.ImportPath}}", thisPackage)
	if !slices.Contains(direct, "os") || len(deps) <= len(direct) {
		t.Fatalf("go list read nothing useful: %d imports, %d deps", len(direct), len(deps))
	}
	for _, r := range refusedImports(direct, deps) {
		t.Errorf("%s %s", thisPackage, r)
	}
}

// The check above passes on an empty answer as well as on a clean one, so
// each rule is shown to refuse a planted import.
func TestRefusedImportsCatchesAPlantedImport(t *testing.T) {
	for _, c := range []struct{ direct, deps []string }{
		{[]string{"os/exec"}, nil},
		{[]string{"net"}, nil},
		{[]string{"net/http"}, nil},
		{[]string{"github.com/brig-sh/brig/internal/secret"}, nil},
		{nil, []string{"github.com/brig-sh/brig/internal/wrap"}},
		{nil, []string{"github.com/brig-sh/brig/internal/hostsrc"}},
		{nil, []string{"github.com/brig-sh/brig/internal/creds"}},
		{nil, []string{"github.com/brig-sh/brig/internal/runtime"}},
		{nil, []string{"net/http"}},
	} {
		if got := refusedImports(c.direct, c.deps); len(got) == 0 {
			t.Errorf("imports %v and deps %v were not refused", c.direct, c.deps)
		}
	}
	if got := refusedImports([]string{"os", "io", "github.com/brig-sh/brig/internal/profile"},
		[]string{"os/exec", "github.com/brig-sh/brig/internal/secret"}); len(got) != 0 {
		t.Errorf("an allowed import was refused: %v", got)
	}
}
