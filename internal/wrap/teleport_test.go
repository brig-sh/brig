package wrap

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/verify"
)

// guestRuntime records what a boot asked of the runtime and every command
// brig ran in the guest, so a case can assert on what a teleportable session
// does inside the sandbox as well as what it handed the runtime. Anything it
// was not written for panics through the embedded nil interface.
type guestRuntime struct {
	runtime.Runtime
	kind    string
	running bool
	// exists is what Exists answers: whether an instance holds the name,
	// stopped or running. The boot asks before it clears a stopped one.
	exists bool
	// ownsDisk and inspectErr are what hull's record of the boot says, the
	// question asked of a sandbox the session index has no entry for.
	ownsDisk   bool
	inspectErr error
	spec       runtime.RunSpec
	boots      int
	stops      int
	removes    int
	execs      [][]string
	feeds      []fed
}

func (r *guestRuntime) Exists(string) (bool, error) { return r.exists, nil }

// SandboxNetwork answers for an instance that exists the way hull does for a
// sandbox booted on its own network, which is what every profile here asks
// for. Without it an Exister that finds an unrecorded sandbox leaves its
// posture unknown, and the run stops on that before any storage question.
func (r *guestRuntime) SandboxNetwork(string) (string, error) {
	if !r.exists {
		return "", nil
	}
	return "isolated", nil
}
func (r *guestRuntime) OwnsItsDisk(string) (bool, error) {
	return r.ownsDisk, r.inspectErr
}

// unaskableRuntime is a runtime that can say nothing about a stopped instance:
// only the Runtime methods of the one it wraps reach the caller, so it is
// neither an Exister nor a StorageInspector.
type unaskableRuntime struct{ runtime.Runtime }

type fed struct {
	cmd   []string
	stdin string
}

func (r *guestRuntime) Kind() string {
	if r.kind == "" {
		return "hull"
	}
	return r.kind
}
func (r *guestRuntime) Bin() string                  { return r.Kind() }
func (r *guestRuntime) Running(string) (bool, error) { return r.running, nil }
func (r *guestRuntime) Stop(string) error            { r.stops++; return nil }
func (r *guestRuntime) Remove(string) error          { r.removes++; return nil }
func (r *guestRuntime) Run(spec runtime.RunSpec) error {
	r.spec = spec
	r.boots++
	return nil
}
func (r *guestRuntime) Probe(runtime.ExecSpec) bool { return true }
func (r *guestRuntime) Isolation(hv string) runtime.Isolation {
	return runtime.Isolation{Boundary: runtime.BoundaryVM, Detail: r.Kind() + ", " + hv + " backend"}
}
func (r *guestRuntime) LogsHint(name string) string { return "hull logs " + name }
func (r *guestRuntime) Output(spec runtime.ExecSpec) (string, error) {
	r.execs = append(r.execs, spec.Cmd)
	return "", nil
}
func (r *guestRuntime) Feed(spec runtime.ExecSpec) error {
	b, err := io.ReadAll(spec.Stdin)
	if err != nil {
		return err
	}
	r.feeds = append(r.feeds, fed{cmd: spec.Cmd, stdin: string(b)})
	return nil
}

// teleportProfile is a profile that asks for teleportable sessions, with an
// onboarding seed so a case can see where it lands.
func teleportProfile(t *testing.T) profile.Profile {
	t.Helper()
	p, err := profile.Parse([]byte(`name: tp
image: img
guestHome: /home/tp
binary: tp
mem: 1
cpus: 1
hypervisor: hvi
teleportable: true
onboarding:
  file: .tp/state.json
  seed:
    hasCompletedOnboarding: true
`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// ordinaryProfile is teleportProfile without the field: the same agent on
// the same backend, with its guest home shared in from the host. It mounts
// no volumes, so a boot needs nothing from the guest's mount table.
func ordinaryProfile(t *testing.T) profile.Profile {
	t.Helper()
	p, err := profile.Parse([]byte(`name: tp
image: img
guestHome: /home/tp
binary: tp
mem: 1
cpus: 1
hypervisor: hvi
network: isolated
`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// teleportState is isolateState with HOME moved too, so a case can assert
// that nothing was made under it: the default guest home of an ordinary
// session lives under the state directory, and the legacy one under HOME.
func teleportState(t *testing.T) {
	t.Helper()
	isolateState(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BRIG_VERIFY", "off")
	for _, key := range []string{"BRIG_HYPERVISOR", "BRIG_ROOTFS_TYPE", "BRIG_SKILLS", "BRIG_GIT_CONFIG"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}

func loadTeleport(t *testing.T, p profile.Profile, o Options, rt runtime.Runtime) *Config {
	t.Helper()
	c, err := Load(p, o, rt)
	if err != nil {
		t.Fatal(err)
	}
	c.Verify = verify.Off
	c.Out, c.Err, c.Progress = &bytes.Buffer{}, &bytes.Buffer{}, &bytes.Buffer{}
	return c
}

// The flag and the profile field each make a session teleportable, and a
// teleportable session has no home on the host: the workspace is empty, the
// agent starts in its guest home, and --skills has nothing to copy into.
func TestLoadTeleportableHasNoHostHome(t *testing.T) {
	teleportState(t)
	claude, _ := profile.Lookup("claude-code")
	for _, c := range []struct {
		what   string
		p      profile.Profile
		o      Options
		source string
	}{
		{"--teleportable", claude, Options{Teleportable: true}, "--teleportable"},
		{"the profile field", teleportProfile(t), Options{}, "teleportable: true"},
	} {
		cfg := loadTeleport(t, c.p, c.o, nil)
		if !cfg.Teleportable {
			t.Errorf("%s: the session is not teleportable", c.what)
		}
		if cfg.Workspace != "" || cfg.EphemeralHome {
			t.Errorf("%s: a teleportable session resolved a host home %q (ephemeral %v)",
				c.what, cfg.Workspace, cfg.EphemeralHome)
		}
		if cfg.GuestCwd != c.p.GuestHome {
			t.Errorf("%s: GuestCwd = %q, want the guest home %q", c.what, cfg.GuestCwd, c.p.GuestHome)
		}
		if !strings.Contains(cfg.teleportSource, c.source) {
			t.Errorf("%s: source = %q, want it to name %q", c.what, cfg.teleportSource, c.source)
		}
		if err := cfg.sessionRefusal(); err != nil {
			t.Errorf("%s: a plain teleportable run was refused: %v", c.what, err)
		}
	}
	// The ordinary run is unchanged by the field existing.
	if cfg := loadTeleport(t, claude, Options{}, nil); cfg.Teleportable || cfg.Workspace == "" {
		t.Errorf("an ordinary run came out teleportable %v with home %q", cfg.Teleportable, cfg.Workspace)
	}
}

// Every way of handing a teleportable session a host directory is refused,
// and each refusal names the thing to drop. Held for the boot rather than
// returned from Load, so stop, rm and info still reach the session.
func TestLoadRefusesWhatATeleportableSessionCannotHave(t *testing.T) {
	project := t.TempDir()
	for _, c := range []struct {
		what string
		o    Options
		env  map[string]string
		want []string
	}{
		{"--home", Options{Teleportable: true, Workspace: t.TempDir()}, nil,
			[]string{"--home", "guest home on the host"}},
		{"BRIG_WORKSPACE", Options{Teleportable: true}, map[string]string{"BRIG_WORKSPACE": t.TempDir()},
			[]string{"BRIG_WORKSPACE"}},
		{"a project", Options{Teleportable: true, Project: project}, nil,
			[]string{"mounts no project", project}},
		{"--skills", Options{Teleportable: true, Skills: true}, nil, []string{"--skills"}},
		{"BRIG_SKILLS", Options{Teleportable: true}, map[string]string{"BRIG_SKILLS": "1"},
			[]string{"BRIG_SKILLS"}},
		{"BRIG_GIT_CONFIG", Options{Teleportable: true}, map[string]string{"BRIG_GIT_CONFIG": "1"},
			[]string{"BRIG_GIT_CONFIG", "git helper"}},
		{"a shared root", Options{Teleportable: true}, map[string]string{"BRIG_ROOTFS_TYPE": "virtiofs"},
			[]string{"BRIG_ROOTFS_TYPE", "block root"}},
	} {
		t.Run(c.what, func(t *testing.T) {
			teleportState(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			claude, _ := profile.Lookup("claude-code")
			cfg, err := Load(claude, c.o, nil)
			if err != nil {
				t.Fatalf("Load refused, which would also refuse stop and rm: %v", err)
			}
			refusal := cfg.sessionRefusal()
			if refusal == nil {
				t.Fatal("the run was not refused")
			}
			for _, w := range c.want {
				if !strings.Contains(refusal.Error(), w) {
					t.Errorf("the refusal does not say %q: %v", w, refusal)
				}
			}
			// Nothing was mounted on the way to the refusal.
			if cfg.Project != "" || cfg.Workspace != "" {
				t.Errorf("a refused session still resolved project %q, home %q", cfg.Project, cfg.Workspace)
			}
		})
	}
}

// The index keeps the property. A later verb on the ref names no flag, and
// it is still teleportable; a later verb that hands it a host home is told it
// cannot switch back without rm.
func TestATeleportableSessionStaysTeleportable(t *testing.T) {
	teleportState(t)
	claude, _ := profile.Lookup("claude-code")
	first := loadTeleport(t, claude, Options{Name: "x", Teleportable: true}, nil)
	first.rememberSession()
	if entry := readSessionIndex()["claude-code@x"]; !entry.Teleportable || entry.Home != "" {
		t.Fatalf("the index recorded %+v, want teleportable with no home", entry)
	}

	later := loadTeleport(t, claude, Options{Name: "x"}, nil)
	if !later.Teleportable || later.Workspace != "" {
		t.Fatalf("a flagless verb lost the property: teleportable %v, home %q", later.Teleportable, later.Workspace)
	}
	if !strings.Contains(later.teleportSource, "started teleportable") {
		t.Errorf("the source does not say it came from the session: %q", later.teleportSource)
	}

	back := loadTeleport(t, claude, Options{Name: "x", Workspace: t.TempDir()}, nil)
	err := back.sessionRefusal()
	if err == nil || !strings.Contains(err.Error(), "cannot be switched back") ||
		!strings.Contains(err.Error(), "brig rm claude-code@x") {
		t.Errorf("a host home on a teleportable session was not refused with the way out: %v", err)
	}
}

// A session that already has a home on the host cannot be made teleportable
// in place: one of the two homes would be lost. Refused, naming rm.
func TestAnOrdinarySessionCannotBecomeTeleportable(t *testing.T) {
	teleportState(t)
	claude, _ := profile.Lookup("claude-code")
	ordinary := loadTeleport(t, claude, Options{Name: "y"}, nil)
	ordinary.rememberSession()

	cfg := loadTeleport(t, claude, Options{Name: "y", Teleportable: true}, nil)
	err := cfg.sessionRefusal()
	if err == nil {
		t.Fatal("an ordinary session was switched to teleportable")
	}
	for _, w := range []string{ordinary.Workspace, "One sandbox cannot be both", "brig rm claude-code@y"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the refusal does not say %q: %v", w, err)
		}
	}
}

// An older brig rewrites sessions.json with the fields it knows, which
// strips Teleportable and leaves the empty home. That entry is still a
// teleportable session's: --teleportable must not be refused as a switch from
// a host home that does not exist, and a flagless verb must not read it as
// ordinary. Either would send the user to `brig rm`, which deletes the disk.
func TestATeleportableEntryAnOlderBrigRewroteStaysTeleportable(t *testing.T) {
	teleportState(t)
	path, err := indexPath(sessionIndexName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	stripped := `{"claude-code@x": {"home": "", "sandbox": "brig-claude-code-x"}}`
	if err := os.WriteFile(path, []byte(stripped), 0o600); err != nil {
		t.Fatal(err)
	}
	// The posture is kept in a file no older release rewrites, so it
	// survives the rewrite that stripped the field.
	if err := runtime.RecordBootedNet("brig-claude-code-x", "isolated"); err != nil {
		t.Fatal(err)
	}
	claude, _ := profile.Lookup("claude-code")

	asked := loadTeleport(t, claude, Options{Name: "x", Teleportable: true}, nil)
	if !asked.Teleportable {
		t.Fatal("--teleportable on the stripped entry is not teleportable")
	}
	if err := asked.sessionRefusal(); err != nil {
		t.Errorf("--teleportable on the stripped entry was refused: %v", err)
	}
	flagless := loadTeleport(t, claude, Options{Name: "x"}, nil)
	if !flagless.Teleportable || flagless.Workspace != "" {
		t.Errorf("a flagless verb read the stripped entry as ordinary: teleportable %v, home %q",
			flagless.Teleportable, flagless.Workspace)
	}
	if err := flagless.sessionRefusal(); err != nil {
		t.Errorf("a flagless verb on the stripped entry was refused: %v", err)
	}
	if !TeleportableSandbox("brig-claude-code-x") {
		t.Error("ls reads the stripped entry as an ordinary sandbox")
	}
	// The next run that records the session puts the field back.
	flagless.rememberSession()
	if entry := readSessionIndex()["claude-code@x"]; !entry.Teleportable {
		t.Errorf("the rewrite did not restore the field: %+v", entry)
	}
}

// The boot itself: block root, hvi, no share of any kind, and nothing made
// on the host -- no home under the state directory, no marker in the
// directory brig ran from. The guest home is prepared inside the guest, and
// the onboarding seed is fed there on stdin.
func TestEnsureRunningBootsATeleportableSessionWithNoShare(t *testing.T) {
	teleportState(t)
	cwd := t.TempDir()
	t.Chdir(cwd)
	rt := &guestRuntime{}
	c := loadTeleport(t, teleportProfile(t), Options{}, rt)

	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatalf("a teleportable boot was refused: %v", err)
	}
	if rt.boots != 1 {
		t.Fatalf("booted %d times, want 1", rt.boots)
	}
	if len(rt.spec.Shares) != 0 {
		t.Errorf("a teleportable session shared host directories in: %+v", rt.spec.Shares)
	}
	if rt.spec.RootfsType != "block" || rt.spec.Hypervisor != "hvi" {
		t.Errorf("booted rootfs %q on %q, want block on hvi", rt.spec.RootfsType, rt.spec.Hypervisor)
	}
	home, err := defaultWorkspace(c.VMName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a host home was made at %s (%v)", home, err)
	}
	if entries, _ := os.ReadDir(cwd); len(entries) != 0 {
		t.Errorf("brig wrote into the directory it ran from: %v", entries)
	}

	made := false
	for _, argv := range rt.execs {
		if len(argv) > 4 && argv[0] == "sh" && argv[3] == "sh" && argv[4] == "/home/tp" {
			made = true
		}
	}
	if !made {
		t.Errorf("the guest home was not made inside the guest: %q", rt.execs)
	}
	if len(rt.feeds) != 1 || rt.feeds[0].cmd[4] != "/home/tp" || rt.feeds[0].cmd[5] != ".tp/state.json" ||
		!strings.Contains(rt.feeds[0].stdin, "hasCompletedOnboarding") {
		t.Errorf("the onboarding seed was not fed into the guest home: %+v", rt.feeds)
	}
	if entry := readSessionIndex()["tp"]; !entry.Teleportable {
		t.Errorf("the boot did not record the session as teleportable: %+v", entry)
	}
}

// The boot of a teleportable session tells the runtime so, which is what
// keeps every credential off `hull run` (see runArgs), and the exec that runs
// the agent still carries them. Asked of the spec the runtime receives, so a
// boot that forgot the mark fails here and not only on the argv test.
func TestATeleportableBootLeavesItsCredentialsToTheExec(t *testing.T) {
	teleportState(t)
	t.Chdir(t.TempDir())
	rt := &guestRuntime{}
	c := loadTeleport(t, teleportProfile(t), Options{}, rt)
	var set creds.Set
	set.Add("IS_SANDBOX", "1", "")
	set.AddSecret("CLAUDE_TOKEN", "from-store", "")
	set.Vars = append(set.Vars, runtime.Var{Name: "GH_TOKEN", Value: "gh-from-shell", Credential: true})

	if err := c.EnsureRunning(set); err != nil {
		t.Fatalf("a teleportable boot was refused: %v", err)
	}
	if !rt.spec.Teleportable {
		t.Fatalf("a teleportable boot did not tell the runtime so: %+v", rt.spec)
	}
	exec := map[string]runtime.Var{}
	for _, v := range c.execSpec(set, []string{"tp"}, false).Env {
		exec[v.Name] = v
	}
	for _, name := range []string{"GH_TOKEN", "CLAUDE_TOKEN", "IS_SANDBOX"} {
		if _, ok := exec[name]; !ok {
			t.Errorf("the agent's exec does not carry %s: %+v", name, exec)
		}
	}
}

// Every run path but hull's hvi refuses a teleportable session before
// anything boots, as a capability, so the CLI exits 7 for it.
func TestEnsureRunningRefusesATeleportableSessionOffHvi(t *testing.T) {
	for _, c := range []struct {
		what, kind, hv string
	}{
		{"hull on vz", "hull", "vz"},
		{"hull on qemu", "hull", "qemu"},
		{"nerdctl", "nerdctl", ""},
	} {
		t.Run(c.what, func(t *testing.T) {
			teleportState(t)
			if c.hv != "" {
				t.Setenv("BRIG_HYPERVISOR", c.hv)
			}
			rt := &guestRuntime{kind: c.kind}
			cfg := loadTeleport(t, teleportProfile(t), Options{}, rt)
			err := cfg.EnsureRunning(creds.Set{})
			var ce *runtime.CapabilityError
			if !errors.As(err, &ce) || ce.Property != runtime.Teleportable {
				t.Fatalf("want a teleportable CapabilityError, got %v", err)
			}
			if !strings.Contains(err.Error(), c.what) {
				t.Errorf("the refusal does not name %s: %v", c.what, err)
			}
			if rt.boots != 0 {
				t.Errorf("booted %d sandboxes on a run path that cannot keep the session", rt.boots)
			}
			// The plan says the same, so a plan cannot promise this run.
			if d := cfg.Plan(); d.Refused == nil || !strings.Contains(d.Refused.Reason, "teleportable") {
				t.Errorf("the plan did not carry the refusal: %+v", d.Refused)
			}
		})
	}
}

// A sandbox the index has no entry for, and that hull's record says owns its
// disk, is a teleportable session whose record was lost. An ordinary run must
// not recreate it, running or stopped: that deletes the disk the agent's
// state is on. A sandbox whose record shows a share, or that hull cannot
// describe, is recreated as it always was.
func TestEnsureRunningDoesNotRecreateASandboxWhoseHomeIsItsDisk(t *testing.T) {
	tp := ordinaryProfile(t)
	for _, c := range []struct {
		what       string
		running    bool
		ownsDisk   bool
		inspectErr error
		refused    string
	}{
		{"running, owning its disk", true, true, nil, "--teleportable"},
		{"stopped, owning its disk", false, true, nil, "hull's store"},
		{"running, with a share", true, false, nil, ""},
		{"stopped, with a share", false, false, nil, ""},
		{"running, hull cannot say", true, true, errors.New("inspect failed"), ""},
	} {
		t.Run(c.what, func(t *testing.T) {
			teleportState(t)
			rt := &guestRuntime{running: c.running, exists: true, ownsDisk: c.ownsDisk, inspectErr: c.inspectErr}
			cfg := loadTeleport(t, tp, Options{Workspace: t.TempDir()}, rt)
			err := cfg.EnsureRunning(creds.Set{})
			if c.refused == "" {
				if err != nil || rt.removes == 0 || rt.boots != 1 {
					t.Errorf("an ordinary sandbox was not recreated: %d removes, %d boots (%v)", rt.removes, rt.boots, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.refused) || !strings.Contains(err.Error(), "brig rm tp") {
				t.Fatalf("an unrecorded sandbox that owns its disk was not refused with the way out: %v", err)
			}
			if rt.stops != 0 || rt.removes != 0 || rt.boots != 0 {
				t.Errorf("it was torn down anyway: %d stops, %d removes, %d boots", rt.stops, rt.removes, rt.boots)
			}
		})
	}
}

// An intact ordinary entry already says what the session is, so the runtime
// is not asked and a posture change still recreates the sandbox. Refusing it
// would leave the sandbox running under the rules the change was tightening.
func TestAnOrdinaryEntryKeepsItsPostureRecreate(t *testing.T) {
	teleportState(t)
	tp := ordinaryProfile(t)
	home := t.TempDir()
	rt := &guestRuntime{}
	if err := loadTeleport(t, tp, Options{Workspace: home}, rt).EnsureRunning(creds.Set{}); err != nil {
		t.Fatalf("the first boot was refused: %v", err)
	}
	if entry := readSessionIndex()["tp"]; entry.Home != home || entry.isTeleportable() {
		t.Fatalf("the first boot did not record an ordinary session: %+v", entry)
	}
	// hull's record contradicting the entry is the case the old guest check
	// could be steered into by a guest that hid its home share.
	rt.running, rt.exists, rt.ownsDisk = true, true, true
	rt.boots, rt.stops, rt.removes = 0, 0, 0
	c := loadTeleport(t, tp, Options{Workspace: home, Network: "offline"}, rt)
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatalf("the posture change on an ordinary session was refused: %v", err)
	}
	if rt.stops == 0 || rt.removes == 0 || rt.boots != 1 {
		t.Errorf("the posture change did not recreate the sandbox: %d stops, %d removes, %d boots",
			rt.stops, rt.removes, rt.boots)
	}
}

// brig info says the session is teleportable and that its home is not on the
// host, rather than printing an empty WORKSPACE row.
func TestInfoNamesATeleportableSession(t *testing.T) {
	teleportState(t)
	c := loadTeleport(t, teleportProfile(t), Options{}, &guestRuntime{})
	out := &bytes.Buffer{}
	c.Out = out
	c.Info(creds.Set{})
	for _, w := range []string{"TELEPORTABLE", "none on the host", "/home/tp"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("info does not say %q:\n%s", w, out.String())
		}
	}
	if d := c.InfoData(creds.Set{}); !d.Teleportable || d.Workspace != "" {
		t.Errorf("the Info payload reads teleportable %v, workspace %q", d.Teleportable, d.Workspace)
	}
	if d := c.Plan(); !d.Teleportable || d.Home.Host != "" || d.Home.Mode != "disk" {
		t.Errorf("the plan reads teleportable %v, home %+v", d.Teleportable, d.Home)
	}
}

// openWorkspace refuses a session with no host home rather than cleaning ""
// to "." and treating the directory brig ran from as the guest home.
func TestOpenWorkspaceRefusesASessionWithNoHostHome(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	c := &Config{Teleportable: true}
	if _, err := c.openWorkspace(); !errors.Is(err, errNoHostHome) {
		t.Fatalf("openWorkspace on a session with no host home: %v", err)
	}
	if err := c.SetupGit(&creds.Set{}); err != nil {
		t.Errorf("SetupGit on a teleportable session: %v", err)
	}
	c.GitConfig = true
	if err := c.SetupGit(&creds.Set{}); err != nil {
		t.Errorf("SetupGit with BRIG_GIT_CONFIG on a teleportable session: %v", err)
	}
	if entries, _ := os.ReadDir(cwd); len(entries) != 0 {
		t.Errorf("something was written where brig ran: %v", entries)
	}
	if _, err := os.Lstat(filepath.Join(cwd, markerFile)); err == nil {
		t.Error("a workspace marker was written where brig ran")
	}
}

// A stopped teleportable sandbox still holds its disk, and an ordinary boot
// clears a stopped instance before it starts a new one. For this session that
// deletes the guest home, so the run refuses instead and names rm; once the
// instance is gone the session starts over.
func TestEnsureRunningRefusesToClearAStoppedTeleportableSandbox(t *testing.T) {
	teleportState(t)
	rt := &guestRuntime{}
	first := loadTeleport(t, teleportProfile(t), Options{}, rt)
	if err := first.EnsureRunning(creds.Set{}); err != nil {
		t.Fatalf("the first boot was refused: %v", err)
	}
	stop := &bytes.Buffer{}
	first.Err = stop
	if err := first.Stop(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stop.String(), "stopping a teleportable session ends it") {
		t.Errorf("stop did not say it ends the session:\n%s", stop.String())
	}

	rt.exists = true
	boots, removes := rt.boots, rt.removes
	again := loadTeleport(t, teleportProfile(t), Options{}, rt)
	err := again.EnsureRunning(creds.Set{})
	if err == nil || !strings.Contains(err.Error(), "is stopped") || !strings.Contains(err.Error(), "brig rm tp") {
		t.Fatalf("a stopped teleportable sandbox was not refused with the way out: %v", err)
	}
	if rt.boots != boots || rt.removes != removes {
		t.Errorf("the stopped sandbox was cleared anyway: %d boots, %d removes", rt.boots-boots, rt.removes-removes)
	}

	rt.exists = false
	if err := loadTeleport(t, teleportProfile(t), Options{}, rt).EnsureRunning(creds.Set{}); err != nil {
		t.Fatalf("a session whose sandbox is gone did not start over: %v", err)
	}
}

// A runtime that cannot say whether a stopped instance holds the name is
// refused, not trusted: clearing the name on that silence is the direction
// that deletes the disk.
func TestATeleportableBootRefusesARuntimeThatCannotSayWhatExists(t *testing.T) {
	teleportState(t)
	rt := &guestRuntime{}
	c := loadTeleport(t, teleportProfile(t), Options{}, unaskableRuntime{rt})
	err := c.EnsureRunning(creds.Set{})
	if err == nil || !strings.Contains(err.Error(), "cannot say whether a sandbox exists") {
		t.Fatalf("a runtime that cannot say what exists was trusted: %v", err)
	}
	if rt.removes != 0 || rt.boots != 0 {
		t.Errorf("the name was cleared anyway: %d removes, %d boots", rt.removes, rt.boots)
	}
}

// A changed posture recreates an ordinary sandbox, which costs a boot. On a
// teleportable one it would delete the disk, so it is refused and the
// sandbox is left as it is.
func TestATeleportableSandboxIsNotRecreatedForAPostureChange(t *testing.T) {
	teleportState(t)
	rt := &guestRuntime{}
	if err := loadTeleport(t, teleportProfile(t), Options{}, rt).EnsureRunning(creds.Set{}); err != nil {
		t.Fatalf("the first boot was refused: %v", err)
	}
	// The first boot clears the name before it starts, which is not what
	// this case is about.
	rt.running = true
	rt.boots, rt.stops, rt.removes = 0, 0, 0
	boots := rt.boots
	c := loadTeleport(t, teleportProfile(t), Options{Network: "offline"}, rt)
	err := c.EnsureRunning(creds.Set{})
	if err == nil || !strings.Contains(err.Error(), "deletes its disk") {
		t.Fatalf("a posture change on a teleportable sandbox was not refused: %v", err)
	}
	if rt.stops != 0 || rt.removes != 0 || rt.boots != boots {
		t.Errorf("it was recreated anyway: %d stops, %d removes, %d boots", rt.stops, rt.removes, rt.boots-boots)
	}
}
