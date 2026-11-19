package wrap

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/memory"
	"github.com/brig-sh/brig/internal/notice"
	"github.com/brig-sh/brig/internal/runtime"
)

// Agent memory: which host-side store a sandbox attaches to, if any.
//
// A run asks for the store of its project folder or for a named store, from
// --agent-memory, BRIG_MEMORY or the profile's memory:, in that order, and
// --no-agent-memory beats all three. The key is decided here, on the host,
// from what the user typed: the guest never names a store.
//
// The key a sandbox was booted with is recorded. A run that finds the sandbox
// up and asks for another store, or turns memory on or off, is refused rather
// than restarted: a switch would change what the agent remembers mid-session,
// and a restart would end the session the user is attached to. The refusal
// names `brig rm`.

// memoryIndexName is the file, inside stateDir, recording the store each
// sandbox was booted with, by sandbox name.
//
// Its own file and not a field of sessionEntry: an older release rewrites the
// session index with the fields it knows, and the record would be lost the
// first time one ran. The network posture is kept apart for the same reason.
const memoryIndexName = "memory.json"

// memoryProject is the value that asks for the project's store rather than a
// named one. memory.ParseSetting reads it, and the on words, as this.
// It is also the mode recorded for a project's store; memoryNamed is the
// mode of a named one.
const (
	memoryProject = "project"
	memoryNamed   = "named"
)

// memoryRecord is what memory.json holds for one sandbox: the store key and
// how it was chosen. The mode is kept because a run that asks nothing
// inherits the store, and a project's store has to follow the project that
// run mounts, where a named store stays the store it names. The key alone
// cannot say which: a named store may look like any key but project-.
type memoryRecord struct {
	Key  string `json:"key"`
	Mode string `json:"mode"`
}

func readMemoryIndex() map[string]memoryRecord {
	return readIndex[memoryRecord](memoryIndexName)
}

// MemoryKey is the store this run attaches to, or "" when memory is off.
func (c *Config) MemoryKey() string { return c.memoryKey }

// MemoryLabel is the store as the user named it: "project (<folder>)" for a
// project's store, the name for a named one, or "" when memory is off. A
// project key is a hash, which tells a reader nothing.
func (c *Config) MemoryLabel() string { return c.memoryLabel }

// memoryTokenVar holds the memory token in the guest's environment. The MCP
// configuration names it, and Claude Code expands it when it reads the file,
// so the token is never written into the guest.
const memoryTokenVar = "BRIG_MEMORY_TOKEN"

// MemoryOfSandboxes is the store key each sandbox was booted with, by sandbox
// name. A sandbox with no entry was booted without memory.
func MemoryOfSandboxes() map[string]string {
	out := map[string]string{}
	for vm, r := range readMemoryIndex() {
		out[vm] = r.Key
	}
	return out
}

// MemoryUsers is the sandboxes that were booted with the store key and that
// the session index still names, sorted.
//
// The index is the check because memory.json is bookkeeping a sandbox removed
// outside brig leaves behind: a record no session names is a leftover, and
// counting it would keep `brig memory rm` from removing a store nothing uses.
func MemoryUsers(key string) []string {
	named := map[string]bool{}
	for _, e := range readSessionIndex() {
		named[e.Sandbox] = true
	}
	var out []string
	for vm, r := range readMemoryIndex() {
		if r.Key == key && named[vm] {
			out = append(out, vm)
		}
	}
	sort.Strings(out)
	return out
}

// resolveMemory decides the store this run asks for, and reads the one the
// sandbox of this name was booted with.
//
// A run that asks nothing -- no flag, no BRIG_MEMORY, no memory: in the
// profile -- keeps the store the sandbox has, as it keeps its network
// posture, so `brig sh claude` after `brig run --agent-memory claude .` is
// not read as "off" and refused. Only an explicit ask, off included, can
// differ from the record. See memoryChange.
//
// A value that cannot be used is kept as memoryRefused rather than failing
// Load: Load also serves stop, rm and info, and an exported BRIG_MEMORY=project
// must not stop `brig rm` of a session that has no project.
//
// Memory is for an agent brig points at its store (see memoryWired). Any
// other run would get a token, an isolated network and no memory, so a flag
// or a profile asking for it there is refused. BRIG_MEMORY is ignored there
// with a warning instead, because it is exported for every run in the shell.
func (c *Config) resolveMemory(o Options) {
	recorded := readMemoryIndex()[c.VMName]
	c.memoryRecorded, c.memoryRecordedMode = recorded.Key, recorded.Mode
	c.memoryUnread = c.unreadMemoryRecord(recorded)
	value, source := "", ""
	fromProfile := false
	switch {
	case o.NoMemory:
		c.memoryAsked = true
		return
	case o.Memory != "":
		value, source = o.Memory, "--agent-memory"
	default:
		// Trimmed, as the strict switches are: a value read from a file
		// carries its newline.
		v, ok := c.env.Get("MEMORY")
		v = strings.TrimSpace(v)
		switch {
		case ok && v != "" && c.memoryWired():
			value, source = v, c.env.SettingName("MEMORY")
		case ok && v != "":
			// An off value asks for no store, so there is nothing to ignore.
			if s, err := memory.ParseSetting(v); s != "" || err != nil {
				c.envWarnings = append(c.envWarnings, fmt.Sprintf("%s=%s is ignored for %s: brig points only "+
					"claude-code at an agent memory store", c.env.SettingName("MEMORY"), v, c.memoryAgent()))
			}
		}
		if value == "" && c.Profile.Memory != "" {
			value, source, fromProfile = c.Profile.Memory, "the profile's memory:", true
		}
	}
	if value == "" {
		c.inheritMemory()
		return
	}
	c.memoryAsked = true
	// Every source reads a switch word the same way: off here beats a lower
	// source, which is what setting it is for, and on is the project's
	// store, never a store called on.
	setting, err := memory.ParseSetting(value)
	if err != nil {
		c.memoryRefused = fmt.Errorf("%s %q is not off, on, \"project\" or a store name: %w", source, value, err)
		return
	}
	if setting == "" {
		return
	}
	// A repository's profile does not choose a store: it could attach the
	// sandbox to a store the user's other projects write, by a name the user
	// never typed.
	if fromProfile && c.target != nil && !c.target.BuiltIn {
		c.memoryRefused = fmt.Errorf("%s, from this repository's Brigfile, sets memory: %s; a repository "+
			"cannot turn agent memory on or choose its store. Remove memory: from the Brigfile, and pass "+
			"--agent-memory yourself on a target that runs builtin:claude-code", c.memoryAgent(), value)
		return
	}
	if !c.memoryWired() {
		c.memoryRefused = fmt.Errorf("%s asks for agent memory, which brig serves only to claude-code, "+
			"so %s would get none; run claude-code, or run without it", source, c.memoryAgent())
		return
	}
	c.memorySource = source
	if fromProfile {
		c.memorySource = "the profile " + c.Profile.Name
	}
	switch {
	case setting == memoryProject:
		// The project this run mounts, which is the remembered one when the
		// line names none. Its path is already absolute and cleaned by
		// ProjectKey, so dir and dir/ are one store.
		if c.Project == "" {
			c.memoryRefused = errors.New("--agent-memory needs a project folder, or name a store with " +
				"--agent-memory=<name>")
			return
		}
		c.useProjectMemory()
	default:
		c.memoryKey, c.memoryLabel, c.memoryMode = setting, setting, memoryNamed
	}
}

// memoryWired reports whether brig points this run's agent at its store,
// by the rule agentArgs in cmd/brig adds --mcp-config under: the claude-code
// profile, or a target whose Brigfile runs the claude-code brig ships. A
// repository can call its own profile claude-code, so a target's name says
// nothing.
func (c *Config) memoryWired() bool {
	if c.target != nil {
		return c.target.ProfileSource == "builtin:claude-code"
	}
	return c.Profile.Name == "claude-code"
}

// memoryAgent names the agent of this run in a memory message: the profile,
// and for a target where the Brigfile took it from.
func (c *Config) memoryAgent() string {
	if c.target != nil {
		return "the profile " + c.Profile.Name + " (" + c.target.ProfileSource + ")"
	}
	return "the profile " + c.Profile.Name
}

// useProjectMemory attaches this run to the store of the project it mounts.
func (c *Config) useProjectMemory() {
	c.memoryKey = memory.ProjectKey(c.Project)
	c.memoryLabel = memoryProject + " (" + c.Project + ")"
	c.memoryMode = memoryProject
}

// unreadMemoryRecord is why r, the record of this sandbox, cannot be read, or
// nil when it can or there is none. A valid record is a project key in
// project mode, or a store name or an older record's switch word in named
// mode. Anything else came from a bug or an unfinished write, and is refused
// rather than guessed: a project key read as a named store would carry the
// last project's memory into this one.
func (c *Config) unreadMemoryRecord(r memoryRecord) error {
	if r == (memoryRecord{}) {
		return nil
	}
	ok := false
	switch r.Mode {
	case memoryProject:
		rest, isProject := strings.CutPrefix(r.Key, memoryProject+"-")
		ok = isProject && len(rest) == 16 && strings.Trim(rest, "0123456789abcdef") == ""
	case memoryNamed:
		_, err := memory.ParseSetting(r.Key)
		ok = err == nil
	}
	if ok {
		return nil
	}
	return fmt.Errorf("brig cannot read the record of the agent memory %s was started with (key %q, mode %q); "+
		"remove it with \"brig rm %s\" and run again", c.VMName, r.Key, r.Mode, sessionKey(c.Profile.Name, c.Slug))
}

// inheritMemory takes the store the sandbox of this session was booted with,
// for a run that asked for none. Only when the session index names this
// sandbox, as rememberedNetwork requires: a record left by a sandbox removed
// outside brig must not hand a new one its store.
//
// A named store is inherited by its key. A project's store is inherited as
// the mode: the key is resolved again from the project this run mounts, so a
// run on another project gets that project's store. A different project means
// a different share, which recreates the sandbox and records the new key at
// its boot (see memoryChange). A run with no project is refused as an
// explicit ask would be. A record that cannot be read is refused.
func (c *Config) inheritMemory() {
	if c.memoryRecorded == "" || readSessionIndex()[sessionKey(c.Profile.Name, c.Slug)].Sandbox != c.VMName {
		return
	}
	if c.memoryUnread != nil {
		c.memoryRefused = c.memoryUnread
		return
	}
	c.memorySource = "kept from the boot of " + c.VMName
	if c.memoryRecordedMode != memoryProject {
		// A record of a switch word, written before the words were
		// reserved, means what the word means, as it would from any
		// other source.
		switch setting, _ := memory.ParseSetting(c.memoryRecorded); setting {
		case "":
			c.memorySource = ""
			return
		case memoryProject:
		default:
			c.memoryKey, c.memoryLabel, c.memoryMode = c.memoryRecorded, c.memoryRecorded, memoryNamed
			return
		}
	}
	if c.Project == "" {
		c.memoryRefused = errors.New("--agent-memory needs a project folder, or name a store with " +
			"--agent-memory=<name>")
		return
	}
	c.useProjectMemory()
}

// narrowForMemory moves a run with memory onto the isolated posture, as a
// run with a route is moved: the guest reaches its store on the broker's own
// address, which an isolated sandbox is allowed and nothing else. An offline
// run keeps its posture and is refused in memoryRefusal. The posture it
// narrowed is kept, so the network line can say memory did it.
func (c *Config) narrowForMemory() {
	if c.memoryKey != "" && c.Network != NetOffline && c.Network != NetIsolated {
		c.memoryNarrowed = c.Network
		c.Network = NetIsolated
	}
}

// memoryRefusal is why this run cannot have the memory it asked for, or nil.
// Read-only, because Plan calls it through sessionRefusal.
//
// The guest reaches its store at brig.internal, so memory needs a run path
// that routes that name to the broker: the same check a route in mode
// endpoint gets. Its wording is not reused, because it is about a policy and
// this run may have none.
func (c *Config) memoryRefusal() error {
	if c.memoryRefused != nil {
		return c.memoryRefused
	}
	if c.memoryKey == "" || c.Runtime == nil {
		return nil
	}
	return c.brigInternalRefusal(fmt.Sprintf("agent memory (%s)", c.memoryLabel), "Agent memory runs",
		"with --no-agent-memory")
}

// memoryChange refuses a run that asks a running sandbox for a store other
// than the one it was booted with, memory on or off included. A sandbox with
// no record was booted without memory. A run that asked nothing has the
// recorded store already, or its project's store when the project moved.
// See resolveMemory and inheritMemory.
func (c *Config) memoryChange() error {
	if c.memoryUnread != nil {
		return c.memoryUnread
	}
	if c.memoryRecorded == c.memoryKey {
		return nil
	}
	// A project's store, asked for or inherited, differs from the record
	// only because the project did, and the share that moved with it
	// recreates the sandbox below, whose boot records the new key. Without
	// that recreate the running guest would be served a store it was not
	// booted with. Another mode, or a named store, is another store.
	if c.memoryRecordedMode == memoryProject && c.memoryMode == memoryProject && c.projectShareStale() != "" {
		return nil
	}
	now := c.memoryLabel
	if now == "" {
		now = "off"
	}
	return fmt.Errorf("%s was created with memory %s; this run asks for %s. Remove it with \"brig rm %s\" "+
		"and run again", c.VMName, c.recordedMemoryLabel(), now, sessionKey(c.Profile.Name, c.Slug))
}

// unrecordedMemory refuses a run on a running sandbox that the broker serves
// memory to and that has no record of its store. memory.json is rewritten
// without a lock and reads as empty when it does not parse, so a record can
// be lost. Read as memory off, the exec would revoke the memory token of
// another exec's agent; read as any store, it would be a guess.
func (c *Config) unrecordedMemory() error {
	if c.memoryRecorded != "" {
		return nil
	}
	st, ok := c.routeBroker().Status()
	if !ok || !slices.Contains(st.Memory, c.VMName) {
		return nil
	}
	return fmt.Errorf("the credential broker serves agent memory to %s, and brig has no record of its "+
		"store; remove it with \"brig rm %s\" and run again", c.VMName, sessionKey(c.Profile.Name, c.Slug))
}

// recordedMemoryLabel is the store the sandbox was booted with, named as
// MemoryLabel names one. The record holds the key, so a project store is
// named by the project the session last ran with when that is its folder,
// and by its key otherwise.
func (c *Config) recordedMemoryLabel() string {
	key := c.memoryRecorded
	if key == "" {
		return "off"
	}
	if project := rememberedProject(sessionKey(c.Profile.Name, c.Slug), c.VMName); project != "" &&
		memory.ProjectKey(project) == key {
		return memoryProject + " (" + project + ")"
	}
	return key
}

// recordMemory records the store this boot attached to, or drops the record
// a previous sandbox of this name left when this one has none.
//
// A failed write is a warning, as in recordPosture: the sandbox is up. The
// next run reads the sandbox as booted with whatever the file still says.
func (c *Config) recordMemory() {
	index := readMemoryIndex()
	recorded, ok := index[c.VMName]
	want := memoryRecord{Key: c.memoryKey, Mode: c.memoryMode}
	if (c.memoryKey == "" && !ok) || (c.memoryKey != "" && recorded == want) {
		c.memoryRecorded, c.memoryRecordedMode = c.memoryKey, c.memoryMode
		return
	}
	if c.memoryKey == "" {
		delete(index, c.VMName)
	} else {
		index[c.VMName] = want
	}
	if err := writeIndex(memoryIndexName, index); err != nil {
		c.warnf("%s", notice.Newf("could not record the memory %s was started with: %v", c.VMName, err).
			Note("a later run may refuse to reuse it until it is removed with \"brig rm %s\"",
				sessionKey(c.Profile.Name, c.Slug)))
		return
	}
	c.memoryRecorded, c.memoryRecordedMode = c.memoryKey, c.memoryMode
}

// forgetMemory drops the record of a removed sandbox, so the next sandbox to
// take the name is not read as booted with its store. Dropped like the rest
// of ForgetSandbox's bookkeeping when the write fails.
func forgetMemory(vmName string) {
	index := readMemoryIndex()
	if _, ok := index[vmName]; !ok {
		return
	}
	delete(index, vmName)
	_ = writeIndex(memoryIndexName, index)
}

// serveMemory registers this sandbox's memory with the broker, mints a memory
// token for this exec, and records what the guest is handed. Called on every
// exec, after serveRoutes: a broker that started again since the last exec
// has lost the registration, and the same registration again keeps the token
// of an agent already running valid.
//
// The routes and the memory of a sandbox share its divert socket and the
// broker's CA, so each is checked against what the gateway was handed and
// what serveRoutes took. See acceptBrigInternal.
func (c *Config) serveMemory() error {
	c.memoryVars = nil
	if c.memoryKey == "" {
		return nil
	}
	addr, err := c.guestBrigInternal()
	if err != nil {
		return err
	}
	b := c.routeBroker()
	reg := injector.MemorySandbox{ID: c.VMName, Session: c.RawName, Key: c.memoryKey, BrigInternal: addr}
	// The broker records a project store's folder, so `brig memory ls` can
	// name it: its key is a hash.
	if c.memoryMode == memoryProject {
		reg.Project = c.Project
	}
	added, err := b.RegisterMemory(reg)
	if err != nil {
		return fmt.Errorf("could not register the memory of %s with the credential broker: %w", c.VMName, err)
	}
	if err := c.acceptBrigInternal("the memory", "its routes", added); err != nil {
		return err
	}
	token, err := b.MintMemory(injector.MemoryMintRequest{
		Sandbox:    c.VMName,
		Session:    c.RawName,
		TTLSeconds: int64(injector.TokenTTL / time.Second),
	})
	if err != nil {
		return fmt.Errorf("could not mint a memory token for %s: %w", c.VMName, err)
	}
	// Secret, so the token never goes on a command line. Claude Code's own
	// memory, a directory in the guest home, is turned off so the agent uses
	// the store alone.
	c.memoryVars = []runtime.Var{
		{Name: memoryTokenVar, Value: token, Secret: true},
		{Name: "CLAUDE_CODE_DISABLE_AUTO_MEMORY", Value: "1"},
	}
	// A run with an endpoint route has the CA variables from its routes.
	if len(c.endpointRoutes()) == 0 {
		c.memoryVars = append(c.memoryVars, injector.CAVars()...)
	}
	return nil
}
