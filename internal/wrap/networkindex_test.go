package wrap

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/verify"
)

type indexedNetworkRuntime struct {
	*legacyNetworkRuntime
	exists    bool
	existsErr error
}

func (r *indexedNetworkRuntime) Exists(string) (bool, error) { return r.exists, r.existsErr }

type indexedHVIRuntime struct{ *indexedNetworkRuntime }

func (r *indexedHVIRuntime) NetworkStale(_, _, net string, _ runtime.Egress) bool {
	return net != r.network
}

func TestExistingNetworksSurviveMissingOrMismatchedSessionIndexes(t *testing.T) {
	for _, kind := range []string{"hull", "nerdctl"} {
		for _, posture := range AllNetworks() {
			for _, index := range []string{"missing", "mismatched"} {
				for _, state := range []string{"running", "stopped"} {
					t.Run(kind+"/"+string(posture)+"/"+index+"/"+state, func(t *testing.T) {
						p, home := legacySession(t, "ubuntu")
						entries := map[string]sessionEntry{}
						if index == "mismatched" {
							entries["ubuntu"] = sessionEntry{Sandbox: "brig-other", Home: home}
						}
						if err := writeSessionIndex(entries); err != nil {
							t.Fatal(err)
						}
						// A stale record without its matching index is no better
						// evidence than the profile's new isolated default.
						if err := runtime.RecordBootedNet("brig-ubuntu", "isolated"); err != nil {
							t.Fatal(err)
						}
						live := &indexedNetworkRuntime{legacyNetworkRuntime: &legacyNetworkRuntime{
							livenessRuntime: &livenessRuntime{running: state == "running", workspace: home},
							kind:            kind, network: posture.RuntimeNet(),
						}, exists: true}
						var rt runtime.Runtime = live
						if kind == "hull" {
							rt = &indexedHVIRuntime{live}
						}
						c, err := Load(p, Options{Workspace: home, NoProject: true}, rt)
						if err != nil {
							t.Fatal(err)
						}
						if c.Network != posture || live.inspections != 1 {
							t.Fatalf("resolved %s after %d inspections; want %s from the existing guest", c.Network, live.inspections, posture)
						}
						c.Verify, c.Out, c.Err = verify.Off, io.Discard, io.Discard
						if err := c.EnsureRunning(creds.Set{}); err != nil {
							t.Fatal(err)
						}
						if state == "running" {
							if live.stops != 0 || live.removes != 0 || live.boots != 0 {
								t.Fatal("lost bookkeeping caused a running guest to be recreated")
							}
						} else if live.boots != 1 || live.spec.Net != posture.RuntimeNet() {
							t.Fatalf("stopped guest booted on %q, want %q", live.spec.Net, posture.RuntimeNet())
						}
					})
				}
			}
		}
	}
}

func TestUnindexedNetworkDiscoveryErrorsDoNotAuthorizeRecreation(t *testing.T) {
	for _, source := range []string{"existence", "inspection", "another indexed ref"} {
		t.Run(source, func(t *testing.T) {
			p, home := legacySession(t, "ubuntu")
			entries := map[string]sessionEntry{}
			if source == "another indexed ref" {
				entries["old-ref"] = sessionEntry{Sandbox: "brig-ubuntu", Home: home}
			}
			if err := writeSessionIndex(entries); err != nil {
				t.Fatal(err)
			}
			live := &indexedNetworkRuntime{legacyNetworkRuntime: &legacyNetworkRuntime{
				livenessRuntime: &livenessRuntime{running: true, workspace: home}, kind: "hull",
			}, exists: source != "another indexed ref"}
			if source == "existence" {
				live.existsErr = errors.New("runtime unavailable")
			} else {
				live.inspectErr = errors.New("invalid stored configuration")
			}
			c, err := Load(p, Options{Workspace: home, NoProject: true}, live)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(c.networkLine(), "unknown") {
				t.Fatalf("uncertain existing guest was reported as %q", c.networkLine())
			}
			if err := c.EnsureRunning(creds.Set{}); err == nil {
				t.Fatal("uncertain guest was reused or recreated")
			}
			if live.stops != 0 || live.removes != 0 || live.boots != 0 {
				t.Fatal("discovery failure mutated the guest")
			}
		})
	}
}

func TestAnUnindexedSandboxAppearingAfterDiscoveryIsNotJoined(t *testing.T) {
	p, home := legacySession(t, "ubuntu")
	if err := writeSessionIndex(map[string]sessionEntry{}); err != nil {
		t.Fatal(err)
	}
	live := &indexedNetworkRuntime{legacyNetworkRuntime: &legacyNetworkRuntime{
		livenessRuntime: &livenessRuntime{workspace: home}, kind: "nerdctl",
	}}
	c, err := Load(p, Options{Workspace: home, NoProject: true}, live)
	if err != nil {
		t.Fatal(err)
	}
	live.running, live.network = true, "shared"
	if err := c.EnsureRunning(creds.Set{}); err == nil || !strings.Contains(err.Error(), "appeared") {
		t.Fatalf("newly appeared guest was accepted: %v", err)
	}
	if live.stops != 0 || live.removes != 0 || live.boots != 0 {
		t.Fatal("newly appeared guest was mutated")
	}
}
