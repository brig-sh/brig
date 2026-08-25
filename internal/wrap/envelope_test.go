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
