package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/runtime"
)

// Each of the two policies reads alone. Bound together, the second one's
// allow lets the guest send its own key to api.anthropic.com past the
// passthrough route, so check refuses what a boot would refuse.
func TestCheckRefusesAPassthroughRouteThatEgressReaches(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	stubDetectRuntimeFor(t, func(runtime.Preference) (runtime.Runtime, error) {
		return routingRuntime{doctorRuntime{bin: "hull"}, nil}, nil
	})
	for name, doc := range map[string]string{
		"checked": "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata:\n  name: checked\nspec:\n" +
			"  egress:\n    default: deny\n  routes:\n    - name: anthropic\n      preset: anthropic-api-key\n" +
			"      passthrough: true\n",
		"open-anthropic": "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata:\n  name: open-anthropic\nspec:\n" +
			"  egress:\n    default: deny\n    allow:\n      - host: api.anthropic.com\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	loadTestProfiles(t)
	if err := attachPolicy([]string{"checked", "claude-code"}); err != nil {
		t.Fatal(err)
	}
	var err error
	captureStderr(t, func() {
		_, err = captureStdout(t, func() error { return checkPolicy([]string{"claude-code"}) })
	})
	if err != nil {
		t.Fatalf("check refused a passthrough route under a deny default: %v", err)
	}

	if err := attachPolicy([]string{"open-anthropic", "claude-code"}); err != nil {
		t.Fatal(err)
	}
	captureStderr(t, func() {
		_, err = captureStdout(t, func() error { return checkPolicy([]string{"claude-code"}) })
	})
	if err == nil {
		t.Fatal("check passed a passthrough route whose upstream another policy allows")
	}
	for _, want := range []string{"refuses the run", "route anthropic of policy checked is passthrough",
		"allow host api.anthropic.com of policy open-anthropic", "remove the allow for api.anthropic.com"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not hold %q: %v", want, err)
		}
	}
}
