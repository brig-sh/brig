package wrap

import (
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
)

// unreadyRuntime boots but never answers the readiness probe.
type unreadyRuntime struct{ *livenessRuntime }

func (unreadyRuntime) Probe(runtime.ExecSpec) bool { return false }

// A boot that never became ready is recorded as failed.
func TestABootThatNeverBecomesReadyIsRecordedFailed(t *testing.T) {
	live := &livenessRuntime{}
	c := livenessConfig(t, live)
	c.Runtime = unreadyRuntime{live}
	c.ReadyTimeout = time.Millisecond

	if err := c.EnsureRunning(creds.Set{}); err == nil {
		t.Fatal("a sandbox that never answered was reported ready")
	}
	rec, ok, err := runtime.LastBoot(c.VMName)
	if err != nil || !ok {
		t.Fatalf("no boot recorded: ok=%v err=%v", ok, err)
	}
	if rec.Result != runtime.BootFailed {
		t.Errorf("result %q, want failed", rec.Result)
	}
}

// A boot that became ready is recorded ok, replacing an earlier failure.
func TestABootThatBecomesReadyIsRecordedOK(t *testing.T) {
	live := &livenessRuntime{}
	c := livenessConfig(t, live)
	_ = runtime.RecordBoot(c.VMName, runtime.BootRecord{Result: runtime.BootFailed, At: time.Now()})

	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := runtime.LastBoot(c.VMName); rec.Result != runtime.BootOK {
		t.Errorf("result %q, want ok", rec.Result)
	}
}

// A sandbox that was already running gets no record.
func TestAnAlreadyRunningSandboxRecordsNoBoot(t *testing.T) {
	live := &livenessRuntime{running: true}
	c := livenessConfig(t, live)
	booted(c)

	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := runtime.LastBoot(c.VMName); ok {
		t.Error("a sandbox this command did not boot got a boot record")
	}
}

// Removing the sandbox drops its record.
func TestRemoveForgetsTheBoot(t *testing.T) {
	live := &livenessRuntime{}
	c := livenessConfig(t, live)
	_ = runtime.RecordBoot(c.VMName, runtime.BootRecord{Result: runtime.BootFailed, At: time.Now()})

	_ = c.Remove()
	if _, ok, _ := runtime.LastBoot(c.VMName); ok {
		t.Error("the removed sandbox still has a boot record")
	}
}

// The record names the image and digest this boot used, ready or not.
func TestTheBootRecordNamesTheImageAndDigestBooted(t *testing.T) {
	digest := "sha256:" + strings.Repeat("0f", 32)
	for name, ready := range map[string]bool{"failed": false, "ok": true} {
		t.Run(name, func(t *testing.T) {
			live := &livenessRuntime{}
			c := livenessConfig(t, live)
			if !ready {
				c.Runtime = unreadyRuntime{live}
				c.ReadyTimeout = time.Millisecond
			}
			c.Image = "ghcr.io/brig-sh/claude-code:test"
			c.BootDigest = digest

			err := c.EnsureRunning(creds.Set{})
			if ready != (err == nil) {
				t.Fatalf("EnsureRunning: %v", err)
			}
			rec, ok, err := runtime.LastBoot(c.VMName)
			if err != nil || !ok {
				t.Fatalf("no boot recorded: ok=%v err=%v", ok, err)
			}
			if rec.Digest != digest {
				t.Errorf("digest %q, want the one the boot used, %q", rec.Digest, digest)
			}
			if rec.Image != c.Image {
				t.Errorf("image %q, want the one the boot used, %q", rec.Image, c.Image)
			}
		})
	}
}
