package wrap

import (
	"fmt"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/session"
)

// wantRemovable fails the test unless msg tells the reader to run `brig rm`
// on a ref that resolves back to the sandbox of c.
func wantRemovable(t *testing.T, c *Config, msg string) {
	t.Helper()
	_, rest, ok := strings.Cut(msg, "brig rm ")
	if !ok {
		t.Fatalf("the message names no `brig rm`: %s", msg)
	}
	word := ""
	if fields := strings.Fields(rest); len(fields) > 0 {
		word = strings.Trim(fields[0], "`.,")
	}
	ref, err := session.ParseRef(word)
	if err != nil {
		t.Fatalf("`brig rm %s` is refused: %v\n%s", word, err, msg)
	}
	p, ok := profile.Lookup(ref.Agent)
	if !ok {
		t.Fatalf("`brig rm %s` is refused: unknown profile %q\n%s", word, ref.Agent, msg)
	}
	back, err := Load(p, Options{Name: ref.Label}, nil)
	if err != nil {
		t.Fatalf("`brig rm %s`: %v", word, err)
	}
	if back.VMName != c.VMName {
		t.Errorf("`brig rm %s` removes %s, not %s", word, back.VMName, c.VMName)
	}
}

// The hint printed the sandbox name, and `brig rm` takes a ref, so the
// command it gave failed with unknown profile "brig-claude-code".
func TestTheNotMountedHintNamesARefBrigRmTakes(t *testing.T) {
	for _, name := range []string{"", "review"} {
		t.Run(fmt.Sprintf("name %q", name), func(t *testing.T) {
			isolateState(t)
			c := mustLoadAs(t, "claude", Options{Name: name})
			g := newGuestFake()
			c.Runtime = g
			// A container runtime took the tmpfs at create time and none of
			// the hostmounts below it.
			for _, v := range c.Profile.Tmpfs() {
				g.mounts[c.guestPath(v.Path)] = true
			}
			err := c.mountVolumes()
			if err == nil || !strings.Contains(err.Error(), "is not mounted") {
				t.Fatalf("err = %v; want one saying a hostmount is not mounted", err)
			}
			wantRemovable(t, c, err.Error())
		})
	}
}

// The publish hint printed the session name as typed, which is empty for a
// default session and not a ref for a named one.
func TestThePublishHintNamesARefBrigRmTakes(t *testing.T) {
	for _, name := range []string{"", "review"} {
		t.Run(fmt.Sprintf("name %q", name), func(t *testing.T) {
			isolateState(t)
			c := mustLoadAs(t, "claude", Options{Name: name})
			g := newGuestFake()
			g.kind = "nerdctl"
			c.Runtime = g
			c.PublishAsked = publications(t, "3000")
			err := c.publishLive()
			if err == nil {
				t.Fatal("a runtime that cannot publish on a live sandbox accepted a port")
			}
			wantRemovable(t, c, err.Error())
		})
	}
}
