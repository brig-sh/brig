package creds

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
)

// storedFrom answers Preview's question about the store from a set of names,
// the way a listing answers it: present or not, and never a value.
func storedFrom(names ...string) func(string) (bool, bool) {
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	return func(n string) (bool, bool) { return have[n], true }
}

// unlisted is a store that did not answer a listing, so nothing about it is
// known.
func unlisted(string) (bool, bool) { return false, false }

func previewByName(ps []Planned) map[string]Planned {
	out := map[string]Planned{}
	for _, p := range ps {
		out[p.Name] = p
	}
	return out
}

// A required secret the store does not have is the case `brig info` exits 6
// on. The preview names it as unresolved and carries on, because the question
// it answers is what a run gets, and "this one is missing" is part of the
// answer.
func TestPreviewMarksAMissingRequiredSecretUnresolved(t *testing.T) {
	p := profileWith(t, "secrets:\n  - gh\nenv:\n  - name: GH_TOKEN\n    ref: secrets.gh\n")
	got := previewByName(Preview(p, p.Env, storedFrom(), lookupFrom(nil), Options{}))
	c, ok := got["GH_TOKEN"]
	if !ok {
		t.Fatalf("GH_TOKEN is not in the preview: %+v", got)
	}
	if c.State != StateUnresolved || c.Secret != "gh" || !c.Required {
		t.Errorf("GH_TOKEN = %+v, want unresolved, secret gh, required", c)
	}
}

// A store that cannot be listed says nothing about what is in it. The row says
// so and claims nothing about the secret.
func TestPreviewDoesNotGuessAtAStoreItCannotList(t *testing.T) {
	p := profileWith(t, "secrets:\n  - gh\nenv:\n  - name: GH_TOKEN\n    ref: secrets.gh\n")
	got := previewByName(Preview(p, p.Env, unlisted, lookupFrom(nil), Options{}))
	if c := got["GH_TOKEN"]; c.State != StateUnknown {
		t.Errorf("GH_TOKEN = %+v, want unknown", c)
	}
}

// The denylist guard applies to a previewed value exactly as it does to a
// bound one, so the preview does not promise a credential the run drops.
func TestPreviewWithholdsADeniedName(t *testing.T) {
	p := profileWith(t, "deny:\n  - K\n")
	bindings := []profile.EnvBinding{{Name: "K", Ref: "env.K"}}
	lookup := lookupFrom(map[string]string{"K": "sk-x"})

	got := previewByName(Preview(p, bindings, storedFrom(), lookup, Options{}))
	if c := got["K"]; c.State != StateWithheld {
		t.Errorf("K = %+v, want withheld", c)
	}
	got = previewByName(Preview(p, bindings, storedFrom(), lookup, Options{AllowDenied: true}))
	if c := got["K"]; c.State != StateResolved || c.Source != SourceEnvironment {
		t.Errorf("K with the guard off = %+v, want resolved from the environment", c)
	}
}

// An unresolved secret-manager reference in the shell is dropped by the run,
// so the preview marks it withheld.
func TestPreviewWithholdsAnUnresolvedReference(t *testing.T) {
	p := profileWith(t, "env:\n  - name: TOK\n    ref: env.TOK\n")
	got := previewByName(Preview(p, p.Env, storedFrom(),
		lookupFrom(map[string]string{"TOK": "op://vault/item"}), Options{}))
	if c := got["TOK"]; c.State != StateWithheld {
		t.Errorf("TOK = %+v, want withheld", c)
	}
}

// A file binding is delivered as a file, and its row is the guest path, so the
// reader sees where the credential lands.
func TestPreviewNamesAFileByItsGuestPath(t *testing.T) {
	p := profileWith(t, "secrets:\n  - tok\n"+
		"volumes:\n  - kind: tmpfs\n    path: .config\n"+
		"files:\n  - ref: secrets.tok\n    path: .config/cred\n    mode: \"0600\"\n")
	got := previewByName(Preview(p, p.Env, storedFrom("tok"), lookupFrom(nil), Options{}))
	c, ok := got[".config/cred"]
	if !ok {
		t.Fatalf("the file is not in the preview: %+v", got)
	}
	if c.Delivery != DeliveryFile || c.State != StateResolved || c.Secret != "tok" {
		t.Errorf("file = %+v, want a resolved file from the secret tok", c)
	}
}

// The preview and the run walk the same chain. Given a store holding what the
// listing says it holds, every variable the preview calls resolved is one Bind
// hands the guest, and nothing Bind hands the guest is missing from it.
func TestPreviewAgreesWithBind(t *testing.T) {
	p := profileWith(t, "secrets:\n  - gh\n  - name: opt\n    required: false\n"+
		"deny:\n  - DENIED\n"+
		"env:\n"+
		"  - name: GH_TOKEN\n    refs: [env.GH_TOKEN, secrets.gh]\n"+
		"  - name: OPT\n    ref: secrets.opt\n"+
		"  - name: CI\n    ref: env.CI\n"+
		"  - name: UNSET\n    ref: env.UNSET\n"+
		"  - name: MODE\n    value: fast\n")
	bindings := append(append([]profile.EnvBinding{}, p.Env...),
		profile.EnvBinding{Name: "DENIED", Ref: "env.DENIED"})
	env := lookupFrom(map[string]string{"CI": "true", "DENIED": "x"})

	for _, store := range []map[string]string{
		{"gh": "ghp_x"},
		{"gh": "ghp_x", "opt": "o"},
		{},
	} {
		var names []string
		for n := range store {
			names = append(names, n)
		}
		set := Bind(p, bindings, store, env, Options{})
		var bound []string
		for _, v := range set.Vars {
			if v.Name == "MODE" {
				continue // a literal is configuration, not a credential
			}
			bound = append(bound, v.Name)
		}
		var previewed []string
		for _, c := range Preview(p, bindings, storedFrom(names...), env, Options{}) {
			if c.Delivery == DeliveryEnv && c.State == StateResolved {
				previewed = append(previewed, c.Name)
			}
		}
		sort.Strings(bound)
		sort.Strings(previewed)
		if len(bound) != len(previewed) {
			t.Errorf("store %v: Bind delivered %v, Preview resolved %v", names, bound, previewed)
			continue
		}
		for i := range bound {
			if bound[i] != previewed[i] {
				t.Errorf("store %v: Bind delivered %v, Preview resolved %v", names, bound, previewed)
				break
			}
		}
	}
}

// The rows come out in one order whatever order the profile wrote them in,
// because the plan digest is taken over them.
func TestPreviewIsSorted(t *testing.T) {
	p := profileWith(t, "env:\n  - name: B\n    ref: env.B\n  - name: A\n    ref: env.A\n")
	got := Preview(p, p.Env, storedFrom(), lookupFrom(map[string]string{"A": "1", "B": "2"}), Options{})
	if len(got) != 2 || got[0].Name != "A" || got[1].Name != "B" {
		t.Errorf("Preview = %+v, want A then B", got)
	}
}

// A store that did not answer a listing is one the run does not read either,
// and Bind falls through to the next element. The preview does the same, so a
// shell fallback after an unlisted secret is resolved, and only a chain that
// ends on the unlisted store is unknown.
func TestPreviewFallsThroughAStoreItCannotList(t *testing.T) {
	p := profileWith(t, "secrets:\n  - name: foo\n    required: false\n"+
		"env:\n  - name: BAR\n    refs: [secrets.foo, env.BAR]\n")

	env := lookupFrom(map[string]string{"BAR": "b"})
	got := previewByName(Preview(p, p.Env, unlisted, env, Options{}))
	if c := got["BAR"]; c.State != StateResolved || c.Source != SourceEnvironment {
		t.Errorf("BAR with the shell set = %+v, want resolved from the environment", c)
	}
	set := Bind(p, p.Env, map[string]string{}, env, Options{})
	if len(set.Vars) != 1 || set.Vars[0].Name != "BAR" {
		t.Errorf("Bind delivered %+v, want BAR alone", set.Vars)
	}

	got = previewByName(Preview(p, p.Env, unlisted, lookupFrom(nil), Options{}))
	if c := got["BAR"]; c.State != StateUnknown {
		t.Errorf("BAR with the shell unset = %+v, want unknown", c)
	}
}

// A required secret stops the run whatever the chain does with it. A chain
// that passes over a missing one and resolves from the shell still leaves the
// run stopped, so the preview names the secret in a row of its own.
func TestPreviewNamesARequiredSecretTheChainPassedOver(t *testing.T) {
	p := profileWith(t, "secrets:\n  - x\nenv:\n  - name: X\n    refs: [secrets.x, env.X]\n")
	env := lookupFrom(map[string]string{"X": "v"})

	for _, tc := range []struct {
		store func(string) (bool, bool)
		want  string
	}{
		{storedFrom(), StateUnresolved},
		{unlisted, StateUnknown},
	} {
		got := previewByName(Preview(p, p.Env, tc.store, env, Options{}))
		c, ok := got["x"]
		if !ok {
			t.Errorf("want %s: the required secret x has no row: %+v", tc.want, got)
			continue
		}
		if c.Delivery != DeliveryNone || c.State != tc.want || !c.Required {
			t.Errorf("x = %+v, want a required %s row with no delivery", c, tc.want)
		}
	}
}

// The agreement above, through the path a run takes. ResolveSecrets reads the
// secrets Needed names out of the store, and Bind binds what it returned.
// Every variable the preview calls resolved is one the run hands the guest,
// every file it calls resolved is one the run has a value for, and a run that
// stops on a missing required secret is one the preview names unresolved.
func TestPreviewAgreesWithResolveSecrets(t *testing.T) {
	p := profileWith(t, "secrets:\n  - gh\n  - tok\n  - name: opt\n    required: false\n"+
		"env:\n"+
		"  - name: GH_TOKEN\n    refs: [env.GH_TOKEN, secrets.gh]\n"+
		"  - name: OPT\n    ref: secrets.opt\n"+
		"  - name: CI\n    ref: env.CI\n"+
		"  - name: MODE\n    value: fast\n"+
		"volumes:\n  - kind: tmpfs\n    path: .config\n"+
		"files:\n  - ref: secrets.tok\n    path: .config/cred\n")

	for _, tc := range []struct {
		name  string
		shell map[string]string
		store fakeStore
		stops bool
	}{
		{"every secret in the store", nil, fakeStore{"gh": "g", "tok": "t", "opt": "o"}, false},
		{"the optional one missing", nil, fakeStore{"gh": "g", "tok": "t"}, false},
		{"the shell answers first", map[string]string{"GH_TOKEN": "s", "CI": "1"},
			fakeStore{"gh": "g", "tok": "t"}, false},
		{"a required secret missing", nil, fakeStore{"tok": "t"}, true},
		{"a required secret the shell passed over", map[string]string{"GH_TOKEN": "s"},
			fakeStore{"tok": "t"}, true},
		{"the file's secret missing", nil, fakeStore{"gh": "g"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := lookupFrom(tc.shell)
			var listed []string
			for n := range tc.store {
				listed = append(listed, n)
			}
			preview := Preview(p, p.Env, storedFrom(listed...), env, Options{})
			res, err := ResolveSecrets(p, "brig-x",
				func() (SecretReader, error) { return tc.store, nil }, env)

			var blocking []string
			for _, c := range preview {
				if c.Required && c.State != StateResolved {
					blocking = append(blocking, c.Secret)
				}
			}
			if tc.stops {
				var missing *MissingSecretsError
				if !errors.As(err, &missing) {
					t.Fatalf("the run did not stop: %v", err)
				}
				var names []string
				for _, m := range missing.Missing {
					names = append(names, m.Name)
				}
				sort.Strings(names)
				sort.Strings(blocking)
				if strings.Join(names, ",") != strings.Join(blocking, ",") {
					t.Errorf("the run stops on %v, the preview names %v", names, blocking)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveSecrets: %v", err)
			}
			if len(blocking) != 0 {
				t.Errorf("the run goes ahead and the preview names %v as stopping it", blocking)
			}

			set := Bind(p, p.Env, res.Values, env, Options{})
			var bound, previewed []string
			for _, v := range set.Vars {
				if v.Name != "MODE" {
					bound = append(bound, v.Name)
				}
			}
			for _, c := range preview {
				switch {
				case c.Delivery == DeliveryEnv && c.State == StateResolved:
					previewed = append(previewed, c.Name)
				case c.Delivery == DeliveryFile:
					_, has := res.Values[c.Secret]
					if has != (c.State == StateResolved) {
						t.Errorf("file %s: preview %s, the run has a value: %v", c.Name, c.State, has)
					}
				}
			}
			sort.Strings(bound)
			sort.Strings(previewed)
			if strings.Join(bound, ",") != strings.Join(previewed, ",") {
				t.Errorf("Bind delivered %v, Preview resolved %v", bound, previewed)
			}
		})
	}
}
