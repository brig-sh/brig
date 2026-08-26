package wrap

import (
	"bytes"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
)

// envelopeConfig is a run resolved far enough to print its envelope: a named
// session on a profile that delivers one credential as a file, with a runtime
// that answers the one question the block asks of it.
func envelopeConfig() *Config {
	return &Config{
		RawName:   "review",
		VMName:    "brig-claude-code-review",
		Workspace: "/Users/me/src/brig",
		Image:     "ghcr.io/brig-sh/claude-code:latest",
		Pull:      "missing",
		Runtime:   fakeRuntime{},
		Profile: profile.Profile{
			Name: "claude-code",
			Files: []profile.FileBinding{{
				Ref:  "secrets.claude-credentials",
				Path: ".claude/.credentials.json",
				Mode: "0600",
			}},
		},
		Out: &bytes.Buffer{},
		Err: &bytes.Buffer{},
	}
}

// The block names the boundary a run is about to trust: the session, the
// profile, the sandbox and its runtime, the workspace, the image, and every
// credential handed in -- the file-delivered one and the environment one alike.
func TestEnvelopeNamesTheBoundary(t *testing.T) {
	c := envelopeConfig()
	c.secrets = creds.Resolution{Values: map[string]string{"claude-credentials": "sk-fixture"}}
	var set creds.Set
	set.Add("GH_TOKEN", "gh-fixture", "GH_TOKEN")

	out := &bytes.Buffer{}
	c.renderEnvelope(out, set)
	got := out.String()

	for _, want := range []string{
		"SESSION      review",
		"PROFILE      claude-code",
		"SANDBOX      brig-claude-code-review (hull)",
		"WORKSPACE    /Users/me/src/brig (read-write)",
		"IMAGE        ghcr.io/brig-sh/claude-code:latest (pull missing)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the block does not carry %q:\n%s", want, got)
		}
	}
	// Both delivery paths reach the CREDENTIALS row: the store secret written as
	// a file, and the variable forwarded through the environment.
	if !strings.Contains(got, "claude-credentials") || !strings.Contains(got, "GH_TOKEN") {
		t.Errorf("the CREDENTIALS row is missing a credential:\n%s", got)
	}
}

// An unnamed run has no session of its own, so the row is left out rather than
// printed empty -- the profile and sandbox rows already say what a bare run is.
func TestEnvelopeOmitsSessionWhenUnnamed(t *testing.T) {
	c := envelopeConfig()
	c.RawName = ""
	out := &bytes.Buffer{}
	c.renderEnvelope(out, creds.Set{})
	if strings.Contains(out.String(), "SESSION") {
		t.Errorf("an unnamed run printed a SESSION row:\n%s", out.String())
	}
}

// A profile that forwards nothing says so, rather than trailing an empty row.
func TestEnvelopeSaysNoneWhenNothingIsForwarded(t *testing.T) {
	c := envelopeConfig()
	c.Profile.Files = nil
	out := &bytes.Buffer{}
	c.renderEnvelope(out, creds.Set{})
	if !strings.Contains(out.String(), "CREDENTIALS  (none)") {
		t.Errorf("the empty credential case is not reported:\n%s", out.String())
	}
}

// The single promise of the whole block: it names credentials, never their
// values. The fixture value is recognisable on purpose, and it must not appear
// in the block or in the full `brig info` report -- neither through the
// environment set nor through the file-delivered secret whose value the row is
// built beside.
func TestEnvelopeNeverPrintsACredentialValue(t *testing.T) {
	const recognisable = "sk-secret-value-do-not-print-me"
	c := envelopeConfig()
	c.secrets = creds.Resolution{Values: map[string]string{"claude-credentials": recognisable}}
	var set creds.Set
	set.AddSecret("ANTHROPIC_API_KEY", recognisable, "ANTHROPIC_API_KEY(secret)")

	block := &bytes.Buffer{}
	c.renderEnvelope(block, set)
	if strings.Contains(block.String(), recognisable) {
		t.Errorf("the block printed a credential value:\n%s", block.String())
	}

	// Info is the block plus the full report; the same value must not surface
	// anywhere in it either.
	report := &bytes.Buffer{}
	c.Out = report
	c.Info(set)
	if strings.Contains(report.String(), recognisable) {
		t.Errorf("brig info printed a credential value:\n%s", report.String())
	}
}

// brig info and brig env answer without a runtime on PATH: the runtime is one
// line of the report, and the person reading it is often the one whose runtime
// is what broke. The SANDBOX row names the runtime kind, so it is the row that
// has to say "unavailable" rather than dereference a runtime that is not there.
func TestEnvelopeReportsWithoutARuntime(t *testing.T) {
	c := testConfig(t, t.TempDir(), t.TempDir())
	c.Runtime = nil
	out := &bytes.Buffer{}
	c.Out = out

	c.renderEnvelope(out, creds.Set{})

	got := out.String()
	if !strings.Contains(got, "SANDBOX") {
		t.Fatalf("the envelope dropped the sandbox row without a runtime:\n%s", got)
	}
	if !strings.Contains(got, "unavailable") {
		t.Errorf("the sandbox row does not mark the runtime unavailable:\n%s", got)
	}
}

// A malformed file ref cannot reach here through a loaded profile: Validate
// refuses one at load, for a file profile and a built-in alike. If one ever
// did, the envelope must not name a credential it cannot identify, and the run
// must not proceed on the strength of a report that stayed quiet: writeSecretFiles
// hard-errors on the same ref. So the contract is under-report, then fail --
// never report a file as delivered when the binding naming it is unreadable.
func TestEnvelopeUnderReportsAMalformedFileRefAndTheRunThenFails(t *testing.T) {
	c := envelopeConfig()
	c.Profile.Files = []profile.FileBinding{{
		Ref:  "no-namespace-here",
		Path: ".claude/.credentials.json",
		Mode: "0600",
	}}
	c.secrets = creds.Resolution{Values: map[string]string{"claude-credentials": "v"}}

	block := &bytes.Buffer{}
	c.renderEnvelope(block, creds.Set{})

	got := block.String()
	if !strings.Contains(got, "CREDENTIALS  (none)") {
		t.Errorf("the envelope named a credential it could not identify:\n%s", got)
	}
	if strings.Contains(got, "claude-credentials") {
		t.Errorf("the envelope reported a file the malformed ref never named:\n%s", got)
	}

	// The other half: the run does not quietly carry on past it.
	if err := c.writeSecretFiles(); err == nil {
		t.Error("a malformed file ref was written rather than failing the run")
	}
}

// The posture is a fact about the sandbox, so it belongs in the block that
// names the boundary rather than in a setting a reader has to go looking for.
func TestEnvelopeNamesTheNetworkPosture(t *testing.T) {
	for _, tc := range []struct{ posture, want string }{
		{"shared", "shared"},
		{"offline", "offline"},
	} {
		c := envelopeConfig()
		c.Network = Network(tc.posture)
		block := &bytes.Buffer{}
		c.renderEnvelope(block, creds.Set{})
		got := block.String()
		if !strings.Contains(got, "NETWORK") {
			t.Fatalf("%s: no NETWORK row:\n%s", tc.posture, got)
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: the row does not name the posture:\n%s", tc.posture, got)
		}
	}
	// Offline says what it means for the reader, not only the word.
	c := envelopeConfig()
	c.Network = NetOffline
	block := &bytes.Buffer{}
	c.renderEnvelope(block, creds.Set{})
	if !strings.Contains(block.String(), "no egress") {
		t.Errorf("the offline row does not say what it costs:\n%s", block.String())
	}
}
