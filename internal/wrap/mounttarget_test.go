package wrap

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
)

// relocatedProfile is volumeProfile with the tmpfs moved off the home share.
const relocatedProfile = `
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
    at: /brig/claude
  - kind: hostmount
    path: .claude/history.jsonl
    file: true
`

func relocatedConfig(t *testing.T, g *guestFake) *Config {
	t.Helper()
	c := bindingConfig(t, relocatedProfile)
	c.Runtime = g
	c.secrets = creds.Resolution{Values: map[string]string{"cred": testCredential}}
	if err := c.prepareVolumeTargets(); err != nil {
		t.Fatalf("prepareVolumeTargets: %v", err)
	}
	for _, rel := range []string{".claude", ".claude/sessions", ".claude/history.jsonl"} {
		g.files["/home/x/"+rel] = &guestFile{owner: "x", mode: "700"}
	}
	return c
}

func TestMountTarget(t *testing.T) {
	c := bindingConfig(t, relocatedProfile)
	cases := map[string]string{
		".claude":                   "/brig/claude",
		".claude/sessions":          "/brig/claude/sessions",
		".claude/.credentials.json": "/brig/claude/.credentials.json",
		".claude.json":              "/home/x/.claude.json", // a sibling, not under the tmpfs
		".claude-backup/x":          "/home/x/.claude-backup/x",
		".config/gh":                "/home/x/.config/gh",
	}
	for rel, want := range cases {
		if got := c.mountTarget(rel); got != want {
			t.Errorf("mountTarget(%q) = %q, want %q", rel, got, want)
		}
	}
	// Without at: nothing moves.
	plain := bindingConfig(t, volumeProfile)
	if got := plain.mountTarget(".claude/sessions"); got != "/home/x/.claude/sessions" {
		t.Errorf("mountTarget without at: = %q", got)
	}
}

// A relocated tmpfs covers nothing in the home, so there is nothing to pin:
// the tmpfs goes on at at:, and each hostmount is bound from its path in the
// home share to the same relative path under it.
func TestARelocatedTmpfsMountsAtItsOwnPathWithNoPin(t *testing.T) {
	g := newGuestFake()
	c := relocatedConfig(t, g)
	if err := c.deliverSecretFiles(); err != nil {
		t.Fatalf("deliverSecretFiles: %v\n%s", err, strings.Join(g.log, "\n"))
	}
	for _, line := range g.log {
		if strings.Contains(line, persistRoot) {
			t.Errorf("a relocated tmpfs was pinned: %q", line)
		}
		if strings.HasPrefix(line, "mount -t tmpfs") && !strings.HasSuffix(line, " /brig/claude") {
			t.Errorf("tmpfs mounted somewhere other than at: %q", line)
		}
	}
	mkdir := indexOf(t, g.log, "mkdir -p /brig/claude")
	cover := indexOf(t, g.log, "mount -t tmpfs")
	sessions := indexOf(t, g.log, "mount --bind /home/x/.claude/sessions /brig/claude/sessions")
	history := indexOf(t, g.log, "mount --bind /home/x/.claude/history.jsonl /brig/claude/history.jsonl")
	write := indexOf(t, g.log, "sh -c set -e; rm -f")
	if mkdir >= cover || cover >= sessions || cover >= history || history >= write {
		t.Errorf("order was mkdir=%d cover=%d sessions=%d history=%d write=%d\n%s",
			mkdir, cover, sessions, history, write, strings.Join(g.log, "\n"))
	}
	if g.mounts["/home/x/.claude"] {
		t.Error("something was mounted in the home share")
	}
	cred := g.files["/brig/claude/.credentials.json"]
	if cred == nil || cred.body != testCredential {
		t.Fatalf("credential at /brig/claude = %+v", cred)
	}
	if _, ok := g.files["/home/x/.claude/.credentials.json"]; ok {
		t.Error("a credential was written into the home share")
	}
	if owner := g.files["/brig/claude"].owner; owner != "x" {
		t.Errorf("/brig/claude is left owned by %q after delivery", owner)
	}
}

// A second delivery finds the tmpfs and its binds in place and stacks nothing.
func TestARelocatedTmpfsIsIdempotent(t *testing.T) {
	g := newGuestFake()
	c := relocatedConfig(t, g)
	if err := c.deliverSecretFiles(); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	c.secrets = creds.Resolution{Values: map[string]string{"cred": testCredential}}
	if err := c.deliverSecretFiles(); err != nil {
		t.Fatalf("second delivery: %v\n%s", err, strings.Join(g.log, "\n"))
	}
}

// The checks run where the mounts really are: a relocated tmpfs that did not
// happen reads as whatever /brig sits on, and the run stops.
func TestARelocatedTmpfsIsVerifiedAtItsOwnPath(t *testing.T) {
	g := newGuestFake()
	c := relocatedConfig(t, g)
	g.mounts["/brig/claude"] = true // claims to be mounted, and is not tmpfs
	g.files["/brig/claude"] = &guestFile{owner: "root", mode: "700"}
	g.files["/brig/claude/sessions"] = &guestFile{owner: "root", mode: "700"}
	g.files["/brig/claude/history.jsonl"] = &guestFile{owner: "root", mode: "600"}
	g.mounts["/brig/claude/sessions"] = true
	g.mounts["/brig/claude/history.jsonl"] = true
	err := c.deliverSecretFiles()
	if err == nil || !strings.Contains(err.Error(), "ephemeral") {
		t.Fatalf("err = %v, want the tmpfs check to fail\n%s", err, strings.Join(g.log, "\n"))
	}
}

// A runtime that takes its mounts at create time is handed the relocated
// paths, never the home ones.
func TestCreateTimeVolumesForTheShippedClaudeCodeProfile(t *testing.T) {
	ws := t.TempDir()
	c := testConfig(t, ws, ws)
	g := newGuestFake()
	g.kind = "nerdctl"
	c.Runtime = g
	tmpfs, shares := c.createTimeVolumes(ws)
	if len(tmpfs) != 1 || !strings.HasPrefix(tmpfs[0], "/brig/claude:size=512m,") {
		t.Errorf("tmpfs = %v", tmpfs)
	}
	if len(shares) == 0 {
		t.Fatal("no hostmount shares")
	}
	sawState := false
	for _, s := range shares {
		if !strings.HasPrefix(s.Guest, "/brig/claude/") {
			t.Errorf("share guest %q is not under /brig/claude", s.Guest)
		}
		rel := strings.TrimPrefix(s.Guest, "/brig/claude/")
		if want := filepath.Join(ws, ".claude", filepath.FromSlash(rel)); s.Host != want {
			t.Errorf("share host = %q, want %q", s.Host, want)
		}
		if s.Guest == "/brig/claude/.claude.json" {
			sawState = true
		}
	}
	if !sawState {
		t.Errorf("no share for the agent's global state: %+v", shares)
	}
	for _, v := range append(tmpfs, func() (g []string) {
		for _, s := range shares {
			g = append(g, s.Guest)
		}
		return g
	}()...) {
		if strings.HasPrefix(v, "/root/") {
			t.Errorf("a home path reached the runtime: %q", v)
		}
	}
}

// A sandbox still running from before the move has its tmpfs in the home.
// Binding the hostmounts from there would bind memory, not the share, so the
// run stops and says to recreate the sandbox instead.
func TestARunningSandboxWithTheOldCoverIsRefused(t *testing.T) {
	g := newGuestFake()
	c := relocatedConfig(t, g)
	g.mounts["/home/x/.claude"] = true
	g.fstype["/home/x/.claude"] = "tmpfs"
	err := c.deliverSecretFiles()
	if err == nil || !strings.Contains(err.Error(), "brig rm") {
		t.Fatalf("err = %v, want a refusal naming brig rm", err)
	}
	for _, line := range g.log {
		if strings.HasPrefix(line, "mount") || strings.HasPrefix(line, "mkdir") {
			t.Errorf("something was mounted before the refusal: %q", line)
		}
	}
}
