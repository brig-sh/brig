package wrap

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

// hostCredConfig builds a run whose host credential comes from a command
// printing the blob given, which is the BRIG_CREDENTIALS_CMD path a user with
// any other secret backend takes. The keychain path resolves the same value
// through the same code.
func hostCredConfig(t *testing.T, body, blob string) *Config {
	t.Helper()
	c := bindingConfig(t, "hostCredential:\n  keychainService: s\n  tokenField: accessToken\n"+
		"  expiryField: expiresAt\n  targetVar: TOK\n  renewHint: run it once\n"+body)
	c.CredentialsCmd = "printf %s " + shellQuote(blob)
	c.Err = &bytes.Buffer{}
	return c
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func warnings(t *testing.T, c *Config) string {
	t.Helper()
	return c.Err.(*bytes.Buffer).String()
}

// The host credential was the one value that reached the guest without passing
// the denylist: BuildEnv called AddSecret directly, so a profile that denies
// its own target variable denied every source of it except the one brig reads
// itself.
func TestHostCredentialPassesTheDenylist(t *testing.T) {
	c := hostCredConfig(t, "deny: [TOK]\n", `{"accessToken":"tok-from-keychain"}`)
	set, err := c.BuildEnv()
	if err != nil {
		t.Fatal(err)
	}
	if set.Has("TOK") {
		t.Errorf("a denied variable was forwarded from the host credential: %v", set.Names)
	}
	if !strings.Contains(warnings(t, c), "denylist") {
		t.Errorf("nothing said why: %q", warnings(t, c))
	}
}

// BRIG_ALLOW_DENIED=1 is the deliberate override, and it has to keep working
// on this path too -- otherwise the fix above turns into a value nobody can
// forward at all.
func TestHostCredentialIsForwardedWhenTheDenylistIsOverridden(t *testing.T) {
	c := hostCredConfig(t, "deny: [TOK]\n", `{"accessToken":"tok-from-keychain"}`)
	c.AllowDenied = true
	set, err := c.BuildEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !set.Has("TOK") {
		t.Errorf("BRIG_ALLOW_DENIED=1 did not forward it: %v", set.Names)
	}
}

// The other guard: a credentials command that is not logged in prints its
// secret-manager reference rather than a token. Forwarded verbatim it fails in
// the guest as "invalid token", which is indistinguishable from a real
// authentication failure.
func TestHostCredentialPassesTheUnresolvedReferenceGuard(t *testing.T) {
	c := hostCredConfig(t, "", `{"accessToken":"op://vault/claude/token"}`)
	set, err := c.BuildEnv()
	if err != nil {
		t.Fatal(err)
	}
	if set.Has("TOK") {
		t.Errorf("an unresolved secret reference was forwarded as the login: %v", set.Names)
	}
	if !strings.Contains(warnings(t, c), "op://") {
		t.Errorf("nothing said why: %q", warnings(t, c))
	}
}

// An expired credential authenticates nothing, so forwarding it buys the
// sandbox no login and puts a real credential inside a machine with
// unrestricted egress. It used to be forwarded and merely warned about.
func TestExpiredHostCredentialIsNotForwarded(t *testing.T) {
	expired := time.Now().Add(-time.Hour).UnixMilli()
	c := hostCredConfig(t, "", fmt.Sprintf(`{"accessToken":"stale","expiresAt":%d}`, expired))
	set, err := c.BuildEnv()
	if err != nil {
		t.Fatalf("an expired credential failed the run: %v", err)
	}
	if set.Has("TOK") {
		t.Errorf("an expired credential was forwarded: %v", set.Names)
	}
	for _, want := range []string{"expired", "BRIG_ALLOW_EXPIRED=1", "run it once"} {
		if !strings.Contains(warnings(t, c), want) {
			t.Errorf("the warning does not mention %q: %q", want, warnings(t, c))
		}
	}
	// Still reported as found-and-expired rather than as absent: the status
	// report is what tells a user why their sandbox is asking them to log in.
	if c.HostCred == nil {
		t.Error("the expired credential was not recorded for the status report")
	}
}

// A credential with no expiry in its blob is not expired: absence is not
// evidence, and every profile whose agent does not record one would otherwise
// stop authenticating.
func TestHostCredentialWithoutAnExpiryIsForwarded(t *testing.T) {
	c := hostCredConfig(t, "", `{"accessToken":"tok"}`)
	set, err := c.BuildEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !set.Has("TOK") {
		t.Errorf("a credential with no expiry was dropped: %v", set.Names)
	}
}

// The escape hatch, for a host whose clock or expiry field cannot be trusted.
func TestExpiredHostCredentialIsForwardedWhenAskedFor(t *testing.T) {
	expired := time.Now().Add(-time.Hour).UnixMilli()
	c := hostCredConfig(t, "", fmt.Sprintf(`{"accessToken":"stale","expiresAt":%d}`, expired))
	c.env = NewEnv(c.Profile.Name, func(name string) (string, bool) {
		if name == "BRIG_ALLOW_EXPIRED" {
			return "1", true
		}
		return "", false
	})
	set, err := c.BuildEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !set.Has("TOK") {
		t.Errorf("BRIG_ALLOW_EXPIRED=1 did not forward it: %v", set.Names)
	}
}

// A credential the environment already supplied is not read from the host at
// all, which is the property that keeps `brig env` from raising a keychain
// prompt for a run that needs nothing from it.
func TestHostCredentialIsNotReadWhenTheEnvironmentSuppliedOne(t *testing.T) {
	c := hostCredConfig(t, "env:\n  - name: TOK\n    value: from-the-profile\n", `{"accessToken":"x"}`)
	set, err := c.BuildEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.HostCred != nil {
		t.Error("the host credential was read even though the run already had one")
	}
	var got string
	for _, v := range set.Vars {
		if v.Name == "TOK" {
			got = v.Value
		}
	}
	if got != "from-the-profile" {
		t.Errorf("TOK = %q, want the profile's own value", got)
	}
}
