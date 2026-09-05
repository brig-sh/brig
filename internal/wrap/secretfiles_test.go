package wrap

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
)

// guestFake is a guest that answers the four questions delivery asks -- what
// is mounted, what filesystem a path sits on, what a file looks like and how
// big it is -- and records every command it was given.
//
// A recording double rather than a stub, because the ORDER is the design: pin
// before cover, cover before bind, create before write. A double that answered
// correctly while losing the sequence could not fail the test this file exists
// to make fail.
type guestFake struct {
	runtime.Runtime
	kind string
	// mounts is the guest's mount table, keyed by mount point. mount commands
	// add to it, which is what makes a second delivery see the first's work.
	mounts map[string]bool
	// fstype answers stat -f. Anything not named reads as the host share.
	fstype map[string]string
	// files is what has been created, and what was fed into it.
	files map[string]*guestFile
	swaps string
	log   []string
	fail  map[string]error
}

type guestFile struct {
	mode  string
	owner string
	body  string
}

func newGuestFake() *guestFake {
	return &guestFake{
		kind:   "hull",
		mounts: map[string]bool{"/": true},
		fstype: map[string]string{},
		files:  map[string]*guestFile{},
		swaps:  "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n",
		fail:   map[string]error{},
	}
}

func (g *guestFake) Kind() string { return g.kind }

func (g *guestFake) record(spec runtime.ExecSpec) string {
	line := strings.Join(spec.Cmd, " ")
	g.log = append(g.log, line)
	return line
}

func (g *guestFake) Output(spec runtime.ExecSpec) (string, error) {
	line := g.record(spec)
	if spec.User != guestRootUser {
		return "", fmt.Errorf("delivery ran %q as %q; the mounts need root", line, spec.User)
	}
	for pattern, err := range g.fail {
		if strings.Contains(line, pattern) {
			return "", err
		}
	}
	argv := spec.Cmd
	switch {
	case len(argv) >= 2 && argv[0] == "cat" && argv[1] == "/proc/self/mountinfo":
		var b strings.Builder
		for point := range g.mounts {
			fmt.Fprintf(&b, "1 2 0:1 / %s rw - tmpfs tmpfs rw\n", point)
		}
		return b.String(), nil
	case len(argv) >= 2 && argv[0] == "cat" && argv[1] == "/proc/swaps":
		return g.swaps, nil
	case argv[0] == "mkdir":
		g.files[argv[len(argv)-1]] = &guestFile{owner: "root", mode: "700"}
		return "", nil
	case argv[0] == "mount":
		return "", g.mount(argv)
	case argv[0] == "chown":
		return "", g.chown(argv[1], argv[2])
	case argv[0] == "stat":
		return g.stat(argv)
	case argv[0] == "sh":
		return "", g.shell(argv)
	}
	return "", fmt.Errorf("guest asked to run %q, which delivery has no business running", line)
}

func (g *guestFake) mount(argv []string) error {
	target := argv[len(argv)-1]
	if g.mounts[target] {
		return fmt.Errorf("%s is already a mountpoint; delivery stacked a mount", target)
	}
	if argv[1] == "--bind" {
		source := argv[2]
		if _, ok := g.files[source]; !ok && !g.mounts[source] {
			return fmt.Errorf("bind source %s does not exist", source)
		}
		if _, ok := g.files[target]; !ok {
			return fmt.Errorf("bind target %s does not exist", target)
		}
		g.fstype[target] = g.fstypeOf(source)
	} else {
		// A tmpfs cover hides everything under it, which is exactly what the
		// pin-first ordering exists to survive.
		for p := range g.files {
			if strings.HasPrefix(p, target+"/") {
				delete(g.files, p)
			}
		}
		g.files[target] = &guestFile{owner: "root", mode: "700"}
		g.fstype[target] = "tmpfs"
	}
	g.mounts[target] = true
	return nil
}

func (g *guestFake) chown(user, target string) error {
	f, ok := g.files[target]
	if !ok {
		return fmt.Errorf("chown on %s, which does not exist", target)
	}
	f.owner = user
	return nil
}

func (g *guestFake) stat(argv []string) (string, error) {
	target := argv[len(argv)-1]
	if argv[1] == "-f" {
		return g.fstypeOf(target), nil
	}
	f, ok := g.files[target]
	if !ok {
		return "", fmt.Errorf("stat on %s, which does not exist", target)
	}
	switch argv[2] {
	case "%s":
		return fmt.Sprint(len(f.body)), nil
	default:
		kind := "regular file"
		if f.body == "" {
			// What stat actually says about a file that has just been
			// created, and the reason the check reads the type as a prefix.
			kind = "regular empty file"
		}
		return fmt.Sprintf("%s|%s|%s", kind, f.owner, f.mode), nil
	}
}

// fstypeOf walks up to the nearest mount, the way stat -f does.
func (g *guestFake) fstypeOf(target string) string {
	for p := target; strings.Contains(p, "/"); p = p[:strings.LastIndex(p, "/")] {
		if t, ok := g.fstype[p]; ok {
			return t
		}
	}
	return "fuseblk"
}

// shell handles the two scripts delivery runs: the create, and the touch that
// makes a file-shaped mount target.
func (g *guestFake) shell(argv []string) error {
	script, args := argv[2], argv[4:]
	switch {
	case strings.Contains(script, "dirname"):
		g.files[args[0]] = &guestFile{owner: "root", mode: "600"}
		return nil
	case strings.Contains(script, "set -C"):
		target, user, mode := args[0], args[1], args[2]
		dir := target[:strings.LastIndex(target, "/")]
		if owner := g.files[dir]; owner == nil || owner.owner != guestRootUser {
			return fmt.Errorf("a credential was created in %s while %s owned it, so the "+
				"sandbox could have planted a symlink at %s", dir, ownerOf(g.files[dir]), target)
		}
		g.files[target] = &guestFile{owner: user, mode: strings.TrimLeft(mode, "0")}
		return nil
	}
	return fmt.Errorf("unexpected script %q", script)
}

func ownerOf(f *guestFile) string {
	if f == nil {
		return "nobody"
	}
	return f.owner
}

func (g *guestFake) Feed(spec runtime.ExecSpec) error {
	g.record(spec)
	body, err := io.ReadAll(spec.Stdin)
	if err != nil {
		return err
	}
	target := spec.Cmd[len(spec.Cmd)-1]
	f, ok := g.files[target]
	if !ok {
		return fmt.Errorf("fed %s, which does not exist", target)
	}
	f.body = string(body)
	return nil
}

const testCredential = `{"claudeAiOauth":{}}`

const volumeProfile = `
secrets:
  - name: cred
    required: false
files:
  - ref: secrets.cred
    path: .claude/.credentials.json
    mode: "0600"
volumes:
  - kind: hostmount
    path: .claude/sessions
  - kind: tmpfs
    path: .claude
  - kind: hostmount
    path: .claude/history.jsonl
    file: true
`

func deliveryConfig(t *testing.T, g *guestFake) *Config {
	t.Helper()
	c := bindingConfig(t, volumeProfile)
	c.Runtime = g
	c.secrets = creds.Resolution{Values: map[string]string{"cred": testCredential}}
	if err := c.prepareVolumeTargets(); err != nil {
		t.Fatalf("prepareVolumeTargets: %v", err)
	}
	// The guest sees the workspace as its home, so what the host prepared is
	// there before anything mounts.
	for _, rel := range []string{".claude", ".claude/sessions", ".claude/history.jsonl"} {
		g.files["/home/x/"+rel] = &guestFile{owner: "x", mode: "700"}
	}
	return c
}

func TestDeliveryPinsBeforeItCovers(t *testing.T) {
	g := newGuestFake()
	c := deliveryConfig(t, g)
	if err := c.deliverSecretFiles(); err != nil {
		t.Fatalf("deliverSecretFiles: %v", err)
	}
	cover := indexOf(t, g.log, "mount -t tmpfs")
	pin := indexOf(t, g.log, "mount --bind /home/x/.claude/sessions /run/brig/persist/")
	back := indexOf(t, g.log, "mount --bind /run/brig/persist/.claude%2Fsessions /home/x/.claude/sessions")
	if !(pin < cover && cover < back) {
		t.Errorf("order was pin=%d cover=%d bind-back=%d; a cover before the pin loses the "+
			"state it was meant to keep\n%s", pin, cover, back, strings.Join(g.log, "\n"))
	}
}

// The whole point, in one assertion: the credential is on the tmpfs and the
// host workspace holds nothing.
func TestDeliveryWritesTheCredentialIntoTheTmpfs(t *testing.T) {
	g := newGuestFake()
	c := deliveryConfig(t, g)
	if err := c.deliverSecretFiles(); err != nil {
		t.Fatalf("deliverSecretFiles: %v", err)
	}
	cred := g.files["/home/x/.claude/.credentials.json"]
	if cred == nil {
		t.Fatal("no credential was written")
	}
	if cred.body != testCredential {
		t.Errorf("credential body = %q", cred.body)
	}
	if cred.owner != "x" || cred.mode != "600" {
		t.Errorf("credential is %s mode %s; want x 600", cred.owner, cred.mode)
	}
	if g.fstypeOf("/home/x/.claude/.credentials.json") != "tmpfs" {
		t.Error("the credential is not on a tmpfs")
	}
	if _, err := os.Stat(filepath.Join(c.Workspace, ".claude", ".credentials.json")); err == nil {
		t.Error("a credential reached the host workspace")
	}
	// The directory is handed back to the agent afterwards, or the agent
	// cannot write its own state into the directory brig just made for it.
	if owner := g.files["/home/x/.claude"].owner; owner != "x" {
		t.Errorf(".claude is left owned by %q", owner)
	}
}

// Never in argv: hull durably logs every exec's argv to a host file, so a
// value there outlives the sandbox in a file the user never sees.
func TestDeliveryNeverPutsAValueInArgv(t *testing.T) {
	g := newGuestFake()
	c := deliveryConfig(t, g)
	if err := c.deliverSecretFiles(); err != nil {
		t.Fatalf("deliverSecretFiles: %v", err)
	}
	for _, line := range g.log {
		if strings.Contains(line, "claudeAiOauth") {
			t.Fatalf("a value reached argv: %q", line)
		}
	}
}

// EnsureRunning calls delivery on an already-running sandbox too, so two runs
// in a row must neither stack mounts nor empty a live credential. guestFake
// fails a second mount on one point outright, which is the stacking half.
func TestSecondDeliveryIsIdempotent(t *testing.T) {
	g := newGuestFake()
	c := deliveryConfig(t, g)
	if err := c.deliverSecretFiles(); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	mounts := len(g.mounts)
	c.secrets = creds.Resolution{Values: map[string]string{"cred": "second"}}
	if err := c.deliverSecretFiles(); err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	if len(g.mounts) != mounts {
		t.Errorf("a second delivery changed the mount count from %d to %d", mounts, len(g.mounts))
	}
	if body := g.files["/home/x/.claude/.credentials.json"].body; body != "second" {
		t.Errorf("the credential is %q after a rotation; a live session never sees the new one", body)
	}
}

// An unresolved binding leaves NOTHING behind. An empty file at a credential
// path is indistinguishable from a real leak to whoever finds it, and it is a
// login prompt the agent cannot explain.
func TestAnUnresolvedBindingWritesNothing(t *testing.T) {
	g := newGuestFake()
	c := deliveryConfig(t, g)
	c.secrets = creds.Resolution{}
	if err := c.deliverSecretFiles(); err != nil {
		t.Fatalf("deliverSecretFiles: %v", err)
	}
	if _, ok := g.files["/home/x/.claude/.credentials.json"]; ok {
		t.Error("an unresolved binding left a file behind")
	}
	// The volumes still mount: they are about what persists, not about
	// secrets, and a sandbox with no credential still needs its state kept.
	if !g.mounts["/home/x/.claude"] {
		t.Error("an unresolved binding skipped the volumes too")
	}
}

// A profile with volumes: and no files: still has to hand the covered
// directory to the agent. The cover leaves it root-owned on purpose, and
// nothing else in the run would ever give it back.
func TestVolumesWithNoFilesStillReachTheAgent(t *testing.T) {
	g := newGuestFake()
	c := bindingConfig(t, "volumes:\n  - kind: tmpfs\n    path: .claude\n")
	c.Runtime = g
	if err := c.prepareVolumeTargets(); err != nil {
		t.Fatalf("prepareVolumeTargets: %v", err)
	}
	g.files["/home/x/.claude"] = &guestFile{owner: "x", mode: "700"}
	if err := c.deliverSecretFiles(); err != nil {
		t.Fatalf("deliverSecretFiles: %v", err)
	}
	if owner := g.files["/home/x/.claude"].owner; owner != "x" {
		t.Errorf(".claude is left owned by %q, so the agent cannot write its own state", owner)
	}
}

// The resolved values do not outlive the delivery that used them.
func TestDeliveryClearsTheResolvedValues(t *testing.T) {
	g := newGuestFake()
	c := deliveryConfig(t, g)
	if err := c.deliverSecretFiles(); err != nil {
		t.Fatalf("deliverSecretFiles: %v", err)
	}
	if len(c.secrets.Values) != 0 {
		t.Errorf("a plaintext credential outlived its delivery: %v", c.secrets.Values)
	}
}

// Fail closed, both ways: a directory that should be ephemeral and is not, and
// a kept path the cover swallowed. Each failure is the guarantee for one half.
func TestVerificationFailsTheRun(t *testing.T) {
	t.Run("the ephemeral directory reads as host disk", func(t *testing.T) {
		g := newGuestFake()
		c := deliveryConfig(t, g)
		if err := c.deliverSecretFiles(); err != nil {
			t.Fatalf("first delivery: %v", err)
		}
		// The mount is still there and stat now says otherwise -- which is
		// what a mount that silently went away looks like from the host, and
		// the only thing standing between it and a token on host disk.
		g.fstype["/home/x/.claude"] = "fuseblk"
		c.secrets = creds.Resolution{Values: map[string]string{"cred": "second"}}
		err := c.deliverSecretFiles()
		if err == nil || !strings.Contains(err.Error(), "reaches host disk") {
			t.Fatalf("err = %v; want one refusing to hand over a credential", err)
		}
		if body := g.files["/home/x/.claude/.credentials.json"].body; body != testCredential {
			t.Errorf("a credential was written despite the verification failing: %q", body)
		}
	})
	t.Run("swap appeared", func(t *testing.T) {
		g := newGuestFake()
		c := deliveryConfig(t, g)
		g.swaps += "/swapfile file 2097148 0 -2\n"
		err := c.deliverSecretFiles()
		if err == nil || !strings.Contains(err.Error(), "swap") {
			t.Fatalf("err = %v; want one naming swap", err)
		}
	})
}

// Every one of these paths is inside the workspace, which the sandbox has had
// write access to for weeks. A root-run mount --bind that followed a planted
// link would be an arbitrary-write primitive reaching the host bundle, so the
// run fails and names the path rather than repairing it.
func TestAPlantedSymlinkAtAVolumeTargetFailsTheRun(t *testing.T) {
	for _, at := range []string{".claude", ".claude/sessions"} {
		t.Run(at, func(t *testing.T) {
			c := bindingConfig(t, volumeProfile)
			c.Runtime = newGuestFake()
			if dir := filepath.Dir(filepath.Join(c.Workspace, at)); dir != c.Workspace {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink("/etc", filepath.Join(c.Workspace, at)); err != nil {
				t.Fatal(err)
			}
			err := c.prepareVolumeTargets()
			if err == nil {
				t.Fatal("a planted symlink was accepted as a mount target")
			}
			if !strings.Contains(err.Error(), at) {
				t.Errorf("the error does not name the path: %v", err)
			}
		})
	}
}

// A container runtime has no privileged exec to mount with, so its mounts have
// to be part of the create request. hull gets neither: it does the three-phase
// mount itself, and passing these would mount the same paths twice.
func TestCreateTimeVolumesAreForTheRuntimesThatCannotMount(t *testing.T) {
	g := newGuestFake()
	c := deliveryConfig(t, g)
	if tmpfs, shares := c.createTimeVolumes(); tmpfs != nil || shares != nil {
		t.Errorf("hull was handed create-time mounts: %v, %v", tmpfs, shares)
	}
	g.kind = "nerdctl"
	tmpfs, shares := c.createTimeVolumes()
	if len(tmpfs) != 1 || !strings.HasPrefix(tmpfs[0], "/home/x/.claude:size=") {
		t.Errorf("tmpfs = %v", tmpfs)
	}
	if len(shares) != 2 || shares[0].Guest != "/home/x/.claude/sessions" {
		t.Errorf("shares = %+v", shares)
	}
	if shares[0].Host != filepath.Join(c.Workspace, ".claude", "sessions") {
		t.Errorf("share host = %q", shares[0].Host)
	}
}

func indexOf(t *testing.T, log []string, prefix string) int {
	t.Helper()
	for i, line := range log {
		if strings.HasPrefix(line, prefix) {
			return i
		}
	}
	t.Fatalf("nothing in the log starts with %q:\n%s", prefix, strings.Join(log, "\n"))
	return -1
}
