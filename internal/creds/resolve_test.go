package creds

import (
	"errors"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/secret"
)

// opens is the open callback for a store that is simply there. Resolution
// takes the callback rather than the store because the decision to open one is
// part of resolving: a run that needs nothing must raise no keychain prompt.
func opens(s SecretReader) func() (SecretReader, error) {
	return func() (SecretReader, error) { return s, nil }
}

// noEnv is a lookup for a shell that carries none of the profile's variables.
func noEnv(string) (string, bool) { return "", false }

func TestResolveSecretsReadsEveryDeclaredName(t *testing.T) {
	p := profileWith(t, "secrets:\n  - a\n  - b\n")
	res, err := ResolveSecrets(p, "brig-x", opens(fakeStore{"a": "one", "b": "two"}), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if res.Values["a"] != "one" || res.Values["b"] != "two" {
		t.Errorf("resolved %v", res.Values)
	}
}

// The message a user actually reads when a run cannot get its credential. It
// has to answer three questions without them having to ask: what is missing,
// what was it needed for, and what do I type to fix it.
func TestOneMissingSecretSaysWhatToDo(t *testing.T) {
	p := profileWith(t, "secrets:\n  - gh_token\n")
	_, err := ResolveSecrets(p, "brig-claude-code", opens(fakeStore{}), noEnv)
	if err == nil {
		t.Fatal("a missing secret resolved")
	}
	got := err.Error()
	want := `missing secret "gh_token" needed by the brig-claude-code sandbox -- ` +
		"create it first with: brig secret create gh_token"
	if got != want {
		t.Errorf("error message is\n  %s\nwant\n  %s", got, want)
	}
}

// Collected rather than short-circuited: a fresh host is fixed in one pass
// instead of one failed run per secret.
func TestEveryMissingSecretIsNamedAtOnce(t *testing.T) {
	p := profileWith(t, "secrets:\n  - a\n  - b\n  - c\n")
	_, err := ResolveSecrets(p, "brig-x", opens(fakeStore{"b": "two"}), noEnv)
	if err == nil {
		t.Fatal("missing secrets resolved")
	}
	var missing *MissingSecretsError
	if !errors.As(err, &missing) {
		t.Fatalf("error is %T, want *MissingSecretsError", err)
	}
	if len(missing.Missing) != 2 || missing.Missing[0].Name != "a" || missing.Missing[1].Name != "c" {
		t.Fatalf("Missing = %+v, want a and c", missing.Missing)
	}
	got := err.Error()
	want := "missing 2 secrets needed by the brig-x sandbox:" +
		"\n  a: create it first with: brig secret create a" +
		"\n  c: create it first with: brig secret create c"
	if got != want {
		t.Errorf("error message is\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, "two") {
		t.Errorf("the error carries a secret value:\n%s", got)
	}
}

// cmd/brig prints "brig: " itself, so an error carrying its own would print it
// twice.
func TestTheErrorDoesNotCarryTheBrigPrefix(t *testing.T) {
	p := profileWith(t, "secrets:\n  - a\n")
	_, err := ResolveSecrets(p, "brig-x", opens(fakeStore{}), noEnv)
	if strings.HasPrefix(err.Error(), "brig: ") {
		t.Errorf("the error carries the prefix cmd/brig adds: %v", err)
	}
}

// A profile that needs nothing never touches the store, so a run with no
// secrets raises no keychain prompt. The store is not merely unread: it is
// never opened, which is where the prompt would come from.
func TestResolveSecretsWithNoRequirementsNeverReads(t *testing.T) {
	p := profileWith(t, "forward:\n  - GH_TOKEN\n")
	res, err := ResolveSecrets(p, "brig-x", func() (SecretReader, error) {
		panic("the store was opened for a profile that declares no secrets")
	}, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Values) != 0 {
		t.Errorf("resolved %v from a profile with no secrets", res.Values)
	}
}

// A backend that fails for a reason other than absence is not a secret you
// forgot to create, and "create it first" would send the user the wrong way
// entirely. The reason survives into the message and into errors.Is.
func TestStoreFailureIsNotReportedAsAbsence(t *testing.T) {
	p := profileWith(t, "secrets:\n  - a\n")
	_, err := ResolveSecrets(p, "brig-x", opens(brokenStore{}), noEnv)
	if err == nil {
		t.Fatal("a broken store resolved")
	}
	if !strings.Contains(err.Error(), "keyring is locked") {
		t.Errorf("the backend's reason was lost: %v", err)
	}
	if strings.Contains(err.Error(), "brig secret create") {
		t.Errorf("a locked store was reported as a secret to create: %v", err)
	}
	if !errors.Is(err, errKeyringLocked) {
		t.Errorf("errors.Is does not reach the backend's reason: %v", err)
	}
}

// A chain whose earlier env. element resolves does not contribute its
// secrets. element to the needed set -- so a run the environment already
// satisfies never touches the store, and raises no keychain prompt.
func TestChainSatisfiedByTheEnvironmentNeedsNoSecret(t *testing.T) {
	p := profile.Profile{
		Secrets: []profile.SecretDecl{{Name: "gh-token", Required: ptr(false)}},
		Env: []profile.EnvBinding{{
			Name: "GH_TOKEN",
			Refs: []string{"env.GH_TOKEN", "secrets.gh-token"},
		}},
	}
	set := profile.SecretNames(Needed(p, func(string) (string, bool) { return "from-the-shell", true }))
	if len(set) != 0 {
		t.Errorf("Needed = %v; want nothing: the environment already answers", set)
	}
	set = profile.SecretNames(Needed(p, func(string) (string, bool) { return "", false }))
	if len(set) != 1 || set[0] != "gh-token" {
		t.Errorf("Needed = %v; want [gh-token]", set)
	}
}

// A required secret is needed whatever the bindings do with it: the
// requirement list is a statement about the workload, not about one binding.
func TestARequiredSecretIsAlwaysNeeded(t *testing.T) {
	p := profile.Profile{Secrets: []profile.SecretDecl{{Name: "must-have"}}}
	if got := profile.SecretNames(Needed(p, func(string) (string, bool) { return "", false })); len(got) != 1 {
		t.Errorf("Needed = %v; want [must-have]", got)
	}
}

func ptr(b bool) *bool { return &b }

// ErrUnsupported is a platform invariant: nothing the user does on this
// run changes it, and saying so every time is noise. Silent for an optional
// secret, fatal for a required one.
func TestUnsupportedStoreIsSilentForAnOptionalSecret(t *testing.T) {
	// An optional secret reached by an env binding with no shell value: the
	// need survives, so the store is opened, so the platform answer is
	p := profile.Profile{
		Secrets: []profile.SecretDecl{{Name: "claude-credentials", Required: ptr(false)}},
		Env:     []profile.EnvBinding{{Name: "TOK", Ref: "secrets.claude-credentials"}},
	}
	res, err := ResolveSecrets(p, "brig-claude-code",
		func() (SecretReader, error) { return nil, secret.ErrUnsupported },
		func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("an optional secret on a storeless platform failed the run: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v; want none: nothing the user does on this run changes it", res.Warnings)
	}
}

// A store that exists and could not be opened -- a locked keychain, a denied
// dialog -- is a state the user CAN change, so it warns and says that rather
// than "import it": the import would hit the same wall.
func TestALockedStoreWarnsWithoutSuggestingImport(t *testing.T) {
	locked := errors.New("security: User interaction is not allowed.")
	p := profile.Profile{Secrets: []profile.SecretDecl{{Name: "s", Required: ptr(false)}},
		Env: []profile.EnvBinding{{Name: "S", Ref: "secrets.s"}}}
	res, err := ResolveSecrets(p, "brig-x",
		func() (SecretReader, error) { return nil, locked },
		func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("warnings = %v; want one", res.Warnings)
	}
	if !strings.Contains(res.Warnings[0], "could not read brig's secret store") {
		t.Errorf("warning = %q", res.Warnings[0])
	}
	if strings.Contains(res.Warnings[0], "secret import") {
		t.Error("the warning suggests an import that would hit the same lock")
	}
}

// A required secret fails in all three, with the reason it actually hit.
func TestARequiredSecretFailsWithTheReasonItHit(t *testing.T) {
	p := profile.Profile{Secrets: []profile.SecretDecl{{Name: "s"}}}
	_, err := ResolveSecrets(p, "brig-x",
		func() (SecretReader, error) { return nil, secret.ErrUnsupported },
		func(string) (string, bool) { return "", false })
	if !errors.Is(err, secret.ErrUnsupported) {
		t.Errorf("err = %v; want one wrapping ErrUnsupported", err)
	}
}

// The one message that names a variable instead of a verb, and it has to be
// computed from the profile's own bindings: envOverride drops names the
// profile binds, so telling someone to export a value bound as a bare
// `ref: secrets.<name>` sends them at a run that fails identically.
func TestTheStorelessHintIsComputedFromTheBindings(t *testing.T) {
	chained := profile.Profile{
		Name:    "mytool",
		Secrets: []profile.SecretDecl{{Name: "mytool-token"}},
		Env: []profile.EnvBinding{{
			Name: "MYTOOL_TOKEN",
			Refs: []string{"env.MYTOOL_TOKEN", "secrets.mytool-token"},
		}},
	}
	_, err := ResolveSecrets(chained, "brig-mytool",
		func() (SecretReader, error) { return nil, secret.ErrUnsupported }, noEnv)
	if got := err.Error(); !strings.Contains(got, "Export MYTOOL_TOKEN before running brig") {
		t.Errorf("a chained binding is not told to export:\n%s", got)
	}

	bare := profile.Profile{
		Name:    "mytool",
		Secrets: []profile.SecretDecl{{Name: "mytool-token"}},
		Env:     []profile.EnvBinding{{Name: "MYTOOL_TOKEN", Ref: "secrets.mytool-token"}},
	}
	_, err = ResolveSecrets(bare, "brig-mytool",
		func() (SecretReader, error) { return nil, secret.ErrUnsupported }, noEnv)
	got := err.Error()
	if strings.Contains(got, "Export MYTOOL_TOKEN before running brig") {
		t.Errorf("a bare ref: is told to export, which envOverride would drop:\n%s", got)
	}
	if !strings.Contains(got, "refs: [env.MYTOOL_TOKEN, secrets.mytool-token]") {
		t.Errorf("the bare-ref message does not say how to make exporting work:\n%s", got)
	}
}

// required: decides whether the run stops; sources: decides which
// command is named. Nothing crosses over.
//
// The importable quadrant names `brig secret import <profile>` and the
// hand-created one `brig secret create <name>`, and each says the other is
// absent. The absent-strings check is the point: the two commands are not
// interchangeable, and a message that named the wrong one would send a user to
// a command that cannot fill the secret it is talking about. Import takes the
// PROFILE because one import fills every source the profile declares; create
// takes the name because nothing but the user knows the value.
func TestMissingSecretMessages(t *testing.T) {
	cases := []struct {
		name    string
		missing Missing
		want    []string
		absent  []string
	}{
		{"required, importable",
			Missing{Name: "mytool-token", Required: true, Importable: true, Reason: secret.ErrNotFound},
			[]string{`missing secret "mytool-token"`, "brig-mytool sandbox", "brig secret import mytool"},
			[]string{"secret create"}},
		{"required, hand-created",
			Missing{Name: "mytool-token", Required: true, Reason: secret.ErrNotFound},
			[]string{"brig secret create mytool-token"},
			[]string{"secret import"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := &MissingSecretsError{Sandbox: "brig-mytool", Profile: "mytool", Missing: []Missing{c.missing}}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%q missing from:\n%s", want, err)
				}
			}
			for _, absent := range c.absent {
				if strings.Contains(err.Error(), absent) {
					t.Errorf("%q should not appear in:\n%s", absent, err)
				}
			}
		})
	}
}

// A profile missing one of each fails on the required name and warns about the
// optional one, in that order, so the reason the run stopped is not buried
// under advice about something that did not stop it.
func TestARequiredMissFailsAndTheOptionalOneOnlyWarns(t *testing.T) {
	p := profile.Profile{Secrets: []profile.SecretDecl{
		{Name: "needed"},
		{Name: "nice-to-have", Required: ptr(false)},
	}}
	store := fakeStore{} // empty: every read is ErrNotFound
	_, err := ResolveSecrets(p, "brig-x",
		func() (SecretReader, error) { return store, nil },
		func(string) (string, bool) { return "", false })
	var missing *MissingSecretsError
	if !errors.As(err, &missing) {
		t.Fatalf("err = %v; want a MissingSecretsError", err)
	}
	if len(missing.Missing) != 1 || missing.Missing[0].Name != "needed" {
		t.Errorf("the error reports %+v; want only the required name", missing.Missing)
	}
}

// The optional-with-no-sources quadrant is the one genuinely new message, so
// it must not name a verb that cannot fill it.
func TestOptionalWithNoSourcesNamesCreateNotImport(t *testing.T) {
	p := profile.Profile{
		Secrets: []profile.SecretDecl{{Name: "mytool-token", Required: ptr(false)}},
		Env:     []profile.EnvBinding{{Name: "MYTOOL_TOKEN", Ref: "secrets.mytool-token"}},
	}
	res, err := ResolveSecrets(p, "brig-mytool",
		func() (SecretReader, error) { return fakeStore{}, nil },
		func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("an optional secret failed the run: %v", err)
	}
	joined := strings.Join(res.Warnings, "\n")
	if !strings.Contains(joined, "brig secret create mytool-token") {
		t.Errorf("warnings do not name create:\n%s", joined)
	}
	if strings.Contains(joined, "secret import") {
		t.Errorf("warnings name import for a secret with no sources:\n%s", joined)
	}
}

// Two optional importable secrets are one block naming both, not two blocks
// naming the profile twice.
//
// This is the shipped claude-code exactly: two optional importable secrets,
// both always needed. Import takes the profile, so a block per secret prints
// the identical `brig secret import claude-code` line twice with nothing but
// the name to tell the two apart -- which reads as a warning repeated rather
// than as two secrets missing.
func TestTwoOptionalImportableMissesAreOneBlock(t *testing.T) {
	p := profile.Profile{
		Name: "claude-code",
		Secrets: []profile.SecretDecl{
			{Name: "claude-credentials", Required: ptr(false), From: "keychain",
				Service: "Claude Code-credentials", Hint: "run `claude` on the host once to log in"},
			{Name: "gh-token", Required: ptr(false), From: "env", Var: "GH_TOKEN"},
		},
		Env: []profile.EnvBinding{
			{Name: "TOK", Ref: "secrets.claude-credentials"},
			{Name: "GH_TOKEN", Ref: "secrets.gh-token"},
		},
	}
	res, err := ResolveSecrets(p, "brig-claude-code",
		func() (SecretReader, error) { return fakeStore{}, nil }, noEnv)
	if err != nil {
		t.Fatalf("two optional secrets failed the run: %v", err)
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("warnings = %#v; want one block", res.Warnings)
	}
	block := res.Warnings[0]
	for _, want := range []string{`"claude-credentials"`, `"gh-token"`,
		"brig secret import claude-code", "run `claude` on the host once to log in"} {
		if !strings.Contains(block, want) {
			t.Errorf("%q missing from the block:\n%s", want, block)
		}
	}
	if n := strings.Count(block, "brig secret import claude-code"); n != 1 {
		t.Errorf("the import command appears %d times:\n%s", n, block)
	}
	// The hint belongs to one of the two, so it says which.
	if !strings.Contains(block, "claude-credentials: run `claude`") {
		t.Errorf("the hint is not attributed to its secret:\n%s", block)
	}
}

// An optional importable secret carries the declaration's own hint:, which is
// the only thing that knows what makes the credential appear -- and it lives on
// the source in the shipped spelling, not on the secret.
func TestTheHintOnASourceReachesTheWarning(t *testing.T) {
	p := profile.Profile{
		Name: "mytool",
		Secrets: []profile.SecretDecl{{Name: "mytool-token", Required: ptr(false),
			Sources: []profile.Source{
				{From: "keychain", Service: "Mytool-credentials"},
				{From: "file", Path: "~/.mytool/creds", Hint: "run `mytool login` on the host once"},
			}}},
		Env: []profile.EnvBinding{{Name: "MYTOOL_TOKEN", Ref: "secrets.mytool-token"}},
	}
	res, err := ResolveSecrets(p, "brig-mytool",
		func() (SecretReader, error) { return fakeStore{}, nil }, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(res.Warnings, "\n")
	if !strings.Contains(joined, "run `mytool login` on the host once") {
		t.Errorf("the source's hint did not reach the warning:\n%s", joined)
	}
}
