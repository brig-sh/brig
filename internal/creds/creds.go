// Package creds resolves credentials on the host and decides which of them
// reach the guest.
//
// The guest cannot see your keychain, your secret manager or your SSH agent,
// and that inaccessibility is the isolation boundary -- so credentials are
// resolved out here and forwarded in. brig resolves nothing itself for the
// environment path: every variable a profile names is read from brig's own
// environment, which means any backend works, from a secret manager's
// run-with-env command to a plain export.
//
// Values are re-read on every exec, so a long-lived sandbox picks up a
// rotated credential without a restart.
package creds

import (
	"strings"

	"github.com/brig-sh/brig/internal/notice"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
)

// Set is what a run forwards, plus what it decided not to.
type Set struct {
	// Vars are the variables to hand the runtime.
	Vars []runtime.Var
	// Names are the credential names for reporting, never values. A name may
	// be annotated, e.g. "CLAUDE_CODE_OAUTH_TOKEN(secret)" for one that came
	// from the store rather than the environment.
	Names []string
	// Warnings explain each variable that was dropped.
	Warnings []string
}

// Add appends a variable and records its reporting name.
//
// Add rather than AddSecret for a credential that came out of the ambient
// environment, and the asymmetry is deliberate rather than an oversight worth
// closing. Marking every resolved credential Secret whatever its source was
// considered: it would have left BRIG_ENV_ARGV applying only to values that are
// not credentials, which is every value that has no need of it. GH_TOKEN and the
// agent's own token are the whole of what a run forwards from the environment;
// what would be left for the hatch to carry is GIT_TERMINAL_PROMPT and the git
// identity. A runtime build that cannot take a bare `--env KEY` -- the one thing
// the hatch exists for -- would go on being handed `--env GH_TOKEN` with no
// value attached, and the sandbox would come up unauthenticated with nothing
// said about why. A hatch that silently stops carrying the values it was built
// to carry is worse than one whose cost is visible.
//
// So the cost is made visible instead: a run that puts values in argv says which
// ones, every time, before the runtime is invoked, and `brig env` reports the
// setting. See wrap.warnArgvExposure and wrap.reportArgv. What stays true either
// way is that a value brig resolved on the user's behalf -- one the user never
// chose to expose anywhere -- is never on the command line, whatever the hatch
// says; see AddSecret.
func (s *Set) Add(name, value, reportAs string) {
	s.Vars = append(s.Vars, runtime.Var{Name: name, Value: value})
	if reportAs == "" {
		reportAs = name
	}
	s.Names = append(s.Names, reportAs)
}

// AddSecret appends a variable whose value brig resolved on the user's behalf
// from the store it owns. It is reported like any other credential, but it
// never travels in argv: the host durably logs every exec's argv, so such a
// value there would outlive the sandbox in a file the user never sees.
func (s *Set) AddSecret(name, value, reportAs string) {
	s.Vars = append(s.Vars, runtime.Var{Name: name, Value: value, Secret: true})
	if reportAs == "" {
		reportAs = name
	}
	s.Names = append(s.Names, reportAs)
}

// AddPlumbing appends a variable that is not a credential, so it does not get
// reported as one. GIT_TERMINAL_PROMPT and the git username go this way.
func (s *Set) AddPlumbing(name, value string) {
	s.Vars = append(s.Vars, runtime.Var{Name: name, Value: value})
}

// Has reports whether a variable is already being forwarded, ignoring any
// reporting annotation.
func (s *Set) Has(name string) bool {
	for _, v := range s.Vars {
		if v.Name == name {
			return true
		}
	}
	return false
}

// Options tune the forwarding rules.
type Options struct {
	// AllowRefs forwards a value that looks like an unresolved secret
	// reference instead of dropping it.
	AllowRefs bool
	// AllowDenied forwards a variable on the profile's denylist. Deliberate,
	// because the denylist is what keeps a metered API key from silently
	// replacing a subscription token.
	AllowDenied bool
}

// admit applies the two guards every value passes, whatever its source, and
// returns the warning explaining a refusal.
//
// fromEnv is whether the value came out of an ambient environment. The
// unresolved-reference guard applies only there: a literal in a profile is
// configuration its author wrote deliberately, and second-guessing it would
// refuse a perfectly good value that merely looks like a reference.
func admit(t profile.Profile, name, value string, fromEnv bool, opt Options) (string, bool) {
	if t.Denied(name) && !opt.AllowDenied {
		return notice.Newf("not forwarding %s: it is on the %s denylist", name, t.Name).
			Note("it outranks the subscription credential, and would move this sandbox onto "+
				"metered billing without saying so").
			Do("to forward it anyway", "BRIG_ALLOW_DENIED=1").String(), false
	}
	if fromEnv && !opt.AllowRefs {
		if scheme, ok := unresolvedRef(value); ok {
			// direnv and friends readily leave a secret-manager reference in the
			// ambient environment unresolved. Forwarded verbatim it yields
			// "Invalid username or token" in the guest, indistinguishable from a
			// wrong username or a broken helper. A real credential is a token
			// and never takes this form.
			//
			// The scheme is quoted only when it reads as one. What comes
			// before :// is part of the value, and a token with :// pasted
			// after it would otherwise be echoed here, and into `brig plan
			// --json`, which carries this text as a reason.
			shown := ""
			if plainScheme(scheme) {
				shown = " (" + scheme + "://...)"
			}
			return notice.Newf("not forwarding %s: it looks like an unresolved secret "+
				"reference%s, not a credential", name, shown).
				Do("resolve it on the host before you run brig", "").
				Do("to forward it as it is", "BRIG_ALLOW_REFS=1").String(), false
		}
	}
	return "", true
}

// plainScheme reports whether s is spelled the way a secret manager spells a
// scheme, such as op or vault: a lowercase letter, then lowercase letters,
// digits, +, . or -. A token's own prefix has upper case or an underscore in
// it more often than not, and fails this.
func plainScheme(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		switch ch := s[i]; {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9', ch == '+', ch == '.', ch == '-':
		default:
			return false
		}
	}
	return true
}

// unresolvedRef reports whether a value is a scheme://... reference that is
// not an ordinary URL. http and https pass through: those are real URLs a
// caller may legitimately be forwarding.
func unresolvedRef(value string) (string, bool) {
	scheme, rest, ok := strings.Cut(value, "://")
	if !ok || scheme == "" || rest == "" {
		return "", false
	}
	if strings.ContainsAny(scheme, " \t") {
		return "", false
	}
	switch scheme {
	case "http", "https":
		return "", false
	}
	return scheme, true
}
