package runtime

import (
	"fmt"
	"regexp"
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

// MigrationMigrating and MigrationMigrated are the states hull's instance
// record (`hull inspect`, the migration object) takes for a move on the
// source: a fenced checkpoint a move has taken over, and a source whose move
// committed. Neither guest may run here.
const (
	MigrationMigrating = "migrating"
	MigrationMigrated  = "migrated"
)

// The states of hull's move record on the source, which `hull migrate
// --status` reports. A move passes preflight, reserved, fenced, transferring,
// transferred, committing and committed, or ends aborted. A streamed move
// skips fenced: it goes from reserved to transferring, the guest runs here
// through the pre-copy rounds, and hvi fences it only when it seals the
// stream, partway through transferring. Up to reserved the guest still runs
// here and nothing was fenced; once fenced it never runs here again unless
// the move is aborted; committed means it runs on the destination. brig
// decides on these words, read back from hull, never on what a call it made
// returned.
const (
	MovePreflight = "preflight"
	MoveReserved  = "reserved"
	MoveCommitted = "committed"
	MoveAborted   = "aborted"
)

// MoveFenced reports whether a move in state may have fenced the source:
// whether brig must take the guest as stopped here for good unless the move
// is aborted. It counts transferring as fenced although a streamed move's
// guest runs here until hvi's seal, which hull's state does not show. That
// is on purpose: a move brig takes as fenced is never undone or resumed by
// brig, so a guest hvi has sealed is never taken as running here.
func MoveFenced(state string) bool {
	switch state {
	case "", MovePreflight, MoveReserved, MoveAborted:
		return false
	}
	return true
}

// MigrateSpec is a request to move a running sandbox.
type MigrateSpec struct {
	// RemoteStore is the hull store on the destination, as the brig there
	// drives it. Empty leaves hull's own default there.
	RemoteStore string
	// RemoteHull is the hull the brig on the destination drives, so the
	// move lands where that brig looks. Empty is the hull on its PATH.
	RemoteHull string
	// GatewaySock is the control socket of the network gateway brig
	// started on the destination for the moved guest, with the network and
	// the rules it had here. hull is handed its stream socket. Empty for a
	// guest with no network.
	GatewaySock string
	// TxID names the move: 32 lower-case hex digits. brig makes it and
	// records it before it asks, so a move whose end it did not see can
	// still be asked about by name.
	TxID string
	// DryRun asks hull to check both hosts, the destination's machine
	// against this one included, and to change nothing on either.
	DryRun bool
	// Progress, when set, is handed each line hull prints on stderr during
	// the move, as hull prints it. A dry run hands it nothing. It runs on
	// the goroutine that reads hull's stderr and must not block, or hull
	// waits on it, in the pause too: one that prints hands the line to an
	// Echo.
	Progress func(line string)
}

// MoveStatus is hull's move record of a sandbox on this host, as `hull
// migrate --status` reports it. State is "" when hull has no move on record.
type MoveStatus struct {
	// Read reports whether hull's record was read at all. The zero status
	// is a record that was not: a caller decides nothing on it, and only a
	// read that found no record says hull has none.
	Read  bool
	TxID  string
	State string
	Peer  string
	// Said is hull's own account, for a person.
	Said string
}

// Migrator moves a running sandbox to another host over ssh.
type Migrator interface {
	// Migrate moves the sandbox to the host named as USER@HOST, then reads
	// hull's move record back. A dry run reads nothing back.
	Migrate(name, to string, spec MigrateSpec) (MoveStatus, error)
	// MigrateStatus reads hull's move record of the sandbox.
	MigrateStatus(name string) (MoveStatus, error)
	// MigrateAbort calls off the move txid names, then reads the record.
	// hull aborts only a move that has not committed, and only with the
	// destination's word that it is not running the guest; it then runs the
	// guest here again itself. Only MoveAborted in the record says it did.
	MigrateAbort(name, txid string) (MoveStatus, error)
}

var _ Migrator = (*hull)(nil)

// moveTransports are the settings with which hull would reach the other host
// of a move over something other than ssh to the host it was given:
// HULL_MIGRATE_SSH names a program hull runs in ssh's place, for hull's own
// tests. A move brig asks for, and hull's status and abort of it, reach the
// other host only through ssh to the host the user named, so no call of
// `hull migrate` is handed one from brig's environment, wherever it was set.
var moveTransports = []string{"HULL_MIGRATE_SSH"}

// Migrate runs `hull migrate`, then reads the move record back, after a move
// that failed too: one that failed after the fence leaves a source that
// cannot run, and that is what a caller decides on. A record that did not
// read comes back unread, with its error beside the move's, so a caller
// never takes a failed read for a record that says the move did not begin.
func (h *hull) Migrate(name, to string, spec MigrateSpec) (MoveStatus, error) {
	args, err := migrateArgs(name, to, spec)
	if err != nil {
		return MoveStatus{}, err
	}
	if spec.DryRun {
		_, err := h.call(args, moveTransports...)
		return MoveStatus{}, err
	}
	_, callErr := h.callLines(args, spec.Progress, moveTransports...)
	st, readErr := h.MigrateStatus(name)
	return st, withReadErr(callErr, readErr)
}

// withReadErr is a call's error with the error of the record read after it,
// on one line. Either may be nil.
func withReadErr(callErr, readErr error) error {
	switch {
	case readErr == nil:
		return callErr
	case callErr == nil:
		return fmt.Errorf("hull's move record did not read: %w", readErr)
	}
	return fmt.Errorf("%w; and hull's move record did not read: %v", callErr, readErr)
}

// MigrateStatus runs `hull migrate --status` and reads its first line, which
// names the move, its state and the other host. A sandbox with no move on
// record is a read status with no move in it, not an error. hull asks the
// destination about the move too, over the ssh a move takes, so this call is
// not handed another transport either.
func (h *hull) MigrateStatus(name string) (MoveStatus, error) {
	out, err := h.call([]string{"migrate", "--status", name}, moveTransports...)
	if err != nil {
		if strings.Contains(err.Error(), "has no move on record") {
			return MoveStatus{Read: true}, nil
		}
		return MoveStatus{}, err
	}
	return moveStatusOf(name, out)
}

// MigrateAbort runs `hull migrate --abort TXID`. The txid, not the name:
// an abort that took whatever move was current could call off a later one
// than the caller meant.
func (h *hull) MigrateAbort(name, txid string) (MoveStatus, error) {
	if err := checkMoveTxID(txid); err != nil {
		return MoveStatus{}, err
	}
	out, err := h.call([]string{"migrate", "--abort", txid}, moveTransports...)
	st, readErr := h.MigrateStatus(name)
	if st.Said == "" {
		st.Said = strings.TrimSpace(out)
	}
	return st, withReadErr(err, readErr)
}

// moveStatusLine is the first line `hull migrate --status` prints.
var moveStatusLine = regexp.MustCompile(`^Move ([0-9a-f]{32}) of (\S+): ([a-z-]+), (?:to|from) (\S+), epoch \d+$`)

// moveStatusOf reads hull's status report of one sandbox's move.
func moveStatusOf(name, out string) (MoveStatus, error) {
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	m := moveStatusLine.FindStringSubmatch(strings.TrimSpace(first))
	if m == nil {
		return MoveStatus{}, fmt.Errorf("hull's move status of %s did not read: %q", name, first)
	}
	if m[2] != name {
		return MoveStatus{}, fmt.Errorf("hull answered the move status of %s for %s", name, m[2])
	}
	return MoveStatus{Read: true, TxID: m[1], State: m[3], Peer: m[4], Said: strings.TrimSpace(out)}, nil
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
	if spec.TxID != "" {
		if err := checkMoveTxID(spec.TxID); err != nil {
			return nil, err
		}
	}
	// hull hands --remote-gateway-sock to hvi restore on the destination as
	// its --net-gateway, the socket hvi carries the guest's frames on. That
	// is the gateway's stream socket, not the control socket brig names a
	// gateway by: on the control socket the gateway waits for a member's
	// file descriptor, hvi sends frames, and the moved guest reaches
	// nothing. hull's own run takes the control socket and derives the
	// stream socket the same way.
	var gateway string
	if spec.GatewaySock != "" {
		gateway = qemuGatewaySocket(spec.GatewaySock)
	}
	args := []string{"migrate", "--to", to}
	for _, f := range []struct{ flag, value string }{
		{"--remote-store", spec.RemoteStore},
		{"--remote-hull", spec.RemoteHull},
		{"--remote-gateway-sock", gateway},
		{"--txid", spec.TxID},
	} {
		if f.value != "" {
			args = append(args, f.flag, f.value)
		}
	}
	if spec.DryRun {
		args = append(args, "--dry-run")
	}
	return append(args, name), nil
}

// moveTxID is hull's shape of a move's txid.
var moveTxID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func checkMoveTxID(txid string) error {
	if !moveTxID.MatchString(txid) {
		return fmt.Errorf("%q is not a move's txid (32 lower-case hex digits)", txid)
	}
	return nil
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
