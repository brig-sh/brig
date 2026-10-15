package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/wrap"
)

// A session that runs on another host moves from there: hull fences the
// guest where it runs, and the brig there holds the move's record. So
// `brig teleport --to`, `--prepare` and `--abort` on such a session go to the
// brig on that host, over ssh, the way sh, run, stop, info and logs do (see
// remote.go), and its lines come back as it prints them. A move it finished
// is then recorded here too: the session runs on the host it went to. When
// that host is this one, the arrival has already filed the session here as
// running, and the copy left on the other host is what `brig rm` there
// removes. --status stays here: it reads this host's record.
//
// The way back needs one thing done here first. The move away left this
// host a copy of the guest, fenced, under the sandbox name the move back
// arrives as, and the probe, the arrival and hull all refuse a destination
// that has it. A move or a prepare whose destination is this host removes
// that copy before it asks the brig there, once it is proven stale as
// `brig rm` proves it: hull's record here says the move committed away, or
// the brig there says the session runs there. The record of where the
// session runs stays until the move back replaces it.

// forwardRemote runs the brig command on the session's host with brig's own
// stdin, stdout and stderr, and returns its exit status. A variable so a
// test can see the line without a process.
var forwardRemote = runtime.RunAttached

// teleportForward hands --to, --prepare or --abort for a session that runs
// on loc.Host to the brig there.
func teleportForward(cfg *wrap.Config, t profile.Profile, ref string, o teleportOptions, loc wrap.Location,
	wantJSON bool) error {
	host := loc.Host
	what, args := "the abort", []string{"teleport", ref, "--abort"}
	switch {
	case o.prepare:
		what, args = "the prepare", []string{"teleport", ref, "--to", o.to, "--prepare"}
	case o.to != "":
		what, args = "the move", []string{"teleport", ref, "--to", o.to}
	}
	if o.to != "" && sameAccount(o.to, host) {
		return fmt.Errorf("%s runs on %s already", ref, host)
	}
	removed := false
	if o.to != "" && isThisHost(o.to) {
		var err error
		if removed, err = clearLeftCopy(cfg, t, ref, loc); err != nil {
			return err
		}
	}
	words := globalWords(globals{quiet: verbosity <= wrap.Quiet, verbose: verbosity >= wrap.Verbose, json: wantJSON})
	argv, err := sshArgv(remoteCall{host: host, store: loc.RemoteStore, args: append(words, args...)})
	if err != nil {
		return err
	}
	warnf("%s runs on %s, and brig hands %s to the brig there, over ssh", ref, host, what)
	code, err := forwardRemote(argv, os.Environ())
	switch {
	case err != nil:
		return fmt.Errorf("cannot run ssh to reach %s: %w%s", host, err, copyRemovedNote(removed))
	case code == sshFailed:
		// ssh failed to connect, or the connection dropped while the brig
		// there ran: the move may have begun, or ended, there.
		return fmt.Errorf("ssh to %s ended without the brig there's answer, so whether %s ran there is not "+
			"known here. `ssh %s brig teleport %s --status` says where it stands. %s%s", host, what, host, ref,
			recordNow(cfg, ref), copyRemovedNote(removed))
	case code != 0:
		// The brig there said why on stderr, and its code is the answer.
		// A move back here that failed after the arrival began leaves it
		// recorded here, until the brig there settles the move.
		if _, ok := wrap.UnfinishedArrival(ref, cfg.VMName, cfg.Runtime); ok {
			warnf("%s%s", recordNow(cfg, ref), copyRemovedNote(removed))
		}
		return &agentExit{code: code}
	}
	if o.to == "" || o.prepare {
		// Neither moves the session: it runs on host still.
		return nil
	}
	return recordForwardedMove(cfg, ref, loc)
}

// recordNow says what this host's record of the session is after a
// forwarded line ended without an answer, read again: a move back here
// files its arrival here before the guest moves, and that brig, not this
// one, settles it.
func recordNow(cfg *wrap.Config, ref string) string {
	if a, ok := wrap.UnfinishedArrival(ref, cfg.VMName, cfg.Runtime); ok {
		return fmt.Sprintf("This host records %s as arriving from %s (move %s), and the move has not "+
			"finished; the brig on %s settles it", ref, a.From, a.TxID, a.From)
	}
	if now := cfg.Location(); now.Host != "" {
		return fmt.Sprintf("This host's record still says %s runs on %s", ref, now.Host)
	} else if now.PendingHost != "" {
		return fmt.Sprintf("This host's record says a move of %s to %s has not finished", ref, now.PendingHost)
	}
	return fmt.Sprintf("This host's record files %s as running here", ref)
}

// copyRemovedNote is the sentence a failed line adds when the brig here
// removed the fenced copy before it handed the line on.
func copyRemovedNote(removed bool) string {
	if !removed {
		return ""
	}
	return ". The fenced copy the move away left here was removed before the line was handed on, " +
		"as it could never run again"
}

// recordForwardedMove brings this host's record up to date after the brig
// on loc.Host said it moved the session: it asks that brig's record where
// the session runs now, which is what that brig decided on.
func recordForwardedMove(cfg *wrap.Config, ref string, loc wrap.Location) error {
	host, vm := loc.Host, cfg.VMName
	out, err := remoteOutput(host, loc.RemoteStore, "teleport", ref, "--status", "--json")
	var doc struct {
		Kind string       `json:"kind"`
		Data teleportData `json:"data"`
	}
	if err == nil {
		if jerr := json.Unmarshal(out, &doc); jerr != nil || doc.Kind != "Teleport" {
			err = fmt.Errorf("its answer did not read")
		}
	}
	if err != nil {
		warnf("could not ask the brig on %s where %s runs now: %v. This host's record still says %s, and "+
			"every verb on %s goes there and on from there", host, ref, err, host, ref)
		return nil
	}
	d := doc.Data
	if d.State != "moved" || d.Host == "" {
		warnf("the brig on %s says %s is %s there, so this host's record is left as it was", host, ref, orNone(d.State))
		return nil
	}
	now := cfg.Location()
	switch {
	case now.Away() && isThisHost(d.Host):
		// The brig there filed the arrival in a state this brig does not
		// read, as under another BRIG_STATE_DIR. A record naming this host
		// would send every verb here to itself over ssh.
		warnf("the brig on %s says %s moved to this host, and no arrival of it is filed here: that brig "+
			"reached this host with other settings, such as another BRIG_STATE_DIR (see "+
			"BRIG_TELEPORT_REMOTE_BRIG). This host's record is left as it was, naming %s", host, ref, host)
		return nil
	case !now.Away():
		// The move came here, and the arrival filed the session as one of
		// this host's own.
		warnf("%s runs on this host again; `ssh %s brig rm %s` removes the copy left there", ref, host, ref)
		return nil
	case now.Host != host || now.TxID != loc.TxID:
		warnf("this host's record of %s changed while the brig on %s moved it, so it is left as it is", ref, host)
		return nil
	}
	if err := wrap.RecordMovedOn(ref, vm, host, loc.TxID, d.Host, d.RemoteStore, d.TxID); err != nil {
		return fmt.Errorf("%s runs on %s now, and brig could not record that here: %w. Its verbs here go to %s, "+
			"whose brig hands them on", ref, d.Host, err, host)
	}
	warnf("%s runs on %s now; this host's record says so", ref, d.Host)
	return nil
}

// clearLeftCopy removes the copy of the session a move away left on this
// host, for a move or a prepare back here, once it is proven stale: hull's
// record here says the move loc names committed away, or the brig on the
// session's host says it runs there. The record of where the session runs
// is kept: the move back replaces it, and an abort puts it back.
func clearLeftCopy(cfg *wrap.Config, t profile.Profile, ref string, loc wrap.Location) (bool, error) {
	vm := cfg.VMName
	if cfg.Runtime == nil {
		return false, nil
	}
	present, err := sandboxPresent(cfg.Runtime, vm)
	if err != nil || !present {
		return false, err
	}
	if !staleByHullsRecord(cfg.Runtime, vm, loc.TxID) {
		if err := confirmRunsThere(loc, t.Name, vm); err != nil {
			return false, fmt.Errorf("this host has the copy of %s that the move to %s left, which a move back "+
				"has to replace, and it is kept: %w", ref, loc.Host, err)
		}
	}
	if err := cfg.Remove(); err != nil {
		return false, fmt.Errorf("could not remove the copy of %s that the move to %s left here, which a move "+
			"back has to replace: %w", ref, loc.Host, err)
	}
	warnf("removed the fenced copy of %s that the move to %s left here", ref, loc.Host)
	return true, nil
}

// teleportArriving is `brig teleport` of a session whose move here has not
// finished. The source drives that move and holds its record, and its
// --status settles it, so --status here says where the move comes from, and
// --to, --prepare and --abort are refused: this host's record of the session
// is the arrival's, and neither a move out nor an abort here acts on it.
func teleportArriving(ref, vm string, o teleportOptions, a wrap.Arrival, wantJSON bool) error {
	settle := fmt.Sprintf("`ssh %s brig teleport %s --status`", a.From, ref)
	if o.status {
		d := teleportData{Ref: ref, Sandbox: vm, State: "arriving", From: a.From, TxID: a.TxID}
		return printTeleport(d, wantJSON, fmt.Sprintf("%s is arriving on this host from %s (move %s), and the "+
			"move has not finished; it is driven from there, and %s settles it", ref, a.From, a.TxID, settle))
	}
	return fmt.Errorf("%s is arriving on this host from %s (move %s), and the move has not finished, so it "+
		"is not moved or called off from here. %s settles it, and `ssh %s brig teleport %s --abort` calls it off",
		ref, a.From, a.TxID, settle, a.From, ref)
}

// isThisHost reports whether USER@HOST names this host and this account, as
// a source names itself to a destination (thisHostName): the user as $USER
// has it, and the host as wrap.SameHost reads two spellings of one.
func isThisHost(to string) bool {
	here := thisHostName()
	return strings.Contains(here, "@") && sameAccount(to, here)
}

// sameAccount reports whether two USER@HOST spellings name one account on
// one host: the same user, on a host wrap.SameHost reads as one.
func sameAccount(a, b string) bool {
	ua, _, _ := strings.Cut(a, "@")
	ub, _, _ := strings.Cut(b, "@")
	return ua == ub && wrap.SameHost(a, b)
}

// thisHostName is how this host names itself to another: USER@HOST, from
// $USER and the hostname, or the hostname alone, or "".
func thisHostName() string {
	name, _ := os.Hostname()
	if u := os.Getenv("USER"); u != "" && name != "" {
		name = u + "@" + name
	}
	return name
}

// thisHostSaid is thisHostName for a person: "this host" when there is no
// name to give.
func thisHostSaid() string {
	if n := thisHostName(); n != "" {
		return n
	}
	return "this host"
}
