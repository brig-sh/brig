package wrap

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/secret"
)

// fakeStore is the secret store as a map: what is in it resolves, what is not
// is ErrNotFound, which is the only distinction resolution makes.
type fakeStore map[string]string

func (f fakeStore) Read(name string) ([]byte, error) {
	v, ok := f[name]
	if !ok {
		return nil, secret.ErrNotFound
	}
	return []byte(v), nil
}

// testProfile parses a profile from its bindings alone, so a case reads as the
// YAML a user would write rather than as a struct literal.
func testProfile(t *testing.T, body string) profile.Profile {
	t.Helper()
	p, err := profile.Parse([]byte(
		"name: x\nimage: i\nguestHome: /home/x\nbinary: x\nmem: 1\ncpus: 1\n" + body))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// bindingConfig is testConfig with the parsed profile's own bindings, which is
// what Load hands BuildEnv when BRIG_FORWARD_ENV is unset.
func bindingConfig(t *testing.T, body string) *Config {
	t.Helper()
	return testConfig(t, t.TempDir(), t.TempDir(), testProfile(t, body))
}

// The cases the bash wrapper's own suite ran for the guest-cwd block.
func TestGuestCwd(t *testing.T) {
	cases := []struct{ cwd, workspace, want string }{
		{"/h/sandboxed-claude-foo/proj", "/h/sandboxed-claude-foo", "/home/claude/proj"},
		{"/h/sandboxed-claude-foo", "/h/sandboxed-claude-foo", "/home/claude"},
		{"/h/elsewhere", "/h/sandboxed-claude-foo", "/home/claude"},
		// A named run's workspace is the suffixed directory, so a cwd in the
		// unnamed base does not match it: this run mounts only its own
		// workspace, so there is no guest directory to start in.
		{"/h/sandboxed-claude/proj", "/h/sandboxed-claude-foo", "/home/claude"},
		{"/h/sandboxed-claude-foo/a/b", "/h/sandboxed-claude-foo", "/home/claude/a/b"},
	}
	for _, c := range cases {
		if got := GuestCwd(c.cwd, c.workspace, "/home/claude"); got != c.want {
			t.Errorf("GuestCwd(%q, %q) = %q, want %q", c.cwd, c.workspace, got, c.want)
		}
	}
}

// The agent resolves the trust key to the git repository root, so keying on
// the cwd writes a key that is never read back and leaves the dialog up from
// any subdirectory of a repository.
func TestTrustKeyResolvesToTheRepositoryRoot(t *testing.T) {
	ws := t.TempDir()
	mustMkdir(t, filepath.Join(ws, "myrepo", "sub"))
	mustMkdir(t, filepath.Join(ws, "myrepo", ".git"))
	mustMkdir(t, filepath.Join(ws, "plain"))

	cases := []struct{ cwd, want string }{
		{filepath.Join(ws, "myrepo", "sub"), "/home/claude/myrepo"},
		{filepath.Join(ws, "myrepo"), "/home/claude/myrepo"},
		{filepath.Join(ws, "plain"), "/home/claude/plain"}, // no repository: the directory itself
		{ws, "/home/claude"},
	}
	for _, c := range cases {
		if got := TrustKey(c.cwd, ws, "/home/claude"); got != c.want {
			t.Errorf("TrustKey(%q) = %q, want %q", c.cwd, got, c.want)
		}
	}
}

// A worktree or submodule checkout has .git as a file, not a directory.
func TestTrustKeyAcceptsAGitFile(t *testing.T) {
	ws := t.TempDir()
	mustMkdir(t, filepath.Join(ws, "wt", "sub"))
	if err := os.WriteFile(filepath.Join(ws, "wt", ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := TrustKey(filepath.Join(ws, "wt", "sub"), ws, "/home/claude"); got != "/home/claude/wt" {
		t.Errorf("TrustKey = %q, want /home/claude/wt", got)
	}
}

// The guest has nothing but the workspace mounted, so a repository ABOVE the
// workspace is invisible in there: the walk must stop at the workspace rather
// than keying on a root the guest cannot see.
func TestTrustKeyWalkStopsAtTheWorkspace(t *testing.T) {
	outer := t.TempDir()
	mustMkdir(t, filepath.Join(outer, ".git"))
	ws := filepath.Join(outer, "workspace")
	mustMkdir(t, filepath.Join(ws, "proj"))

	if got := TrustKey(filepath.Join(ws, "proj"), ws, "/home/claude"); got != "/home/claude/proj" {
		t.Errorf("TrustKey = %q, want /home/claude/proj (the .git above the workspace is not mounted)", got)
	}
}

// BRIG_FORWARD_ENV replaces the env-sourced set and nothing else: a secret
// binding is the profile's own declaration of what the workload needs, not
// something a list of bare variable names was ever able to speak about.
func TestForwardEnvOverrideReplacesOnlyTheEnvSourcedSet(t *testing.T) {
	p := testProfile(t, "secrets:\n  - gh\nenv:\n"+
		"  - name: GH_TOKEN\n    ref: secrets.gh\n"+
		"  - name: A\n    ref: env.A\n"+
		"  - name: MODE\n    value: fast\n")

	got, warnings := envOverride(p.Env, []string{"B", "C"})

	names := bindingNames(got)
	want := []string{"GH_TOKEN", "MODE", "B", "C"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for _, b := range got {
		if b.Name == "B" && b.Ref != "env.B" {
			t.Errorf("B = %+v, want ref env.B", b)
		}
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none: nothing was dropped", warnings)
	}
}

// The override never replaces a binding it could not have expressed. mergeEnv
// is last-wins, so appending an override ref for a name the profile already
// binds would put the ambient shell's value over the keychain's -- silently,
// and for the credential the profile exists to supply.
func TestForwardEnvOverrideNeverOutranksAProfileBinding(t *testing.T) {
	p := testProfile(t, "secrets:\n  - gh\nenv:\n"+
		"  - name: GH_TOKEN\n    ref: secrets.gh\n"+
		"  - name: MODE\n    value: fast\n")

	got, warnings := envOverride(p.Env, []string{"GH_TOKEN", "MODE", "B"})

	names := bindingNames(got)
	if strings.Join(names, ",") != "GH_TOKEN,MODE,B" {
		t.Fatalf("names = %v, want GH_TOKEN,MODE,B (each bound once)", names)
	}
	if got[0].Ref != "secrets.gh" {
		t.Errorf("GH_TOKEN = %+v, want the profile's secrets.gh binding", got[0])
	}
	if got[1].Value != "fast" || got[1].Ref != "" {
		t.Errorf("MODE = %+v, want the profile's literal", got[1])
	}

	// Dropping something the user asked for is worth saying, by name. Never a
	// value: a warning is output, and output never carries one.
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"GH_TOKEN", "MODE", "BRIG_FORWARD_ENV"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the warnings do not name %q: %q", want, joined)
		}
	}
	if strings.Contains(joined, "fast") || strings.Contains(joined, "secrets.gh") {
		t.Errorf("a warning carries a value or a ref target: %q", joined)
	}
}

// No override leaves the profile's bindings exactly as declared.
func TestNoForwardEnvOverrideLeavesBindingsAlone(t *testing.T) {
	p := testProfile(t, "env:\n  - name: A\n    ref: env.A\n")
	got, warnings := envOverride(p.Env, nil)
	if len(got) != 1 || got[0].Name != "A" {
		t.Fatalf("bindings = %+v", got)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

// The requirement: a run whose secret cannot be resolved FAILS, naming the
// secret, the sandbox it was needed for, and the command that creates it. Not a
// warning, not a binding quietly dropped -- BuildEnv returns before
// EnsureRunning, so nothing is created.
func TestBuildEnvFailsOnAMissingSecret(t *testing.T) {
	c := bindingConfig(t,
		"secrets:\n  - gh_token\nenv:\n  - name: GH\n    ref: secrets.gh_token\n")
	c.VMName = "brig-claude-code"
	c.OpenStore = func() (creds.SecretReader, error) { return fakeStore{}, nil }

	_, err := c.BuildEnv()
	if err == nil {
		t.Fatal("a missing secret did not fail the run")
	}
	for _, want := range []string{"gh_token", "brig-claude-code", "brig secret create gh_token"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not carry %q: %v", want, err)
		}
	}
}

func TestBuildEnvBindsAResolvedSecret(t *testing.T) {
	c := bindingConfig(t, "secrets:\n  - gh\nenv:\n  - name: GH\n    ref: secrets.gh\n")
	c.OpenStore = func() (creds.SecretReader, error) { return fakeStore{"gh": "ghp_x"}, nil }

	set, err := c.BuildEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !set.Has("GH") {
		t.Fatalf("the resolved secret was not bound: %+v", set.Names)
	}
	// A store value must never reach argv: hull logs every exec's argv to a
	// host file that outlives the sandbox. Set.Secret is what keeps it out.
	for _, v := range set.Vars {
		if v.Name == "GH" && !v.Secret {
			t.Errorf("GH is not marked as a secret, so BRIG_ENV_ARGV would log it")
		}
	}
}

// The host credential is read out of a keychain too, so it gets the same argv
// exemption a store secret gets. BRIG_ENV_ARGV is a debugging hatch, and the
// host durably logs every exec's argv -- a token in there outlives the sandbox
// in a file nobody thinks to look at.
func TestBuildEnvKeepsTheHostCredentialOutOfArgv(t *testing.T) {
	c := hostCredConfig(t, "", `{"accessToken":"tok"}`)

	set, err := c.BuildEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !set.Has("TOK") {
		t.Fatalf("the host credential was not bound: %+v", set.Names)
	}
	for _, v := range set.Vars {
		if v.Name == "TOK" && !v.Secret {
			t.Errorf("TOK is not marked as a secret, so BRIG_ENV_ARGV would log it")
		}
	}
}

// A profile with no secrets never opens the store, so no keychain prompt is
// raised for a run with nothing to read -- the same property BuildEnv already
// protects for the host credential.
func TestBuildEnvNeverOpensTheStoreWithoutSecrets(t *testing.T) {
	c := bindingConfig(t, "env:\n  - name: NOPE\n    ref: env.NOPE\n")
	c.OpenStore = func() (creds.SecretReader, error) {
		t.Error("the store was opened for a profile that declares no secrets")
		return nil, nil
	}
	if _, err := c.BuildEnv(); err != nil {
		t.Fatal(err)
	}
}

// A dropped override is news whether or not a later step fails. The decision
// was already made when the binding list was built, so withholding it because
// the secret could not be resolved tells the user half of what happened to
// their BRIG_FORWARD_ENV -- and the resolution error still prints last, so it
// stays the visible news.
func TestBuildEnvWarnsAboutADroppedOverrideEvenWhenTheRunFails(t *testing.T) {
	c := bindingConfig(t, "secrets:\n  - gh\nenv:\n  - name: GH\n    ref: secrets.gh\n")
	c.Env, c.envWarnings = envOverride(c.Profile.Env, []string{"GH"})
	c.OpenStore = func() (creds.SecretReader, error) { return fakeStore{}, nil }
	warned := &bytes.Buffer{}
	c.Err = warned

	if _, err := c.BuildEnv(); err == nil {
		t.Fatal("a missing secret did not fail the run")
	}
	if !strings.Contains(warned.String(), "GH") ||
		!strings.Contains(warned.String(), "BRIG_FORWARD_ENV") {
		t.Errorf("the dropped override went unmentioned: %q", warned.String())
	}
}

// A store that cannot be opened fails the run as it came: a locked keyring is
// not a secret anyone forgot to create, and dressing it up as one would send
// the user the wrong way.
func TestBuildEnvReturnsAStoreFailureAsItCame(t *testing.T) {
	c := bindingConfig(t, "secrets:\n  - gh\nenv:\n  - name: GH\n    ref: secrets.gh\n")
	c.OpenStore = func() (creds.SecretReader, error) { return nil, errors.New("keyring is locked") }

	if _, err := c.BuildEnv(); err == nil || !strings.Contains(err.Error(), "keyring is locked") {
		t.Fatalf("err = %v, want the store's own failure", err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// BRIG_CREDENTIALS_CMD is removed, and a run that still sets it fails rather
// than ignoring it: the variable named the command that read the host
// credential, so a run that carried on would boot a sandbox without the login
// the user believes they configured, and the only symptom would be the guest
// asking them to authenticate.
//
// Both spellings, because the per-profile prefix is how a shell carries a
// setting for one profile and the removal has to reach the same names the
// setting did.
func TestCredentialsCmdFailsTheRunAndNamesTheImport(t *testing.T) {
	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("no claude-code profile")
	}
	for _, name := range []string{"BRIG_CREDENTIALS_CMD", "BRIG_CLAUDE_CODE_CREDENTIALS_CMD"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "your-secret-tool read claude/credentials")

			_, err := Load(p, Options{}, nil)
			if err == nil {
				t.Fatal("a run with BRIG_CREDENTIALS_CMD set was loaded anyway")
			}
			for _, want := range []string{"BRIG_CREDENTIALS_CMD", "removed",
				"brig secret import claude-code <name> --from-command"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not carry %q: %v", want, err)
				}
			}
		})
	}
}

// And an unset one loads, including the empty spelling: `export
// BRIG_CREDENTIALS_CMD=` is how a shell profile turns the setting off, and
// failing on it would refuse the very state the user moved to.
func TestLoadAcceptsAnEmptyCredentialsCmd(t *testing.T) {
	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("no claude-code profile")
	}
	t.Setenv("BRIG_CREDENTIALS_CMD", "")

	if _, err := Load(p, Options{}, nil); err != nil {
		t.Fatalf("an empty BRIG_CREDENTIALS_CMD failed the run: %v", err)
	}
}
