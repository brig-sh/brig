package wrap

import (
	"bytes"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/secret"
	"github.com/brig-sh/brig/internal/verify"
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

// The warning belongs to the run, not to BuildEnv. BuildEnv resolves the set
// for every verb, brig env included, so a warning from there is printed on a
// preview that spawns nothing and lands twice on env, next to reportArgv.
// BuildEnv stays silent about the hatch; EnsureRunning is where the run says it.
func TestBuildEnvDoesNotWarnAboutArgv(t *testing.T) {
	t.Setenv("BRIG_ENV_ARGV", "1")
	t.Setenv("GH_TOKEN", "ghp_secret")
	c := bindingConfig(t, "env:\n  - name: GH_TOKEN\n    ref: env.GH_TOKEN\n")

	if _, err := c.BuildEnv(); err != nil {
		t.Fatal(err)
	}
	if got := c.Err.(*bytes.Buffer).String(); strings.Contains(got, "BRIG_ENV_ARGV") {
		t.Errorf("BuildEnv warned about the hatch, which doubles it on brig env:\n%s", got)
	}
}

// The hatch is opted into once, in a shell profile, and then said nowhere. A
// run that puts a credential on the command line has to say so on the run, and
// name the variable, because "some value" is not something a user can act on.
// The run says it, not BuildEnv, so a preview does not repeat it. Forced to
// refuse at the hypervisor floor, EnsureRunning still warns first: the warning
// is before anything the runtime touches.
func TestEnsureRunningWarnsWhenValuesWouldReachArgv(t *testing.T) {
	t.Setenv("BRIG_ENV_ARGV", "1")
	t.Setenv("GH_TOKEN", "ghp_secret")
	c := bindingConfig(t, "env:\n  - name: GH_TOKEN\n    ref: env.GH_TOKEN\n")
	// A space in the workspace makes PrepareWorkspace refuse at the very start
	// of EnsureRunning, so the run stops before it touches a runtime and the
	// test can assert the warning was printed first.
	c.Workspace = "/brig warns first/ws"

	set, err := c.BuildEnv()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range set.Vars {
		if v.Name == "GH_TOKEN" && v.Secret {
			t.Fatal("GH_TOKEN is exempt from the hatch, so there is nothing to warn about")
		}
	}
	// Clear whatever BuildEnv said, so the warning this test finds can only have
	// come from EnsureRunning. Without this the test passes on the old code too,
	// where BuildEnv is the one warning.
	c.Err.(*bytes.Buffer).Reset()
	if err := c.EnsureRunning(set); err == nil {
		t.Fatal("a workspace with a space must refuse, so the test can assert the warning came first")
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

// Off is the default, and a run with the hatch off must be exactly as quiet as
// it was.
func TestEnsureRunningSaysNothingWithTheHatchOff(t *testing.T) {
	t.Setenv("BRIG_ENV_ARGV", "")
	t.Setenv("GH_TOKEN", "ghp_secret")
	c := bindingConfig(t, "env:\n  - name: GH_TOKEN\n    ref: env.GH_TOKEN\n")
	c.Workspace = "/brig warns first/ws"

	set, err := c.BuildEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureRunning(set); err == nil {
		t.Fatal("a workspace with a space must refuse")
	}
	if got := c.Err.(*bytes.Buffer).String(); strings.Contains(got, "BRIG_ENV_ARGV") {
		t.Errorf("a run with the hatch off mentioned it:\n%s", got)
	}
}

// brig env resolves the set with BuildEnv, like every verb, then previews it
// with Status. The hatch line must appear once, from reportArgv, not also from
// a run warning BuildEnv should not be making.
func TestEnvReportsTheArgvHatchExactlyOnce(t *testing.T) {
	t.Setenv("BRIG_ENV_ARGV", "1")
	t.Setenv("GH_TOKEN", "ghp_secret")
	c := bindingConfig(t, "env:\n  - name: GH_TOKEN\n    ref: env.GH_TOKEN\n")
	c.Runtime = fakeRuntime{}

	set, err := c.BuildEnv()
	if err != nil {
		t.Fatal(err)
	}
	c.Status(set)

	lines := 0
	for _, w := range []*bytes.Buffer{c.Out.(*bytes.Buffer), c.Err.(*bytes.Buffer)} {
		for _, line := range strings.Split(w.String(), "\n") {
			if strings.Contains(line, "command line") {
				lines++
			}
		}
	}
	if lines != 1 {
		t.Fatalf("brig env should mention the command line once, got %d", lines)
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

// brig info is where a user asks what a sandbox is about to be handed, and the
// verify mode decides whether any of it was checked. It was not in the report
// at all, so no command would tell a user that checking was off or that the
// policy had been replaced.
func TestStatusReportsTheVerifyMode(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		c := bindingConfig(t, "")
		c.Runtime = fakeRuntime{}
		c.Verify = verify.Off
		out := &bytes.Buffer{}
		c.Out = out
		c.Status(creds.Set{})
		if got := out.String(); !strings.Contains(got, "off") || !strings.Contains(got, "verif") {
			t.Errorf("the report does not say verification is off:\n%s", got)
		}
	})
	t.Run("replaced policy", func(t *testing.T) {
		c := bindingConfig(t, "")
		c.Runtime = fakeRuntime{}
		c.Verify = verify.Warn
		c.VerifyPolicy = verify.DefaultPolicy()
		c.VerifyPolicy.Identity = `.*`
		out := &bytes.Buffer{}
		c.Out = out
		c.Status(creds.Set{})
		if got := out.String(); !strings.Contains(got, "replaced") {
			t.Errorf("the report does not say the trust policy was replaced:\n%s", got)
		}
	})
	t.Run("shipped policy", func(t *testing.T) {
		c := bindingConfig(t, "")
		c.Runtime = fakeRuntime{}
		c.Verify = verify.Warn
		c.VerifyPolicy = verify.DefaultPolicy()
		out := &bytes.Buffer{}
		c.Out = out
		c.Status(creds.Set{})
		if got := out.String(); strings.Contains(got, "replaced") {
			t.Errorf("the shipped policy is reported as replaced:\n%s", got)
		}
	})
}
