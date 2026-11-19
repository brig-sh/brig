package wrap

import (
	"strings"
	"testing"
)

// passthroughPolicy is a passthrough route on the anthropic-api-key preset,
// with egress as given (YAML under spec:).
func passthroughPolicy(egress string) string {
	return `apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: {name: brokered}
spec:
` + egress + `  routes:
    - name: anthropic
      preset: anthropic-api-key
      passthrough: true
`
}

// Under an allow default the guest can send its own key straight to
// api.anthropic.com, so the run is refused before the hull is asked anything.
// Under a deny default with no allow for the host the run goes on.
func TestARunWhoseEgressReachesAPassthroughUpstreamIsRefused(t *testing.T) {
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	shortStateDir(t)
	rec := &hullRecorder{}
	cfg := routedConfig(t, passthroughPolicy("  egress:\n    default: allow\n"), rec)
	shortStateDir(t)
	err := cfg.routeRefusal()
	if err == nil {
		t.Fatal("a passthrough run under an allow default was not refused")
	}
	for _, want := range []string{"route anthropic of policy brokered is passthrough", "api.anthropic.com",
		"default: deny"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
	if rec.linkLocalProbes != 0 {
		t.Errorf("a refused run probed the hull %d times", rec.linkLocalProbes)
	}

	ok := routedConfig(t, passthroughPolicy("  egress:\n    default: deny\n"), &hullRecorder{})
	shortStateDir(t)
	if err := ok.routeRefusal(); err != nil {
		t.Errorf("a passthrough run under a deny default was refused: %v", err)
	}
}
