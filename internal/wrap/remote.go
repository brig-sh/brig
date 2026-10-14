package wrap

import (
	"fmt"
	"os"
	"sort"

	"github.com/brig-sh/brig/internal/profile"
)

// Where a session runs, as this host's index records it. A teleported
// session runs on the host it was moved to, and every verb that acts on it
// goes there over ssh; see cmd/brig/remote.go. The index is the only record
// of that on this host: the runtime here lists the session's old copy, fenced,
// or not at all.

// Location is where the index says a session is.
type Location struct {
	// Ref is the session, as the index files it, and Sandbox its sandbox
	// name, which is the same on both hosts.
	Ref     string
	Sandbox string
	// Host is the host the session runs on once a move committed, as
	// USER@HOST, or "" for a session that runs here.
	Host string
	// PendingHost is the destination of a move that has begun and whose end
	// brig has not seen. While it is set the session is in neither place for
	// certain, and only `brig teleport --status` or `--abort` act on it.
	PendingHost string
	// RemoteStore is the hull store the session runs in on Host, and TxID
	// the move that took it there.
	RemoteStore string
	TxID        string
}

// Away reports whether the session is not, or not certainly, on this host.
func (l Location) Away() bool { return l.Host != "" || l.PendingHost != "" }

func (e sessionEntry) location(ref string) Location {
	return Location{Ref: ref, Sandbox: e.Sandbox, Host: e.Host, PendingHost: e.PendingHost,
		RemoteStore: e.RemoteStore, TxID: e.TxID}
}

// SessionLocation reads where the session of profile t and the label name
// is, without loading it. A verb on a moved session must not load it here:
// Load asks this host's runtime about a sandbox that is not here, and the
// credential resolution after it would open this host's store for a session
// that runs on another. ok is false when the index has no entry for the
// session's sandbox.
func SessionLocation(t profile.Profile, name string) (Location, bool, error) {
	env := NewEnv(t.Name, os.LookupEnv)
	slug, _, vmName, err := sessionNames(t, env, name)
	if err != nil {
		return Location{}, false, err
	}
	key := sessionKey(t.Name, slug)
	entry, ok := readSessionIndex()[key]
	if !ok || entry.Sandbox != vmName {
		return Location{}, false, nil
	}
	return entry.location(key), true, nil
}

// Location is where the index says this session is.
func (c *Config) Location() Location {
	key := sessionKey(c.Profile.Name, c.Slug)
	entry, ok := readSessionIndex()[key]
	if !ok || entry.Sandbox != c.VMName {
		return Location{Ref: key, Sandbox: c.VMName}
	}
	return entry.location(key)
}

// RemoteSessions is every session the index says runs on another host or is
// being moved to one, sorted by sandbox. `brig ls` lists them beside what
// this host's runtime has.
func RemoteSessions() []Location {
	var out []Location
	for ref, entry := range readSessionIndex() {
		if entry.away() {
			out = append(out, entry.location(ref))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sandbox < out[j].Sandbox })
	return out
}

// updateEntry rewrites one session's entry under the index's usual rules,
// refusing when the entry is not the session's: a sandbox renamed since, or
// no entry at all. A move is only ever recorded against the entry it began
// from.
func updateEntry(ref, sandbox string, edit func(*sessionEntry) error) error {
	index := readSessionIndex()
	entry, ok := index[ref]
	if !ok || entry.Sandbox != sandbox {
		return fmt.Errorf("brig has no record of %s on sandbox %s", ref, sandbox)
	}
	if err := edit(&entry); err != nil {
		return err
	}
	index[ref] = entry
	return writeSessionIndex(index)
}

// RecordMoveStarted records that a move of the session to host began, under
// txid. Written before hull is asked, so a move whose end nobody saw is one
// brig can still name. A session already moving or moved is refused.
func RecordMoveStarted(ref, sandbox, host, txid string) error {
	return updateEntry(ref, sandbox, func(e *sessionEntry) error {
		switch {
		case e.Host != "":
			return fmt.Errorf("%s already runs on %s", ref, e.Host)
		case e.PendingHost != "" && e.TxID != txid:
			return fmt.Errorf("a move of %s to %s (%s) has not finished", ref, e.PendingHost, e.TxID)
		}
		e.PendingHost, e.TxID = host, txid
		return nil
	})
}

// RecordMoved records that the move txid committed: the session runs on
// host, in store there. From here on every verb on it goes to host.
func RecordMoved(ref, sandbox, host, store, txid string) error {
	return updateEntry(ref, sandbox, func(e *sessionEntry) error {
		if e.TxID != txid {
			return fmt.Errorf("the record of %s names move %s, not %s", ref, e.TxID, txid)
		}
		e.Host, e.RemoteStore, e.PendingHost = host, store, ""
		return nil
	})
}

// ClearMove drops the record of the move txid, for a move hull confirmed
// never left this host: the session is here, as it was.
func ClearMove(ref, sandbox, txid string) error {
	return updateEntry(ref, sandbox, func(e *sessionEntry) error {
		if e.TxID != txid || e.Host != "" {
			return fmt.Errorf("the record of %s does not name an unfinished move %s", ref, txid)
		}
		e.PendingHost, e.TxID = "", ""
		return nil
	})
}

// ForgetRemoteSession drops the entry of a session that runs on another
// host, once nothing of it is left here. Only the record goes: the session
// on the other host is untouched, and its own brig still has it.
func ForgetRemoteSession(ref, sandbox string) error {
	index := readSessionIndex()
	entry, ok := index[ref]
	if !ok || entry.Sandbox != sandbox || entry.Host == "" {
		return fmt.Errorf("brig has no record of %s running on another host", ref)
	}
	delete(index, ref)
	return writeSessionIndex(index)
}
