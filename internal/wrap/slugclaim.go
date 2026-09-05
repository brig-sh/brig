package wrap

import (
	"fmt"
	"os"
)

// The session-claim index: which session name owns each sandbox.
//
// A session name is turned into a slug for its paths, and the sandbox is
// brig-<profile>-<slug>. Sanitising is not one-to-one: Foo, foo! and foo are
// all foo, so two different names can land on one sandbox and one workspace --
// with whatever credentials each was handed. brig used to warn about that and
// carry on, which left two agents in one guest home with different logins.
// Refuse the second name instead.
//
// A slug used to be cut to ten characters as well, which collided two long
// names that shared their leading characters the same way. That cut is gone
// (see session.Slug) and this refusal is not: what remains is the collapse,
// which no length budget was ever protecting against.
//
// The first name to take a sandbox writes down that it owns it; a later run
// whose name lands on the same sandbox but is not that name is refused and
// told to pick another --name. The same name returning is the ordinary repeat
// run and is let through.
//
// Keyed by sandbox name, not by the bare slug: the sandbox name carries the
// profile, and two profiles that sanitise to one slug share nothing, since the
// sandbox and the workspace both include the profile. It is also the key rm and
// reset already have in hand, so releasing a claim sits beside ForgetSandbox
// on both paths. Same file conventions as the session index (see index.go):
// atomic write, an unusable file treated as empty, and a bookkeeping failure
// never fatal to a command that would otherwise work.
//
// A file of its own, and not the session index: this is the one question that
// cannot be asked of a ref-keyed file, because two colliding names slug the
// same and so produce the one ref. What is recorded here is which name that
// ref belongs to, which is the fact a ref has already thrown away.

// slugClaimIndexName is the file, inside stateDir. It held the name
// sessions.json until the session index took it; see migrateSlugClaims for
// what happens to the claims an older release left under that name.
const slugClaimIndexName = "slug-claims.json"

// migrateSlugClaims carries an older release's claims across the rename, when
// slug-claims.json is not there and sessions.json still holds them.
//
// Unlike the session index, which is deliberately not migrated because a
// sandbox name cannot be split back into an agent and a label without
// guessing, there is nothing here to resolve: the old file's shape is exactly
// this file's shape, the same sandbox-name key and the same owning name, so
// the migration is the rename and no more. Worth doing rather than dropping,
// because what a claim buys is the refusal in claimSlug -- and dropping the
// claims turns that refusal off until every session has run again, which is
// the window in which two agents land in one guest home with whichever
// credentials arrived last.
//
// The safety of it is the parse. A current sessions.json is a map of session
// entries, whose values are JSON objects, so it cannot parse as a map of
// strings: readIndex hands back an empty map for it exactly as it does for a
// corrupt file, and an empty map is nothing to migrate. So this cannot fire on
// a file the session index owns and cannot delete a session entry -- when it
// does fire, what it read was claims, and a file of claims holds nothing the
// session index wants.
//
// Idempotent by construction: it runs only when slug-claims.json is absent,
// and writing that file is the first thing it does. It has to stay ahead of
// rememberSession, which is the write that would otherwise replace the file
// the old claims are still in -- see the read in claimSlug, which is where
// that ordering is arranged.
func migrateSlugClaims() {
	path, err := indexPath(slugClaimIndexName)
	if err != nil {
		return
	}
	// Only a stat that says the file is not there: one that fails some other
	// way has not established that, and migrating over a claim index brig
	// merely could not look at would be the one way to lose a claim here.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		return
	}
	claims := readIndex[string](sessionIndexName)
	if len(claims) == 0 {
		return
	}
	if err := writeIndex(slugClaimIndexName, claims); err != nil {
		// Dropped like the rest of the bookkeeping: the claims stay where they
		// are and the next command tries again. Nothing has been deleted yet,
		// which is why the write comes first.
		return
	}
	old, err := indexPath(sessionIndexName)
	if err != nil {
		return
	}
	_ = os.Remove(old)
}

// readSlugClaimIndex is the claim index's name for the shared read, with the
// migration ahead of it.
func readSlugClaimIndex() map[string]string {
	migrateSlugClaims()
	return readIndex[string](slugClaimIndexName)
}

// claimSlug records that this run's session name owns its sandbox, and refuses
// the run when a different name owns it already.
//
// An unnamed run cannot collide: its sandbox is brig-<profile> with no slug,
// and every unnamed run of a profile is meant to be the one session. So it
// claims nothing and is always let through.
//
// A failure to write the claim is a warning, not an error, the way
// rememberSession treats its own: the sandbox this run wants is free, so
// refusing it over a bookkeeping file that could not be rewritten would deny
// the thing that was actually asked for. The only cost is that a later
// colliding name goes uncaught.
func (c *Config) claimSlug() error {
	// Read before the unnamed run is let through, because the read is what
	// carries an older release's claims across the rename and an unnamed run
	// has to do that too: it claims nothing, but the rememberSession later in
	// this same boot would replace the file those claims are still sitting in.
	// This is the first thing EnsureRunning does, which is what puts it ahead
	// of that write on every path. See migrateSlugClaims.
	index := readSlugClaimIndex()
	if c.RawName == "" {
		return nil
	}
	owner, taken := index[c.VMName]
	if taken && owner != c.RawName {
		return fmt.Errorf("session %q becomes %q, the sandbox session %q already "+
			"uses (%s). Sharing it would put both agents in one home directory with "+
			"whichever credentials arrived last, so run one under a different --name",
			c.RawName, c.Slug, owner, c.VMName)
	}
	if owner == c.RawName {
		return nil
	}
	index[c.VMName] = c.RawName
	if err := writeIndex(slugClaimIndexName, index); err != nil {
		c.warnf("could not record %s as the owner of %s (%v). A later run whose name "+
			"lands on the same sandbox will not be refused.", c.RawName, c.VMName, err)
	}
	return nil
}

// ForgetSlugClaim drops a removed sandbox's claim, so the next session name
// that lands on it can take it. Errors are dropped the way ForgetSandbox
// drops its own: a removal that worked must not report a failure because a
// bookkeeping file could not be rewritten.
func ForgetSlugClaim(vmName string) {
	index := readSlugClaimIndex()
	if _, ok := index[vmName]; !ok {
		return
	}
	delete(index, vmName)
	_ = writeIndex(slugClaimIndexName, index)
}
