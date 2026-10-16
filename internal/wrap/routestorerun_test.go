package wrap

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/secret"
	"github.com/brig-sh/brig/internal/verify"
)

// tagHull is hull on hvi that boots by tag, so a verification runs.
type tagHull struct{ *hullRecorder }

func (tagHull) PinsDigest() bool { return false }

// routedOnHvi is claude-code bound to the Claude route on hull on hvi, with
// a fake broker and the store open.
func routedOnHvi(t *testing.T, open func() (creds.SecretReader, error)) (*Config, *hullRecorder, *fakeBroker) {
	t.Helper()
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	rec := &hullRecorder{}
	cfg := routedConfig(t, claudeEndpoint, tagHull{rec})
	shortStateDir(t)
	cfg.Verify = verify.Off
	cfg.OpenStore = open
	broker := &fakeBroker{}
	cfg.broker = broker
	return cfg, rec, broker
}

// A route whose secret is not in the store is refused before boot, with the
// exit code of a credential and the command that imports it. Neither the
// broker nor the runtime is asked anything.
func TestARoutedRunWithItsSecretMissingIsRefusedBeforeBoot(t *testing.T) {
	cfg, rec, broker := routedOnHvi(t, func() (creds.SecretReader, error) {
		return fakeSecretStore{"gh-token": "x"}, nil
	})
	set, err := cfg.BuildEnv()
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.EnsureRunning(set)
	var ce creds.CredentialError
	if !errors.As(err, &ce) {
		t.Fatalf("EnsureRunning = %v, want a credential refusal", err)
	}
	for _, want := range []string{`"claude-credentials"`, "brig secret import claude-code", "brokered (route anthropic)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
	if !errors.Is(err, secret.ErrNotFound) {
		t.Errorf("the refusal does not unwrap to ErrNotFound: %v", err)
	}
	if len(broker.registered) != 0 || len(broker.minted) != 0 {
		t.Errorf("the broker was asked: registered %v, minted %v", broker.registered, broker.minted)
	}
	if rec.spec != nil {
		t.Error("a run whose route has no secret booted")
	}
}

// With no store on the host, a route is refused before boot too, saying a
// Secret Service is what it needs. The broker's 502 is for a store that goes
// away after the boot.
func TestARoutedRunWithNoStoreIsRefusedBeforeBoot(t *testing.T) {
	cfg, rec, broker := routedOnHvi(t, func() (creds.SecretReader, error) {
		return nil, secret.ErrUnsupported
	})
	set, err := cfg.BuildEnv()
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.EnsureRunning(set)
	var ce creds.CredentialError
	if !errors.As(err, &ce) || !errors.Is(err, secret.ErrUnsupported) {
		t.Fatalf("EnsureRunning = %v, want a credential refusal for the store", err)
	}
	for _, want := range []string{"Secret Service", "claude-credentials", "brig secret import claude-code"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "--from-command") {
		t.Errorf("the refusal offers --from-command, which writes to the same store: %v", err)
	}
	if len(broker.registered) != 0 || rec.spec != nil {
		t.Errorf("the broker (%v) or the runtime (%v) was asked", broker.registered, rec.spec != nil)
	}
}

// A run that stops before it boots leaves nothing registered with the broker:
// a refused verification never registers, and a boot that fails forgets the
// sandbox again, which stops a broker that serves no other.
func TestARunThatStopsBeforeBootLeavesTheBrokerNothing(t *testing.T) {
	store := func() (creds.SecretReader, error) { return fakeSecretStore{"claude-credentials": "{}"}, nil }

	cfg, rec, broker := routedOnHvi(t, store)
	cfg.Verify = verify.Require
	old := checkTag
	checkTag = func(verify.Policy, string) verify.Result { return verify.Result{Outcome: verify.Failed} }
	t.Cleanup(func() { checkTag = old })
	err := cfg.EnsureRunning(creds.Set{})
	var vr *VerifyRefusedError
	if !errors.As(err, &vr) {
		t.Fatalf("EnsureRunning = %v, want a verification refusal", err)
	}
	if len(broker.registered) != 0 || rec.spec != nil {
		t.Errorf("a refused verification registered %v or booted", broker.registered)
	}
	if !onlySandbox(broker.forgotten, cfg.VMName) {
		t.Errorf("forgotten %v, want the sandbox, which an earlier run may have registered", broker.forgotten)
	}

	cfg, rec, broker = routedOnHvi(t, store)
	err = cfg.EnsureRunning(creds.Set{})
	if err == nil || !strings.Contains(err.Error(), "not booting") || rec.spec == nil {
		t.Fatalf("the run did not reach the runtime: %v", err)
	}
	if len(broker.registered) != 1 {
		t.Fatalf("registered %v, want the sandbox once", broker.registered)
	}
	if !onlySandbox(broker.forgotten, cfg.VMName) {
		t.Errorf("forgotten %v after a failed boot, want the sandbox", broker.forgotten)
	}
}

// A routed run removes the sandbox from the broker before it registers it.
// The registration is then a new one, with a new key, so a token of the
// guest before it is refused although the routes are the same. A removal
// after the registration would revoke the new guest's tokens instead.
func TestARoutedBootForgetsBeforeItRegisters(t *testing.T) {
	cfg, rec, broker := routedOnHvi(t, func() (creds.SecretReader, error) {
		return fakeSecretStore{"claude-credentials": "{}"}, nil
	})
	if err := cfg.EnsureRunning(creds.Set{}); rec.spec == nil {
		t.Fatalf("the run did not reach the runtime: %v", err)
	}
	want := []string{"forget " + cfg.VMName, "register " + cfg.VMName}
	if len(broker.calls) < 2 || !slices.Equal(broker.calls[:2], want) {
		t.Errorf("the broker was asked %v, want %v first", broker.calls, want)
	}
}

// onlySandbox reports whether forgotten names vmName, and nothing else. The
// run forgets the sandbox before the boot and again when the boot fails.
func onlySandbox(forgotten []string, vmName string) bool {
	for _, id := range forgotten {
		if id != vmName {
			return false
		}
	}
	return len(forgotten) > 0
}

// brig info and brig plan name a routed secret the store does not hold, and
// the plan says the run is refused.
func TestInfoAndPlanNameAMissingRoutedSecret(t *testing.T) {
	cfg, _, _ := routedOnHvi(t, func() (creds.SecretReader, error) { return fakeSecretStore{}, nil })
	if got := cfg.brokerLine(); !strings.Contains(got,
		"not in brig's secret store: claude-credentials, so the run is refused; brig secret import claude-code") {
		t.Errorf("BROKER row: %q", got)
	}
	d := cfg.Plan()
	if d.Refused == nil || !strings.Contains(d.Refused.Reason, `missing secret "claude-credentials"`) {
		t.Errorf("plan refused = %+v", d.Refused)
	}
	cfg.Out = &bytes.Buffer{}
	cfg.PrintPlan(d)
	out := cfg.Out.(*bytes.Buffer).String()
	if !strings.Contains(out, "BROKER ") || !strings.Contains(out, "not in brig's secret store: claude-credentials") {
		t.Errorf("the plan has no BROKER row naming the missing secret:\n%s", out)
	}
}
