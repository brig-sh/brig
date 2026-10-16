package main

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/injector"
)

// closedUnanswered reports whether c is closed without a byte written to it.
func closedUnanswered(t *testing.T, c net.Conn) bool {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := c.Read(make([]byte, 1))
	return n == 0 && err != nil && !errors.Is(err, os.ErrDeadlineExceeded)
}

// The preamble is one strict line, and anything else closes the connection
// before a byte is answered. A connection to a destination the sandbox has
// nothing on is closed too.
func TestDivertClosesOnABadPreamble(t *testing.T) {
	rig := newInternalRig(t)
	added := rig.add(t, "brig-a", internalA)
	lines := []string{
		"BRIG-DIVERT/2 198.18.1.1 443 10.100.0.2 40000\n",
		"BRIG-DIVERT/1 198.18.1.1 0443 10.100.0.2 40000\n",
		"BRIG-DIVERT/1 198.18.1.01 443 10.100.0.2 40000\n",
		"BRIG-DIVERT/1 [::1] 443 10.100.0.2 40000\n",
		"BRIG-DIVERT/1 fe80::1%en0 443 10.100.0.2 40000\n",
		"BRIG-DIVERT/1 198.18.1.1 443 10.100.0.2 40000 extra\n",
		"BRIG-DIVERT/1  198.18.1.1 443 10.100.0.2 40000\n",
		"BRIG-DIVERT/1 198.18.1.1 443 10.100.0.2 70000\n",
		"BRIG-DIVERT/1 198.18.1.1 443 10.100.0.2 40000" + strings.Repeat(" ", 200) + "\n",
		// A destination that is not the sandbox's brig.internal address.
		"BRIG-DIVERT/1 192.0.2.10 443 10.100.0.2 40000\n",
	}
	for _, line := range lines {
		c, err := net.Dial("unix", added.DivertSocket)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(c, line); err != nil {
			t.Fatal(err)
		}
		if !closedUnanswered(t, c) {
			t.Errorf("%q: want the connection closed unanswered", line)
		}
		_ = c.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		pre, port := auditWith(t, rig.dir, "msg=divert", "check=preamble"), auditWith(t, rig.dir, "msg=divert", "check=port")
		if len(pre) == 9 && len(port) == 1 && strings.Contains(port[0], "dst=192.0.2.10:443") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the audit lines do not name the refusals: %q %q", pre, port)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(rig.up.requests()); n != 0 {
		t.Errorf("the upstream saw %d requests", n)
	}
}

// The divert socket is the sandbox's own: 0600 in a 0700 directory, named by
// the registration, and gone with the sandbox or the broker.
func TestADivertSocketIsPrivateAndGoesWithItsSandbox(t *testing.T) {
	rig := newInternalRig(t)
	added := rig.add(t, "brig-a", internalA)
	info, err := os.Lstat(added.DivertSocket)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("the divert socket: %v, %v", info, err)
	}
	if info, err := os.Lstat(filepath.Dir(added.DivertSocket)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the divert socket directory: %v, %v", info, err)
	}
	if want, _ := injector.DivertSocket(rig.dir, "brig-a"); added.DivertSocket != want {
		t.Errorf("the divert socket is %s, want %s", added.DivertSocket, want)
	}
	if again := rig.add(t, "brig-a", internalA); again.DivertSocket != added.DivertSocket {
		t.Fatalf("registering again named %s", again.DivertSocket)
	}
	if _, err := rig.client.RemoveSandbox("brig-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(added.DivertSocket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the divert socket outlived its sandbox: %v", err)
	}
	if c, err := net.Dial("unix", added.DivertSocket); err == nil {
		_ = c.Close()
		t.Error("the divert socket still answers")
	}
	other := rig.add(t, "brig-b", internalB)
	rig.cancel()
	<-rig.done
	rig.done <- nil
	if _, err := os.Lstat(other.DivertSocket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the divert socket outlived the broker: %v", err)
	}
}

// One broker serves the host, and caps the diverted connections of every
// sandbox together. A connection over the cap is closed, and named so.
func TestDivertCapsConnectionsAcrossSandboxes(t *testing.T) {
	saved := hostDivertSlots
	hostDivertSlots = make(chan struct{}, 2)
	t.Cleanup(func() { hostDivertSlots = saved })
	rig := newInternalRig(t)
	a := rig.add(t, "brig-a", internalA)
	b := rig.add(t, "brig-b", internalB)
	// Two connections of brig-a hold both slots while they wait for their
	// preamble.
	for range 2 {
		c, err := net.Dial("unix", a.DivertSocket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
	}
	time.Sleep(100 * time.Millisecond)
	c, err := net.Dial("unix", b.DivertSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !closedUnanswered(t, c) {
		t.Error("a connection over the host cap was not closed")
	}
	if got := auditWith(t, rig.dir, "sandbox=brig-b", "check=capacity.host"); len(got) == 0 {
		t.Error("no audit line says capacity.host")
	}
}
