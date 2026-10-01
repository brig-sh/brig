package wrap

import (
	"errors"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
)

type networkDiagnosticRuntime struct {
	fakeRuntime
	err error
}

func (r networkDiagnosticRuntime) SandboxNetwork(string) (string, error) { return "", r.err }

// A parser refusal is different from an unavailable runtime. Both leave the
// posture unknown, but info must carry the cause needed to repair the session.
func TestNetworkInfoExplainsWhyTheLegacyPostureIsUnknown(t *testing.T) {
	for _, reason := range []string{
		"",
		"inspect network of brig-claude-code: invalid hull response: unexpected end of JSON input",
		"inspect network of brig-claude-code: cannot connect to the runtime daemon",
	} {
		for _, explicit := range []string{"", "offline"} {
			t.Run(reason+"/"+explicit, func(t *testing.T) {
				p, _ := legacySession(t, "claude-code")
				var rt runtime.Runtime = fakeRuntime{}
				if reason != "" {
					rt = networkDiagnosticRuntime{err: errors.New(reason)}
				}
				c, err := Load(p, Options{Network: explicit}, rt)
				if err != nil {
					t.Fatal(err)
				}
				cause := reason
				if cause == "" {
					cause = "runtime inspection unavailable"
				}
				want := "unknown (no recorded posture; " + cause + ")"
				if explicit != "" {
					want += "; offline from its next boot"
				}
				var text string
				for _, row := range c.envelope(creds.Set{}) {
					if row.label == "NETWORK" {
						text = row.value
					}
				}
				if text != want {
					t.Errorf("text network = %q, want %q", text, want)
				}
				if got := c.InfoData(creds.Set{}).Network; got != want {
					t.Errorf("JSON network = %q, want %q", got, want)
				}
			})
		}
	}
}
