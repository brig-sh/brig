package wrap

import "fmt"

// The session-claim index: which session name owns each sandbox.
//
// A session name is turned into a short slug for its paths, and the sandbox is
// brig-<profile>-<slug>. The slug budget is tight, so two different names that
// share their leading characters can shorten to one slug and land on one
// sandbox and one workspace -- with whatever credentials each was handed. brig
// used to warn about that and carry on, which left two agents in one guest home
// with different logins. Refuse the second name instead.
//
// The first name to take a sandbox writes down that it owns it; a later run
// whose name shortens to the same sandbox but is not that name is refused and
// told to pick another --name. The same name returning is the ordinary repeat
// run and is let through.
//
// Keyed by sandbox name, not by the bare slug: the sandbox name carries the
// profile, and two profiles that shorten to one slug share nothing, since the
// sandbox and the workspace both include the profile. It is also the key rm and
// reset already have in hand, so releasing a claim sits beside ForgetWorkspace
// on both paths. Same file conventions as the workspace index (see index.go):
// atomic write, an unusable file treated as empty, and a bookkeeping failure
// never fatal to a command that would otherwise work.

// sessionIndexName is the file, inside stateDir.
const sessionIndexName = "sessions.json"

// claimSlug records that this run's session name owns its sandbox, and refuses
// the run when a different name owns it already.
//
// An unnamed run cannot collide: its sandbox is brig-<profile> with no slug,
// and every unnamed run of a profile is meant to be the one session. So it
// claims nothing and is always let through.
//
// A failure to write the claim is a warning, not an error, the way
// rememberWorkspace treats its own: the sandbox this run wants is free, so
// refusing it over a bookkeeping file that could not be rewritten would deny
// the thing that was actually asked for. The only cost is that a later
// colliding name goes uncaught.
func (c *Config) claimSlug() error {
	if c.RawName == "" {
		return nil
	}
	index := readIndex(sessionIndexName)
	owner, taken := index[c.VMName]
	if taken && owner != c.RawName {
		return fmt.Errorf("session %q shortens to %q, the sandbox session %q already "+
			"uses (%s). Sharing it would put both agents in one home directory with "+
			"whichever credentials arrived last, so run one under a different --name",
			c.RawName, c.Slug, owner, c.VMName)
	}
	if owner == c.RawName {
		return nil
	}
	index[c.VMName] = c.RawName
	if err := writeIndex(sessionIndexName, index); err != nil {
		c.warnf("could not record %s as the owner of %s (%v). A later run whose name "+
			"shortens to the same sandbox will not be refused.", c.RawName, c.VMName, err)
	}
	return nil
}

// ForgetSession drops a removed sandbox's claim, so the next session name that
// shortens to it can take it. Errors are dropped the way ForgetWorkspace drops
// its own: a removal that worked must not report a failure because a
// bookkeeping file could not be rewritten.
func ForgetSession(vmName string) {
	index := readIndex(sessionIndexName)
	if _, ok := index[vmName]; !ok {
		return
	}
	delete(index, vmName)
	_ = writeIndex(sessionIndexName, index)
}
