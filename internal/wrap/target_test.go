package wrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/profile"
)

// Two roots and two targets are four sandboxes, four homes and four index
// keys. The name starts brig--, which no profile run's name can, and stays
// within 63 bytes however long the target name is.
func TestProjectIdentityIsOnePerRootAndTarget(t *testing.T) {
	sandboxes, keys := map[string]bool{}, map[string]bool{}
	for _, root := range []string{"/src/a", "/src/b"} {
		for _, target := range []string{"dev", "ci"} {
			id := ProjectIdentity(root, target, "", "")
			if !strings.HasPrefix(id.Sandbox, "brig--"+target+"-") || !strings.HasPrefix(id.Key, "@p/") {
				t.Errorf("%s %s: %+v", root, target, id)
			}
			sandboxes[id.Sandbox], keys[id.Key] = true, true
		}
	}
	if len(sandboxes) != 4 || len(keys) != 4 {
		t.Errorf("four targets gave %d sandboxes and %d keys", len(sandboxes), len(keys))
	}
	named := ProjectIdentity("/src/a", "dev", "x", "")
	if !strings.HasPrefix(named.Sandbox, "brig--dev.x-") || named == ProjectIdentity("/src/a", "dev", "", "") {
		t.Errorf("a named session of dev: %+v", named)
	}
	long := ProjectIdentity("/src/a", strings.Repeat("t", 80), "", "")
	if len(long.Sandbox) > 63 {
		t.Errorf("a long target gives a %d-byte name: %s", len(long.Sandbox), long.Sandbox)
	}
	if long == ProjectIdentity("/src/a", strings.Repeat("t", 81), "", "") {
		t.Error("two target names that share their first 36 bytes share a sandbox")
	}
}

// repoClaude is a profile a repository wrote and called claude-code.
func repoClaude(t *testing.T) profile.Profile {
	t.Helper()
	p, err := profile.Parse([]byte("name: claude-code\nimage: example.com/repo:1\nguestHome: /root\n" +
		"binary: x\nmem: 1024\ncpus: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// loadTarget loads p as a Brigfile target of the project at /src/a would,
// with no runtime.
func loadTarget(t *testing.T, p profile.Profile, o Options) *Config {
	t.Helper()
	id := ProjectIdentity("/src/a", "dev", "", o.Target.ProfileAlias)
	o.Identity = &id
	if o.Policies == nil {
		o.Policies = &PolicySet{}
	}
	c, err := Load(p, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func repoTarget() *Target {
	return &Target{Name: "dev", ProfileAlias: "", ProfileSource: "file:.brig/claude.yaml",
		Base: Origins{Image: "profile claude (file:.brig/claude.yaml)", Mem: "target dev",
			CPUs: "profile claude (file:.brig/claude.yaml)", Network: "profile claude (file:.brig/claude.yaml)"}}
}

// A repository profile called claude-code reads none of the settings you
// keep for claude-code. The global ones still apply, and say so.
func TestATargetReadsNoSettingOfTheProfileNameItChose(t *testing.T) {
	isolateState(t)
	t.Setenv("BRIG_CLAUDE_CODE_FORWARD_ENV", "PLANTED_VAR")
	t.Setenv("BRIG_CLAUDE_CODE_IMAGE", "example.com/yours:1")
	t.Setenv("BRIG_CLAUDE_CODE_ALLOW_DENIED", "1")
	t.Setenv("BRIG_CLAUDE_CODE_MEM", "9999")
	t.Setenv("BRIG_IMAGE", "example.com/global:1")
	c := loadTarget(t, repoClaude(t), Options{Target: repoTarget()})

	if c.Image != "example.com/global:1" || c.origins.Image != "env BRIG_IMAGE" {
		t.Errorf("image %s from %q, want the global setting", c.Image, c.origins.Image)
	}
	if c.Mem != 1024 || c.origins.Mem != "target dev" {
		t.Errorf("mem %d from %q, want the profile's value from its base", c.Mem, c.origins.Mem)
	}
	for _, b := range c.Env {
		if b.Name == "PLANTED_VAR" {
			t.Error("BRIG_CLAUDE_CODE_FORWARD_ENV reached a repository profile's run")
		}
	}
	if c.AllowDenied {
		t.Error("BRIG_CLAUDE_CODE_ALLOW_DENIED reached a repository profile's run")
	}

	// The same profile name run as your own does read them, which is what
	// makes the case above worth testing.
	own, err := Load(repoClaude(t), Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if own.Image != "example.com/yours:1" || !own.AllowDenied {
		t.Errorf("the profile run reads %s, allowDenied %v", own.Image, own.AllowDenied)
	}
}

// A target of a profile called claude-code is not your claude-code session:
// not its sandbox, not its home, and not its index entry. BRIG_NAME and
// BRIG_WORKSPACE do not move it there.
func TestATargetIsNotTheSessionOfItsProfileName(t *testing.T) {
	isolateState(t)
	yours, err := defaultWorkspace("brig-claude-code")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIG_NAME", "brig-claude-code")
	t.Setenv("BRIG_WORKSPACE", t.TempDir())
	t.Setenv("BRIG_CLAUDE_CODE_WORKSPACE", t.TempDir())
	c := loadTarget(t, repoClaude(t), Options{Target: repoTarget()})
	want := ProjectIdentity("/src/a", "dev", "", "")
	if c.VMName != want.Sandbox || c.sessionRef() != want.Key {
		t.Errorf("sandbox %s, key %s, want %+v", c.VMName, c.sessionRef(), want)
	}
	home, err := defaultWorkspace(want.Sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if c.Workspace != home || c.Workspace == yours || c.homeGiven {
		t.Errorf("home %s (given %v), want %s", c.Workspace, c.homeGiven, home)
	}

	_, err = Load(repoClaude(t), Options{Identity: &want, Workspace: t.TempDir()}, nil)
	if err == nil {
		t.Error("a target took a home")
	}
}

// A sandbox name that starts brig-- belongs to a Brigfile target, so a run of
// a profile may not take one through BRIG_NAME, under either spelling.
func TestBRIGNameCannotTakeATargetsPrefix(t *testing.T) {
	for _, key := range []string{"BRIG_NAME", "BRIG_CLAUDE_CODE_NAME"} {
		t.Run(key, func(t *testing.T) {
			isolateState(t)
			t.Setenv(key, "brig--dev-0123456789abcdef0123")
			_, err := Load(repoClaude(t), Options{}, nil)
			if err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "Brigfile target") {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func egressDoc(name, host string) policy.Policy {
	return policy.Policy{Metadata: v1alpha1.ObjectMeta{Name: name}, Spec: v1alpha1.PolicySpec{
		Egress: &policy.Egress{Default: "deny", Allow: []policy.Rule{{Host: host}}}}}
}

// Policies handed over are the run's policies, and the directory is not read:
// a policy there attached to the profile's name, and one there with the name
// of a handed-over document, apply to nothing.
func TestHandedOverPoliciesReadNoDirectory(t *testing.T) {
	isolateState(t)
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writeTestPolicy(t, dir, "yours")
	writeTestPolicy(t, dir, "repo")
	var a policy.Attachments
	a.AttachToProfile("yours", "claude-code")
	if err := a.Save(dir); err != nil {
		t.Fatal(err)
	}
	p := repoClaude(t)
	p.Policy = []string{"yours"}
	c := loadTarget(t, p, Options{Target: repoTarget(), Policies: &PolicySet{
		Docs: []policy.Policy{egressDoc("repo", "repo.example")}, Origins: []string{"brigfile repo (inline)"}}})
	if strings.Join(c.Policies, ",") != "repo" || len(c.Egress.Allow) != 1 || c.Egress.Allow[0].Host != "repo.example" {
		t.Errorf("policies %v, egress %+v, want the handed-over document alone", c.Policies, c.Egress)
	}
}

// A repository profile may ask for the shared network. A policy that sets an
// egress default still takes the run to a network of its own, and the plan
// says what was asked and where from.
func TestAHandedOverPolicyNarrowsTheNetwork(t *testing.T) {
	isolateState(t)
	p := repoClaude(t)
	p.Network = "shared"
	c := loadTarget(t, p, Options{Target: repoTarget(), Policies: &PolicySet{
		Docs: []policy.Policy{egressDoc("repo", "repo.example")}, Origins: []string{"brigfile repo (inline)"}}})
	if c.Network != NetIsolated || c.origins.NetworkAsked != "shared" ||
		c.origins.Network != "profile claude (file:.brig/claude.yaml)" {
		t.Errorf("network %s, origins %+v", c.Network, c.origins)
	}
	free := loadTarget(t, p, Options{Target: repoTarget()})
	if free.Network != NetShared || free.origins.NetworkAsked != "" {
		t.Errorf("with no policy: network %s, origins %+v", free.Network, free.origins)
	}
}

// Two documents can carry one metadata.name. Each route is still said to
// come from the document that declares it.
func TestRouteOriginsGoByDocument(t *testing.T) {
	isolateState(t)
	route := func(name string, preset v1alpha1.Preset, secret string) v1alpha1.Route {
		return v1alpha1.Route{Name: name, Preset: preset,
			Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: secret}}}
	}
	one := policy.Policy{Metadata: v1alpha1.ObjectMeta{Name: "same"},
		Spec: v1alpha1.PolicySpec{Routes: []v1alpha1.Route{route("first", v1alpha1.PresetGitHubToken, "s1")}}}
	two := policy.Policy{Metadata: v1alpha1.ObjectMeta{Name: "same"},
		Spec: v1alpha1.PolicySpec{Routes: []v1alpha1.Route{route("second", v1alpha1.PresetAnthropicAPIKey, "s2")}}}
	c := loadTarget(t, repoClaude(t), Options{Target: repoTarget(), Policies: &PolicySet{
		Docs: []policy.Policy{one, two}, Origins: []string{"brigfile a (inline)", "attachment to claude-code"}}})
	if c.routesErr != nil {
		t.Fatal(c.routesErr)
	}
	if c.routeOrigin["first"] != "brigfile a (inline)" || c.routeOrigin["second"] != "attachment to claude-code" {
		t.Errorf("route origins %v", c.routeOrigin)
	}
}

// Flag beats setting beats the target's override beats the profile, for the
// image, the memory and the vCPUs, and each value says where it came from.
func TestTargetPrecedenceAndOrigins(t *testing.T) {
	base := Origins{Image: "target dev", Mem: "target dev", CPUs: "profile claude (builtin:claude-code)",
		Network: "profile claude (builtin:claude-code)"}
	for _, tc := range []struct {
		name  string
		env   map[string]string
		o     Options
		image string
		mem   int
		cpus  int
		want  Origins
	}{
		{name: "base", image: "example.com/repo:1", mem: 1024, cpus: 1,
			want: Origins{Image: "target dev", Mem: "target dev", CPUs: "profile claude (builtin:claude-code)"}},
		{name: "env", env: map[string]string{"BRIG_IMAGE": "e:1", "BRIG_MEM": "2048", "BRIG_CPUS": "3"},
			image: "e:1", mem: 2048, cpus: 3,
			want: Origins{Image: "env BRIG_IMAGE", Mem: "env BRIG_MEM", CPUs: "env BRIG_CPUS"}},
		{name: "a setting that is not a number", env: map[string]string{"BRIG_MEM": "lots"},
			image: "example.com/repo:1", mem: 1024, cpus: 1,
			want: Origins{Image: "target dev", Mem: "target dev", CPUs: "profile claude (builtin:claude-code)"}},
		{name: "flag", env: map[string]string{"BRIG_IMAGE": "e:1", "BRIG_MEM": "2048", "BRIG_CPUS": "3"},
			o: Options{Image: "f:1", Mem: 4096, CPUs: 4}, image: "f:1", mem: 4096, cpus: 4,
			want: Origins{Image: "flag", Mem: "flag", CPUs: "flag"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateState(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			o := tc.o
			o.Target = &Target{Name: "dev", Base: base}
			c := loadTarget(t, repoClaude(t), o)
			got := c.origins
			got.Network, got.NetworkAsked = "", ""
			if c.Image != tc.image || c.Mem != tc.mem || c.CPUs != tc.cpus || got != tc.want {
				t.Errorf("%s %d %d from %+v, want %s %d %d from %+v", c.Image, c.Mem, c.CPUs, got,
					tc.image, tc.mem, tc.cpus, tc.want)
			}
		})
	}
}

// A target's run looks for no home an older release kept under the name of
// its profile. One there under ~/brig/claude-code is yours.
func TestATargetLooksForNoLegacyHome(t *testing.T) {
	isolateState(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "brig", "claude-code"), 0o700); err != nil {
		t.Fatal(err)
	}
	c := loadTarget(t, repoClaude(t), Options{Target: repoTarget()})
	if c.legacyHome != "" || strings.Contains(c.Workspace, filepath.Join("brig", "claude-code")) {
		t.Errorf("legacy home %q, workspace %q", c.legacyHome, c.Workspace)
	}
}
