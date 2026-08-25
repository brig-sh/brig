package wrap

import (
	"strings"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
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
	// The runtime is the one line here that needs a runtime. When none is on
	// PATH env still reports the rest -- the profile, the environment, the host
	// git config -- and marks this line unavailable rather than failing the
	// whole report, because the person reading it is often the one whose runtime
	// is what broke.
	if c.Runtime != nil {
		c.sayf("runtime %s (%s)", c.Runtime.Kind(), c.Runtime.Bin())
	} else {
		c.sayf("runtime unavailable (no runtime found on PATH)")
	}
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
	c.reportDeny(set)
	c.reportOverride()

	if c.GitConfig {
		c.sayf("guest git over HTTPS: on, user %q (hosts: %s)", c.GitUser,
			strings.Join(c.GitHosts, " "))
	} else {
		c.sayf("guest git over HTTPS: off (BRIG_GIT_CONFIG=1 to enable)")
	}
	c.reportArgv(set)

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

	// Where an imported login comes from, and whether it is still good --
	// the same question the block above answers from Profile.HostCredential,
	// asked instead of the store's own provenance for a profile that has
	// moved to secrets: rather than hostCredential:. Both can report at once
	// while a profile still carries the old field; a later PR drops it, and
	// this block keeps reporting on its own. listSecrets (expiry.go) is the
	// same no-decrypt read warnExpiredSecrets uses, so a status report raises
	// no keychain dialog either.
	if secrets, ok := c.listSecrets(); ok {
		for _, decl := range c.Profile.Secrets {
			s, found := secrets[decl.Name]
			if !found || s.Provenance.From == "" {
				// Not present, or hand-created: provenance is the only thing
				// that says a secret was imported rather than typed in by
				// hand, and this line is about an imported login.
				continue
			}
			if s.Provenance.ExpiresAt != 0 && s.Provenance.ExpiresAt < nowMilli() {
				c.sayf("guest login: %s, imported from %s (EXPIRED)", s.Name, s.Provenance.From)
			} else {
				c.sayf("guest login: %s, imported from %s", s.Name, s.Provenance.From)
			}
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

// reportArgv says that BRIG_ENV_ARGV is on and which values it will put on the
// runtime's command line, and says nothing at all when it is off.
//
// It is reported here because this preview is where a user asks what a sandbox
// is about to be handed, and until now the answer left out the one setting that
// changes where those values end up. The silence when the hatch is off is
// deliberate rather than an omission: brig keeps values out of argv as a matter
// of course, and a line restating that on every `brig env` would bury the lines
// that are about this run.
//
// Named from the resolved set for the same reason reportDeny is: the hatch on
// its own is a claim about the future, and what this run will actually put on
// the command line is the thing worth knowing.
func (c *Config) reportArgv(set creds.Set) {
	names := runtime.ArgvExposed(set.Vars)
	if len(names) == 0 {
		return
	}
	c.sayf("BRIG_ENV_ARGV=1, so these values go on the runtime's command line, "+
		"where `ps` can read them: %s", strings.Join(names, " "))
}

// warnArgvExposure is reportArgv on stderr, for a run rather than a preview.
//
// The hatch says it is opt-in in a comment, and the opting in happens once, in
// a shell profile, for a runtime build that has since been replaced. Nothing
// has said a word about it since. The shipped codex and claude-code profiles
// resolve GH_TOKEN from the environment first, and an environment-sourced value
// is not one brig resolved on the user's behalf, so the common case is a real
// credential going onto the command line on every run with nothing said -- while
// the credential deliberately imported into brig's own store, the rarer of the
// two, is the one the exemption protects.
//
// One line, before the runtime is invoked, so it is on screen before the value
// is anywhere anything else can read it. BuildEnv is the last point where the
// whole forwarded set is known and nothing has been spawned yet.
//
// The list is everything whose value lands on the command line, brig's own
// plumbing included -- GIT_TERMINAL_PROMPT is not a credential and is on there
// all the same. A warning that named only the interesting half would be a false
// statement about the command line, and the command line is the whole subject.
func (c *Config) warnArgvExposure(set creds.Set) {
	names := runtime.ArgvExposed(set.Vars)
	if len(names) == 0 {
		return
	}
	c.warnf("BRIG_ENV_ARGV=1 puts these values on the runtime's command line, where "+
		"`ps` can read them and the host's argv log keeps them after the sandbox is "+
		"gone: %s. Unset BRIG_ENV_ARGV to forward them by name only",
		strings.Join(names, " "))
}

// reportDeny says what the denylist did to THIS run, rather than reciting the
// list.
//
// The list on its own is a claim about the future, and BRIG_ALLOW_DENIED=1
// makes it a false one: the report said "forwarding to guest:
// ANTHROPIC_API_KEY" and, four lines later, "never forwarded for claude-code:
// ANTHROPIC_API_KEY". This preview is the only window a user has into what a
// sandbox is about to receive, so it is answered from the resolved set: what
// was actually admitted, what is still being withheld, and -- when the guard
// is off -- that it is off, whether or not anything happened to trip it this
// time.
func (c *Config) reportDeny(set creds.Set) {
	if len(c.Profile.Deny) == 0 {
		return
	}
	var forwarded, withheld []string
	for _, name := range c.Profile.Deny {
		if set.Has(name) {
			forwarded = append(forwarded, name)
			continue
		}
		withheld = append(withheld, name)
	}
	// Quote the setting the user actually wrote, not a hardcoded =1: with the
	// strict reading BRIG_ALLOW_DENIED=true is what turned this on, and a report
	// that answers "why is my sandbox on metered billing" with a value the user
	// never set sends them looking for a variable that is not there.
	override := c.env.setting("ALLOW_DENIED")
	if len(forwarded) > 0 {
		c.sayf("DENYLIST OVERRIDDEN by %s, forwarding: %s "+
			"(this moves the sandbox onto metered billing)", override, strings.Join(forwarded, " "))
	}
	if len(withheld) == 0 {
		return
	}
	if c.AllowDenied {
		c.sayf("denylist off for %s (%s), so these would be forwarded "+
			"if set: %s", c.Profile.Name, override, strings.Join(withheld, " "))
		return
	}
	c.sayf("never forwarded for %s: %s (they would move this sandbox onto metered billing)",
		c.Profile.Name, strings.Join(withheld, " "))
}

// reportOverride says when the profile being run came out of a file rather
// than out of brig, and what that file dropped.
//
// Overriding a built-in by name is deliberate -- it is how you pin your own
// image for a profile brig already knows about -- but it is also how the
// billing guard disappears: a file that omits deny: silently removes it, and
// the old report printed nothing at all in that case, because it printed the
// deny list and the list was empty. "There is a file I forgot about" and "why
// did my sandbox end up on metered billing" are the same question, so the
// answer names the file and the entries it dropped.
func (c *Config) reportOverride() {
	name := c.Profile.Name
	if !profile.OverridesBuiltIn(name) {
		return
	}
	where, _ := profile.Path(name)
	if where == "" {
		where = "a file in " + profile.Dir()
	}
	c.sayf("profile %s comes from %s, overriding the one brig ships", name, where)
	dropped := droppedDeny(profile.BuiltInDeny(name), c.Profile.Deny)
	if len(dropped) > 0 {
		c.sayf("  it drops the built-in denylist, so nothing stops these from reaching "+
			"the guest and moving it onto metered billing: %s", strings.Join(dropped, " "))
	}
}

// droppedDeny is what the built-in denied and the file no longer does.
func droppedDeny(builtin, current []string) []string {
	if len(builtin) == 0 {
		return nil
	}
	kept := make(map[string]bool, len(current))
	for _, name := range current {
		kept[name] = true
	}
	var dropped []string
	for _, name := range builtin {
		if !kept[name] {
			dropped = append(dropped, name)
		}
	}
	return dropped
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
