package wrap

import (
	"bytes"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/secret"
)

// fakeRuntime answers the two questions a status report asks. The interface is
// embedded rather than implemented: a report that reached any other method
// would be doing something a report has no business doing, and a nil panic
// says that louder than a stub returning nothing.
type fakeRuntime struct{ runtime.Runtime }

func (fakeRuntime) Kind() string { return "hull" }
func (fakeRuntime) Bin() string  { return "hull" }

const credentialProfile = "hostCredential:\n  keychainService: s\n" +
	"  tokenField: accessToken\n  expiryField: expiresAt\n" +
	"  targetVar: TOK\n  renewHint: run it once\n"

func statusOutput(t *testing.T, body string, set creds.Set) string {
	t.Helper()
	c := bindingConfig(t, body)
	out := &bytes.Buffer{}
	c.Out = out
	c.Runtime = fakeRuntime{}
	c.Status(set)
	return out.String()
}

// With no runtime on PATH, env reports what it can and marks the one line that
// needs a runtime unavailable, rather than failing the whole report -- the
// reader is often the one whose runtime is what broke.
func TestStatusReportsWithoutARuntime(t *testing.T) {
	c := bindingConfig(t, credentialProfile)
	out := &bytes.Buffer{}
	c.Out = out
	c.Runtime = nil // no runtime detected
	c.Status(creds.Set{})
	got := out.String()
	if !strings.Contains(got, "runtime unavailable") {
		t.Errorf("the runtime line was not marked unavailable:\n%s", got)
	}
	// The rest of the report is still there: the profile's image is knowable
	// without a runtime, and is the kind of line env exists to print.
	if !strings.Contains(got, "image ") {
		t.Errorf("the report dropped the lines it could still give:\n%s", got)
	}
}

// Set.Names annotates a store-sourced variable as "TOK(secret)", the way it
// already annotates the host credential's as "TOK(host)". An exact-string
// check against the bare name misses it, and because a secret-bound run
// correctly skips the host credential read, HostCred is nil and the report
// falls through to "no host credential found" -- about a sandbox that
// authenticates perfectly well from the keychain.
func TestStatusReportsAGuestLoginBoundFromTheStore(t *testing.T) {
	var set creds.Set
	set.AddSecret("TOK", "s3cr3t", "TOK(secret)")

	got := statusOutput(t, "secrets:\n  - tok\nenv:\n  - name: TOK\n    ref: secrets.tok\n"+
		credentialProfile, set)

	if strings.Contains(got, "no host credential found") {
		t.Errorf("a secret-bound login was reported as no credential at all:\n%s", got)
	}
	if !strings.Contains(got, "guest login: from TOK in the secret store") {
		t.Errorf("the login was not reported as coming from the store:\n%s", got)
	}
	if strings.Contains(got, "s3cr3t") {
		t.Errorf("the status report printed a value:\n%s", got)
	}
}

// The environment case keeps its own wording: the annotation is what tells the
// two apart, so stripping it must not merge them.
func TestStatusStillDistinguishesTheEnvironmentAndTheHost(t *testing.T) {
	var fromEnv creds.Set
	fromEnv.Add("TOK", "t", "TOK")
	got := statusOutput(t, credentialProfile, fromEnv)
	if !strings.Contains(got, "guest login: from TOK in the environment") {
		t.Errorf("an environment-sourced login was misreported:\n%s", got)
	}

	var fromHost creds.Set
	fromHost.AddSecret("TOK", "t", "TOK(host)")
	// Assert the annotation directly: the reported wording below is reached on
	// HostCred alone, so it survives a sourceOf that has regressed to matching
	// the whole string. This is what pins "(host)" the way the store test above
	// pins "(secret)".
	if source, ok := sourceOf(fromHost.Names, "TOK"); !ok || source != "host" {
		t.Errorf("sourceOf(%q) = %q, %v; want \"host\", true", fromHost.Names, source, ok)
	}
	c := bindingConfig(t, credentialProfile)
	out := &bytes.Buffer{}
	c.Out = out
	c.Runtime = fakeRuntime{}
	c.HostCred = &creds.HostCredential{Token: "t", Source: "the host keychain"}
	c.Status(fromHost)
	if !strings.Contains(out.String(), "guest login: from the host keychain") {
		t.Errorf("a host-sourced login was misreported:\n%s", out.String())
	}
}

// What a run could hand the guest is reported by name, and the "forwarding
// nothing" line names the bindings rather than the retired Forward list.
func TestStatusNamesTheBindingsWhenNothingIsForwarded(t *testing.T) {
	got := statusOutput(t, "env:\n  - name: A\n    ref: env.A\n", creds.Set{})
	if !strings.Contains(got, "forwarding no credentials (none of: A)") {
		t.Errorf("the empty report does not name the bindings:\n%s", got)
	}
}

// The status report says where the guest login comes from and whether it is
// still good, without reading a value -- the same question the old
// HostCredential block answered, asked of the store instead.
func TestStatusReportsTheImportedLogin(t *testing.T) {
	const now = 1755436980000
	old := nowMilli
	nowMilli = func() int64 { return now }
	t.Cleanup(func() { nowMilli = old })

	newConfig := func(expiresAt int64) *Config {
		return &Config{
			Out:     &bytes.Buffer{},
			Err:     &bytes.Buffer{},
			Runtime: fakeRuntime{},
			Profile: profile.Profile{
				Name:    "claude-code",
				Secrets: []profile.SecretDecl{{Name: "claude-credentials", Required: ptr(false)}},
			},
			OpenStore: func() (creds.SecretReader, error) {
				return listingStore{{
					Name: "claude-credentials",
					Provenance: secret.Provenance{
						V:         secret.ProvenanceVersion,
						From:      "keychain:Claude Code-credentials",
						ExpiresAt: expiresAt,
					},
				}}, nil
			},
		}
	}

	good := newConfig(now + 3*60*60*1000)
	good.Status(creds.Set{})
	if got := good.Out.(*bytes.Buffer).String(); !strings.Contains(got,
		"guest login: claude-credentials, imported from keychain:Claude Code-credentials\n") {
		t.Errorf("a good login was not reported:\n%s", got)
	}

	expired := newConfig(now - 1)
	expired.Status(creds.Set{})
	if got := expired.Out.(*bytes.Buffer).String(); !strings.Contains(got,
		"guest login: claude-credentials, imported from keychain:Claude Code-credentials (EXPIRED)\n") {
		t.Errorf("an expired login was not reported:\n%s", got)
	}
}
