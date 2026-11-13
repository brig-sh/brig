package wrap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
)

// digestOf loads the dev session of /src/a with p, the options o and a change
// to its target, and returns the digest of what it would boot.
func targetDigest(t *testing.T, p profile.Profile, o Options, change func(*Target)) string {
	t.Helper()
	id := ProjectIdentity("/src/a", "dev", "", "")
	tg := repoTarget()
	tg.Root, tg.Brigfile = "/src/a", "/src/a/Brigfile"
	if change != nil {
		change(tg)
	}
	o.Identity, o.Target = &id, tg
	if o.Policies == nil {
		o.Policies = &PolicySet{}
	}
	c, err := Load(p, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c.ConfigDigest()
}

// digestHost is a host with no setting that changes what a target boots.
func digestHost(t *testing.T) {
	t.Helper()
	isolateHome(t)
	for _, key := range []string{"BRIG_IMAGE", "BRIG_MEM", "BRIG_CPUS"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}

// The digest is pinned: a session recorded by one build reads as unchanged to
// the next unless what it covers changed. Each input that changes what boots
// changes it, and nothing else does.
func TestTheConfigDigestCoversWhatBoots(t *testing.T) {
	digestHost(t)
	base := targetDigest(t, repoClaude(t), Options{}, nil)
	const want = "ee93583f063b678660c5e445e9fb3ca5f04b8d157860e5c76088f08b373f8a44"
	if base != want {
		t.Errorf("the digest of a fixed configuration is %s, want %s", base, want)
	}

	deny := policy.Policy{Metadata: v1alpha1.ObjectMeta{Name: "p"},
		Spec: v1alpha1.PolicySpec{Egress: &v1alpha1.Egress{Default: "deny"}}}
	other := repoClaude(t)
	other.Binary = "y"
	changes := map[string]string{
		"the image":   targetDigest(t, repoClaude(t), Options{Image: "example.com/other:1"}, nil),
		"the memory":  targetDigest(t, repoClaude(t), Options{Mem: 2048}, nil),
		"the vCPUs":   targetDigest(t, repoClaude(t), Options{CPUs: 4}, nil),
		"the network": targetDigest(t, repoClaude(t), Options{Network: "offline"}, nil),
		"a policy":    targetDigest(t, repoClaude(t), Options{Policies: &PolicySet{Docs: []policy.Policy{deny}}}, nil),
		"the binary":  targetDigest(t, other, Options{}, nil),
		"the args":    targetDigest(t, repoClaude(t), Options{}, func(tg *Target) { tg.Args = []string{"--x"} }),
		"the root":    targetDigest(t, repoClaude(t), Options{}, func(tg *Target) { tg.Root = "/src/b" }),
		"BRIG_IMAGE": func() string {
			t.Setenv("BRIG_IMAGE", "example.com/env:1")
			defer os.Unsetenv("BRIG_IMAGE")
			return targetDigest(t, repoClaude(t), Options{}, nil)
		}(),
	}
	for what, d := range changes {
		if d == base {
			t.Errorf("changing %s left the digest as it was", what)
		}
	}

	described := repoClaude(t)
	described.Desc = "a new line for brig agent ls"
	described.Name = "renamed"
	hinted := repoClaude(t)
	hinted.Secrets = []profile.SecretDecl{{Name: "tok", Hint: "another hint"}}
	elsewhere := t.TempDir()
	same := map[string]string{
		"the description and name": targetDigest(t, described, Options{}, nil),
		"a published port":         targetDigest(t, repoClaude(t), Options{Publish: []string{"8080"}}, nil),
		"the notes and the Brigfile path": targetDigest(t, repoClaude(t), Options{}, func(tg *Target) {
			tg.Notes, tg.Brigfile = []string{"a note"}, "/src/a/ci/Brigfile"
		}),
		"where each value came from": targetDigest(t, repoClaude(t), Options{}, func(tg *Target) {
			tg.Base = Origins{Image: "x", Mem: "y", CPUs: "z", Network: "w"}
		}),
		"the working directory": func() string { t.Chdir(elsewhere); return targetDigest(t, repoClaude(t), Options{}, nil) }(),
	}
	for what, d := range same {
		if d != base {
			t.Errorf("changing %s changed the digest", what)
		}
	}
	withSecret := repoClaude(t)
	withSecret.Secrets = []profile.SecretDecl{{Name: "tok", Hint: "a hint"}}
	if targetDigest(t, withSecret, Options{}, nil) != targetDigest(t, hinted, Options{}, nil) {
		t.Error("the hint of a secret changed the digest")
	}
	if targetDigest(t, withSecret, Options{}, nil) == base {
		t.Error("declaring a secret left the digest as it was")
	}
}

// The check the run of a target makes before it reuses a sandbox: nothing to
// compare without a record, a match after one, and a change once the
// configuration moves.
func TestConfigChangedComparesWithTheRecord(t *testing.T) {
	digestHost(t)
	c := loadProject(t, "/src/a", "dev", "")
	if changed, recorded := c.ConfigChanged(); changed || recorded {
		t.Errorf("with no record: changed %v, recorded %v", changed, recorded)
	}
	ps, ok := c.ProjectSession()
	if !ok || ps.ConfigDigest != c.ConfigDigest() || ps.Home != c.Workspace || ps.Target != "dev" {
		t.Fatalf("ProjectSession = %+v, %v", ps, ok)
	}
	if err := RecordProject(ps); err != nil {
		t.Fatal(err)
	}
	if changed, recorded := c.ConfigChanged(); changed || !recorded {
		t.Errorf("against its own record: changed %v, recorded %v", changed, recorded)
	}
	id := ProjectIdentity("/src/a", "dev", "", "")
	tg := repoTarget()
	tg.Name, tg.Root = "dev", "/src/a"
	bigger, err := Load(repoClaude(t), Options{Identity: &id, Policies: &PolicySet{}, Target: tg, Mem: 4096}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if changed, recorded := bigger.ConfigChanged(); !changed || !recorded {
		t.Errorf("with more memory: changed %v, recorded %v", changed, recorded)
	}

	if _, ok := mustLoad(t, Options{}).ProjectSession(); ok {
		t.Error("a profile's run has a project session")
	}
}

// Recreate removes the sandbox and keeps the home and the project entry. The
// next run's Load and reap find the home owned, and boot on it.
func TestRecreateKeepsTheHomeAndTheEntry(t *testing.T) {
	digestHost(t)
	c := loadProject(t, "/src/a", "dev", "")
	ps, _ := c.ProjectSession()
	bootHome(t, c)
	if err := RecordProject(ps); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RecordBootedNet(c.VMName, "isolated"); err != nil {
		t.Fatal(err)
	}

	rt := &listingRuntime{names: []string{c.VMName}}
	c.Runtime = rt
	if err := c.Recreate(); err != nil {
		t.Fatal(err)
	}
	if len(rt.removed) != 1 || rt.removed[0] != c.VMName {
		t.Errorf("removed %q", rt.removed)
	}
	if got, ok := LookupProject(c.VMName); !ok || got != ps {
		t.Errorf("the entry is %+v (%v), want %+v", got, ok, ps)
	}
	if WorkspaceOfSandbox(c.VMName) != "" {
		t.Error("the session entry of the removed sandbox stays")
	}
	if word, _ := runtime.BootedNet(c.VMName); word != "" {
		t.Errorf("the network of the removed sandbox stays: %q", word)
	}

	next := loadProject(t, "/src/a", "dev", "")
	next.Runtime = &listingRuntime{}
	next.reapOrphanHome()
	if next.Workspace != c.Workspace {
		t.Errorf("the next run's home is %s, want %s", next.Workspace, c.Workspace)
	}
	if _, err := os.Stat(guestState(next.Workspace)); err != nil {
		t.Errorf("the next run reaped the home: %v", err)
	}
}

// A recreate needs the entry, without which the next run would take the home
// for an orphan, and is only for a target's sandbox. A sandbox the runtime
// could not remove is reported, and the entry and home stay.
func TestRecreateRefuses(t *testing.T) {
	digestHost(t)
	if err := mustLoad(t, Options{}).Recreate(); err == nil {
		t.Error("a profile's sandbox was recreated")
	}
	c := loadProject(t, "/src/a", "dev", "")
	c.Runtime = &listingRuntime{names: []string{c.VMName}}
	if err := c.Recreate(); err == nil || !strings.Contains(err.Error(), "no record") {
		t.Errorf("an unrecorded session: %v", err)
	}

	bootProject(t, c)
	c.Runtime = &stuckRuntime{listingRuntime{names: []string{c.VMName}}}
	if err := c.Recreate(); err == nil {
		t.Error("a sandbox that would not go was reported recreated")
	}
	if _, ok := LookupProject(c.VMName); !ok {
		t.Error("the entry went")
	}
	if _, err := os.Stat(filepath.Join(c.Workspace, ".local")); err != nil {
		t.Errorf("the home went: %v", err)
	}
}

// stuckRuntime refuses to remove anything and keeps listing it.
type stuckRuntime struct{ listingRuntime }

func (r *stuckRuntime) Remove(string) error { return errors.New("busy") }
