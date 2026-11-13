package wrap

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/secret"
	"github.com/brig-sh/brig/internal/verify"
)

// pinnedPlanConfig is a run of a profile with every row of the plan filled
// in: a policy with an egress rule and a route, a stored secret, a missing
// one, a project and a port. It is built by hand, with no runtime and no
// settings, so the plan of it is the same bytes on every machine.
func pinnedPlanConfig(t *testing.T) *Config {
	t.Helper()
	// planProfile forwards SHELL_TOK from the shell. Unset, it is the same
	// unresolved row on every machine.
	t.Setenv("SHELL_TOK", "")
	if err := os.Unsetenv("SHELL_TOK"); err != nil {
		t.Fatal(err)
	}
	c := bindingConfig(t, planProfile)
	c.Runtime = nil
	c.Workspace = "/home-dir"
	c.Cwd = "/cwd"
	c.Project, c.ProjectReal, c.GuestProject = "/p", "/real/p", "/work/p"
	c.Image, c.Pull = "ghcr.io/brig-sh/x:1", "missing"
	c.Mem, c.CPUs = 2048, 2
	c.Verify = verify.Warn
	c.VerifyPolicy = verify.DefaultPolicy()
	c.Network = NetIsolated
	c.Policies = []string{"base", "locked"}
	c.Egress = policy.Egress{Default: "deny", Allow: []policy.Rule{{Host: "a.example"}}}
	c.routes = []v1alpha1.Route{{Name: "plain", Upstream: "https://api.example",
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: "routed"}}}}
	c.routePolicy = map[string]string{"plain": "locked"}
	c.routeSkipped = []string{"routed"}
	reads := 0
	c.OpenStore = func() (creds.SecretReader, error) {
		return readCountingStore{list: []secret.Secret{{Name: "tok"}, {Name: "routed"}}, reads: &reads}, nil
	}
	return c
}

// The plan of a run that names no target is what it was before a plan could
// name one: the same JSON, byte for byte, and so the same digest. A field
// added for a target that leaks into this plan, even as an empty value,
// moves the digest every reader of an older plan compares against.
func TestPlanOfAProfileIsPinned(t *testing.T) {
	c := pinnedPlanConfig(t)
	d := c.Plan()
	blob, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if string(blob) != pinnedPlanJSON {
		t.Errorf("the plan of a profile changed:\n got %s\nwant %s", blob, pinnedPlanJSON)
	}
	if d.Digest != pinnedPlanDigest {
		t.Errorf("digest %s, want %s", d.Digest, pinnedPlanDigest)
	}
	var text bytes.Buffer
	c.Out = &text
	c.PrintPlan(d)
	if text.String() != pinnedPlanText {
		t.Errorf("the text plan of a profile changed:\n got:\n%s\nwant:\n%s", text.String(), pinnedPlanText)
	}
}

const pinnedPlanDigest = "sha256:316c791e024a9005c74dd0d147af8ea5273e5f4d47a7b849f8cd91995bc8431e"

const pinnedPlanJSON = `{"profile":"x","sandbox":"brig-x","runtime":{"available":false},"isolation":"unknown (no runtime, so brig cannot tell what a sandbox here would stand on)","home":{"host":"/home-dir","guest":"/home/x","mode":"read-write"},"project":{"host":"/real/p","guest":"/work/p","mode":"read-write"},"skills":[],"image":{"ref":"ghcr.io/brig-sh/x:1","pull":"missing"},"verify":{"mode":"warn","policy":"brig"},"network":"isolated (a network of this sandbox's own)","posture":"isolated","ports":[],"policies":["base","locked"],"noPolicy":false,"egress":{"default":"deny","allow":[{"host":"a.example"}],"deny":[]},"broker":{"transport":"brig.internal","routes":["plain"],"keptOnHost":["routed"],"modes":[],"unavailable":{"endpoint":"cannot enforce: there is no runtime here"}},"credentials":[{"name":"GH_TOKEN","delivery":"env","secret":"gh","required":true,"state":"unresolved","reason":"the secret gh is not in the store"},{"name":"SHELL_TOK","delivery":"env","required":false,"state":"unresolved","reason":"no source in the chain has a value"},{"name":".config/cred","delivery":"file","source":"secret","secret":"tok","required":true,"state":"resolved"}],"limits":{"mem":2048,"cpus":2},"digest":"sha256:316c791e024a9005c74dd0d147af8ea5273e5f4d47a7b849f8cd91995bc8431e"}`

const pinnedPlanText = `PROFILE      x
SANDBOX      brig-x
RUNTIME      unavailable (no runtime on PATH)
ISOLATION    unknown (no runtime, so brig cannot tell what a sandbox here would stand on)
HOME         /home-dir (read-write, mounted at /home/x)
PROJECT      /real/p (read-write, mounted at /work/p)
IMAGE        ghcr.io/brig-sh/x:1 (pull missing)
VERIFY       warn, against brig's own trust policy
NETWORK      isolated (a network of this sandbox's own)
POLICY       base, locked
EGRESS       default deny
             allow a.example
BROKER       transport brig.internal, routes plain; kept on the host: routed; modes none
CREDENTIALS  GH_TOKEN (env) unresolved: the secret gh is not in the store (required)
             SHELL_TOK (env) unresolved: no source in the chain has a value
             .config/cred (file) from the secret tok
LIMITS       2048 MB, 2 vCPUs
DIGEST       sha256:316c791e024a9005c74dd0d147af8ea5273e5f4d47a7b849f8cd91995bc8431e
`
