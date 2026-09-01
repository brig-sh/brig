package wrap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/brig-sh/brig/internal/session"
)

// The session index: what brig has to remember about a session between one
// invocation and the next, which today is the host directory its sandbox was
// started with.
//
// Every invocation resolves the workspace from scratch -- --workspace, then
// BRIG_WORKSPACE, then ~/brig/<profile>-<name> -- and EnsureRunning compares
// the running sandbox against whatever that produced. That is right for an
// invocation that names a directory and wrong for one that does not: a session
// created with --workspace never matches its own default, so the next flagless
// verb reads the mismatch as a stale share and restarts the sandbox. The work
// survives that, because it is in the workspace on the host. The guest's
// memory-only state does not, and an in-sandbox login goes with it.
//
// So the path a sandbox was started with is written down, and read back when
// the invocation names none. It is an index and not a source of truth: the
// runtime stays authoritative about what is actually mounted, the stale-share
// check still runs against whatever path is resolved, and an explicit
// --workspace or BRIG_WORKSPACE still wins -- asking for a different directory
// is something a user is entitled to do, restart and all.
//
// Keyed by the ref: agent@label, and the bare agent for a session that has no
// label, built from the resolved profile name and the slug. Three things
// follow from that key, and none of them from the sandbox name this file used
// to be keyed by:
//
//   - A sandbox name cannot be read back. brig-claude-code-refactor is a
//     refactor session of claude-code and equally the default session of a
//     profile called claude-code-refactor, and the dash between them cannot
//     say which. A ref keeps the two apart, because '@' is only ever the one
//     separator.
//   - The resolved profile name, so an alias finds one entry: `brig run claude`
//     and `brig run claude-code` are one profile, and a session started under
//     either spelling is the entry the other one reads.
//   - The slug rather than the name as typed, because --name is lenient and
//     Slug sanitises what it is handed, so the raw name is not a stable
//     identifier. The slug is what actually names the sandbox and the home.
//
// The value is the home and the sandbox: what the session is, and which
// instance is carrying it just now. Both directions are read -- the home by
// ref, which is what a flagless verb needs, and the home by sandbox name,
// which is all `brig ls` and `brig reset` have in hand.
//
// Nothing is migrated out of the sandbox-keyed workspaces.json this replaces.
// Deriving a ref from its keys is precisely the ambiguity above, so a migration
// would have to guess; the file is deleted on sight instead and each session in
// it costs one restart, which is what an absent entry has always cost and which
// is safe because everything persistent lives in the workspace on the host.
//
// It lives beside gateway-ips.json, which is the same kind of file for the
// same kind of reason: small per-sandbox bookkeeping that has to outlive one
// invocation. Deliberately not a record inside the workspace itself, which
// would make that directory both state and identity -- copy it to a second
// machine, or point two sandbox names at it, and it would carry a claim about
// which sandbox owns it.
//
// Two brig processes recording at once can lose one of the two entries, since
// each reads the file, edits its own key and writes the whole map back. The
// cost is one restart for whichever session lost, which is the cost of not
// having the index at all -- worth less than a lock file on the path every
// boot takes.

// sessionIndexName is the file, inside stateDir. legacyWorkspaceIndexName is
// the sandbox-keyed file it replaces, kept as a name only so it can be
// deleted; see dropLegacyWorkspaceIndex.
const (
	sessionIndexName         = "sessions.json"
	legacyWorkspaceIndexName = "workspaces.json"
)

// sessionEntry is what the index records about one session.
//
// Home is the guest's home directory on the host -- the same path the run
// calls the workspace. It is named for what it is to the session rather than
// for the flag that supplies it, because it is the directory that makes the
// session the session: an entry with a different home is a different session,
// and #6 adds the per-run project mount beside it, which is not.
//
// Sandbox is the instance carrying the session at the moment, which is the
// part that is not identity: it is removed and recreated by an ordinary
// restart. It is here because `brig ls` and `brig reset` work from the
// runtime's instance list and have no ref to look up -- see WorkspaceOfSandbox
// and ForgetSandbox -- and because it is what tells an entry about the sandbox
// this invocation is addressing from one about a differently named sandbox of
// the same session. See rememberedWorkspace.
type sessionEntry struct {
	Home    string `json:"home"`
	Sandbox string `json:"sandbox"`
}

// stateDir is where brig keeps what has to outlive a single invocation.
// BRIG_STATE_DIR moves it, which is what lets a test run without writing into
// the state of whoever is running the test.
func stateDir() (string, error) {
	if dir := os.Getenv("BRIG_STATE_DIR"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory to keep brig's state in: %w", err)
	}
	return filepath.Join(home, ".brig"), nil
}

// indexPath is stateDir/name, for one of brig's small bookkeeping files.
func indexPath(name string) (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// readIndex returns the map recorded under name, and an empty one for every
// way the read can fail.
//
// Generic in the value rather than duplicated per file: the tolerant read and
// the atomic write below are the whole reason these files behave the same way
// as each other, and two copies of them would be two chances for one file to
// stop being atomic or stop being tolerant. The session index records a
// struct and the slug-claim index a string; nothing else differs.
//
// A file that is missing, unreadable or corrupt is not worth failing a command
// over: at worst an absent entry costs one restart, which is exactly the
// behaviour of the release before these files existed. Failing instead would
// make a stray file in ~/.brig able to stop every brig command on the host. A
// file of the wrong shape for V -- an older release's index under a name this
// one has reused -- is corrupt in exactly that sense and reads as empty too.
func readIndex[V any](name string) map[string]V {
	index := map[string]V{}
	path, err := indexPath(name)
	if err != nil {
		return index
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return index
	}
	if err := json.Unmarshal(blob, &index); err != nil {
		return map[string]V{}
	}
	return index
}

// writeIndex replaces the file named name with the map it is given.
func writeIndex[V any](name string, index map[string]V) error {
	path, err := indexPath(name)
	if err != nil {
		return err
	}
	// 0700 for the directory: it holds nothing secret, but it names host
	// directories of somebody's, and brig's other state is already private.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	blob, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	// Written through a temporary file and renamed, so a crash mid-write
	// cannot leave a half-parsed map behind -- which would cost every entry in
	// the file rather than one of them.
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+name+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(blob, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// readSessionIndex and writeSessionIndex are the session index's names for the
// shared read and write above.
func readSessionIndex() map[string]sessionEntry {
	return readIndex[sessionEntry](sessionIndexName)
}

func writeSessionIndex(index map[string]sessionEntry) error {
	return writeIndex(sessionIndexName, index)
}

// dropLegacyWorkspaceIndex deletes the sandbox-keyed file this index replaces.
//
// On sight rather than on upgrade, because brig has no upgrade step to hang it
// on: it is called from the two paths that already touch the state directory
// on an ordinary command, recording a session and listing the sandboxes. Its
// absence is the normal case -- a host that never ran the older release, or one
// that has already been past here -- so a failed removal is dropped like the
// rest of the bookkeeping and the only cost of it is a stale file nobody reads.
func dropLegacyWorkspaceIndex() {
	path, err := indexPath(legacyWorkspaceIndexName)
	if err != nil {
		return
	}
	_ = os.Remove(path)
}

// sessionKey is how a session is filed: its ref, from the resolved profile
// name and the slug. One function so the two callers that build it -- Load,
// which looks an entry up before there is a Config, and rememberSession, which
// writes it -- cannot spell the key two ways.
func sessionKey(agent, slug string) string {
	return session.Ref{Agent: agent, Label: slug}.String()
}

// rememberedWorkspace is the workspace the sandbox of this session was started
// with, or "" when nothing usable has been recorded -- a session created
// before this index existed, one whose entry has been pruned, or one recorded
// against a different sandbox.
//
// The sandbox has to agree because BRIG_NAME renames the sandbox without
// changing the ref: the same profile and slug under two sandbox names are two
// instances, and handing one the directory recorded for the other is the stale
// share this file exists to avoid.
func rememberedWorkspace(ref, vmName string) string {
	entry := readSessionIndex()[ref]
	if entry.Sandbox != vmName {
		return ""
	}
	return entry.Home
}

// WorkspaceOfSandbox is the workspace recorded for whichever session is
// carrying this sandbox, or "" when none is.
//
// The other direction of the same lookup, exported for the callers that have a
// sandbox name and no ref: `brig ls`, which reports the directory a running
// sandbox actually has rather than the one its name would derive. A sandbox
// carries one session, so the first entry naming it is the only one -- two
// refs pointing at one sandbox takes a BRIG_NAME that overrides the sandbox
// name of both, and a listing could not tell those apart whatever it did here.
func WorkspaceOfSandbox(vmName string) string {
	for _, entry := range readSessionIndex() {
		if entry.Sandbox == vmName {
			return entry.Home
		}
	}
	return ""
}

// ForgetSandbox drops the entry of the session carrying a removed sandbox, so
// the next session to take that name starts from the ordinary resolution
// rather than inheriting a directory chosen for a different one.
//
// Errors are dropped rather than returned, the way releaseGatewayIP drops its
// own: a removal that worked must not report a failure because a bookkeeping
// file could not be rewritten.
func ForgetSandbox(vmName string) {
	index := readSessionIndex()
	dropped := false
	for key, entry := range index {
		if entry.Sandbox == vmName {
			delete(index, key)
			dropped = true
		}
	}
	if !dropped {
		return
	}
	_ = writeSessionIndex(index)
}

// PruneSessions drops every entry whose sandbox is not in the list of what the
// runtime has.
//
// `brig ls` is where this runs, because it is the one verb that asks the
// runtime what exists and so the one place brig learns that a sandbox went
// away without going through Remove -- removed with the runtime's own CLI, or
// lost with a runtime that was reinstalled. The cost of being wrong is small
// in the direction that matters: a list that came back short prunes an entry
// that was still good, and the session pays one restart for it.
//
// Called with every instance the runtime lists rather than only brig's own, so
// that a sandbox brig would not recognise still counts as present.
func PruneSessions(live []string) {
	dropLegacyWorkspaceIndex()
	index := readSessionIndex()
	if len(index) == 0 {
		return
	}
	have := make(map[string]bool, len(live))
	for _, name := range live {
		have[name] = true
	}
	dropped := false
	for key, entry := range index {
		if !have[entry.Sandbox] {
			delete(index, key)
			dropped = true
		}
	}
	if !dropped {
		return
	}
	_ = writeSessionIndex(index)
}

// rememberSession records what this run has just started: the ref it is filed
// under, the directory the sandbox was given as its home, and the sandbox
// carrying it.
//
// Also called when the running sandbox already matches, which is what fills
// the index in for a session created before it existed: the entry is written
// once, and every invocation after that is a read.
//
// A failure to write is a warning rather than an error. The sandbox is up and
// working, so failing the command here would be refusing the thing that
// succeeded over the note about it -- but the cost lands later and nowhere
// near this line, on some flagless verb that restarts the sandbox, so it is
// worth saying out loud where the reason is still visible.
func (c *Config) rememberSession() {
	dropLegacyWorkspaceIndex()
	key := sessionKey(c.Profile.Name, c.Slug)
	entry := sessionEntry{Home: c.Workspace, Sandbox: c.VMName}
	index := readSessionIndex()
	if index[key] == entry {
		return
	}
	index[key] = entry
	if err := writeSessionIndex(index); err != nil {
		c.warnf("could not record %s as the workspace of %s (%v). A later command "+
			"that names no workspace will fall back to the default one and restart "+
			"the sandbox.", c.Workspace, c.VMName, err)
	}
}
