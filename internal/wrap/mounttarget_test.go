package wrap

import (
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
