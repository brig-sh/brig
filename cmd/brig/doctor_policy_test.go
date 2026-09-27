package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/verify"
)

// recordingCosign installs a stub cosign that writes each argv it gets, one
// line per call, and answers the way a registry holding a signed image does:
// triangulate names a digest and verify exits 0. It returns the log path.
//
// The doctor tests assert on what cosign was asked, because that is where a
// doctor reading its own policy and a run reading the host's part company. A
// finding that says "verified" proves nothing about which identity verified.
func recordingCosign(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "argv.log")
	bin := filepath.Join(dir, "cosign")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> '" + log + "'\n" +
		"if [ \"$1\" = triangulate ]; then\n" +
		"  echo \"${2%%:*}:sha256-" + strings.Repeat("a", 64) + ".sig\"\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIG_COSIGN_BIN", bin)
	t.Setenv("BRIG_VERIFY", "warn")
	// Unset, so a developer shell that sets any of them cannot decide these
	// tests. Unset and not empty: a per-agent variable set to "" is found
	// first, and its empty value then hides the global one a case sets.
	for _, k := range []string{"REGISTRY", "IDENTITY", "ISSUER"} {
		for _, name := range []string{"BRIG_VERIFY_" + k, "BRIG_X_VERIFY_" + k} {
			t.Setenv(name, "")
			os.Unsetenv(name)
		}
	}
	return log
}

// cosignVerifyCalls is the verify lines out of the stub's log.
func cosignVerifyCalls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the stub cosign was never run: %v", err)
	}
	var calls []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "verify ") {
			calls = append(calls, line)
		}
	}
	if len(calls) == 0 {
		t.Fatalf("cosign was run but never asked to verify:\n%s", b)
	}
	return calls
}

// With BRIG_VERIFY_IDENTITY set, a run checks the image against that
// identity. Doctor checked it against the shipped one, so on such a host doctor
// said ok about a signature the run refuses, and the reverse.
func TestDoctorImageUsesHostIdentity(t *testing.T) {
	healthyHost(t)
	log := recordingCosign(t)
	const mine = `^https://github\.com/me/images/\.github/workflows/sign\.yml@refs/heads/main$`
	t.Setenv("BRIG_VERIFY_IDENTITY", mine)

	agent := fakeProfile
	runDoctor(&agent, nil)

	shipped := verify.DefaultPolicy().Identity
	for _, call := range cosignVerifyCalls(t, log) {
		if strings.Contains(call, shipped) {
			t.Errorf("cosign got the shipped identity with BRIG_VERIFY_IDENTITY set: %s", call)
		}
		if !strings.Contains(call, mine) {
			t.Errorf("cosign did not get the host identity %s: %s", mine, call)
		}
	}
}

// The per-agent spelling wins over the global one for a run, so it has to for
// doctor too. fakeProfile is named x, so its override is BRIG_X_VERIFY_*.
func TestDoctorImageUsesPerAgentOverride(t *testing.T) {
	healthyHost(t)
	log := recordingCosign(t)
	const mine = `^https://github\.com/me/x/\.github/workflows/sign\.yml@refs/heads/main$`
	t.Setenv("BRIG_X_VERIFY_IDENTITY", mine)

	agent := fakeProfile
	runDoctor(&agent, nil)

	shipped := verify.DefaultPolicy().Identity
	for _, call := range cosignVerifyCalls(t, log) {
		if strings.Contains(call, shipped) {
			t.Errorf("cosign got the shipped identity with BRIG_X_VERIFY_IDENTITY set: %s", call)
		}
		if !strings.Contains(call, mine) {
			t.Errorf("cosign did not get the per-agent identity %s: %s", mine, call)
		}
	}
}

// A host that sets nothing checks against the shipped policy, and the report
// carries no trust row. The row is there to flag a host that differs, and one
// on every host is a line readers learn to skip.
func TestDoctorShippedPolicyPrintsNoTrustRow(t *testing.T) {
	healthyHost(t)
	log := recordingCosign(t)

	agent := fakeProfile
	checks := runDoctor(&agent, nil)
	var buf bytes.Buffer
	renderDoctor(&buf, checks)

	for _, c := range checks {
		if c.Name == "trust" {
			t.Errorf("a host with the shipped policy has a trust row: %+v", c)
		}
	}
	if strings.Contains(buf.String(), "replaced") {
		t.Errorf("a host with the shipped policy prints a replaced line:\n%s", buf.String())
	}
	shipped := verify.DefaultPolicy().Identity
	for _, call := range cosignVerifyCalls(t, log) {
		if !strings.Contains(call, shipped) {
			t.Errorf("cosign did not get the shipped identity: %s", call)
		}
	}
}

// A replaced policy is named in full, with no agent given too: the global
// variables apply to every run, so doctor reports them before it knows which
// agent you mean. Without an agent the per-agent spelling is not read, the same
// as a run of any other agent ignores it.
func TestDoctorNamesReplacedPolicy(t *testing.T) {
	healthyHost(t)
	recordingCosign(t)
	t.Setenv("BRIG_VERIFY_REGISTRY", "ghcr.io/me/")
	t.Setenv("BRIG_VERIFY_ISSUER", "https://issuer.example")

	checks := runDoctor(nil, nil)
	var buf bytes.Buffer
	renderDoctor(&buf, checks)
	out := buf.String()

	row := findCheck(t, checks, "trust")
	for _, want := range []string{"ghcr.io/me/", verify.DefaultPolicy().Identity, "https://issuer.example"} {
		if !strings.Contains(row.Finding, want) {
			t.Errorf("the trust row does not name %q: %s", want, row.Finding)
		}
		if !strings.Contains(out, want) {
			t.Errorf("the rendered report does not name %q:\n%s", want, out)
		}
	}
	if row.State == stateFail {
		t.Errorf("a replaced policy is the host's choice, not a failure: %+v", row)
	}
}

// Without an agent a per-agent override applies to nothing, so it must not
// show up as the host's policy.
func TestDoctorIgnoresPerAgentOverrideWithoutAgent(t *testing.T) {
	healthyHost(t)
	recordingCosign(t)
	t.Setenv("BRIG_X_VERIFY_IDENTITY", "^anything$")

	for _, c := range runDoctor(nil, nil) {
		if c.Name == "trust" {
			t.Errorf("a per-agent override was reported with no agent named: %+v", c)
		}
	}
}
