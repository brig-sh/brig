package wrap

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/secret"
	"github.com/brig-sh/brig/internal/verify"
)

// readCountingStore lists what it holds and counts every Read, which is the
// decrypt a plan must never make. It answers a Read with the planted value, so
// a plan that reads it has the value in hand to leak.
type readCountingStore struct {
	list  []secret.Secret
	value string
	reads *int
}

func (s readCountingStore) Read(string) ([]byte, error) {
	*s.reads++
	return []byte(s.value), nil
}

func (s readCountingStore) List() ([]secret.Secret, error) { return s.list, nil }

// countReads puts a store in front of c that lists the secrets planProfile
// and the claude-code profile declare, answers a Read with a planted value,
// and counts every Read. The count is what a plan must leave at zero.
func countReads(c *Config) *int {
	reads := 0
	c.OpenStore = func() (creds.SecretReader, error) {
		return readCountingStore{
			list: []secret.Secret{{Name: "gh"}, {Name: "tok"},
				{Name: "claude-credentials"}, {Name: "gh-token"}},
			value: "PLANTED-SECRET-VALUE",
			reads: &reads,
		}, nil
	}
	return &reads
}

// planProfile delivers one stored secret as an environment variable, one as a
// file, and forwards one variable from the shell, so every way a credential
// reaches the guest is in the plan.
const planProfile = "secrets:\n  - gh\n  - tok\n" +
	"env:\n  - name: GH_TOKEN\n    ref: secrets.gh\n  - name: SHELL_TOK\n    ref: env.SHELL_TOK\n" +
	"volumes:\n  - kind: tmpfs\n    path: .config\n" +
	"files:\n  - ref: secrets.tok\n    path: .config/cred\n    mode: \"0600\"\n"

// The plan is the permission view a reader asks for before any secret is
// opened. A value seeded in the store, and one in the shell, appear in no field
// of either form, and the store is never read: listing it is what says a
// secret is there.
func TestPlanNeverReadsOrPrintsASecretValue(t *testing.T) {
	const planted = "PLANTED-SECRET-VALUE"
	const plantedEnv = "PLANTED-SHELL-VALUE"
	t.Setenv("SHELL_TOK", plantedEnv)

	c := bindingConfig(t, planProfile)
	c.Runtime = nil
	reads := 0
	c.OpenStore = func() (creds.SecretReader, error) {
		return readCountingStore{
			list:  []secret.Secret{{Name: "gh"}, {Name: "tok"}},
			value: planted,
			reads: &reads,
		}, nil
	}

	d := c.Plan()
	blob, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var text bytes.Buffer
	c.Out = &text
	c.PrintPlan(d)

	if reads != 0 {
		t.Errorf("the plan read %d secret values from the store, want none", reads)
	}
	for _, out := range []string{string(blob), text.String()} {
		if strings.Contains(out, planted) || strings.Contains(out, plantedEnv) {
			t.Fatalf("the plan printed a credential value:\n%s", out)
		}
	}

	// The names are there with their delivery, which is the point of the view.
	got := map[string]PlanCredential{}
	for _, cr := range d.Credentials {
		got[cr.Name] = cr
	}
	for name, want := range map[string]PlanCredential{
		"GH_TOKEN":     {Delivery: "env", Source: "secret", Secret: "gh", State: "resolved"},
		"SHELL_TOK":    {Delivery: "env", Source: "environment", State: "resolved"},
		".config/cred": {Delivery: "file", Source: "secret", Secret: "tok", State: "resolved"},
	} {
		g := got[name]
		if g.Delivery != want.Delivery || g.Source != want.Source || g.Secret != want.Secret || g.State != want.State {
			t.Errorf("%s = %+v, want %+v", name, g, want)
		}
	}
}

// A required secret missing from the store is named and marked unresolved,
// and the plan still answers.
func TestPlanMarksAMissingRequiredSecretUnresolved(t *testing.T) {
	c := bindingConfig(t, planProfile)
	reads := 0
	c.OpenStore = func() (creds.SecretReader, error) {
		return readCountingStore{reads: &reads}, nil
	}
	d := c.Plan()
	var found bool
	for _, cr := range d.Credentials {
		if cr.Name == "GH_TOKEN" {
			found = true
			if cr.State != "unresolved" || !cr.Required {
				t.Errorf("GH_TOKEN = %+v, want required and unresolved", cr)
			}
		}
	}
	if !found {
		t.Errorf("the missing credential is not named: %+v", d.Credentials)
	}
	if reads != 0 {
		t.Errorf("the plan read the store %d times", reads)
	}
}

// plan --json and info --json read the same Config, so they agree on the
// network, the image reference and the pull policy (#240).
//
// Through Load and the real hull adapter over a stub binary, because the
// network sentence is built from what Load resolved and what the runtime
// says: the vz fallback, a profile's isolated network the backend refuses,
// and a running sandbox whose next boot differs. A Config built by hand has
// none of that, and agreed with info while the command did not.
func TestPlanAgreesWithInfo(t *testing.T) {
	for _, tc := range []struct {
		name, hypervisor, body string
		opts                   Options
		running                bool
		want                   string
	}{
		{name: "a profile's isolated network on vz", hypervisor: "vz", body: "network: isolated\n",
			want: "the x profile sets network: isolated, and the vz backend cannot"},
		{name: "the vz default with no posture named", body: "",
			want: "vz does not support isolated networking"},
		{name: "a running shared sandbox whose next boot is offline", hypervisor: "vz",
			opts: Options{Network: "offline"}, running: true, want: "offline from its next boot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateState(t)
			t.Setenv("BRIG_HYPERVISOR", tc.hypervisor)
			home := t.TempDir()
			p := testProfile(t, tc.body)
			script := absentHull
			if tc.running {
				booted(loadThroughHull(t, p, Options{Workspace: home, Network: "shared"}, script))
				script = runningHull("brig-x")
			}
			tc.opts.Workspace = home
			c := loadThroughHull(t, p, tc.opts, script)
			c.Image, c.Pull = "ghcr.io/brig-sh/x:1", "always"

			plan, info := c.Plan(), c.InfoData(creds.Set{})
			if plan.Network != info.Network || plan.Image != info.Image {
				t.Errorf("plan says %q %+v, info says %q %+v", plan.Network, plan.Image, info.Network, info.Image)
			}
			if !strings.Contains(plan.Network, tc.want) {
				t.Errorf("network = %q, want it to say %q", plan.Network, tc.want)
			}
		})
	}
}

// absentHull is a hull that holds no sandbox: ps lists nothing, and inspect
// answers the way hull does for a name it does not have.
const absentHull = "#!/bin/sh\ncase \"$1\" in\n  inspect) echo \"instance not found: $2\" >&2; exit 1 ;;\nesac\nexit 0\n"

// runningHull is a hull whose ps lists one sandbox, running.
func runningHull(name string) string {
	return "#!/bin/sh\ncase \"$1\" in\n  ps) echo \"" + name + " running\" ;;\n" +
		"  inspect) echo \"instance not found: $2\" >&2; exit 1 ;;\nesac\nexit 0\n"
}

// loadThroughHull resolves p the way a command does, with the real hull
// adapter over a stub binary running script. Only the binary is fake, so
// what Load and the plan ask the runtime is what they ask on a Mac.
func loadThroughHull(t *testing.T, p profile.Profile, o Options, script string) *Config {
	t.Helper()
	t.Setenv("BRIG_RUNTIME", "hull")
	bin := filepath.Join(t.TempDir(), "hull")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIG_RUNTIME_BIN", bin)
	rt, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	c, err := Load(p, o, rt)
	if err != nil {
		t.Fatal(err)
	}
	c.Out, c.Err, c.Progress = io.Discard, io.Discard, io.Discard
	return c
}

// The guest home is named with its host directory, its guest path and its
// mode, and the project the same way when the run has one.
func TestPlanStatesTheHomeAndTheProject(t *testing.T) {
	c := bindingConfig(t, "")
	d := c.Plan()
	if d.Home.Host != c.Workspace || d.Home.Guest != c.Profile.GuestHome || d.Home.Mode != "read-write" {
		t.Errorf("home = %+v", d.Home)
	}
	if d.Project != nil {
		t.Errorf("a run with no project planned one: %+v", d.Project)
	}
	c.Project, c.ProjectReal, c.GuestProject = "/p", "/real/p", "/work/p"
	d = c.Plan()
	if d.Project == nil || d.Project.Host != "/real/p" || d.Project.Guest != "/work/p" || d.Project.Mode != "read-write" {
		t.Errorf("project = %+v", d.Project)
	}
}

// Every bound policy is listed, and a field of its own says when none binds,
// so a reader does not have to infer it from an empty list.
func TestPlanSaysWhenNoPolicyBinds(t *testing.T) {
	c := bindingConfig(t, "")
	d := c.Plan()
	if !d.NoPolicy || d.Policies == nil || len(d.Policies) != 0 {
		t.Errorf("no policy: noPolicy %v, policies %#v", d.NoPolicy, d.Policies)
	}
	c.Policies = []string{"locked", "base"}
	c.Egress = policy.Egress{Default: "deny", Allow: []policy.Rule{{Host: "b.example"}, {Host: "a.example"}}}
	d = c.Plan()
	if d.NoPolicy || strings.Join(d.Policies, ",") != "base,locked" {
		t.Errorf("bound: noPolicy %v, policies %v", d.NoPolicy, d.Policies)
	}
	if d.Egress == nil || d.Egress.Default != "deny" || len(d.Egress.Allow) != 2 || d.Egress.Allow[0].Host != "a.example" {
		t.Errorf("egress = %+v", d.Egress)
	}
}

// The digest is the same for the same inputs, whatever order the lists came
// in, and moves when any permission moves.
func TestPlanDigestIsStableAndMovesWithPermissions(t *testing.T) {
	base := func() *Config {
		c := bindingConfig(t, planProfile)
		c.Workspace = "/home-dir"
		c.Policies = []string{"a", "b"}
		c.Egress = policy.Egress{Default: "deny", Allow: []policy.Rule{{Host: "x.example"}, {Host: "y.example"}}}
		c.Mem, c.CPUs = 2048, 2
		reads := 0
		c.OpenStore = func() (creds.SecretReader, error) {
			return readCountingStore{list: []secret.Secret{{Name: "gh"}, {Name: "tok"}}, reads: &reads}, nil
		}
		return c
	}
	want := base().Plan().Digest
	if !strings.HasPrefix(want, "sha256:") {
		t.Fatalf("digest %q is not a sha256", want)
	}
	if again := base().Plan().Digest; again != want {
		t.Errorf("the same inputs gave %s then %s", want, again)
	}
	reordered := base()
	reordered.Policies = []string{"b", "a"}
	reordered.Egress.Allow = []policy.Rule{{Host: "y.example"}, {Host: "x.example"}}
	if got := reordered.Plan().Digest; got != want {
		t.Errorf("reordering the lists moved the digest: %s, want %s", got, want)
	}

	for name, change := range map[string]func(c *Config){
		"home":    func(c *Config) { c.Workspace = "/other" },
		"network": func(c *Config) { c.Network = NetOffline },
		"policy":  func(c *Config) { c.Policies = []string{"a"} },
		"egress":  func(c *Config) { c.Egress.Allow = c.Egress.Allow[:1] },
		"mem":     func(c *Config) { c.Mem = 4096 },
		"cpus":    func(c *Config) { c.CPUs = 4 },
		"image":   func(c *Config) { c.Image = "other:1" },
		"credential": func(c *Config) {
			reads := 0
			c.OpenStore = func() (creds.SecretReader, error) {
				return readCountingStore{list: []secret.Secret{{Name: "tok"}}, reads: &reads}, nil
			}
		},
	} {
		c := base()
		change(c)
		if got := c.Plan().Digest; got == want {
			t.Errorf("changing the %s left the digest at %s", name, got)
		}
	}
}

// With no runtime on PATH the plan still answers, and marks the rows that
// needed one.
func TestPlanWithoutARuntimeMarksTheRuntimeRows(t *testing.T) {
	c := bindingConfig(t, "")
	c.Runtime = nil
	d := c.Plan()
	if d.Runtime.Available {
		t.Error("the runtime is marked available with none on PATH")
	}
	if d.Isolation == "" || d.Profile == "" || d.Sandbox == "" {
		t.Errorf("the plan left rows empty: %+v", d)
	}
}

// A required secret the chain passed over has a row with no delivery. When the
// store has it, the row must not read like one the guest receives: the value
// reaches nowhere, and "from the secret x" says it does. A delivered secret
// still reads that way.
func TestPlanDoesNotDeliverARequiredSecretNoBindingUses(t *testing.T) {
	passedOver := credentialText(PlanCredential{Name: "x", Delivery: creds.DeliveryNone,
		Source: creds.SourceSecret, Secret: "x", Required: true, State: creds.StateResolved})
	if strings.Contains(passedOver, "from the secret") {
		t.Errorf("a secret no binding delivers reads as delivered: %q", passedOver)
	}
	if !strings.Contains(passedOver, "no binding delivers it") || !strings.Contains(passedOver, "(required)") {
		t.Errorf("row = %q, want it to say no binding delivers the required secret", passedOver)
	}

	delivered := credentialText(PlanCredential{Name: "GH_TOKEN", Delivery: creds.DeliveryEnv,
		Source: creds.SourceSecret, Secret: "gh", Required: true, State: creds.StateResolved})
	if want := "GH_TOKEN (env) from the secret gh"; delivered != want {
		t.Errorf("delivered row = %q, want %q", delivered, want)
	}
}

// A plan of a run `brig run` refuses says so, with the reason the run prints,
// rather than showing a policy or a network the backend will not give it.
// EnsureRunning is asked the same question afterwards, so the test holds the
// plan to the run rather than to a copy of its wording.
//
// The profile declares secrets and the store holds them, and the plan reads
// none. These are the cases where a runtime is present, so they are the ones
// that reach backendRefusal.
func TestPlanReportsWhatTheBackendRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, hypervisor, body string
		opts                   Options
		want                   string
	}{
		{name: "a policy on vz", hypervisor: "vz", body: "policy:\n  - no-net\n",
			want: "cannot enforce the egress policy"},
		{name: "a policy on qemu", hypervisor: "qemu", body: "policy:\n  - no-net\n",
			want: "cannot enforce the egress policy"},
		{name: "a profile's isolated network on vz", hypervisor: "vz", body: "network: isolated\n",
			want: "the x profile sets network: isolated"},
		{name: "a profile's isolated network on qemu", hypervisor: "qemu", body: "network: isolated\n",
			want: "the x profile sets network: isolated"},
		{name: "--network isolated on vz", hypervisor: "vz", opts: Options{Network: "isolated"},
			want: "--network isolated gives the sandbox a network of its own"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateState(t)
			policies := t.TempDir()
			writeTestPolicy(t, policies, "no-net")
			t.Setenv("BRIG_POLICY_DIR", policies)
			t.Setenv("BRIG_HYPERVISOR", tc.hypervisor)
			tc.opts.Workspace = t.TempDir()
			c := loadThroughHull(t, testProfile(t, planProfile+tc.body), tc.opts, absentHull)
			reads := countReads(c)

			d := c.Plan()
			if *reads != 0 {
				t.Errorf("the plan read %d secret values from the store, want none", *reads)
			}
			if d.Refused == nil {
				t.Fatalf("the plan does not say the run is refused: network %q, policies %v", d.Network, d.Policies)
			}
			if !strings.Contains(d.Refused.Reason, tc.want) {
				t.Errorf("refused = %q, want it to say %q", d.Refused.Reason, tc.want)
			}
			c.Verify = verify.Off
			err := c.EnsureRunning(creds.Set{})
			if err == nil {
				t.Fatal("the run was not refused, so the case tests nothing")
			}
			if d.Refused.Reason != err.Error() {
				t.Errorf("the plan says\n  %s\nand the run says\n  %s", d.Refused.Reason, err)
			}
		})
	}
}

// The other side: a run the backend takes is not marked refused. hvi enforces
// a policy and gives a sandbox its own network, and vz runs a shared one.
func TestPlanDoesNotRefuseARunTheBackendTakes(t *testing.T) {
	for _, tc := range []struct{ name, hypervisor, body string }{
		{"a policy on hvi", "hvi", "policy:\n  - no-net\n"},
		{"a profile's isolated network on hvi", "hvi", "network: isolated\n"},
		{"the shared default on vz", "vz", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateState(t)
			policies := t.TempDir()
			writeTestPolicy(t, policies, "no-net")
			t.Setenv("BRIG_POLICY_DIR", policies)
			t.Setenv("BRIG_HYPERVISOR", tc.hypervisor)
			c := loadThroughHull(t, testProfile(t, planProfile+tc.body), Options{Workspace: t.TempDir()}, absentHull)
			c.MacOSVersion = func() string { return "15.4" }
			reads := countReads(c)
			if d := c.Plan(); d.Refused != nil {
				t.Errorf("a run the backend takes is marked refused: %s", d.Refused.Reason)
			}
			if *reads != 0 {
				t.Errorf("the plan read %d secret values from the store, want none", *reads)
			}
		})
	}
}

// hvi needs macOS 15. The run refuses it on 14 before anything starts, and
// the plan says so with the same sentence.
func TestPlanReportsAHypervisorTheHostCannotBoot(t *testing.T) {
	isolateState(t)
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	c := loadThroughHull(t, testProfile(t, planProfile), Options{Workspace: t.TempDir()}, absentHull)
	c.MacOSVersion = func() string { return "14.5" }
	reads := countReads(c)
	d := c.Plan()
	if *reads != 0 {
		t.Errorf("the plan read %d secret values from the store, want none", *reads)
	}
	if d.Refused == nil || !strings.Contains(d.Refused.Reason, "needs macOS 15 or newer") {
		t.Fatalf("refused = %+v, want the macOS 15 floor", d.Refused)
	}
	if err := c.EnsureRunning(creds.Set{}); err == nil || err.Error() != d.Refused.Reason {
		t.Errorf("the plan says %q and the run says %v", d.Refused.Reason, err)
	}
}

// A remembered project reached through a link stops the run (see
// TestARememberedProjectReachedThroughALinkRefusesTheRun). The plan says the
// run is refused, and its PROJECT row does not read as a run going on
// without the project.
func TestPlanReportsARememberedProjectReachedThroughALink(t *testing.T) {
	isolateState(t)
	home := t.TempDir()
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	mustMkdir(t, target)
	project := filepath.Join(dir, "myproject")
	mustMkdir(t, project)
	mustLoad(t, Options{Workspace: home, Project: project}).rememberSession()
	if err := os.Remove(project); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, project); err != nil {
		t.Fatal(err)
	}

	c := mustLoad(t, Options{})
	reads := countReads(c)
	d := c.Plan()
	if *reads != 0 {
		t.Errorf("the plan read %d secret values from the store, want none", *reads)
	}
	if d.Refused == nil || !errors.Is(c.sessionRefusal(), errPlantedSymlink) {
		t.Fatalf("the plan does not say the run is refused: %+v", d.Refused)
	}
	if d.Refused.Reason != d.ProjectRefused || !strings.Contains(d.Refused.Reason, "remembered from an earlier run") {
		t.Errorf("refused = %q, projectRefused = %q", d.Refused.Reason, d.ProjectRefused)
	}
	if d.Project != nil {
		t.Errorf("the plan mounts a refused project: %+v", d.Project)
	}

	var text bytes.Buffer
	c.Out = &text
	c.PrintPlan(d)
	out := text.String()
	if !strings.HasPrefix(out, "REFUSED") {
		t.Errorf("the text does not open on the refusal:\n%s", out)
	}
	if strings.Contains(out, "not mounted") {
		t.Errorf("the text reads as a run that goes on without its project:\n%s", out)
	}
	if !strings.Contains(out, "PROJECT      "+project+" (refused, see REFUSED)") {
		t.Errorf("the PROJECT row does not name the refused directory:\n%s", out)
	}

	c.Runtime, c.Verify = &livenessRuntime{workspace: c.Workspace}, verify.Off
	if err := c.EnsureRunning(creds.Set{}); err == nil || err.Error() != d.Refused.Reason {
		t.Errorf("the plan says %q and the run says %v", d.Refused.Reason, err)
	}
}

// A sandbox the runtime holds with no posture on record, whose network the
// runtime will not say: info reports unknown and the run refuses it. The plan
// says unknown too, and that the run is refused, rather than promising the
// posture of a fresh sandbox.
func TestPlanReportsALegacyNetworkItCannotRead(t *testing.T) {
	isolateState(t)
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	broken := "#!/bin/sh\ncase \"$1\" in\n  inspect) echo \"permission denied\" >&2; exit 1 ;;\nesac\nexit 0\n"
	c := loadThroughHull(t, testProfile(t, planProfile+"network: isolated\n"), Options{Workspace: t.TempDir()}, broken)
	c.MacOSVersion = func() string { return "15.4" }
	reads := countReads(c)

	d := c.Plan()
	if *reads != 0 {
		t.Errorf("the plan read %d secret values from the store, want none", *reads)
	}
	if d.Posture != "unknown" || !strings.HasPrefix(d.Network, "unknown (") {
		t.Errorf("posture %q, network %q, want unknown", d.Posture, d.Network)
	}
	if d.Network != c.InfoData(creds.Set{}).Network {
		t.Errorf("plan and info disagree on the network")
	}
	if d.Refused == nil || !strings.Contains(d.Refused.Reason, "cannot determine the network of sandbox brig-x") {
		t.Fatalf("refused = %+v, want the unknown-network refusal", d.Refused)
	}
	c.Verify = verify.Off
	if err := c.EnsureRunning(creds.Set{}); err == nil || err.Error() != d.Refused.Reason {
		t.Errorf("the plan says %q and the run says %v", d.Refused.Reason, err)
	}

	// Naming a posture is the documented way on, and the run takes it.
	explicit := loadThroughHull(t, testProfile(t, "network: isolated\n"),
		Options{Workspace: t.TempDir(), Network: "isolated"}, broken)
	explicit.MacOSVersion = func() string { return "15.4" }
	if d := explicit.Plan(); d.Refused != nil || d.Posture != "isolated" {
		t.Errorf("with --network isolated: refused %+v, posture %q", d.Refused, d.Posture)
	}
}

// --skills copies the host's own skills and plugins into the guest home. That
// is host data the guest receives, so the plan names each directory and where
// it lands, and the digest moves with it.
func TestPlanNamesTheSkillsItCopies(t *testing.T) {
	host := t.TempDir()
	t.Setenv("HOME", host)
	mustMkdir(t, filepath.Join(host, ".claude", "skills", "s1"))
	mustMkdir(t, filepath.Join(host, ".claude", "plugins", "p1"))
	p := testProfile(t, "hostConfigDir: ~/.claude\nprojectPaths: [skills, plugins]\n")

	plain := bindingConfig(t, "")
	plain.Profile, plain.Workspace = p, "/home-dir"
	skills := bindingConfig(t, "")
	skills.Profile, skills.Workspace = p, "/home-dir"
	skills.HostConfig = hostProjections(p, true)

	d := plain.Plan()
	if d.Skills == nil || len(d.Skills) != 0 {
		t.Errorf("a run without --skills plans copies: %#v", d.Skills)
	}
	got := skills.Plan()
	want := []PlanMount{
		{Host: filepath.Join(host, ".claude", "plugins"), Guest: "/home/x/.claude/plugins", Mode: "copy"},
		{Host: filepath.Join(host, ".claude", "skills"), Guest: "/home/x/.claude/skills", Mode: "copy"},
	}
	if len(got.Skills) != len(want) || got.Skills[0] != want[0] || got.Skills[1] != want[1] {
		t.Errorf("skills = %+v, want %+v", got.Skills, want)
	}
	if got.Digest == d.Digest {
		t.Errorf("--skills left the digest at %s", d.Digest)
	}
	var text bytes.Buffer
	skills.Out = &text
	skills.PrintPlan(got)
	if !strings.Contains(text.String(), "SKILLS       "+want[0].Host+" (copied to "+want[0].Guest+")") {
		t.Errorf("the text has no SKILLS row:\n%s", text.String())
	}
}

// With BRIG_ENV_ARGV=1 the plan names what the run would put on the runtime's
// command line, the same names info reads off the set BuildEnv builds. A value
// from the store is never among them, and a literal the denylist drops is not
// either.
func TestPlanArgvExposedAgreesWithBuildEnv(t *testing.T) {
	t.Setenv("BRIG_ENV_ARGV", "1")
	t.Setenv("SHELL_TOK", "from-the-shell")
	c := bindingConfig(t, "secrets:\n  - gh\n"+
		"env:\n  - name: GH_TOKEN\n    ref: secrets.gh\n  - name: SHELL_TOK\n    ref: env.SHELL_TOK\n"+
		"  - name: MODE\n    value: fast\n")
	// Validate refuses a denied name bound in a profile file, so a denied
	// literal reaches Bind only from a profile built in Go. Bind still drops
	// it, and the plan has to as well.
	c.Profile.Deny = append(c.Profile.Deny, "DENIED_LIT")
	c.Env = append(c.Env, profile.EnvBinding{Name: "DENIED_LIT", Value: "x"})
	c.GitIdentity, c.GitName, c.GitEmail = true, "A Person", "a@example.com"
	c.GitConfig, c.GitUser = true, "someone"
	c.OpenStore = func() (creds.SecretReader, error) { return fakeSecretStore{"gh": "ghp_x"}, nil }

	planned := c.Plan().ArgvExposed
	set, err := c.BuildEnv()
	if err != nil {
		t.Fatal(err)
	}
	run := runtime.ArgvExposed(set.Vars)
	sort.Strings(run)
	if strings.Join(planned, " ") != strings.Join(run, " ") {
		t.Errorf("the plan names %v, the run puts %v on the command line", planned, run)
	}
	for _, name := range []string{"GH_TOKEN", "DENIED_LIT"} {
		for _, got := range planned {
			if got == name {
				t.Errorf("the plan names %s, which the run keeps off the command line", name)
			}
		}
	}
	if len(planned) == 0 {
		t.Fatal("the plan names nothing, so the case tests nothing")
	}

	t.Setenv("BRIG_ENV_ARGV", "")
	if got := c.Plan().ArgvExposed; len(got) != 0 {
		t.Errorf("with BRIG_ENV_ARGV off the plan still names %v", got)
	}
}

// fakeSecretStore lists and reads what it holds, for the cases where the run
// path reads the store too.
type fakeSecretStore map[string]string

func (f fakeSecretStore) Read(name string) ([]byte, error) {
	v, ok := f[name]
	if !ok {
		return nil, secret.ErrNotFound
	}
	return []byte(v), nil
}

func (f fakeSecretStore) List() ([]secret.Secret, error) {
	var out []secret.Secret
	for name := range f {
		out = append(out, secret.Secret{Name: name})
	}
	return out, nil
}

// A withheld credential's reason runs to several lines: the refusal, then
// the advice. Each line takes a row of its own, so the advice stays in the
// value column instead of starting at the left margin, and "(required)"
// stays beside the name.
func TestPlanTextKeepsAReasonInItsColumn(t *testing.T) {
	t.Setenv("TOK", "op://vault/item/field")
	t.Setenv("OTHER", "plain")
	c := bindingConfig(t, "env:\n  - name: TOK\n    ref: env.TOK\n  - name: OTHER\n    ref: env.OTHER\n")
	c.Runtime = nil
	d := c.Plan()
	var text bytes.Buffer
	c.Out = &text
	c.PrintPlan(d)
	out := text.String()

	var col int
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "DIGEST") {
			col = len(line) - len(strings.TrimLeft(strings.TrimPrefix(line, "DIGEST"), " "))
		}
	}
	if col == 0 {
		t.Fatalf("no DIGEST row:\n%s", out)
	}
	advice := 0
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		label := strings.TrimRight(line[:min(col, len(line))], " ")
		if strings.Trim(label, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" || len(line) <= col || line[col-1] != ' ' {
			t.Errorf("a row leaves the value column: %q\n%s", line, out)
		}
		if strings.Contains(line, "BRIG_ALLOW_REFS=1") {
			advice++
		}
	}
	if advice != 1 {
		t.Errorf("the withheld credential's advice is not in the plan:\n%s", out)
	}

	required := credentialText(PlanCredential{Name: "GH_TOKEN", Delivery: creds.DeliveryEnv,
		Source: creds.SourceSecret, Secret: "gh", Required: true, State: creds.StateWithheld,
		Reason: "not forwarding GH_TOKEN: it is on the x denylist\n  → to forward it anyway:  BRIG_ALLOW_DENIED=1"})
	if first, _, _ := strings.Cut(required, "\n"); !strings.HasSuffix(first, "(required)") {
		t.Errorf("(required) is not beside the name: %q", required)
	}
}
