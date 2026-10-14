package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/brig-sh/brig/internal/buildinfo"
	"github.com/brig-sh/brig/internal/notice"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/wrap"
)

// brig teleport moves a teleportable session's running sandbox to another
// host. hull does the move: `hull migrate` fences the guest, ships its
// memory and disk over ssh, restores it paused on the destination and
// resumes it there. brig's part is everything a move must not get wrong
// about the session:
//
//   - It refuses, with the reason and before anything is paused, a session
//     that is not teleportable, a destination whose brig, hull or hvi is
//     missing, one that cannot enforce the session's egress policy, and one
//     that already has the session's name or address.
//   - It gives the guest the network it left on the destination, rules
//     included, before hull resumes it there (teleport-receive).
//   - It records where the session went, and from then on hands every verb
//     on it to the brig there over ssh.
//   - It never resumes the source once a move began, unless hull's abort
//     says the destination cannot run the guest. A move whose end it did not
//     see is left recorded as unfinished, for --status to settle from hull's
//     own record.
//
// The two hidden verbs are the destination's half, run there over ssh by the
// source's brig.
const (
	probeVerb   = "teleport-probe"
	receiveVerb = "teleport-receive"
	// probeFormat is the shape of the probe's answer. A source that reads a
	// shape it does not know refuses rather than guesses. 2 added the policy
	// the destination binds to the session.
	probeFormat = 2
)

// teleportOptions is the teleport line: a move, or one of the two questions
// about one.
type teleportOptions struct {
	to     string
	status bool
	abort  bool
}

// teleportData is a Teleport document: where the session is after the verb.
// State is brig's word for it: here, moving, moved or aborted. Hull is the
// state of hull's own record, which is what brig decided on.
type teleportData struct {
	Ref         string `json:"ref"`
	Sandbox     string `json:"sandbox"`
	State       string `json:"state"`
	Host        string `json:"host,omitempty"`
	PendingHost string `json:"pendingHost,omitempty"`
	RemoteStore string `json:"remoteStore,omitempty"`
	TxID        string `json:"txid,omitempty"`
	Hull        string `json:"hull,omitempty"`
}

// teleportCmd is `brig teleport`, after the session is loaded and before any
// credential is resolved.
func teleportCmd(cfg *wrap.Config, t profile.Profile, ref string, o teleportOptions, wantJSON bool) error {
	asked := 0
	for _, on := range []bool{o.to != "", o.status, o.abort} {
		if on {
			asked++
		}
	}
	if asked != 1 {
		return usagef("`brig teleport` takes one of --to USER@HOST, --status and --abort")
	}
	if o.to != "" {
		if err := runtime.CheckRemote(o.to); err != nil {
			return usagef("%s", err)
		}
	}
	// The ref the index files the session under, the resolved profile and
	// the slug, which is also how the destination's brig files it.
	ref = cfg.Location().Ref
	// One teleport of a session at a time on this host, the move and the
	// two questions alike. Taken before the record is read, so what each
	// one decides on is not changing under it.
	unlock, err := wrap.LockMove(cfg.VMName)
	if errors.Is(err, wrap.ErrMoveBusy) {
		return fmt.Errorf("a `brig teleport` of %s is running on this host. Let it finish, then run this again", ref)
	}
	if err != nil {
		return fmt.Errorf("cannot take the move lock of %s: %w", ref, err)
	}
	defer unlock()
	switch {
	case o.status:
		return teleportStatus(cfg, ref, wantJSON)
	case o.abort:
		return teleportAbort(cfg, ref, wantJSON)
	}
	return teleportMove(cfg, t, ref, o.to, wantJSON)
}

// moveRefusal is a refusal of a move as a capability: exit 7.
func moveRefusal(why, remedy string) error {
	return &runtime.CapabilityError{Property: runtime.Teleport, State: runtime.CannotEnforce, Why: why, Remedy: remedy}
}

// newTxID names a move: 128 random bits in lower-case hex, the shape hull
// takes, so two moves never share a name in either host's records.
func newTxID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// sourceChecks is what this host must hold true before a move is asked:
// the session is teleportable and runs on hull's hvi, booted owning its
// disk, the sandbox runs and is not parked by an earlier checkpoint, and its
// name is the one the destination will derive.
func sourceChecks(cfg *wrap.Config, t profile.Profile, ref string) (runtime.Migrator, error) {
	vm := cfg.VMName
	// A ref with no sandbox is a not-found, 3, whatever kind of session it
	// would be: there is nothing to move, and calling it a capability would
	// send the reader to the wrong fix.
	if cfg.Runtime != nil {
		present, err := sandboxPresent(cfg.Runtime, vm)
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, noSandboxf(ref)
		}
	}
	if !cfg.Teleportable {
		return nil, moveRefusal(fmt.Sprintf("%s is not a teleportable session: its guest home is a host "+
			"directory shared in, and a move carries only what is inside the sandbox", ref),
			fmt.Sprintf("Start one that is with `brig run --teleportable %s`", t.Name))
	}
	rt := cfg.Runtime
	if rt == nil || rt.Kind() != "hull" {
		kind := "no runtime"
		if rt != nil {
			kind = rt.Kind()
		}
		return nil, moveRefusal(fmt.Sprintf("%s runs on %s, and only hull's hvi backend can move a sandbox", ref, kind),
			"Run teleportable sessions under hull (BRIG_RUNTIME=hull)")
	}
	inspector, ok := rt.(runtime.StorageInspector)
	if !ok {
		return nil, moveRefusal("this runtime cannot say how the sandbox was booted", "")
	}
	owns, err := inspector.OwnsItsDisk(vm)
	if err != nil {
		return nil, fmt.Errorf("cannot read how %s was booted, so it is not moved: %w", vm, err)
	}
	if !owns {
		return nil, moveRefusal(fmt.Sprintf("hull's record of %s does not show an hvi guest on a block root "+
			"with no host directory shared in", vm), "")
	}
	mover, ok := rt.(runtime.Migrator)
	if !ok {
		return nil, moveRefusal("this runtime has no way to move a sandbox", "")
	}
	running, err := rt.Running(vm)
	if err != nil {
		return nil, fmt.Errorf("cannot tell whether %s is running: %w", vm, err)
	}
	if !running {
		return nil, fmt.Errorf("%s is not running. A move carries a running guest, and a stopped "+
			"teleportable session cannot be started again; see `brig teleport --help`", vm)
	}
	if mi, ok := rt.(runtime.MigrationInspector); ok {
		m, err := mi.Migration(vm)
		if err != nil {
			return nil, fmt.Errorf("cannot read hull's migration record of %s: %w", vm, err)
		}
		switch m.State {
		case runtime.MigrationFenced, runtime.MigrationRestoring, runtime.MigrationMigrating, runtime.MigrationMigrated:
			return nil, fmt.Errorf("hull's record of %s says %s (%s), so its guest is not one a move "+
				"may pause. `hull migrate-status %s` says more", vm, m.State, m.TxID, vm)
		}
	}
	want := wrap.NamePrefix + t.Name
	if cfg.Slug != "" {
		want += "-" + cfg.Slug
	}
	if vm != want {
		return nil, fmt.Errorf("BRIG_NAME names this sandbox %s, and the brig on the destination derives "+
			"%s for %s, so it could not reach the session afterwards. Unset BRIG_NAME", vm, want, ref)
	}
	return mover, nil
}

// teleportMove is the move: checks here, the probe there, the record, hull's
// dry run, the destination's network, and then hull's move.
func teleportMove(cfg *wrap.Config, t profile.Profile, ref, host string, wantJSON bool) error {
	vm := cfg.VMName
	loc := cfg.Location()
	switch {
	case loc.Host != "":
		return fmt.Errorf("%s already runs on %s. A second move goes from there", ref, loc.Host)
	case loc.PendingHost != "":
		return fmt.Errorf("a move of %s to %s (%s) has not finished. `brig teleport %s --status` "+
			"finds out where it stands", ref, loc.PendingHost, loc.TxID, ref)
	}
	mover, err := sourceChecks(cfg, t, ref)
	if err != nil {
		return err
	}
	net, err := cfg.MoveNetwork()
	if err != nil {
		return err
	}
	txid, err := newTxID()
	if err != nil {
		return err
	}
	report, err := probeRemote(host, ref, t.Name, vm, net)
	if err != nil {
		return err
	}
	if err := checkProbe(cfg, report, host, ref, txid, net); err != nil {
		return err
	}
	if len(cfg.Publish) > 0 {
		warnf("%s", notice.Newf("%s publishes ports on this host, and they stay here", ref).
			Note("publish them on %s once it runs there: ssh %s brig network publish %s PORT", host, host, ref))
	}
	spec := runtime.MigrateSpec{RemoteStore: report.Store.Named, RemoteHull: report.Runtime.Bin, TxID: txid}

	// From here a record names the move, so whatever happens next can be
	// asked about by name. The destination takes the guest's network first,
	// so hull's dry run can check the gateway answers; the dry run is the
	// machine check, this host's against the destination's, and pauses
	// nothing. Either failing gives the network back.
	if err := wrap.RecordMoveStarted(ref, vm, host, spec.RemoteStore, txid); err != nil {
		return err
	}
	sock, err := receiveRemote(host, spec.RemoteStore, ref, vm, txid, net)
	if err != nil {
		// The destination may have taken the network and only the answer
		// was lost. Giving it back is harmless when it took nothing.
		abortReceive(host, spec.RemoteStore, ref, vm, txid)
		_ = wrap.ClearMove(ref, vm, txid)
		return fmt.Errorf("the brig on %s could not take %s's network, and nothing was paused: %w", host, ref, err)
	}
	spec.GatewaySock = sock
	dry := spec
	dry.TxID, dry.DryRun = "", true
	if _, err := mover.Migrate(vm, host, dry); err != nil {
		abortReceive(host, spec.RemoteStore, ref, vm, txid)
		_ = wrap.ClearMove(ref, vm, txid)
		return fmt.Errorf("hull would not move %s to %s, and paused nothing: %w", vm, host, err)
	}

	warnf("moving %s to %s (move %s): the guest pauses here and resumes there", ref, host, txid)
	st, moveErr := mover.Migrate(vm, host, spec)
	// hull's record, read back, says the move never fenced the guest, or
	// names no part of this move. A record that did not read says neither.
	undo := moveErr != nil && st.Read && (st.TxID != txid || !runtime.MoveFenced(st.State)) &&
		st.State != runtime.MoveCommitted
	if undo && st.TxID == "" {
		undo, _ = noMoveBegan(cfg.Runtime, vm)
	}
	switch {
	case st.TxID == txid && st.State == runtime.MoveCommitted:
		if moveErr != nil {
			warnf("%s", notice.Newf("hull reported a failure after the move committed: %v", moveErr).
				Note("its record says the guest runs on %s, and that is what brig records", host))
		}
		return finishMove(ref, vm, host, spec.RemoteStore, txid, st.State, wantJSON)
	case undo:
		// hull's record says the move never fenced the guest, or names no
		// part of this move: the guest still runs here, and brig undoes
		// its own half. A record hull aborted is the same: hull has run the
		// guest here again. A record hull left at preflight or reserved is
		// ended too, which is safe with nothing fenced, or the next move of
		// the session would find it in hull's way. No record at all counts
		// only while the sandbox is here to hold one, and a record that did
		// not read says none of this: both are the case below.
		if st.TxID == txid && st.State != runtime.MoveAborted {
			if _, err := mover.MigrateAbort(vm, txid); err != nil {
				warnf("%s", notice.Newf("could not end hull's record of move %s: %v", txid, err).
					Note("`hull migrate --abort %s` ends it; nothing was fenced", txid))
			}
		}
		abortReceive(host, spec.RemoteStore, ref, vm, txid)
		_ = wrap.ClearMove(ref, vm, txid)
		if st.TxID == txid && st.State == runtime.MoveAborted {
			// hull records the abort before it restores the guest here, so
			// the record alone does not say the restore worked.
			return fmt.Errorf("hull called off the move of %s to %s: %w. hull restores the guest here "+
				"itself; %s", vm, host, moveErr, restoreRemedy(vm))
		}
		return fmt.Errorf("hull did not move %s to %s, and its record says the guest was not paused for "+
			"good: %w. `hull migrate --status %s` says more", vm, host, moveErr, vm)
	}
	// Anything else is a move whose end brig did not see: it may have
	// committed, or not, whatever the call returned. Nothing is resumed and
	// the record stays unfinished, for --status to settle from hull's record.
	why := "hull's record says " + orNone(st.State)
	switch {
	case moveErr != nil:
		why = moveErr.Error()
	case !st.Read:
		why = "hull's record of it did not read"
	}
	return fmt.Errorf("the move of %s to %s (%s) did not report how it ended: %s. brig resumes the guest "+
		"on neither host until hull's record says. `brig teleport %s --status` reads that record (`hull "+
		"migrate --status %s`) and settles this one, and `brig teleport %s --abort` calls the move off if "+
		"it has not committed", ref, host, txid, why, ref, vm, ref)
}

func errSuffix(err error) string {
	if err == nil {
		return ""
	}
	return " (" + err.Error() + ")"
}

// finishMove records a move hull committed: the destination's brig marks the
// arrival finished, and this host's record says where the session runs.
func finishMove(ref, vm, host, store, txid, hullState string, wantJSON bool) error {
	if _, err := remoteOutput(host, store, receiveVerb, "--json", "--commit", "--sandbox", vm, "--txid", txid); err != nil {
		// The destination reads hull's record for itself when this is lost.
		// See wrap.arrivalRefusal.
		warnf("%s", notice.Newf("could not tell the brig on %s that the move finished: %v", host, err).
			Note("it reads hull's record there instead, so the session works there all the same"))
	}
	if err := wrap.RecordMoved(ref, vm, host, store, txid); err != nil {
		return fmt.Errorf("%s runs on %s now, and brig could not record that here: %w. "+
			"Reach it with ssh %s brig sh %s", ref, host, err, host, ref)
	}
	d := teleportData{Ref: ref, Sandbox: vm, State: "moved", Host: host, RemoteStore: store, TxID: txid, Hull: hullState}
	if wantJSON {
		return writeJSONDocument(os.Stdout, "Teleport", d)
	}
	fmt.Printf("teleported %s to %s (move %s)\n", ref, host, txid)
	warnf("brig sh %s, run, stop, info and logs now run there; brig rm %s removes the fenced copy left here", ref, ref)
	return nil
}

// abortReceive takes the destination's network back after a move that did
// not happen. Best effort and said: a gateway left there costs a process,
// and the move is over either way.
func abortReceive(host, store, ref, vm, txid string) {
	if _, err := remoteOutput(host, store, receiveVerb, "--json", "--abort", "--ref", ref, "--sandbox", vm, "--txid", txid); err != nil {
		warnf("%s", notice.Newf("could not take back the network %s was given on %s: %v", vm, host, err).
			Note("ssh %s brig %s --abort --ref %s --sandbox %s --txid %s does it", host, receiveVerb, ref, vm, txid))
	}
}

// teleportStatus is --status: hull's record of the move, and the record here
// brought up to date with it.
func teleportStatus(cfg *wrap.Config, ref string, wantJSON bool) error {
	vm := cfg.VMName
	loc := cfg.Location()
	d := teleportData{Ref: ref, Sandbox: vm, State: "here", Host: loc.Host, PendingHost: loc.PendingHost,
		RemoteStore: loc.RemoteStore, TxID: loc.TxID}
	if loc.Host != "" {
		d.State = "moved"
		return printTeleport(d, wantJSON, fmt.Sprintf("%s runs on %s (move %s)", ref, loc.Host, loc.TxID))
	}
	if loc.PendingHost == "" {
		return printTeleport(d, wantJSON, fmt.Sprintf("%s runs on this host; no move of it is recorded", ref))
	}
	mover, ok := cfg.Runtime.(runtime.Migrator)
	if !ok || cfg.Runtime == nil {
		return fmt.Errorf("a move of %s to %s (%s) is recorded, and this runtime cannot say where it "+
			"stands. `hull migrate --status %s` does", ref, loc.PendingHost, loc.TxID, vm)
	}
	st, err := mover.MigrateStatus(vm)
	if err != nil {
		return fmt.Errorf("cannot read hull's record of the move of %s (%s): %w. `hull migrate --status %s` "+
			"says where it stands", ref, loc.TxID, err, vm)
	}
	d.Hull = st.State
	if st.TxID == "" {
		// No record at all says the move never began only while the
		// sandbox is here to hold one: `hull rm` of a source whose move
		// committed takes the record with it.
		never, err := noMoveBegan(cfg.Runtime, vm)
		if !never {
			cause := "This host has no sandbox " + vm + " either, and the record may have gone with it"
			if err != nil {
				cause = "brig cannot tell whether the sandbox is still here: " + err.Error()
			}
			return fmt.Errorf("hull has no record of the move of %s to %s (%s). %s. The move may have "+
				"committed, so brig keeps it recorded and resumes nothing. `ssh %s %s` says whether the "+
				"guest arrived there", ref, loc.PendingHost, loc.TxID, cause, loc.PendingHost,
				remoteHullStatus(loc.RemoteStore, vm))
		}
	}
	switch {
	case st.TxID == loc.TxID && st.State == runtime.MoveCommitted:
		return finishMove(ref, vm, loc.PendingHost, loc.RemoteStore, loc.TxID, st.State, wantJSON)
	case st.TxID == loc.TxID && st.State == runtime.MoveAborted:
		abortReceive(loc.PendingHost, loc.RemoteStore, ref, vm, loc.TxID)
		if err := wrap.ClearMove(ref, vm, loc.TxID); err != nil {
			return err
		}
		d.State, d.PendingHost = "aborted", ""
		return printTeleport(d, wantJSON, fmt.Sprintf("the move of %s to %s (%s) was called off, and hull "+
			"restores the session on this host itself; %s", ref, loc.PendingHost, loc.TxID, restoreRemedy(vm)))
	case st.TxID != loc.TxID && !runtime.MoveFenced(st.State):
		// hull has no record of this move past its start: it never fenced
		// the guest, which still runs here.
		abortReceive(loc.PendingHost, loc.RemoteStore, ref, vm, loc.TxID)
		if err := wrap.ClearMove(ref, vm, loc.TxID); err != nil {
			return err
		}
		d.State, d.PendingHost = "here", ""
		return printTeleport(d, wantJSON, fmt.Sprintf("hull has no record of the move of %s (%s), so it "+
			"never began; the session runs on this host", ref, loc.TxID))
	}
	d.State = "moving"
	said := st.Said
	if said == "" {
		said = "hull's record says " + orNone(st.State)
	}
	return printTeleport(d, wantJSON, fmt.Sprintf("the move of %s to %s (%s) has not finished: %s\n"+
		"brig resumes the guest on neither host until hull's record says how the move ended. "+
		"`hull migrate %s --txid %s` takes it on from where it stopped, `hull migrate --status %s` has "+
		"hull's account, and `brig teleport %s --abort` calls it off", ref, loc.PendingHost, loc.TxID,
		said, vm, loc.TxID, vm, ref))
}

// noMoveBegan reports whether hull's having no move on record of the sandbox
// means no move of it began: only while the sandbox is here. hull allows
// `hull rm` of a source once its move committed away, and the record goes
// with the instance, so a sandbox that is gone, or that brig cannot find,
// says nothing about where the guest runs.
func noMoveBegan(rt runtime.Runtime, vm string) (bool, error) {
	if rt == nil {
		return false, errors.New("no runtime")
	}
	return sandboxPresent(rt, vm)
}

// remoteHullStatus is the hull line that asks the destination's record of a
// move into store there.
func remoteHullStatus(store, vm string) string {
	if store == "" {
		return "hull migrate --status " + vm
	}
	return "hull --store-dir " + shellJoin([]string{store}) + " migrate --status " + vm
}

// restoreRemedy is what brings back a guest an abort left stopped here.
func restoreRemedy(vm string) string {
	return fmt.Sprintf("if `brig sh` then refuses it, `hull restore %s` brings it back from its checkpoint", vm)
}

func orNone(s string) string {
	if s == "" {
		return "nothing"
	}
	return s
}

// teleportAbort is --abort: hull calls the move off, and only when its record
// then says the destination cannot run the guest does brig give the network
// there back and ask hull to resume the source.
func teleportAbort(cfg *wrap.Config, ref string, wantJSON bool) error {
	vm := cfg.VMName
	loc := cfg.Location()
	if loc.Host != "" {
		return fmt.Errorf("the move of %s to %s committed, and it runs there. A committed move is not "+
			"called off; `brig teleport` from there moves it again", ref, loc.Host)
	}
	if loc.PendingHost == "" {
		return fmt.Errorf("no move of %s is recorded, so there is nothing to call off", ref)
	}
	mover, ok := cfg.Runtime.(runtime.Migrator)
	if !ok || cfg.Runtime == nil {
		return fmt.Errorf("this runtime cannot call off a move; `hull migrate-abort --txid %s %s` does", loc.TxID, vm)
	}
	st, err := mover.MigrateAbort(vm, loc.TxID)
	if st.TxID != loc.TxID || st.State != runtime.MoveAborted {
		cause := "hull's record says " + orNone(st.State)
		if err != nil {
			cause = err.Error()
		}
		return fmt.Errorf("hull did not confirm the move of %s (%s) is off: %s. The session stays as it "+
			"is on both hosts; `hull migrate --status %s` says where it stands", ref, loc.TxID, cause, vm)
	}
	// hull aborts only with the destination's word that it does not run the
	// guest and will refuse the commit, and then runs the guest here again
	// itself. brig resumes nothing: that is hull's to do, on that word.
	abortReceive(loc.PendingHost, loc.RemoteStore, ref, vm, loc.TxID)
	if err := wrap.ClearMove(ref, vm, loc.TxID); err != nil {
		return err
	}
	if err != nil {
		// hull records the abort before it restores the guest here, so a
		// restore that failed still reads aborted. The move is over either
		// way; the guest may be stopped here.
		return fmt.Errorf("called off the move of %s to %s (%s), and hull reported: %v. The guest may be "+
			"stopped here with its fenced checkpoint: %s", ref, loc.PendingHost, loc.TxID, err, restoreRemedy(vm))
	}
	d := teleportData{Ref: ref, Sandbox: vm, State: "aborted", TxID: loc.TxID, Hull: st.State}
	return printTeleport(d, wantJSON, fmt.Sprintf("called off the move of %s to %s (%s); hull runs it "+
		"on this host again", ref, loc.PendingHost, loc.TxID))
}

func printTeleport(d teleportData, wantJSON bool, line string) error {
	if wantJSON {
		return writeJSONDocument(os.Stdout, "Teleport", d)
	}
	fmt.Println(line)
	return nil
}

// probeReport is what `brig teleport-probe` answers on a destination: what
// is there, and whether it can take this session.
type probeReport struct {
	Format   int           `json:"format"`
	Brig     string        `json:"brig"`
	Runtime  probeRuntime  `json:"runtime"`
	Hvi      probeTool     `json:"hvi"`
	Egress   probeEgress   `json:"egress"`
	StateDir string        `json:"stateDir"`
	Store    probeStore    `json:"store"`
	Agent    *probeAgent   `json:"agent,omitempty"`
	Sandbox  *probeSandbox `json:"sandbox,omitempty"`
	Network  *probeNetwork `json:"network,omitempty"`
	Policy   *probePolicy  `json:"policy,omitempty"`
}

// probePolicy is the egress the destination's own policy files bind to the
// session, which its brig resolves for the session once it arrived.
type probePolicy struct {
	Egress   policy.Egress `json:"egress"`
	Policies []string      `json:"policies,omitempty"`
	Error    string        `json:"error,omitempty"`
}

type probeRuntime struct {
	Kind    string `json:"kind,omitempty"`
	Bin     string `json:"bin,omitempty"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

type probeTool struct {
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
}

type probeEgress struct {
	Enforced bool   `json:"enforced"`
	Why      string `json:"why,omitempty"`
}

// probeStore is the hull store a moved session would land in. Named is
// BRIG_HULL_STORE_DIR there, or "" for hull's own default, Dir the directory
// either way.
type probeStore struct {
	Named     string `json:"named,omitempty"`
	Dir       string `json:"dir"`
	FreeBytes uint64 `json:"freeBytes"`
	Error     string `json:"error,omitempty"`
}

type probeAgent struct {
	Name  string `json:"name"`
	Known bool   `json:"known"`
}

type probeSandbox struct {
	Name     string `json:"name"`
	Exists   bool   `json:"exists"`
	Running  bool   `json:"running"`
	Arriving string `json:"arriving,omitempty"`
	Error    string `json:"error,omitempty"`
}

type probeNetwork struct {
	Net    string `json:"net"`
	Slot   int    `json:"slot"`
	Holder string `json:"holder,omitempty"`
	Error  string `json:"error,omitempty"`
}

// teleportProbeCmd is `brig teleport-probe`, run on a destination by the
// source's brig. It answers and changes nothing.
func teleportProbeCmd(out io.Writer, args []string) error {
	fs := flag.NewFlagSet(probeVerb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var agent, sandbox, net, ref string
	slot := -1
	fs.Bool("json", true, "")
	fs.StringVar(&ref, "ref", "", "")
	fs.StringVar(&agent, "agent", "", "")
	fs.StringVar(&sandbox, "sandbox", "", "")
	fs.StringVar(&net, "network", "", "")
	fs.IntVar(&slot, "slot", -1, "")
	if err := fs.Parse(args); err != nil {
		return usagef("%s: %v", probeVerb, err)
	}
	r := probeReport{Format: probeFormat, Brig: buildinfo.Read().String()}
	r.StateDir = stateDirHere()
	rt, rtErr := runtime.Detect()
	if rtErr != nil {
		r.Runtime.Error = rtErr.Error()
	} else {
		r.Runtime.Kind, r.Runtime.Bin = rt.Kind(), rt.Bin()
		r.Runtime.Version = firstLine(probeOutput(rt.Bin(), "--version"))
		if rt.Kind() == "hull" {
			if p := runtime.HviPath(rt.Bin()); p != "" {
				r.Hvi = probeTool{Path: p, Version: firstLine(probeOutput(p, "--version"))}
			}
			if err := runtime.GatewayEnforcesEgress(rt.Bin()); err != nil {
				r.Egress.Why = err.Error()
			} else {
				r.Egress.Enforced = true
			}
		} else {
			r.Egress.Why = "the runtime here is " + rt.Kind() + ", not hull"
		}
	}
	r.Store = storeHere()
	if agent != "" {
		_, known := profile.Lookup(agent)
		r.Agent = &probeAgent{Name: agent, Known: known}
	}
	if ref != "" {
		e, names, err := wrap.BoundEgress(ref)
		r.Policy = &probePolicy{Egress: e, Policies: names}
		if err != nil {
			r.Policy.Error = err.Error()
		}
	}
	if sandbox != "" {
		sb := &probeSandbox{Name: sandbox, Arriving: wrap.ArrivingMove(sandbox)}
		if rt != nil {
			if ex, ok := rt.(runtime.Exister); ok {
				exists, err := ex.Exists(sandbox)
				sb.Exists = exists
				if err != nil {
					sb.Error = err.Error()
				}
			} else {
				sb.Error = "the runtime here cannot say whether a sandbox exists"
			}
			if running, err := rt.Running(sandbox); err == nil {
				sb.Running = running
			} else if sb.Error == "" {
				sb.Error = err.Error()
			}
		}
		r.Sandbox = sb
		if net != "" && slot >= 0 {
			holder, err := runtime.AddressHolder(net, slot, sandbox)
			nw := &probeNetwork{Net: net, Slot: slot, Holder: holder}
			if err != nil {
				nw.Error = err.Error()
			}
			r.Network = nw
		}
	}
	return writeJSONDocument(out, "TeleportProbe", r)
}

// stateDirHere is where brig keeps its state on this host.
func stateDirHere() string {
	if d := os.Getenv("BRIG_STATE_DIR"); d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".brig")
	}
	return ""
}

// storeHere is the hull store brig drives on this host and the room left on
// the filesystem it is on.
func storeHere() probeStore {
	st := probeStore{Named: runtime.StoreDir()}
	st.Dir = st.Named
	if st.Dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			st.Error = err.Error()
			return st
		}
		st.Dir = filepath.Join(home, ".hull", "store")
	}
	// The store may not exist yet: the room that counts is that of the
	// nearest directory that does.
	dir := st.Dir
	for {
		var fsStat syscall.Statfs_t
		if err := syscall.Statfs(dir, &fsStat); err == nil {
			st.FreeBytes = uint64(fsStat.Bavail) * uint64(fsStat.Bsize)
			return st
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			st.Error = "no part of " + st.Dir + " exists"
			return st
		}
		dir = parent
	}
}

// probeOutput is what a binary prints for one question, bounded: a probe
// that hangs must not hold the move.
func probeOutput(bin string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// probeRemote asks the destination's brig what it has.
func probeRemote(host, ref, agent, sandbox string, net wrap.MoveNetwork) (probeReport, error) {
	out, err := remoteOutput(host, "", probeVerb, "--json", "--agent", agent, "--sandbox", sandbox,
		"--network", net.Net, "--slot", strconv.Itoa(net.Slot), "--ref", ref)
	if err != nil {
		return probeReport{}, fmt.Errorf("could not ask the brig on %s whether it can take %s, and nothing "+
			"was paused: %w. It needs ssh to %s without a password prompt, and brig on the PATH there "+
			"(or %s naming it)", host, sandbox, err, host, remoteBrigSetting)
	}
	var doc struct {
		Kind string      `json:"kind"`
		Data probeReport `json:"data"`
	}
	if err := json.Unmarshal(out, &doc); err != nil || doc.Kind != "TeleportProbe" {
		return probeReport{}, fmt.Errorf("the brig on %s did not answer the probe in a form this brig reads; "+
			"it may be older than teleport. Nothing was paused", host)
	}
	if doc.Data.Format != probeFormat {
		return probeReport{}, fmt.Errorf("the brig on %s answers the probe in format %d, and this brig reads %d. "+
			"Nothing was paused", host, doc.Data.Format, probeFormat)
	}
	return doc.Data, nil
}

// checkProbe refuses a destination that cannot take this session, each
// with its reason, before anything is paused.
func checkProbe(cfg *wrap.Config, r probeReport, host, ref, txid string, net wrap.MoveNetwork) error {
	vm := cfg.VMName
	switch {
	case r.Runtime.Error != "":
		return fmt.Errorf("the brig on %s has no runtime to take %s: %s. Nothing was paused", host, ref, r.Runtime.Error)
	case r.Runtime.Kind != "hull":
		return fmt.Errorf("the brig on %s resolves the %s runtime, and a moved session needs hull there "+
			"(BRIG_RUNTIME=hull). Nothing was paused", host, r.Runtime.Kind)
	case r.Hvi.Path == "":
		return fmt.Errorf("%s has no hvi beside its hull or on its PATH, and the moved guest runs on hvi. "+
			"Nothing was paused", host)
	case r.Agent == nil || !r.Agent.Known:
		return fmt.Errorf("the brig on %s has no agent %s, so it could not reach %s afterwards. Add the "+
			"agent there first. Nothing was paused", host, cfg.Profile.Name, ref)
	}
	// An offline guest has no gateway, here or there, so its rules need
	// none to enforce them; they go with it as the record of its policy.
	if net.Egress.Default != "" && net.Net != "none" && !r.Egress.Enforced {
		return moveRefusal(fmt.Sprintf("%s is under an egress policy, and %s cannot enforce one: %s",
			ref, host, orNone(r.Egress.Why)),
			"Install a hull there whose network-gateway takes --egress-default, or detach the policy here")
	}
	// The brig there resolves its own binding for the session first, and the
	// carried rules only when it binds none. A binding with other rules would
	// read as a changed policy there, and every verb on the moved session
	// would refuse while the guest ran under the carried ones.
	switch p := r.Policy; {
	case p == nil || p.Error != "":
		why := "it did not say"
		if p != nil {
			why = p.Error
		}
		return fmt.Errorf("cannot tell which policy %s binds to %s: %s. Nothing was paused", host, ref, why)
	case p.Egress.Default != "" && !wrap.SameEgress(p.Egress, net.Egress):
		return fmt.Errorf("%s binds %s to %s, whose rules are not the ones %s runs under here, and the brig "+
			"there would refuse the moved session as a changed policy. Detach it there (`ssh %s brig policy "+
			"ls` names the binding), or bind the same rules. Nothing was paused", host,
			strings.Join(p.Policies, ", "), ref, ref, host)
	}
	if sb := r.Sandbox; sb == nil || sb.Error != "" {
		why := "it did not answer for the sandbox"
		if sb != nil {
			why = sb.Error
		}
		return fmt.Errorf("cannot tell whether %s already has a sandbox %s: %s. Nothing was paused", host, vm, why)
	} else if (sb.Exists || sb.Arriving != "") && sb.Arriving != txid {
		what := "a sandbox " + vm
		if sb.Arriving != "" {
			what = "move " + sb.Arriving + " of " + vm + " arriving"
		}
		return fmt.Errorf("%s already has %s. Remove it there first (ssh %s brig rm %s). Nothing was paused",
			host, what, host, ref)
	}
	if net.Net != "none" {
		switch nw := r.Network; {
		case nw == nil || nw.Error != "":
			return fmt.Errorf("cannot tell whether %s can give %s its address. Nothing was paused", host, vm)
		case nw.Holder != "":
			return fmt.Errorf("%s gives the address %s keeps (slot %d of its %s network) to %s, and a moved "+
				"guest keeps its address. Nothing was paused", host, vm, net.Slot, net.Net, nw.Holder)
		}
	}
	need := uint64(cfg.Mem) << 20
	if di, ok := cfg.Runtime.(runtime.DiskInspector); ok {
		if disk, err := di.RootDisk(vm); err == nil && disk != "" {
			if fi, err := os.Stat(disk); err == nil {
				need += uint64(fi.Size())
			}
		}
	}
	if r.Store.Error == "" && r.Store.FreeBytes < need {
		return fmt.Errorf("%s has %d MiB free at %s, and %s needs %d MiB for its memory and disk. "+
			"Nothing was paused", host, r.Store.FreeBytes>>20, r.Store.Dir, ref, need>>20)
	}
	return nil
}

// receiveRemote asks the destination's brig to take the session's network
// and file it, before hull is asked to move anything.
func receiveRemote(host, store, ref, vm, txid string, net wrap.MoveNetwork) (string, error) {
	egress, err := json.Marshal(net.Egress)
	if err != nil {
		return "", err
	}
	from, _ := os.Hostname()
	if u := os.Getenv("USER"); u != "" && from != "" {
		from = u + "@" + from
	}
	args := []string{receiveVerb, "--json", "--ref", ref, "--sandbox", vm, "--txid", txid,
		"--from", from, "--network", net.Net, "--slot", strconv.Itoa(net.Slot), "--egress", string(egress)}
	if len(net.Policies) > 0 {
		args = append(args, "--policies", strings.Join(net.Policies, ","))
	}
	out, err := remoteOutput(host, store, args...)
	if err != nil {
		return "", err
	}
	var doc struct {
		Data struct {
			Socket string `json:"socket"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return "", fmt.Errorf("its answer did not read: %w", err)
	}
	return doc.Data.Socket, nil
}

// teleportReceiveCmd is `brig teleport-receive`, run on a destination by the
// source's brig: take a moving session's network and file it, mark its move
// finished, or give back what was taken for a move that did not happen.
func teleportReceiveCmd(out io.Writer, args []string) error {
	fs := flag.NewFlagSet(receiveVerb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var spec wrap.ArrivalSpec
	var ref, egress, policies string
	var commit, abort bool
	fs.Bool("json", true, "")
	fs.StringVar(&ref, "ref", "", "")
	fs.StringVar(&spec.Sandbox, "sandbox", "", "")
	fs.StringVar(&spec.TxID, "txid", "", "")
	fs.StringVar(&spec.From, "from", "", "")
	fs.StringVar(&spec.Network, "network", "", "")
	fs.IntVar(&spec.Slot, "slot", 0, "")
	fs.StringVar(&egress, "egress", "", "")
	fs.StringVar(&policies, "policies", "", "")
	fs.BoolVar(&commit, "commit", false, "")
	fs.BoolVar(&abort, "abort", false, "")
	if err := fs.Parse(args); err != nil {
		return usagef("%s: %v", receiveVerb, err)
	}
	if spec.Sandbox == "" || spec.TxID == "" {
		return usagef("%s needs --sandbox and --txid", receiveVerb)
	}
	reply := struct {
		State  string `json:"state"`
		Socket string `json:"socket,omitempty"`
	}{}
	switch {
	case commit:
		if err := wrap.CommitArrival(spec.Sandbox, spec.TxID); err != nil {
			return err
		}
		reply.State = "committed"
	case abort:
		rt, err := runtime.Detect()
		if err != nil {
			return err
		}
		if err := wrap.AbortArrival(spec.Sandbox, spec.TxID, rt); err != nil {
			return err
		}
		reply.State = "aborted"
	default:
		if egress != "" {
			if err := json.Unmarshal([]byte(egress), &spec.Egress); err != nil {
				return fmt.Errorf("the rules sent with %s do not read: %w", spec.Sandbox, err)
			}
		}
		if policies != "" {
			spec.Policies = strings.Split(policies, ",")
		}
		rt, err := runtime.Detect()
		if err != nil {
			return err
		}
		if rt.Kind() != "hull" {
			return errors.New("a moved session needs hull here, and this brig resolves " + rt.Kind())
		}
		sock, err := wrap.ArrivalReceive(ref, spec, rt)
		if err != nil {
			return err
		}
		reply.State, reply.Socket = "prepared", sock
	}
	return writeJSONDocument(out, "TeleportReceive", reply)
}
