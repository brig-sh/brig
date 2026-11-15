package wrap

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
)

// keptRuntime is livenessRuntime whose removal fails, and which says whether
// the sandbox is still there afterwards.
type keptRuntime struct {
	livenessRuntime
	removeErr error
	exists    bool
	existsErr error
	asked     int
}

func (r *keptRuntime) Remove(string) error { r.removes++; return r.removeErr }
func (r *keptRuntime) Exists(string) (bool, error) {
	r.asked++
	return r.exists, r.existsErr
}

// keptConfig is livenessConfig on rt.
func keptConfig(t *testing.T, rt *keptRuntime) *Config {
	t.Helper()
	c := livenessConfig(t, &rt.livenessRuntime)
	c.Runtime = rt
	return c
}

// What `hull rm` on Linux says when it keeps an instance it could not detach
// from its CNI network.
const hullKeptIt = "hull rm brig-x: exit status 1: could not detach it from its network, so it is kept: " +
	"bridge: hook output; hull rm tries again, and hull rm --force removes it without the detach"

// A removal that fails and leaves the sandbox stops the run with the
// runtime's reason. Ignored, it reached the boot as "instance exists", which
// says neither why the name is taken nor what to do. The broker forgets the
// guest first, so the guest the runtime keeps holds no valid token.
func TestEnsureRunningStopsWhenTheRuntimeKeepsTheSandbox(t *testing.T) {
	for name, running := range map[string]bool{"restart": true, "stopped": false} {
		t.Run(name, func(t *testing.T) {
			rt := &keptRuntime{livenessRuntime: livenessRuntime{running: running}, removeErr: errors.New(hullKeptIt), exists: true}
			c := keptConfig(t, rt)
			// The running guest no longer mounts this workspace, so brig
			// restarts it.
			rt.workspace = t.TempDir()
			broker := &fakeBroker{}
			c.broker = broker

			err := c.EnsureRunning(creds.Set{})

			if err == nil {
				t.Fatal("a run went ahead over a sandbox the runtime kept")
			}
			if rt.boots != 0 {
				t.Errorf("booted %d times over a sandbox the runtime kept", rt.boots)
			}
			for _, want := range []string{c.VMName, "could not be removed", "so it is kept", "hull rm tries again"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not say %q: %v", want, err)
				}
			}
			if !slices.Contains(broker.forgotten, c.VMName) {
				t.Errorf("forgotten %v, want the kept guest removed from the broker", broker.forgotten)
			}
			if len(broker.registered) != 0 {
				t.Errorf("a run that never booted registered %+v", broker.registered)
			}
		})
	}
}

// A removal fails on every first boot, since nothing holds the name. That is
// no reason to stop, and the boot goes ahead.
func TestEnsureRunningBootsWhenNothingHeldTheName(t *testing.T) {
	rt := &keptRuntime{removeErr: errors.New("hull rm brig-x: exit status 1: instance not found: brig-x")}
	c := keptConfig(t, rt)

	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if rt.asked != 1 {
		t.Errorf("asked whether the sandbox is there %d times, want once", rt.asked)
	}
	if rt.boots != 1 {
		t.Errorf("booted %d times, want once", rt.boots)
	}
}

// A runtime that cannot say whether the sandbox is there leaves the boot to
// go ahead as it did before: the runtime refuses a name that is taken, so
// the boot cannot take it from a second sandbox.
func TestEnsureRunningBootsWhenTheRuntimeCannotSayWhatItKept(t *testing.T) {
	rt := &keptRuntime{removeErr: errors.New(hullKeptIt), existsErr: errors.New("cannot connect to containerd")}
	c := keptConfig(t, rt)

	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if rt.boots != 1 {
		t.Errorf("booted %d times, want once", rt.boots)
	}
}

// A removal that succeeds needs no second question.
func TestEnsureRunningAsksNothingMoreAfterARemoval(t *testing.T) {
	rt := &keptRuntime{}
	c := keptConfig(t, rt)

	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if rt.asked != 0 {
		t.Errorf("asked whether the sandbox is there %d times after a removal that worked", rt.asked)
	}
	if rt.boots != 1 {
		t.Errorf("booted %d times, want once", rt.boots)
	}
}
