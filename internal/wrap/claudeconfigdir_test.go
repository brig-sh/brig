package wrap

import (
	"testing"

	"github.com/brig-sh/brig/internal/creds"
)

// With the tmpfs off the home, CLAUDE_CONFIG_DIR is what keeps the agent's
// credential off the share, so neither a host value nor BRIG_FORWARD_ENV may
// replace the profile's own.
func TestClaudeConfigDirIsTheProfilesWhateverTheHostHolds(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/Users/x/.claude")
	ws := t.TempDir()
	c := testConfig(t, ws, ws)
	c.OpenStore = func() (creds.SecretReader, error) { return fakeStore{}, nil }
	c.Env, c.envWarnings = envOverride(c.Profile.Env, []string{"CLAUDE_CONFIG_DIR"})
	set, err := c.BuildEnv()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range c.guestEnv(set) {
		if v.Name == "CLAUDE_CONFIG_DIR" {
			got = append(got, v.Value)
		}
	}
	if len(got) != 1 || got[0] != "/brig/claude" {
		t.Errorf("CLAUDE_CONFIG_DIR in the guest = %v, want only /brig/claude", got)
	}
}
