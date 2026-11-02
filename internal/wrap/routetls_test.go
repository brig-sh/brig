package wrap

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
)

const tlsRefusalPolicy = `apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: {name: inventory-mtls}
spec:
  routes:
    - name: inventory
      upstream: https://inventory.internal.example.com
      inject:
        tls:
          clientCertificate: {secretRef: {name: inventory-client}}
          ca: {secretRef: {name: example-root-ca}}
      guest:
        env: {INVENTORY_URL: "{route.url}"}
`

// A route with inject.tls is refused before anything boots or is written, on
// every runtime kind, because this build neither presents the client
// certificate nor pins the CA: the request would go upstream with no
// certificate and with the system roots trusted. The reason says so, and the
// refusal is a CapabilityError, so brig exits 7.
func TestARouteWithInjectTLSIsRefusedBeforeBoot(t *testing.T) {
	for _, c := range []struct{ name, kind, hypervisor string }{
		{"hull on vz", "hull", "vz"},
		{"hull on hvi", "hull", "hvi"},
		{"nerdctl", "nerdctl", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "inventory-mtls.yaml"), []byte(tlsRefusalPolicy), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("BRIG_POLICY_DIR", dir)
			t.Setenv("BRIG_HYPERVISOR", c.hypervisor)
			state := filepath.Join(t.TempDir(), "state")
			t.Setenv("BRIG_STATE_DIR", state)

			cfg := loadWithPolicy(t, "inventory-mtls")
			rec := &kindRecorder{kind: c.kind}
			cfg.Runtime = rec
			broker := &fakeBroker{}
			cfg.broker = broker
			before := listTree(t, state)
			err := cfg.EnsureRunning(creds.Set{})
			for _, want := range []string{"inventory-mtls (route inventory) sets inject.tls",
				"inject.tls is not served by this build of brig"} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
			var ce *runtime.CapabilityError
			if !errors.As(err, &ce) {
				t.Errorf("the refusal is not a CapabilityError, so brig would not exit 7: %v", err)
			}
			if len(rec.asked) != 0 {
				t.Errorf("the runtime was asked %v before the refusal", rec.asked)
			}
			if len(broker.registered) != 0 {
				t.Errorf("a sandbox was registered with the broker: %+v", broker.registered)
			}
			if after := listTree(t, state); !slices.Equal(before, after) {
				t.Errorf("the refused run wrote state: %v, was %v", after, before)
			}
			// brig info says the refusal's own reason, not a transport fault.
			row := cfg.brokerLine()
			if !strings.Contains(row, "inject.tls is not served by this build of brig") ||
				strings.Contains(row, "cannot route brig.internal") {
				t.Errorf("the BROKER row does not give the TLS refusal: %q", row)
			}
			if d := cfg.Plan(); d.Refused == nil || !strings.Contains(d.Refused.Reason, "inject.tls is not served") {
				t.Errorf("the plan does not carry the refusal: %+v", d.Refused)
			}
		})
	}
}
