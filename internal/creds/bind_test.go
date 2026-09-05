package creds

import (
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
)

func TestBindResolvesBothNamespaces(t *testing.T) {
	p := profileWith(t, "secrets:\n  - gh\nenv:\n"+
		"  - name: GH_TOKEN\n    ref: secrets.gh\n"+
		"  - name: CI\n    ref: env.CI\n"+
		"  - name: MODE\n    value: fast\n")
	set := Bind(p, p.Env, map[string]string{"gh": "ghp_x"},
		lookupFrom(map[string]string{"CI": "true"}), Options{})

	want := map[string]string{"GH_TOKEN": "ghp_x", "CI": "true", "MODE": "fast"}
	if len(set.Vars) != len(want) {
		t.Fatalf("bound %+v, want %d vars", set.Vars, len(want))
	}
	for _, v := range set.Vars {
		if want[v.Name] != v.Value {
			t.Errorf("%s = %q, want %q", v.Name, v.Value, want[v.Name])
		}
	}
}

// A ref'd secret is annotated with its origin, the way the host credential
// already reads as CLAUDE_CODE_OAUTH_TOKEN(host).
func TestBindAnnotatesOrigin(t *testing.T) {
	p := profileWith(t, "secrets:\n  - gh\nenv:\n  - name: GH_TOKEN\n    ref: secrets.gh\n")
	set := Bind(p, p.Env, map[string]string{"gh": "ghp_x"}, lookupFrom(nil), Options{})
	if len(set.Names) != 1 || set.Names[0] != "GH_TOKEN(secret)" {
		t.Fatalf("Names = %v, want [GH_TOKEN(secret)]", set.Names)
	}
}

// An env. ref that is unset is skipped, exactly as forward: always was --
// binding it empty would shadow a value baked into the image. Silently: unlike
// an emptied secret, an unset or empty ambient variable is ordinary, and
// BRIG_FORWARD_ENV misses on plenty of names nobody meant to forward.
func TestBindSkipsUnsetEnvRefs(t *testing.T) {
	p := profileWith(t, "env:\n  - name: A\n    ref: env.A\n  - name: B\n    ref: env.B\n")
	set := Bind(p, p.Env, nil, lookupFrom(map[string]string{"A": "x", "B": ""}), Options{})
	if len(set.Vars) != 1 || set.Vars[0].Name != "A" {
		t.Fatalf("bound %+v, want only A", set.Vars)
	}
	if len(set.Warnings) != 0 {
		t.Errorf("an unset env var warned: %v", set.Warnings)
	}
}

// The denylist is the billing guard: a metered API key silently outranking the
// subscription token moves the sandbox onto metered billing. That consequence
// does not change because the value arrived by an explicit ref.
func TestDenyAppliesToRefdValues(t *testing.T) {
	p := profileWith(t, "deny:\n  - K\n")
	// Built by hand: a profile may not declare a binding it also denies, so
	// this is the shape a BRIG_FORWARD_ENV override produces.
	bindings := []profile.EnvBinding{{Name: "K", Ref: "env.K"}}
	env := lookupFrom(map[string]string{"K": "sk-metered"})

	set := Bind(p, bindings, nil, env, Options{})
	if len(set.Vars) != 0 {
		t.Fatalf("a denied variable was bound: %+v", set.Vars)
	}
	if len(set.Warnings) != 1 || !strings.Contains(set.Warnings[0], "BRIG_ALLOW_DENIED=1") {
		t.Errorf("the warning does not say how to override it: %v", set.Warnings)
	}

	allowed := Bind(p, bindings, nil, env, Options{AllowDenied: true})
	if len(allowed.Vars) != 1 {
		t.Errorf("BRIG_ALLOW_DENIED=1 did not override the denylist: %+v", allowed.Vars)
	}
}

// A literal value: is configuration its author wrote, not something picked out
// of an ambient environment, so the unresolved-reference guard would be wrong to
// second-guess it.
func TestLiteralValuesSkipTheRefGuard(t *testing.T) {
	p := profileWith(t, "env:\n  - name: URL\n    value: op://vault/item/field\n")
	set := Bind(p, p.Env, nil, lookupFrom(nil), Options{})
	if len(set.Vars) != 1 || set.Vars[0].Value != "op://vault/item/field" {
		t.Fatalf("a literal value was dropped: %+v, warnings %v", set.Vars, set.Warnings)
	}
}

// The ambient environment still gets the guard: direnv readily leaves a
// secret-manager reference unresolved, and forwarding it verbatim yields an auth
// error indistinguishable from a wrong token.
func TestEnvRefsStillGetTheUnresolvedRefGuard(t *testing.T) {
	p := profileWith(t, "env:\n  - name: T\n    ref: env.T\n")
	set := Bind(p, p.Env, nil, lookupFrom(map[string]string{"T": "op://vault/item/field"}), Options{})
	if len(set.Vars) != 0 {
		t.Fatalf("an unresolved reference was forwarded: %+v", set.Vars)
	}
	if len(set.Warnings) != 1 || !strings.Contains(set.Warnings[0], "BRIG_ALLOW_REFS=1") {
		t.Errorf("warnings = %v", set.Warnings)
	}
}

// A secret whose value is empty is a real secret, not an absent one -- but
// binding it empty would still shadow the image, so it is skipped for the same
// reason an unset env var is.
func TestBindSkipsEmptySecretValues(t *testing.T) {
	p := profileWith(t, "secrets:\n  - gh\nenv:\n  - name: GH_TOKEN\n    ref: secrets.gh\n")
	set := Bind(p, p.Env, map[string]string{"gh": ""}, lookupFrom(nil), Options{})
	if len(set.Vars) != 0 {
		t.Fatalf("an empty secret was bound: %+v", set.Vars)
	}
}

// An emptied secret is not silently dropped: brig itself refuses to write one
// empty, so seeing one here means another keychain tool did, and the guest
// would otherwise just fail to authenticate with no explanation.
func TestBindWarnsOnEmptySecretValue(t *testing.T) {
	p := profileWith(t, "secrets:\n  - gh\nenv:\n  - name: GH_TOKEN\n    ref: secrets.gh\n")
	set := Bind(p, p.Env, map[string]string{"gh": ""}, lookupFrom(nil), Options{})
	if len(set.Warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", set.Warnings)
	}
	for _, want := range []string{"GH_TOKEN", "gh", "brig secret update gh", "brig secret delete gh"} {
		if !strings.Contains(set.Warnings[0], want) {
			t.Errorf("warning does not mention %q: %s", want, set.Warnings[0])
		}
	}
}

// A ref that does not parse is reachable from a profile built in Go rather
// than read from a file -- Validate rejects it at parse time, but a hand-built
// binding (the same shape TestDenyAppliesToRefdValues constructs) skips that
// guard.
func TestBindWarnsOnUnparsableRef(t *testing.T) {
	p := profileWith(t, "")
	bindings := []profile.EnvBinding{{Name: "X", Ref: "nope.x"}}
	set := Bind(p, bindings, nil, lookupFrom(nil), Options{})
	if len(set.Vars) != 0 {
		t.Fatalf("a binding with an unparsable ref was bound: %+v", set.Vars)
	}
	if len(set.Warnings) != 1 || !strings.Contains(set.Warnings[0], "X") {
		t.Fatalf("warnings = %v, want one naming X", set.Warnings)
	}
}

// A secrets.-sourced binding must never travel in argv, whatever
// BRIG_ENV_ARGV says: the host durably logs every exec's argv. A literal or an
// env. ref is not held to that -- it was never in the keychain to begin with.
func TestBindMarksOnlySecretsSourcedVarsSecret(t *testing.T) {
	p := profileWith(t, "secrets:\n  - gh\nenv:\n"+
		"  - name: GH_TOKEN\n    ref: secrets.gh\n"+
		"  - name: CI\n    ref: env.CI\n"+
		"  - name: MODE\n    value: fast\n")
	set := Bind(p, p.Env, map[string]string{"gh": "ghp_x"},
		lookupFrom(map[string]string{"CI": "true"}), Options{})

	secret := map[string]bool{}
	for _, v := range set.Vars {
		secret[v.Name] = v.Secret
	}
	if !secret["GH_TOKEN"] {
		t.Error("GH_TOKEN came from secrets. and should be marked Secret")
	}
	if secret["CI"] {
		t.Error("CI came from env. and should not be marked Secret")
	}
	if secret["MODE"] {
		t.Error("MODE is a literal value and should not be marked Secret")
	}
}

// A chain takes the first element that yields a value, which is what lets one
// variable keep a shell override and gain a store fallback at the same time.
func TestBindWalksAChainInOrder(t *testing.T) {
	p := profileWith(t, "secrets:\n  - gh\nenv:\n  - name: GH_TOKEN\n"+
		"    refs: [env.GH_TOKEN, secrets.gh]\n")
	shell := Bind(p, p.Env, map[string]string{"gh": "from-the-store"},
		lookupFrom(map[string]string{"GH_TOKEN": "from-the-shell"}), Options{})
	if len(shell.Vars) != 1 || shell.Vars[0].Value != "from-the-shell" {
		t.Errorf("the shell override lost to the store: %+v", shell.Vars)
	}
	// The shell's copy is not a secret, so it keeps the plain annotation and
	// the argv exemption a store secret gets does not apply to it.
	if len(shell.Names) != 1 || shell.Names[0] != "GH_TOKEN" {
		t.Errorf("Names = %v, want [GH_TOKEN]", shell.Names)
	}

	store := Bind(p, p.Env, map[string]string{"gh": "from-the-store"}, lookupFrom(nil), Options{})
	if len(store.Vars) != 1 || store.Vars[0].Value != "from-the-store" {
		t.Errorf("the store fallback was not reached: %+v", store.Vars)
	}
	if len(store.Names) != 1 || store.Names[0] != "GH_TOKEN(secret)" {
		t.Errorf("Names = %v, want [GH_TOKEN(secret)]", store.Names)
	}
}

// An element that missed is a step along the road, not the end of it: only a
// last element that is a secrets. ref has anything to complain about, or every
// chained binding would warn on every run that used its first element.
func TestBindDoesNotWarnAboutAnEarlierChainElement(t *testing.T) {
	p := profileWith(t, "secrets:\n  - gh\nenv:\n  - name: GH_TOKEN\n"+
		"    refs: [env.GH_TOKEN, secrets.gh]\n")
	set := Bind(p, p.Env, map[string]string{"gh": "from-the-store"}, lookupFrom(nil), Options{})
	if len(set.Warnings) != 0 {
		t.Errorf("warnings = %v; want none: the chain resolved", set.Warnings)
	}
}

// A declared, optional secret the store does not have is Bind's business to
// stay quiet about: resolution has already said so, naming the secret and the
// command that supplies it.
//
// The message this replaced said the name was "not on this profile's secrets
// list" while it was sitting there in the file -- which nothing could reach
// until a shipped profile bound an optional secret through a chain, and then
// `brig env claude-code` said it on any host with no stored gh-token.
func TestBindIsSilentAboutADeclaredOptionalSecret(t *testing.T) {
	p := profile.Profile{
		Name:    "mine",
		Secrets: []profile.SecretDecl{{Name: "gh-token", Required: ptr(false)}},
	}
	bindings := []profile.EnvBinding{{Name: "GH_TOKEN", Refs: []string{"env.GH_TOKEN", "secrets.gh-token"}}}

	set := Bind(p, bindings, map[string]string{}, lookupFrom(nil), Options{})

	if set.Has("GH_TOKEN") {
		t.Errorf("bound a secret that was never resolved: %+v", set.Names)
	}
	for _, w := range set.Warnings {
		if strings.Contains(w, "not on this profile's secrets list") {
			t.Errorf("said a declared secret is not declared: %s", w)
		}
	}
	if len(set.Warnings) != 0 {
		t.Errorf("warned twice about one missing optional secret: %v", set.Warnings)
	}
}

// A ref to a name nothing declared still warns, because nothing ever looked
// for it: Validate refuses that in a file, so it means a Profile built in Go
// whose secrets list does not cover its own bindings.
//
// And it is a different mistake from an emptied secret, so it says so: telling
// the caller a name absent from the map "is empty, not absent" would send them
// to `brig secret update` for a secret nothing declared.
func TestBindWarnsAboutAnUndeclaredSecretRef(t *testing.T) {
	p := profile.Profile{Name: "mine"}
	bindings := []profile.EnvBinding{{Name: "GH_TOKEN", Ref: "secrets.gh-token"}}

	set := Bind(p, bindings, map[string]string{}, lookupFrom(nil), Options{})

	if len(set.Warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", set.Warnings)
	}
	if !strings.Contains(set.Warnings[0], "not on this profile's secrets list") {
		t.Errorf("said nothing about a ref no secrets list covers: %s", set.Warnings[0])
	}
	if strings.Contains(set.Warnings[0], "not absent") {
		t.Errorf("an absent secret is reported as an emptied one: %s", set.Warnings[0])
	}
}
