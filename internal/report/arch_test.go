package report

import (
	"os/exec"
	"strings"
	"testing"
)

// A report is written to be posted in public, so the code that builds it
// must have no way to read a credential. It masks by shape, never by value,
// and this keeps it so: neither the report nor the masking package may
// depend on the secret store or the host credential importer, directly or
// through any package between them. The CLI resolves the session and the
// profile, which do reach the store, and hands the facts in as plain data.
func TestReportReachesNoSecretStore(t *testing.T) {
	const module = "github.com/brig-sh/brig/"
	forbidden := []string{module + "internal/secret", module + "internal/hostsrc"}
	for _, pkg := range []string{module + "internal/report", module + "internal/redact"} {
		out, err := exec.Command("go", "list", "-deps", pkg).CombinedOutput()
		if err != nil {
			t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
		}
		for _, dep := range strings.Fields(string(out)) {
			for _, f := range forbidden {
				if dep == f {
					t.Errorf("%s depends on %s", pkg, f)
				}
			}
		}
	}
}
