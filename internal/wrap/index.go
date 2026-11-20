package wrap

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"

	"github.com/brig-sh/brig/internal/notice"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/session"
)

// The session index: what brig has to remember about a session between one
// invocation and the next, which today is the host directory its sandbox was
// started with.
//
// Every invocation resolves the workspace from scratch -- --workspace, then
// BRIG_WORKSPACE, then ~/.brig/homes/<sandbox> -- and EnsureRunning compares
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
// session the session: an entry with a different home is a different session.
//
// Sandbox is the instance carrying the session at the moment, which is the
// part that is not identity: it is removed and recreated by an ordinary
// restart. It is here because `brig ls` and `brig reset` work from the
// runtime's instance list and have no ref to look up -- see WorkspaceOfSandbox
// and ForgetSandbox -- and because it is what tells an entry about the sandbox
// this invocation is addressing from one about a differently named sandbox of
// the same session. See rememberedWorkspace.
//
// Project is the host directory the session LAST RAN with, and the only key
// here that is not the session at all. It is omitted when there is none, which
// is most sessions: the identity is the ref, and the ref is the agent plus the
// label, which is the home. Two runs of one session on two projects are the
// same session, so an entry with a different project is the same entry with a
// new value -- which is exactly what makes it worth recording, because a share
// cannot be attached to a live sandbox and this is what the next run compares
// against to find out whether it has to recreate one. See projectShareStale.
//
// Ephemeral is set when brig chose Home itself, because the run named no
// workspace. Such a home goes with the sandbox on `brig rm`. An entry without
// it keeps its home, which covers every session recorded by an older release.
// See DropEphemeralHome.
//
// Runtime is the kind of runtime that booted the sandbox: hull, nerdctl or
// docker. Every runtime brig drives reads this one file, and none sees the
// others' sandboxes, so without it a session booted under nerdctl looked
// absent to hull: hull booted a second VM of the same name on the same home,
// its `brig ls` pruned the entry, and its next run or `brig rm` deleted the
// home under the VM still running on it. See runtimeRefusal. An entry without
// it is from an older release or rewritten by one. See entryRuntime for the
// runtime it is read as.
//
// The network posture is not here, although it is the same kind of fact. An
// older release rewrites this file with the fields it knows, so a posture kept
// here is lost the first time one runs. See rememberedNetwork.
type sessionEntry struct {
	Home      string `json:"home"`
	Sandbox   string `json:"sandbox"`
	Project   string `json:"project,omitempty"`
	Ephemeral bool   `json:"ephemeral,omitempty"`
	Runtime   string `json:"runtime,omitempty"`
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
//
// The nil check is the one corrupt file the unmarshal accepts: the JSON
// literal null parses without error and leaves the map nil, which would be a
// panic at the first write rather than an empty read -- a louder version of
// the failure this tolerance exists to avoid.
func readIndex[V any](name string) map[string]V {
	index := map[string]V{}
	blob, err := readStateFile(name)
	if err != nil || blob == nil {
		return index
	}
	if err := json.Unmarshal(blob, &index); err != nil || index == nil {
		return map[string]V{}
	}
	return index
}

// readStateFile returns the bytes of the file named name in the state
// directory, and nil with no error when there is no such file.
func readStateFile(name string) ([]byte, error) {
	path, err := indexPath(name)
	if err != nil {
		return nil, err
	}
	blob, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return blob, err
}

// writeIndex replaces the file named name with the map it is given.
func writeIndex[V any](name string, index map[string]V) error {
	return writeStateFile(name, index)
}

// writeStateFile replaces the file named name in the state directory with v
// as indented JSON.
func writeStateFile(name string, v any) error {
	path, err := indexPath(name)
	if err != nil {
		return err
	}
	// 0700 for the directory: it holds nothing secret, but it names host
	// directories of somebody's, and brig's other state is already private.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	blob, err := json.MarshalIndent(v, "", "  ")
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
// with and whether brig created it, or "" when nothing usable has been recorded -- a session created
// before this index existed, one whose entry has been pruned, or one recorded
// against a different sandbox.
//
// The sandbox has to agree because BRIG_NAME renames the sandbox without
// changing the ref: the same profile and slug under two sandbox names are two
// instances, and handing one the directory recorded for the other is the stale
// share this file exists to avoid.
func rememberedWorkspace(ref, vmName string) (home string, ephemeral bool) {
	entry := readSessionIndex()[ref]
	if entry.Sandbox != vmName {
		return "", false
	}
	return entry.Home, entry.Ephemeral
}

// rememberedProject is the project the sandbox of this session last ran with,
// or "" when there was none, when nothing has been recorded, or when what is
// recorded belongs to a differently named sandbox.
//
// The sandbox has to agree for the same reason rememberedWorkspace requires it:
// BRIG_NAME renames the sandbox without changing the ref, and the mounts of one
// instance say nothing about the mounts of another.
func rememberedProject(ref, vmName string) string {
	entry := readSessionIndex()[ref]
	if entry.Sandbox != vmName {
		return ""
	}
	return entry.Project
}

// rememberedNetwork is the posture the sandbox of this session was last booted
// with, or "" when none is recorded: a sandbox an older release booted, one
// never booted, or one whose session entry names a different sandbox. The
// boolean reports whether any session entry names this sandbox, even when
// there is no usable posture record. That history requires inspection even
// if the runtime's existence check can no longer find its metadata.
//
// Kept by the runtime package beside the publications, in a file no older
// release rewrites. See runtime.BootedNet.
//
// The session entry has to agree as well, for the reason rememberedWorkspace
// gives, and because an older release's `brig rm` drops the entry and leaves
// the posture record behind. Without the check the next sandbox to take the
// name would inherit a posture from the one that was removed.
//
// A recorded word that is not a posture reads as absent. brig wrote it, nobody
// typed it, and the posture then comes from runtime inspection. A file
// that cannot be read at all is an error, the way the publication record is.
func rememberedNetwork(ref, vmName string) (Network, bool, error) {
	index := readSessionIndex()
	if index[ref].Sandbox != vmName {
		for _, entry := range index {
			if entry.Sandbox == vmName {
				return "", true, nil
			}
		}
		return "", false, nil
	}
	word, err := runtime.BootedNet(vmName)
	if err != nil {
		return "", true, err
	}
	network, _ := networkFromRuntime(word)
	return network, true, nil
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

// RefOfSandbox is the ref of whichever session is carrying this sandbox, or ""
// when none is.
//
// The key rather than the value, which is the other thing `brig ls` has to
// recover from a sandbox name: the listing prints the ref every other verb
// takes, and a sandbox name is not one -- brig-claude-code-refactor cannot say
// which of its dashes separated the agent from the label, and that is the
// ambiguity this index was keyed by ref to remove.
//
// Read on the same terms as WorkspaceOfSandbox: a sandbox carries one session,
// so the first entry naming it is the only one.
//
// The key of a session of a Brigfile target is not a ref, and a caller that
// printed it would hand the reader a word no verb takes, so it is "".
func RefOfSandbox(vmName string) string {
	for ref, entry := range readSessionIndex() {
		if entry.Sandbox == vmName {
			if strings.HasPrefix(ref, projectKeyPrefix) {
				return ""
			}
			return ref
		}
	}
	return ""
}

// ForgetSandbox drops the entry of the session carrying a removed sandbox, so
// the next session to take that name starts from the ordinary resolution
// rather than inheriting a directory chosen for a different one.
//
// It reports whether any entry named vmName, and the error of the rewrite
// that drops it. Remove and `brig rm --all` ignore that error, the way
// releaseGatewayIP drops its own: a removal that worked must not report a
// failure because a bookkeeping file could not be rewritten. rm of a sandbox
// the runtime does not have needs it, since forgetting the session is all
// that rm does.
func ForgetSandbox(vmName string) (bool, error) {
	forgetGitWatch(func(name string) bool { return name != vmName })
	return dropSessionEntries(vmName)
}

// dropSessionEntries is ForgetSandbox without the look at the project's .git,
// for a recreate, which removes the sandbox and keeps the session.
func dropSessionEntries(vmName string) (bool, error) {
	index := readSessionIndex()
	dropped := false
	for key, entry := range index {
		if entry.Sandbox == vmName {
			delete(index, key)
			dropped = true
		}
	}
	if !dropped {
		return false, nil
	}
	return true, writeSessionIndex(index)
}

// runtimeKind is rt's kind, or "" when there is no runtime to name.
func runtimeKind(rt runtime.Runtime) string {
	if rt == nil {
		return ""
	}
	return rt.Kind()
}

// hostOS is the host whose default decides what an entry that records no
// runtime is read as, a variable so a test on one host can read the index as
// the other does.
var hostOS = goruntime.GOOS

// unrecordedRuntime is the runtime that booted every entry that records none:
// nerdctl on Linux, under either of its binaries, and "" on macOS, where such
// an entry is the reader's. See entryRuntime.
func unrecordedRuntime() string {
	if hostOS == "darwin" {
		return ""
	}
	return "nerdctl"
}

// entryRuntime is the runtime a reader on kind reads entry as: the one it
// records, and for an entry that records none, the runtime that booted it.
//
// The index records the runtime only since 154f53e4. On macOS an entry from
// before it belongs to whichever runtime reads it, as before: hull was the
// default there then and is now. On Linux every such entry was booted on
// nerdctl, the default then, or on docker through it. hull is now the Linux
// default when a hull is on PATH, so an upgrade moves such a host under hull
// with those entries in the index. Read as hull's, a run of one would boot a
// second VM on the same guest home, hull's brig ls would prune it, and brig rm
// would delete the home under the VM still running on nerdctl. So hull on
// Linux reads it as nerdctl's and refuses it. The nerdctl runtime, under
// either binary, reads it as its own.
//
// An entry that hull booted on Linux before 154f53e4, under BRIG_RUNTIME=hull,
// records no runtime either, as does one that an older release rewrote. hull
// refuses it as nerdctl's. The refusal boots and deletes nothing, and
// brig rm --forget is the way past it.
func entryRuntime(entry sessionEntry, kind string) string {
	if entry.Runtime != "" {
		return entry.Runtime
	}
	if on := unrecordedRuntime(); on != "" && kind == "hull" {
		return on
	}
	return kind
}

// recordedEntry is the session index entry of the sandbox vmName, and false
// when none names it. An entry that records a runtime wins over one that does
// not.
func recordedEntry(vmName string) (sessionEntry, bool) {
	found, unrecorded := false, sessionEntry{}
	for _, entry := range readSessionIndex() {
		if entry.Sandbox != vmName {
			continue
		}
		if entry.Runtime != "" {
			return entry, true
		}
		found, unrecorded = true, entry
	}
	return unrecorded, found
}

// OtherRuntimeError is the refusal of a command on a sandbox that the session
// index records on another runtime than the one the command would drive.
//
// OnPath is whether that runtime's binary is on PATH. It only words the
// refusal: a runtime brig reaches through BRIG_RUNTIME_BIN or a profile's
// runtimeBin is not on PATH, so it cannot say the runtime is gone. When it is
// not on PATH the refusal also names brig rm --forget, the one way past for a
// session whose runtime is gone, which the reader asks for.
type OtherRuntimeError struct {
	Sandbox, Recorded, Current string
	OnPath                     bool
}

func (e *OtherRuntimeError) Error() string {
	msg := fmt.Sprintf("the sandbox %s runs on %s, not %s: set %s to use it or to remove it",
		e.Sandbox, e.Recorded, e.Current, runtimeSetting(e.Recorded))
	if !e.OnPath {
		msg += fmt.Sprintf(". brig finds no %s on PATH; if it is gone, brig rm --forget forgets the "+
			"session, touches no sandbox and keeps its guest home", e.Recorded)
	}
	return msg
}

// runtimeSetting is the setting that selects the runtime of kind. docker is
// not a value of BRIG_RUNTIME: it is the nerdctl runtime driving docker,
// which BRIG_RUNTIME_BIN selects.
func runtimeSetting(kind string) string {
	if kind == "docker" {
		return "BRIG_RUNTIME_BIN=docker"
	}
	return "BRIG_RUNTIME=" + kind
}

// RuntimeRefusal is runtimeRefusal for `brig rm`, which asks it before the
// runtime is asked whether the sandbox is there.
func (c *Config) RuntimeRefusal() error { return c.runtimeRefusal() }

// runtimeRefusal refuses a command on this session's sandbox when the session
// index records it on another runtime.
//
// The runtimes do not see each other's sandboxes, so to this one the sandbox
// is not there. A run would boot a second VM of the same name on the same
// guest home, and an rm would forget a session that is still running, so the
// next run of this runtime deletes its home as one no session owns. The
// refusal names the runtime that has it, which is the setting that reaches it.
//
// No runtime, as under brig plan with none installed, and no entry refuse
// nothing. An entry that records no runtime is read as entryRuntime says.
func (c *Config) runtimeRefusal() error {
	entry, ok := recordedEntry(c.VMName)
	// An entry that records no runtime on macOS is the reader's, whichever it
	// is, so the runtime is not asked its kind.
	if !ok || (entry.Runtime == "" && unrecordedRuntime() == "") {
		return nil
	}
	current := runtimeKind(c.Runtime)
	if current == "" {
		return nil
	}
	recorded := entryRuntime(entry, current)
	if recorded == current {
		return nil
	}
	onPath := runtime.OnPath(recorded)
	// An entry that records no runtime may be one the nerdctl runtime booted
	// through docker, which it drives when no nerdctl is on PATH. The
	// BRIG_RUNTIME=nerdctl the refusal names reaches it there, so a docker on
	// PATH is not a runtime gone, and the refusal does not point at --forget.
	// An entry that records nerdctl was booted by nerdctl itself, and docker
	// does not see it.
	if entry.Runtime == "" && !onPath {
		onPath = runtime.OnPath("docker")
	}
	return &OtherRuntimeError{Sandbox: c.VMName, Recorded: recorded, Current: current, OnPath: onPath}
}

// SandboxesOfOtherRuntimes names every sandbox the session index records on a
// runtime other than kind.
//
// `brig ls` counts them as present when it prunes. Its runtime cannot list
// them, and a prune would drop the entry of a session still running elsewhere,
// after which a run of it here deletes its home as one no session owns.
func SandboxesOfOtherRuntimes(kind string) []string {
	var out []string
	for _, s := range OtherRuntimeSessions(kind) {
		out = append(out, s.Sandbox)
	}
	return out
}

// OtherRuntimeSession is a session the index records on another runtime: its
// ref, "" for a session of a Brigfile target, whose key is no ref; its
// sandbox; its guest home; and the runtime it is recorded on.
type OtherRuntimeSession struct {
	Ref, Sandbox, Home, Runtime string
}

// OtherRuntimeSessions lists the sessions the index records on a runtime other
// than kind, in sandbox order. `brig ls` shows them: its runtime cannot list
// their sandboxes, and without a row a session whose runtime is gone is one
// the reader cannot find to forget. An entry that records no runtime is read
// as entryRuntime says, so hull on Linux keeps and shows one of nerdctl's.
func OtherRuntimeSessions(kind string) []OtherRuntimeSession {
	var out []OtherRuntimeSession
	for key, entry := range readSessionIndex() {
		on := entryRuntime(entry, kind)
		if on == kind {
			continue
		}
		ref := key
		if strings.HasPrefix(key, projectKeyPrefix) {
			ref = ""
		}
		out = append(out, OtherRuntimeSession{Ref: ref, Sandbox: entry.Sandbox, Home: entry.Home, Runtime: on})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sandbox < out[j].Sandbox })
	return out
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
	have := make(map[string]bool, len(live))
	for _, name := range live {
		have[name] = true
	}
	// The look at a sandbox's project is not pruned here, only by rm. run takes
	// it before the boot, and the runtime lists a sandbox only once it created
	// it, so an ls while a first run still pulls its image would drop the look
	// that run just took, and the next command would record whatever the guest
	// had planted by then with no warning. An entry left by a sandbox removed
	// outside brig costs at most a warning about changes made since.
	index := readSessionIndex()
	if len(index) == 0 {
		return
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
// under, the directory the sandbox was given as its home, the sandbox carrying
// it, and the project it was started on if it named one.
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
	key := c.sessionRef()
	entry := sessionEntry{
		Home:      c.Workspace,
		Sandbox:   c.VMName,
		Project:   c.Project,
		Ephemeral: c.EphemeralHome,
		Runtime:   runtimeKind(c.Runtime),
	}
	index := readSessionIndex()
	if index[key] == entry {
		return
	}
	index[key] = entry
	if err := writeSessionIndex(index); err != nil {
		c.warnf("%s", notice.Newf("could not record %s as the workspace of %s: %v", c.Workspace, c.VMName, err).
			Note("a later command that names no workspace falls back to the default one "+
				"and restarts the sandbox"))
	}
}

// SessionRefs returns the ref of every session the index holds, sorted.
//
// The index is keyed by ref, so this is a read of what is already there. It
// exists for completion, which needs the labels an agent has without asking
// the runtime: detecting a runtime and listing its sandboxes is hundreds of
// milliseconds on a keystroke, and returns nothing at all on a host that has
// no runtime installed. The cost of reading the index instead is bounded --
// a sandbox removed outside brig stays listed until the next `brig ls` prunes
// it or `brig rm <ref>` forgets it -- and a stale candidate is a name that
// fails when run, not a wrong action.
//
// The key of a session of a Brigfile target is not a ref. No verb takes it,
// so it is left out.
func SessionRefs() []string {
	index := readSessionIndex()
	refs := make([]string, 0, len(index))
	for ref := range index {
		if strings.HasPrefix(ref, projectKeyPrefix) {
			continue
		}
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs
}
