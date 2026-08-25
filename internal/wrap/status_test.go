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

// The hatch is opted into once, in a shell profile, and then said nowhere. A
// run that puts a credential on the command line has to say so on the run, not
// only in the documentation for the setting -- and it has to name the variable,
// because "some value" is not something a user can act on.
func TestBuildEnvWarnsWhenValuesWouldReachArgv(t *testing.T) {
	t.Setenv("BRIG_ENV_ARGV", "1")
	t.Setenv("GH_TOKEN", "ghp_secret")
	c := bindingConfig(t, "env:\n  - name: GH_TOKEN\n    ref: env.GH_TOKEN\n")

	set, err := c.BuildEnv()
	if err != nil {
		t.Fatal(err)
	}
	// The premise of the warning: this value really is the one that lands on
	// the command line, which is what an ambient value not being marked Secret
	// means.
	for _, v := range set.Vars {
		if v.Name == "GH_TOKEN" && v.Secret {
			t.Fatal("GH_TOKEN is exempt from the hatch, so there is nothing to warn about")
		}
	}

	var warnings []string
	for _, line := range strings.Split(c.Err.(*bytes.Buffer).String(), "\n") {
		if strings.Contains(line, "BRIG_ENV_ARGV") {
			warnings = append(warnings, line)
		}
	}
	if len(warnings) != 1 {
		t.Fatalf("want exactly one warning, got %d:\n%s", len(warnings),
			strings.Join(warnings, "\n"))
	}
	if !strings.Contains(warnings[0], "GH_TOKEN") {
		t.Errorf("the warning does not name the variable: %s", warnings[0])
	}
	if strings.Contains(warnings[0], "ghp_secret") {
		t.Errorf("the warning printed the value it is warning about: %s", warnings[0])
	}
}

// Off is the default, and the default run must be exactly as quiet as it was.
func TestBuildEnvSaysNothingWithTheHatchOff(t *testing.T) {
	t.Setenv("BRIG_ENV_ARGV", "")
	t.Setenv("GH_TOKEN", "ghp_secret")
	c := bindingConfig(t, "env:\n  - name: GH_TOKEN\n    ref: env.GH_TOKEN\n")

	if _, err := c.BuildEnv(); err != nil {
		t.Fatal(err)
	}
	if got := c.Err.(*bytes.Buffer).String(); strings.Contains(got, "BRIG_ENV_ARGV") {
		t.Errorf("a run with the hatch off mentioned it:\n%s", got)
	}
}

// `brig env` is where a user asks what a sandbox is about to be handed, so it
// is where the setting that decides how those values travel belongs.
func TestStatusReportsTheArgvHatch(t *testing.T) {
	t.Setenv("BRIG_ENV_ARGV", "1")
	var set creds.Set
	set.Add("GH_TOKEN", "ghp_secret", "GH_TOKEN")
	set.AddSecret("TOK", "s3cr3t", "TOK(secret)")

	got := statusOutput(t, "env:\n  - name: GH_TOKEN\n    ref: env.GH_TOKEN\n", set)
	if !strings.Contains(got, "BRIG_ENV_ARGV=1") {
		t.Errorf("the report does not mention the setting:\n%s", got)
	}
	if !strings.Contains(got, "GH_TOKEN") {
		t.Errorf("the report does not name what goes on the command line:\n%s", got)
	}
	// A value brig resolved is exempt whatever the hatch says, so naming it
	// here would be a false statement about the command line.
	for _, v := range []string{"TOK ", "ghp_secret", "s3cr3t"} {
		if strings.Contains(got, v) {
			t.Errorf("the report carries %q, which does not reach argv:\n%s", v, got)
		}
	}
}

// With the hatch off, nothing about it is reported: brig keeps values out of
// argv as a matter of course, and a line saying so on every preview would bury
// the lines that are about this run.
func TestStatusIsSilentAboutTheArgvHatchWhenItIsOff(t *testing.T) {
	t.Setenv("BRIG_ENV_ARGV", "")
	var set creds.Set
	set.Add("GH_TOKEN", "ghp_secret", "GH_TOKEN")

	got := statusOutput(t, "env:\n  - name: GH_TOKEN\n    ref: env.GH_TOKEN\n", set)
	if strings.Contains(got, "BRIG_ENV_ARGV") {
		t.Errorf("the hatch was reported while off:\n%s", got)
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
