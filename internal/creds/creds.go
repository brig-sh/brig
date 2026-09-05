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
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	"github.com/brig-sh/brig/internal/jsonfind"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
)

// Set is what a run forwards, plus what it decided not to.
type Set struct {
	// Vars are the variables to hand the runtime.
	Vars []runtime.Var
	// Names are the credential names for reporting, never values. A name may
	// be annotated, e.g. "CLAUDE_CODE_OAUTH_TOKEN(host)" for one that came
	// from the host credential rather than the environment.
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

// AddSecret appends a variable whose value brig resolved on the user's behalf:
// from the store it owns, or from the host credential it read out of the
// keychain. It is reported like any other credential, but it never travels in
// argv: the host durably logs every exec's argv, so such a value there would
// outlive the sandbox in a file the user never sees.
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
		return fmt.Sprintf(
			"not forwarding %s: it is on the %s denylist, because it outranks the "+
				"subscription credential and would move this sandbox onto metered "+
				"billing without saying so. Set BRIG_ALLOW_DENIED=1 if that is what "+
				"you want", name, t.Name), false
	}
	if fromEnv && !opt.AllowRefs {
		if scheme, ok := unresolvedRef(value); ok {
			// direnv and friends readily leave a secret-manager reference in the
			// ambient environment unresolved. Forwarded verbatim it yields
			// "Invalid username or token" in the guest, indistinguishable from a
			// wrong username or a broken helper. A real credential is a token
			// and never takes this form.
			return fmt.Sprintf(
				"not forwarding %s: it looks like an unresolved secret reference "+
					"(%s://...), not a credential. Resolve it on the host before "+
					"invoking brig, or set BRIG_ALLOW_REFS=1 to forward it as-is",
				name, scheme), false
		}
	}
	return "", true
}

// AdmitHost is admit for the credential brig reads off the host itself,
// exported because that one is resolved in wrap rather than in Bind.
//
// It was the one value that reached the guest without passing either guard:
// the keychain read called AddSecret directly, so a profile that denies its own
// target variable did not deny this path, and a blob whose token field held
// "op://vault/item" -- a secret-manager reference rather than a token -- was
// forwarded as the login and left the guest failing to authenticate for a
// reason nothing said out loud.
//
// Treated as ambient, like an environment-sourced value, because that is what
// it is: brig did not write this blob, it read whatever the keychain handed
// back.
func AdmitHost(p profile.Profile, name, value string, opt Options) (string, bool) {
	return admit(p, name, value, true, opt)
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

// HostCredential is a credential read from the host itself.
type HostCredential struct {
	Token string
	// ExpiresAt is epoch milliseconds, or zero when the blob carries none.
	ExpiresAt int64
	// Source describes where it came from, for reporting.
	Source string
}

// KeychainRead reads the raw credential blob a host keychain holds for one
// service. Only ReadHost calls it, and only a test supplies one of its own:
// the real read talks to the login keychain of whoever is running brig.
type KeychainRead func(service string) ([]byte, error)

// ReadHost resolves the profile's host credential.
//
// The backend is the macOS keychain, which is where the host's own login
// already lives -- that is what lets a fresh sandbox work without anyone
// minting a token. Any other backend is imported once, into brig's own store,
// rather than read on every run:
//
//	brig secret import <profile> <name> --from-command '<command>'
//
// That import is what replaced BRIG_CREDENTIALS_CMD, which used to point this
// read at a command of the user's own and ran it on every boot.
//
// read is nil everywhere but in tests, where it stands in for the keychain: a
// suite must not read, or depend on, the keychain of whoever runs it.
//
// Absence is ordinary: nobody has run the agent on this host, or this is not a
// Mac. It is reported as a nil credential rather than as a failure, and there
// is nothing else left to report: the keychain's own stderr is noise rather
// than an explanation when the common case is an item that was never created.
func ReadHost(hc *profile.HostCredential, read KeychainRead) *HostCredential {
	if hc == nil {
		return nil
	}
	if read == nil {
		read = keychain
	}
	out, err := read(hc.KeychainService)
	if err != nil {
		return nil
	}
	blob := bytes.TrimSpace(out)
	if len(blob) == 0 {
		return nil
	}
	token, _ := findString(blob, hc.TokenField)
	if token == "" {
		return nil
	}
	expiry, _ := findNumber(blob, hc.ExpiryField)
	return &HostCredential{Token: token, ExpiresAt: expiry, Source: "the host keychain"}
}

// Expired reports whether the credential is past its expiry. A blob carrying
// no expiry is not treated as expired: absence is not evidence.
func (c *HostCredential) Expired(nowUnixMilli int64) bool {
	return c != nil && c.ExpiresAt > 0 && c.ExpiresAt < nowUnixMilli
}

// securityBin is the macOS keychain tool, named by its absolute path for the
// same reason internal/secret names it that way: a bare "security" is resolved
// through whatever $PATH the invoking shell carried, and this call hands back
// the host's own login token, so a shim earlier in $PATH decides what brig
// forwards into the sandbox as the guest's credential. The tool ships with the
// operating system at a fixed path; on any other one this call fails, exactly
// as looking a missing binary up on $PATH did.
const securityBin = "/usr/bin/security"

func keychain(service string) ([]byte, error) {
	cmd := exec.Command(securityBin, "find-generic-password", "-s", service, "-w")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// findString and findNumber are jsonfind under the names ReadHost already
// uses. Both go when ReadHost does.
func findString(blob []byte, field string) (string, bool) { return jsonfind.String(blob, field) }
func findNumber(blob []byte, field string) (int64, bool)  { return jsonfind.Number(blob, field) }
