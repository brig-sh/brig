package wrap

import (
	"io"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/verify"
)

// Cursor's default macOS backend cannot isolate, but that must not put its
// Linux guests on the shared bridge. Exercise the shipped profile through the
// boot request: testing an otherwise empty profile would miss this regression.
func TestCursorUsesTheBackendNetworkDefault(t *testing.T) {
	for _, tc := range []struct {
		kind, hypervisor, want string
	}{
		{"nerdctl", "", "isolated"},
		{"nerdctl", "vz", "isolated"},
		{"hull", "", "shared"},
		{"hull", "hvi", "isolated"},
		{"hull", "qemu", "shared"},
	} {
		t.Run(tc.kind+"/"+tc.hypervisor, func(t *testing.T) {
			isolateState(t)
			t.Setenv("BRIG_POLICY_DIR", t.TempDir())
			t.Setenv("BRIG_HYPERVISOR", tc.hypervisor)
			t.Setenv("BRIG_CURSOR_NETWORK", "")
			p, ok := profile.Lookup("cursor")
			if !ok {
				t.Fatal("the shipped cursor profile is missing")
			}
			home := t.TempDir()
			rt := &networkDefaultRuntime{livenessRuntime: &livenessRuntime{workspace: home}, kind: tc.kind}
			// Cursor's image is unpublished; users supply their own with --image.
			c, err := Load(p, Options{Workspace: home, Image: "example.test/cursor:local"}, rt)
			if err != nil {
				t.Fatal(err)
			}
			c.Verify, c.MacOSVersion = verify.Off, func() string { return "" }
			c.Out, c.Err, c.Progress = io.Discard, io.Discard, io.Discard
			if err := c.EnsureRunning(creds.Set{}); err != nil {
				t.Fatal(err)
			}
			if rt.spec.Net != tc.want {
				t.Errorf("cursor booted on %q, want %q", rt.spec.Net, tc.want)
			}
		})
	}
}
