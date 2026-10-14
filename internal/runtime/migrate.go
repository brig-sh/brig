package runtime

import (
	"fmt"
	"strings"
)

// The seam `brig teleport` is written against: move a running sandbox to
// another host, ask where a move stands, and call one off. hull does the move
// itself (`hull migrate`): it checkpoints and fences the guest, copies the
// bundle and the disk over ssh, restores the copy paused on the destination
// and resumes it there. brig asks for the move, checks first that the
// destination can keep the session's promises, and keeps its own record of
// where the session went.
//
// Optional, like Checkpointer: a runtime that cannot hold a teleportable
// session has nothing to move, and a caller asks for the interface before it
// relies on one.

// The states of hull's migration record a move leaves on the source. hull's
// migrate verbs write them; brig reads them back through MigrationInspector
// rather than parse what hull prints, so a change in hull's wording does not
// change what brig decides.
//
// MigrationMigrating is a move under way, or one whose end nobody saw: the
// source may already be fenced, and the destination may or may not run.
// MigrationMigrated is a move the destination committed: the guest runs
// there, and the source is fenced for good. MigrationAborted is a move hull
// called off and confirmed the destination will not run.
const (
	MigrationMigrating = "migrating"
	MigrationMigrated  = "migrated"
	MigrationAborted   = "aborted"
)

// MigrateSpec is a request to move a running sandbox.
type MigrateSpec struct {
	// RemoteStore is the hull store on the destination, as the brig there
	// drives it. Empty leaves hull's own default there.
	RemoteStore string
	// TxID names the move. brig makes it and records it before it asks, so a
	// move whose end it did not see can still be asked about by name.
	TxID string
	// DryRun asks hull to check the move, the destination's machine against
	// this one included, and to pause nothing.
	DryRun bool
}

// MigrateResult is what a move left behind, read back from the runtime's
// record once the move returned, for the reason CheckpointResult is.
type MigrateResult struct {
	// Migration is the source's record after the move. It is the record only
	// when RecordErr is nil.
	Migration Migration
	// RecordErr is why the record could not be read back after a move hull
	// reported as done. A caller must not take it for a failed move: the
	// guest may already run on the destination.
	RecordErr error
	// Summary is the runtime's own line about the move, for a person.
	Summary string
}

// MigrateStatus is where a move stands: the source's record, and what hull's
// own status verb said about it, for a person.
type MigrateStatus struct {
	Migration Migration
	Said      string
}

// Migrator moves a running sandbox to another host over ssh.
type Migrator interface {
	// Migrate moves the sandbox to the host named as USER@HOST.
	Migrate(name, to string, spec MigrateSpec) (MigrateResult, error)
	// MigrateStatus reports where a move of the sandbox stands.
	MigrateStatus(name string) (MigrateStatus, error)
	// MigrateAbort calls off the move txid names. It returns the record
	// after the abort, and hull's state there says whether the destination
	// can still run the guest: only MigrationAborted says it cannot.
	MigrateAbort(name, txid string) (MigrateStatus, error)
}

var _ Migrator = (*hull)(nil)

// Migrate runs `hull migrate`, then reads hull's record back.
func (h *hull) Migrate(name, to string, spec MigrateSpec) (MigrateResult, error) {
	args, err := migrateArgs(name, to, spec)
	if err != nil {
		return MigrateResult{}, err
	}
	out, err := h.call(args)
	res := MigrateResult{Summary: strings.TrimSpace(out)}
	if err != nil {
		// Read back on a failure too: a move that failed after the fence
		// left a record that says so, and that is what a caller decides on.
		res.Migration, res.RecordErr = h.Migration(name)
		return res, err
	}
	if spec.DryRun {
		return res, nil
	}
	res.Migration, res.RecordErr = h.Migration(name)
	if res.RecordErr == nil && res.Migration.TxID != spec.TxID {
		res.RecordErr = fmt.Errorf("hull recorded move %q for %s after it said it finished move %q",
			res.Migration.TxID, name, spec.TxID)
	}
	return res, nil
}

// MigrateStatus runs `hull migrate-status` for its words, and reads the
// record for the state. A status verb that fails still leaves the record to
// read, and the record is what a caller decides on.
func (h *hull) MigrateStatus(name string) (MigrateStatus, error) {
	out, callErr := h.call([]string{"migrate-status", name})
	st := MigrateStatus{Said: strings.TrimSpace(out)}
	m, err := h.Migration(name)
	if err != nil {
		if callErr != nil {
			return st, fmt.Errorf("%w; and %w", callErr, err)
		}
		return st, err
	}
	st.Migration = m
	if callErr != nil && st.Said == "" {
		st.Said = callErr.Error()
	}
	return st, nil
}

// MigrateAbort runs `hull migrate-abort` for one move, then reads the record.
func (h *hull) MigrateAbort(name, txid string) (MigrateStatus, error) {
	args, err := migrateAbortArgs(name, txid)
	if err != nil {
		return MigrateStatus{}, err
	}
	out, err := h.call(args)
	st := MigrateStatus{Said: strings.TrimSpace(out)}
	m, readErr := h.Migration(name)
	st.Migration = m
	if err != nil {
		return st, err
	}
	return st, readErr
}

// migrateArgs is the one place the move's command line is built. The host
// is checked here as well as where the user typed it: it goes to hull, and
// hull hands it to ssh.
func migrateArgs(name, to string, spec MigrateSpec) ([]string, error) {
	if err := CheckRemote(to); err != nil {
		return nil, err
	}
	if spec.TxID == "" && !spec.DryRun {
		return nil, fmt.Errorf("a move of %s needs a txid, so it can be asked about by name", name)
	}
	args := []string{"migrate", "--to", to}
	if spec.RemoteStore != "" {
		args = append(args, "--remote-store", spec.RemoteStore)
	}
	if spec.TxID != "" {
		args = append(args, "--txid", spec.TxID)
	}
	if spec.DryRun {
		args = append(args, "--dry-run")
	}
	return append(args, name), nil
}

// migrateAbortArgs is the abort's command line. An abort always names the
// move it calls off: an abort that took whatever move was current could call
// off a later one than the user meant.
func migrateAbortArgs(name, txid string) ([]string, error) {
	if txid == "" {
		return nil, fmt.Errorf("an abort of a move of %s needs the move's txid", name)
	}
	return []string{"migrate-abort", "--txid", txid, name}, nil
}

// CheckRemote refuses a destination that is not USER@HOST, or that ssh could
// read as something else. The host goes on ssh's command line, and through
// hull onto another one: a word starting with a dash is an option there,
// and a space or a shell character would let the name carry a second word.
// The user part and the host part are each a plain name, an address, or a
// bracketed IPv6 address.
func CheckRemote(to string) error {
	user, host, ok := strings.Cut(to, "@")
	if !ok || user == "" || host == "" || strings.Contains(host, "@") {
		return fmt.Errorf("the destination %q must be USER@HOST, for example pmoust@build-box", to)
	}
	if strings.HasPrefix(user, "-") || strings.HasPrefix(host, "-") {
		return fmt.Errorf("the destination %q starts with a dash, which ssh would read as an option", to)
	}
	for _, r := range user {
		if !plainNameRune(r) {
			return fmt.Errorf("the destination %q has %q in its user name; brig takes letters, digits, '.', '_' and '-' there", to, r)
		}
	}
	h := host
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	if h == "" {
		return fmt.Errorf("the destination %q names no host", to)
	}
	for _, r := range h {
		if !plainNameRune(r) && r != ':' {
			return fmt.Errorf("the destination %q has %q in its host name; brig takes letters, digits, '.', '-', ':' and '_' there", to, r)
		}
	}
	return nil
}

func plainNameRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
		r == '.' || r == '_' || r == '-'
}
