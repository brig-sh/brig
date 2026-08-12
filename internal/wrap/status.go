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
		c.sayf("forwarding no credentials (none of: %s)", strings.Join(c.Forward, " "))
	} else {
		c.sayf("forwarding to guest:")
		for _, n := range names {
			c.sayf("  %s", n)
		}
	}
	if len(c.Agent.Deny) > 0 {
		c.sayf("never forwarded for %s: %s (they would move this sandbox onto metered billing)",
			c.Agent.Name, strings.Join(c.Agent.Deny, " "))
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
	if hc := c.Agent.HostCredential; hc != nil {
		switch {
		case contains(names, hc.TargetVar):
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

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func orUnset(s string) string {
	if s == "" {
		return "<unset>"
	}
	return s
}

// nowMilli is split out so tests can reason about expiry without a clock.
var nowMilli = defaultNowMilli
