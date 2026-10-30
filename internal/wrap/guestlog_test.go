package wrap

import (
	"bytes"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
)

// The log of a real failure. virtiofsd's warning is the only line with a level
// on it, and readers blamed it. The line that says what happened has none.
func TestGuestLogTailKeepsTheGuestsWords(t *testing.T) {
	log := "[2026-10-30T09:45:05Z WARN  virtiofsd::limits] Failure when trying to set the limit to 1000000\n" +
		"[2026-10-30T09:45:05Z INFO  virtiofsd] Client connected, servicing requests\n" +
		"exec sleep failed: No such file or directory\n" +
		"\n" +
		"[    0.533747] reboot: Rest[2026-10-30T09:45:05Z INFO  virtiofsd] Client disconnected, shutting down\n" +
		"arting system\n"
	got := guestLogTail(log, 5)
	want := []string{"exec sleep failed: No such file or directory", "[    0.533747] reboot: Rest", "arting system"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("guestLogTail = %q, want %q", got, want)
	}
}

// Guest output reaches the host's terminal here, so nothing a terminal would
// act on may survive.
func TestGuestLogTailDropsControlSequences(t *testing.T) {
	got := guestLogTail("\x1b]0;owned\x07hello\x1b[2J\r\n", 5)
	if len(got) != 1 {
		t.Fatalf("got %q", got)
	}
	for _, r := range got[0] {
		if r < 0x20 || r == 0x7f {
			t.Errorf("a control character survived in %q", got[0])
		}
	}
}

func TestGuestLogTailKeepsTheEnd(t *testing.T) {
	got := guestLogTail("1\n2\n3\n4\n", 2)
	if want := []string{"3", "4"}; !reflect.DeepEqual(got, want) {
		t.Errorf("guestLogTail = %q, want %q", got, want)
	}
}

// deadGuest is a sandbox the runtime calls running, whose guest failed in boot.
type deadGuest struct {
	*livenessRuntime
	logged string
}

func (d *deadGuest) Probe(runtime.ExecSpec) bool { return false }
func (d *deadGuest) Logs(spec runtime.LogsSpec) error {
	// As nerdctl 2.3.5 answers --tail: a fragment of the last line. The line
	// that says why the guest died is only in the whole log.
	if spec.Tail >= 0 {
		_, err := io.WriteString(spec.Out, "  0.333049] reboot: Restarting system\n")
		return err
	}
	if _, err := io.WriteString(spec.Err, "[2026-10-30T09:45:05Z WARN  virtiofsd::limits] Failure\n"); err != nil {
		return err
	}
	_, err := io.WriteString(spec.Out, d.logged)
	return err
}

func deadGuestConfig(t *testing.T, running bool) (*Config, *deadGuest) {
	t.Helper()
	rt := &deadGuest{livenessRuntime: &livenessRuntime{running: running},
		logged: "exec sleep failed: No such file or directory\n"}
	c := livenessConfig(t, rt.livenessRuntime)
	c.Runtime = rt
	c.ReadyTimeout = time.Millisecond
	return c, rt
}

// Nothing was renamed on this host, and the message must not send the reader to
// look for what was. The log goes with the restart, so it is printed first.
func TestEnsureRunningDoesNotCallADeadGuestAStaleShare(t *testing.T) {
	c, rt := deadGuestConfig(t, true)
	if err := c.EnsureRunning(creds.Set{}); err == nil {
		t.Fatal("a guest that never answered became ready")
	}
	said := c.Err.(*bytes.Buffer).String()
	if strings.Contains(said, "went stale") {
		t.Errorf("a guest that never answered was reported as a stale share:\n%s", said)
	}
	if !strings.Contains(said, "not answering") {
		t.Errorf("the reason for the restart is missing:\n%s", said)
	}
	if !strings.Contains(said, rt.logged[:20]) {
		t.Errorf("the guest's log is missing:\n%s", said)
	}
}

// A guest that answers with the wrong marker is the case the stale message is for.
func TestEnsureRunningStillReportsAStaleShare(t *testing.T) {
	rt := &livenessRuntime{running: true}
	c := livenessConfig(t, rt)
	wrong := &wrongMarker{livenessRuntime: rt}
	c.Runtime = wrong

	_ = c.EnsureRunning(creds.Set{})

	if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, "went stale") {
		t.Errorf("a guest mounting something else was not reported as stale:\n%s", said)
	}
}

type wrongMarker struct{ *livenessRuntime }

func (w *wrongMarker) Output(runtime.ExecSpec) (string, error) { return "another workspace", nil }

// A boot that never answers says what the guest said, and the message that
// follows stays as it was.
func TestEnsureRunningShowsTheGuestLogWhenNotReady(t *testing.T) {
	c, rt := deadGuestConfig(t, false)
	err := c.EnsureRunning(creds.Set{})
	if err == nil || !strings.Contains(err.Error(), "sandbox did not become ready") {
		t.Fatalf("err = %v, want the not-ready error", err)
	}
	if strings.Contains(err.Error(), "exec sleep") {
		t.Errorf("guest output reached the error, and errors reach telemetry: %v", err)
	}
	if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, rt.logged[:20]) {
		t.Errorf("the guest's log is missing:\n%s", said)
	}
}

func TestTailBufferKeepsTheEnd(t *testing.T) {
	b := &tailBuffer{limit: 4}
	for _, part := range []string{"abc", "defg"} {
		if n, err := b.Write([]byte(part)); n != len(part) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", part, n, err)
		}
	}
	if got := string(b.data); got != "defg" {
		t.Errorf("kept %q, want defg", got)
	}
}

// Nothing on the console, so what the runtime and the monitor said is all there
// is. virtiofsd's own lines stay out of that too.
func TestWarnGuestLogFallsBackToStderr(t *testing.T) {
	c, rt := deadGuestConfig(t, false)
	rt.logged = ""
	c.Runtime = &stderrOnly{rt}
	_ = c.EnsureRunning(creds.Set{})
	said := c.Err.(*bytes.Buffer).String()
	if !strings.Contains(said, "Cannot create virtio-fs device") {
		t.Errorf("the monitor's error is missing:\n%s", said)
	}
	if strings.Contains(said, "virtiofsd::limits") {
		t.Errorf("virtiofsd's warning was shown:\n%s", said)
	}
}

type stderrOnly struct{ *deadGuest }

func (s *stderrOnly) Logs(spec runtime.LogsSpec) error {
	_, err := io.WriteString(spec.Err, "[2026-10-30T09:45:05Z WARN  virtiofsd::limits] Failure\nCannot create virtio-fs device\n")
	return err
}
