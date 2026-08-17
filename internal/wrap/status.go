package wrap

import (
	"strings"

	"github.com/brig-sh/brig/internal/creds"
)

// Status reports what this invocation would do, by name only -- never a
// value. It is the answer to "is the sandbox going to be authenticated, and
// as whom", asked without starting anything.
func (c *Config) Status(set creds.Set) {
	if c.RawName != "" {
		c.sayf("session %q -> %s (sandbox %s)", c.RawName, c.Workspace, c.VMName)
	} else {
		c.sayf("workspace %s (sandbox %s)", c.Workspace, c.VMName)
	}
	c.sayf("runtime %s (%s)", c.Runtime.Kind(), c.Runtime.Bin())
	c.sayf("image %s (pull %s)", c.Image, c.Pull)

	names := credentialNames(set)
	if len(names) == 0 {
		c.sayf("forwarding no credentials (none of: %s)",
			strings.Join(bindingNames(c.Env), " "))
	} else {
		c.sayf("forwarding to guest:")
		for _, n := range names {
			c.sayf("  %s", n)
		}
	}
	if len(c.Profile.Deny) > 0 {
		c.sayf("never forwarded for %s: %s (they would move this sandbox onto metered billing)",
			c.Profile.Name, strings.Join(c.Profile.Deny, " "))
	}

	if c.GitConfig {
		c.sayf("guest git over HTTPS: on, user %q (hosts: %s)", c.GitUser,
			strings.Join(c.GitHosts, " "))
	} else {
		c.sayf("guest git over HTTPS: off (BRIG_GIT_CONFIG=1 to enable)")
	}

	// Where the guest's login comes from, and whether it is usable: an
	// expired host token is exactly what sends the sandbox back to its login
	// screen. Reported from what is actually being forwarded, not from the
	// keychain alone -- with a token from the environment and no host login,
	// consulting the keychain would report "no credential" about a sandbox
	// that authenticates fine.
	if hc := c.Profile.HostCredential; hc != nil {
		source, forwarded := sourceOf(names, hc.TargetVar)
		switch {
		case forwarded && source == "secret":
			c.sayf("guest login: from %s in the secret store", hc.TargetVar)
		case forwarded && source == "":
			c.sayf("guest login: from %s in the environment (nothing on disk)", hc.TargetVar)
		case c.HostCred != nil && c.HostCred.Expired(nowMilli()):
			c.sayf("guest login: host credential found but EXPIRED (%s)", hc.RenewHint)
		case c.HostCred != nil:
			c.sayf("guest login: from %s, forwarded as environment (nothing on disk)",
				c.HostCred.Source)
		default:
			c.sayf("guest login: no host credential found (the sandbox will ask you to log in)")
		}
	}

	// Identity is resolved from the invoking directory, so report which one:
	// with per-directory includeIf rules the answer changes as you move
	// around the filesystem.
	if c.GitIdentity {
		c.sayf("guest commit identity, as resolved in %s:", c.Cwd)
		c.sayf("  %s <%s> (unsigned)", orUnset(c.GitName), orUnset(c.GitEmail))
	}
}

// credentialNames is the reporting list: the plumbing variables brig adds of
// its own accord (git identity, the terminal-prompt guard) are not
// credentials and must not be reported as if they were.
func credentialNames(set creds.Set) []string { return set.Names }

// sourceOf finds a variable in the reporting names and says where its value
// came from: "" for the ambient environment, or the annotation Set carries --
// "host" for the host credential, "secret" for the store.
//
// Matching is on the bare name because the annotation is part of the entry, not
// part of the variable: comparing the whole string finds "TOK" and misses
// "TOK(secret)", which is how a sandbox authenticating fine from the keychain
// gets reported as having no credential at all -- the exact misreport the
// comment above says this code exists to prevent.
func sourceOf(names []string, want string) (string, bool) {
	for _, s := range names {
		bare, annotation := s, ""
		if i := strings.LastIndex(s, "("); i > 0 && strings.HasSuffix(s, ")") {
			bare, annotation = s[:i], s[i+1:len(s)-1]
		}
		if bare == want {
			return annotation, true
		}
	}
	return "", false
}

func orUnset(s string) string {
	if s == "" {
		return "<unset>"
	}
	return s
}

// nowMilli is split out so tests can reason about expiry without a clock.
var nowMilli = defaultNowMilli
