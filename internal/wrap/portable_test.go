package wrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/verify"
)

// writePortableAssets is writeBootAssets for the portable machine: the
// bundle's initrd, and a portable kernel beside it in place of the bundle's
// kernel, the way runtime.forMachine resolves them.
func writePortableAssets(t *testing.T, kernel []byte, named bool) runtime.BootAssets {
	t.Helper()
	assets := writeBootAssets(t, []byte("the bundle's kernel, which does not boot"), publishedInitrd, false)
	assets.Kernel = filepath.Join(filepath.Dir(assets.Initrd), "portable-arm64-v1", "Image")
	if err := os.MkdirAll(filepath.Dir(assets.Kernel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(assets.Kernel, kernel, 0o644); err != nil {
		t.Fatal(err)
	}
	assets.Machine = runtime.MachinePortableArm64V1
	assets.KernelNamed = named
	return assets
}

// A teleportable session asks the resolve and the boot for the machine this
// host's teleportable sessions boot, and every other session asks for none.
// The envelope names the machine on a teleportable session only.
func TestATeleportableSessionAsksForTheMachineItBoots(t *testing.T) {
	c := testConfig(t, t.TempDir(), t.TempDir())
	if got := c.machine(); got != "" {
		t.Errorf("an ordinary session asks for the %q machine, want none", got)
	}
	for _, row := range c.envelope(creds.Set{}) {
		if row.label == "MACHINE" {
			t.Errorf("an ordinary session's envelope has a MACHINE row: %q", row.value)
		}
	}

	c.Teleportable = true
	want := runtime.TeleportableMachine()
	if got := c.machine(); got != want {
		t.Errorf("a teleportable session asks for the %q machine, want %q", got, want)
	}
	name := want
	if name == "" {
		name = runtime.MachineNative
	}
	var line string
	for _, row := range c.envelope(creds.Set{}) {
		if row.label == "MACHINE" {
			line = row.value
		}
	}
	if !strings.HasPrefix(line, name+" (") {
		t.Errorf("the MACHINE row is %q, want it to name %s", line, name)
	}
	if got := c.InfoData(creds.Set{}).Machine; got != name {
		t.Errorf("info --json names the machine %q, want %q", got, name)
	}
}

// What hull's record says the sandbox runs beats what a boot would ask for,
// and a record that differs says so: that is a session booted before brig
// chose the portable machine.
func TestTheMachineRowReadsHullsRecord(t *testing.T) {
	c := testConfig(t, t.TempDir(), t.TempDir())
	c.Teleportable = true
	want := c.machineName()

	c.machineSeen = want
	if got := c.machineLine(); !strings.HasPrefix(got, want+" (") {
		t.Errorf("a record that agrees reads %q", got)
	}
	other := runtime.MachineNative
	if want == runtime.MachineNative {
		other = runtime.MachinePortableArm64V1
	}
	c.machineSeen = other
	got := c.machineLine()
	if !strings.HasPrefix(got, other+", as hull records the boot") || !strings.Contains(got, want) {
		t.Errorf("a record that differs reads %q, want %s as recorded and %s as what a boot asks for", got, other, want)
	}
	if c.infoMachine() != other {
		t.Errorf("info --json names %q, want the recorded %q", c.infoMachine(), other)
	}
}

// The resolve and the boot of a teleportable session both name the machine,
// so the kernel that boots is the one resolved for it and hull is asked for
// the same machine. An ordinary session names none to either.
func TestATeleportableBootHandsTheMachineToTheResolveAndTheRun(t *testing.T) {
	registryServes(t, published())
	for _, teleportable := range []bool{false, true} {
		// Both named, so no digest stops the boot under warn: what this
		// reads is the machine handed down, not the checks.
		assets := writePortableAssets(t, []byte("a portable kernel"), true)
		assets.Named = true
		c, rr := digestRun(t, verify.Warn, assets)
		// A teleportable boot asks first whether a stopped sandbox holds
		// the session's disk. None does here.
		c.Runtime = absentResolver{rr}
		c.Teleportable = teleportable
		want := ""
		if teleportable {
			want = runtime.TeleportableMachine()
		}

		err := c.EnsureRunning(creds.Set{})

		if len(rr.fetches) == 0 {
			t.Fatalf("teleportable %v: the boot assets were never resolved: %v", teleportable, err)
		}
		if got := rr.fetches[0].Machine; got != want {
			t.Errorf("teleportable %v: the resolve was asked for the %q machine, want %q", teleportable, got, want)
		}
		reachedRun(t, "teleportable", rr)
		if rr.booted.Profile != want {
			t.Errorf("teleportable %v: the run asks for the %q machine, want %q", teleportable, rr.booted.Profile, want)
		}
	}
}

// absentResolver is a resolvingRuntime that can say no sandbox exists.
type absentResolver struct{ *resolvingRuntime }

func (absentResolver) Exists(string) (bool, error) { return false, nil }

// hostMachine makes this host one whose teleportable sessions boot machine,
// "" for a host with no portable machine, whatever the test runs on.
func hostMachine(t *testing.T, machine string) {
	t.Helper()
	was := teleportableMachine
	teleportableMachine = func() string { return machine }
	t.Cleanup(func() { teleportableMachine = was })
}

// brig hands the portable kernel to hull through the boot assets, and only a
// genericBoot profile has any. On a host with a portable machine a
// teleportable session of any other profile would ask hull for that machine
// with no kernel brig chose or checked, so it is refused before the runtime
// is asked, and the plan says the same. On a host with no portable machine
// the session boots the native one, and nothing is refused.
func TestATeleportableProfileWithoutGenericBootIsRefusedOnAPortableHost(t *testing.T) {
	for _, machine := range []string{runtime.MachinePortableArm64V1, ""} {
		teleportState(t)
		hostMachine(t, machine)
		p := ordinaryProfile(t)
		if p.GenericBoot {
			t.Fatal("ordinaryProfile became a genericBoot profile; this case needs one that is not")
		}
		rt := &guestRuntime{}
		cfg := loadTeleport(t, p, Options{Teleportable: true}, rt)
		err := cfg.EnsureRunning(creds.Set{})
		if machine == "" {
			if err != nil || rt.boots != 1 {
				t.Errorf("no portable machine: the session was not booted on the native one: %d boots (%v)", rt.boots, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "genericBoot: true") ||
			!strings.Contains(err.Error(), machine) || !strings.Contains(err.Error(), "--teleportable") {
			t.Fatalf("want the refusal naming the machine, genericBoot and --teleportable, got %v", err)
		}
		if rt.boots != 0 {
			t.Errorf("booted %d sandboxes for a teleportable session with no portable kernel", rt.boots)
		}
		if d := cfg.Plan(); d.Refused == nil || !strings.Contains(d.Refused.Reason, "genericBoot: true") {
			t.Errorf("the plan did not carry the refusal: %+v", d.Refused)
		}
		if strings.Contains(err.Error(), "brig rm") {
			t.Errorf("a new session is told to remove itself: %v", err)
		}

		// A session the index records as teleportable cannot be run as an
		// ordinary one, so its refusal names rm as the way out instead.
		cfg.rememberSession()
		later := loadTeleport(t, p, Options{}, &guestRuntime{})
		err = later.EnsureRunning(creds.Set{})
		if err == nil || !strings.Contains(err.Error(), "genericBoot: true") ||
			!strings.Contains(err.Error(), "`brig rm tp`") {
			t.Errorf("a recorded session: want the refusal naming `brig rm tp`, got %v", err)
		}
	}
}

// The run line names the machine and the resolve supplies its kernel, and a
// boot where the two disagree is refused before the runtime is asked: a
// runtime that resolved the bundle's kernel and no portable one would boot a
// kernel with no hvi-time driver on the portable machine. BRIG_VERIFY=off
// skips the digest comparison, not this.
func TestABootWhoseResolveNamesNoMachineIsRefused(t *testing.T) {
	hostMachine(t, runtime.MachinePortableArm64V1)
	registryServes(t, published())
	for _, mode := range []verify.Mode{verify.Off, verify.Warn, verify.Require} {
		assets := writeBootAssets(t, publishedKernel, publishedInitrd, false)
		c, rr := digestRun(t, mode, assets)
		c.Runtime = absentResolver{rr}
		c.Teleportable = true

		err := c.EnsureRunning(creds.Set{})

		if err == nil || rr.booted != nil {
			t.Fatalf("%s: a boot with no portable kernel reached the runtime (err %v)", mode, err)
		}
		if !strings.Contains(err.Error(), "no portable kernel was resolved") {
			t.Errorf("%s: want the refusal that names the missing portable kernel, got %v", mode, err)
		}
	}
}
