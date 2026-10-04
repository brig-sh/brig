// Package telemetry sends brig's own usage events and crash reports.
//
// The client is hull's, and so is the state. The consent answer, the install
// id and the queues live where hull keeps them for its default store, so one
// answer covers both tools and their events join on one install id. brig counts each of its
// commands once and reports its own panics. hull, driven by brig, sends what
// only it can see: the boot, the VM's lifetime and its resource use. The hull
// calls brig makes read Counting and Version to tell hull which of those to
// send; see internal/runtime's telemetryEnv.
//
// docs/telemetry.md lists every field. A field that is not listed there must
// not be sent from here.
package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	hull "github.com/brig-sh/hull/pkg/telemetry"

	"github.com/brig-sh/brig/internal/buildinfo"
)

// Product is what brig's events, and the hull events brig counts, are
// attributed to.
const Product = "brig"

// docsURL is the page the consent prompt links to.
const docsURL = "https://github.com/brig-sh/brig/blob/main/docs/telemetry.md"

// Custom is the agent name sent for a profile brig does not ship.
const Custom = "custom"

// agentSalt seeds the hash a custom profile's name is sent as. It is the same
// on every install, so one name hashes the same everywhere and can be counted.
const agentSalt = "brig-agent-v1"

// agentHashLen is how many hex digits of that hash are sent.
const agentHashLen = 16

// state is this invocation's client and the fields its command event carries.
// One brig process is one command, so it is package state.
var state struct {
	sync.Mutex
	client  *hull.Client
	version string
	fields  map[string]string
	sent    bool
	// queued is the outbox file Handover wrote, until the exec replaces
	// the process.
	queued string
}

// uploadTimeout bounds how long Upload keeps sending.
const uploadTimeout = 30 * time.Second

// SpawnUploader starts the upload of the queues in a process that outlives
// this one. main sets it; while it is nil, as under test, nothing is started.
//
// A command never uploads in its own process. An upload takes longer than a
// short command lives, so the process would end in the middle of it and leave
// the event claimed, and so unsent, for minutes.
var SpawnUploader func()

// homeOwner returns the owner of a directory, as a variable so a test can
// answer for a home that belongs to someone else.
var homeOwner = func(dir string) (uid int, ok bool) {
	fi, err := os.Stat(dir)
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// StateDir returns where the answer, the install id and the queues live:
// hull's state directory, ~/.hull. It returns "" when there is no home
// directory, or when the home belongs to another user -- sudo keeping the
// caller's HOME -- so brig never leaves files there that the user then
// cannot write.
func StateDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	if uid, ok := homeOwner(home); ok && uid != os.Geteuid() {
		return ""
	}
	return hull.DefaultStateDir()
}

// Start reads the answer on file for command, the verb this invocation counts
// as. When interactive is true and nobody has answered yet, it asks. An
// unattended invocation on an install nobody has asked sends nothing.
//
// brigd starts with no command. It sends no command event, and the sessions it
// boots report through hull as the CLI's do.
func Start(command string, interactive bool) {
	dir := StateDir()
	if dir == "" {
		return
	}
	state.Lock()
	defer state.Unlock()
	state.version = Version()
	state.client = hull.Init(hull.Config{
		StoreDir:    dir,
		Version:     state.version,
		OSVersion:   hull.HostOS(),
		Uname:       hull.HostUname(),
		Interactive: interactive,
		Product:     Product,
		DocsURL:     docsURL,
		AskFirst:    true,
		Stdin:       os.Stdin,
		Stderr:      os.Stderr,
	})
	state.fields = map[string]string{"command": command}
	state.sent = false
	state.queued = ""
}

// Version returns brig's version as events report it: the module version
// without its leading v, the spelling hull uses.
func Version() string {
	return strings.TrimPrefix(buildinfo.Read().Version, "v")
}

// Counting reports whether this invocation's events go out.
func Counting() bool {
	state.Lock()
	defer state.Unlock()
	return state.client.Enabled()
}

// SetAgent records the agent profile the command runs. builtIn is whether brig
// ships a profile of that name. Any other name is sent as Custom, with a
// salted hash of the name in agent_hash.
func SetAgent(name string, builtIn bool) {
	agent, hash := AgentFields(name, builtIn)
	set("agent", agent)
	set("agent_hash", hash)
}

// AgentFields returns the agent and agent_hash fields for a profile name. A
// profile brig ships is sent by name and has no hash.
func AgentFields(name string, builtIn bool) (agent, hash string) {
	if builtIn {
		return name, ""
	}
	sum := sha256.Sum256([]byte(agentSalt + "|" + name))
	return Custom, hex.EncodeToString(sum[:])[:agentHashLen]
}

// SetRuntime records the runtime brig drives: hull, nerdctl or docker.
func SetRuntime(kind string) {
	set("runtime", kind)
}

// set records one field of the command event. An empty value removes it.
func set(key, value string) {
	state.Lock()
	defer state.Unlock()
	if state.fields == nil {
		return
	}
	if value == "" {
		delete(state.fields, key)
		return
	}
	state.fields[key] = value
}

// Finish queues the command event, once, and starts the upload. outcome is
// "ok" or "error", and class is the error class on failure. It waits for
// nothing.
func Finish(outcome, class string) {
	c, fields := commandEvent(outcome, class)
	if c == nil {
		return
	}
	if c.Queue("command", fields) != "" {
		spawnUploader()
	}
}

// Handover queues the command event of a run that hands its process to the
// agent, and starts the upload, before the exec. The exit path's Finish then
// does nothing.
func Handover() {
	c, fields := commandEvent("ok", "")
	if c == nil {
		return
	}
	path := c.Queue("command", fields)
	state.Lock()
	state.queued = path
	state.Unlock()
	if path != "" {
		spawnUploader()
	}
}

// spawnUploader calls SpawnUploader when main has set it.
func spawnUploader() {
	if SpawnUploader != nil {
		SpawnUploader()
	}
}

// Upload sends what the queues hold. It returns when they are empty, when a
// round of uploads sends nothing, or after about uploadTimeout. It is the
// work of the process SpawnUploader starts. One Upload runs at a time for a
// state directory: another returns at once, and the one running sends what
// is queued while it runs.
func Upload() {
	Start("", false)
	state.Lock()
	c := state.client
	state.Unlock()
	if !c.Enabled() {
		return
	}
	dir := StateDir()
	lock, err := os.OpenFile(filepath.Join(dir, uploadLock), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = lock.Close() }()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return
	}
	deadline := time.Now().Add(uploadTimeout)
	for time.Now().Before(deadline) {
		before := queuedFiles(dir)
		if before == 0 {
			return
		}
		c.UploadPending()
		if queuedFiles(dir) >= before {
			return
		}
	}
}

// uploadLock is the file in the state directory whose lock an Upload holds.
const uploadLock = "telemetry-upload.lock"

// queuedFiles counts the events waiting in the queues of dir. A file an
// upload has claimed is not waiting.
func queuedFiles(dir string) int {
	n := 0
	for _, q := range []string{"crashes", "outbox"} {
		files, _ := filepath.Glob(filepath.Join(dir, q, "*.json"))
		n += len(files)
	}
	return n
}

// HandoverFailed takes back the event Handover queued, for an exec that
// returned instead of replacing the process. The exit path's Finish then
// sends the failure. An event an upload already claimed stays as it is, so
// the command is never counted twice.
func HandoverFailed() {
	state.Lock()
	defer state.Unlock()
	if state.queued == "" {
		return
	}
	if os.Remove(state.queued) == nil {
		state.sent = false
	}
	state.queued = ""
}

// commandEvent returns the client and the command event's fields, the first
// time it is asked, and nil afterwards or when there is nothing to send.
func commandEvent(outcome, class string) (*hull.Client, map[string]string) {
	state.Lock()
	defer state.Unlock()
	if state.client == nil || state.sent || state.fields["command"] == "" {
		return nil, nil
	}
	state.sent = true
	fields := map[string]string{"outcome": outcome}
	for k, v := range state.fields {
		fields[k] = v
	}
	if class != "" {
		fields["error_class"] = class
	}
	return state.client, fields
}

// CapturePanic queues a crash report for a panic in brig, the panic's type and
// its scrubbed stack, never its message, and starts the upload.
func CapturePanic(recovered any, stack []byte) {
	state.Lock()
	c, command := state.client, state.fields["command"]
	state.Unlock()
	if !c.Sends("crash") {
		return
	}
	c.CapturePanic(recovered, stack, command, "")
	spawnUploader()
}

// Answer is the consent answer in force, in brig's words.
type Answer string

const (
	// On means someone answered yes, and events go out.
	On Answer = "on"
	// Off means a recorded no, or a variable in this shell.
	Off Answer = "off"
	// Unanswered means no answer covers what brig sends today: nobody has
	// answered, or the answer was to an older, shorter list. Nothing goes
	// out until someone answers.
	Unanswered Answer = "unanswered"
)

// errNoHome is what Status and Set return without a home directory.
var errNoHome = errors.New("cannot find the telemetry settings: there is no home directory")

// errOtherHome is what Status and Set return when the home directory belongs
// to another user, which is what sudo keeping the caller's HOME looks like.
var errOtherHome = errors.New("cannot use the telemetry settings: the home directory belongs to another user. Run brig without sudo")

// settingsDir returns StateDir, or the error that says why there is none.
func settingsDir() (string, error) {
	if dir := StateDir(); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errNoHome
	}
	return "", errOtherHome
}

// Status returns the answer in force. When a variable in the environment
// decided it, setting names it, as NAME=1.
func Status() (answer Answer, setting string, err error) {
	dir, err := settingsDir()
	if err != nil {
		return Off, "", err
	}
	a, setting := hull.Effective(dir)
	switch a {
	case hull.On:
		return On, "", nil
	case hull.Off:
		return Off, setting, nil
	default:
		return Unanswered, "", nil
	}
}

// InstallID returns the install id on file, or "" when there is none yet. It
// only reads, as Status does. The id is in hull's state file, or, before a
// run of a hull or brig that moved the state, in the file of the default
// store, which that run copies it from.
func InstallID() string {
	dir := StateDir()
	if dir == "" {
		return ""
	}
	for _, path := range []string{
		filepath.Join(dir, "telemetry.json"),
		filepath.Join(dir, "store", "telemetry.json"),
	} {
		var st struct {
			InstallID string `json:"install_id"`
		}
		if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &st) == nil && st.InstallID != "" {
			return st.InstallID
		}
	}
	return ""
}

// Set records an answer for every later brig and hull invocation.
func Set(on bool) error {
	dir, err := settingsDir()
	if err != nil {
		return err
	}
	return hull.SetConsent(dir, on)
}
