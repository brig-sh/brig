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
func (s *Set) Add(name, value, reportAs string) {
	s.Vars = append(s.Vars, runtime.Var{Name: name, Value: value})
	if reportAs == "" {
		reportAs = name
	}
	s.Names = append(s.Names, reportAs)
}

// AddSecret appends a variable whose value brig resolved on the user's behalf:
// from the store it owns, or from the host credential -- however that was
// obtained, keychain or BRIG_CREDENTIALS_CMD. It is reported like any other
// credential, but it never travels in argv: the host durably logs every exec's
// argv, so such a value there would outlive the sandbox in a file the user
// never sees.
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
// the keychain read and BRIG_CREDENTIALS_CMD called AddSecret directly, so a
// profile that denies its own target variable did not deny this path, and a
// credentials command that printed "op://vault/item" -- which is exactly what
// a secret-manager helper prints when it is not logged in -- forwarded that
// string as the login and left the guest failing to authenticate for a reason
// nothing said out loud.
//
// Treated as ambient, like an environment-sourced value, because that is what
// it is: brig did not write this blob, it read whatever the keychain or the
// user's own command handed back.
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

// ReadHost resolves the profile's host credential.
//
// The default backend is the macOS keychain, which is where the host's own
// login already lives -- that is what lets a fresh sandbox work without
// anyone minting a token. Any other backend is a command that prints the same
// JSON on stdout:
//
//	BRIG_CREDENTIALS_CMD='your-secret-tool read claude/credentials'
//
// Absence is ordinary: nobody has run the agent on this host. It is reported
// as (nil, nil), not as an error. A failing custom command IS an error,
// because its stderr is the only explanation the caller will ever get.
func ReadHost(hc *profile.HostCredential, command string) (*HostCredential, error) {
	if hc == nil {
		return nil, nil
	}
	var blob []byte
	source := ""
	if command != "" {
		out, err := runShell(command)
		if err != nil {
			return nil, fmt.Errorf("BRIG_CREDENTIALS_CMD failed: %w", err)
		}
		blob, source = out, "BRIG_CREDENTIALS_CMD"
	} else {
		out, err := keychain(hc.KeychainService)
		if err != nil {
			// The keychain's own stderr is noise rather than an explanation
			// here: not having run the agent on this host is the common case.
			return nil, nil
		}
		blob, source = out, "the host keychain"
	}
	blob = bytes.TrimSpace(blob)
	if len(blob) == 0 {
		return nil, nil
	}
	token, _ := findString(blob, hc.TokenField)
	if token == "" {
		return nil, nil
	}
	expiry, _ := findNumber(blob, hc.ExpiryField)
	return &HostCredential{Token: token, ExpiresAt: expiry, Source: source}, nil
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

func runShell(command string) ([]byte, error) {
	cmd := exec.Command("sh", "-c", command)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg != "" {
			return nil, fmt.Errorf("%w: %s", err, firstLines(msg, 3))
		}
		return nil, err
	}
	return out.Bytes(), nil
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " ")
}

// findString and findNumber are jsonfind under the names ReadHost already
// uses. Both go when ReadHost does.
func findString(blob []byte, field string) (string, bool) { return jsonfind.String(blob, field) }
func findNumber(blob []byte, field string) (int64, bool)  { return jsonfind.Number(blob, field) }
