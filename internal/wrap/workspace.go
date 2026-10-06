package wrap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/brig-sh/brig/internal/notice"
)

// markerFile identifies the workspace from inside the guest. See Marker.
const markerFile = ".brig-workspace"

// warnf says something the reader has to act on: an expired credential, an
// image that did not verify, a share that went stale.
//
// It stays in the default output, because a warning is an action. Only -q
// drops it, and -q is a script asking for identifiers and errors and
// nothing between the two.
func (c *Config) warnf(format string, a ...any) {
	if c.Verbosity < Normal {
		return
	}
	c.notices().Say(fmt.Sprintf(format, a...))
}

// alertf says something about verification that has to reach the reader
// whatever level was asked for.
//
// It outranks warnf, and the rank is the whole point. -q hides warnings because
// a script asked for identifiers and errors, and that is a fair trade for an
// expired credential or a stale share: the run still works, the fact keeps, and
// the next interactive run says it again. It is not a fair trade for "nothing
// checked the image this sandbox boots". Verification is the claim brig exists
// to make, and a run that skipped the check, could not make it, or made it and
// did not like the answer has to say so even to a caller that asked for
// silence -- because the caller asking for silence is exactly the unattended
// one where an unchecked image matters most.
//
// Only verification belongs here, and that scoping is not fussiness. A level
// that means "important" collects everything within a release, and then -q is
// back to printing what it was added to suppress. Everything else brig warns
// about stays a warning: the credential expiry, the workspace notices, the
// deprecations.
func (c *Config) alertf(format string, a ...any) {
	c.notices().Say(fmt.Sprintf(format, a...))
}

// sayf writes one line of the report `brig info` prints.
//
// Deliberately not levelled, unlike the two below it. This is the answer to the
// command rather than narration about one: somebody typed the verb whose whole
// output this is, and a report that went quiet because it was not asked for
// twice would be a command that does nothing.
func (c *Config) sayf(format string, a ...any) {
	fmt.Fprintf(c.Out, "brig: "+format+"\n", a...)
}

// progressf narrates what the run is doing. See Config.Progress for why this
// is not warnf, and Verbosity for why it waits: a line saying a boot has
// started is not something anybody acts on, so it is printed for the reader who
// asked for detail and to nobody else.
//
// A nil Progress is silence rather than a panic, unlike Out and Err: losing a
// line of narration costs the reader nothing, and every test that builds a
// Config by hand would otherwise have to declare where its progress goes to
// ask a question about something else.
func (c *Config) progressf(format string, a ...any) {
	if c.Progress == nil || c.Verbosity < Verbose {
		return
	}
	fmt.Fprintf(c.Progress, "brig: "+format+"\n", a...)
}

// runtimeOutput is where the runtime's own output goes: the writer a run
// narrates to when --verbose asked for it, and nil otherwise.
//
// nil is not "throw it away". It is what tells the runtime adapter to hold the
// output and quote it back if the boot fails, which is the one situation a
// reader who wanted none of the detail still needs all of it. See
// runtime.RunSpec.Progress.
func (c *Config) runtimeOutput() io.Writer {
	if c.Verbosity < Verbose {
		return nil
	}
	return c.Progress
}

// runtimeNotice is where a long operation says, in one line, that it has
// started and that it is done.
//
// It sits at the default level rather than behind --verbose: a first run
// downloads a kernel, and a minute of silence with nothing on screen reads as a
// hang. The stream behind that line is
// runtimeOutput's, and it stays behind --verbose.
//
// On a terminal at the default level the operation spins instead of printing
// a line each end; canSpin says which terminals. Under --verbose the runtime's
// stream follows the notice, and a line redrawn in place would break it up. A
// caller with no terminal of its own, such as brigd, gets the lines.
func (c *Config) runtimeNotice() io.Writer {
	if c.Verbosity < Normal {
		return nil
	}
	if f, ok := c.Progress.(*os.File); ok && c.Verbosity < Verbose && !c.NoTerminal && canSpin(f) {
		return newSpinner(f)
	}
	return c.Progress
}

// Marker identifies this workspace by path and inode.
//
// A share is bound at boot and cannot be changed on a live VM, so a
// long-lived sandbox can end up mounting something other than the workspace:
// point BRIG_WORKSPACE somewhere new while the VM is up, or rename or replace
// the directory underneath it, and the guest keeps the old binding -- which,
// once the original path is gone, surfaces as an EMPTY guest home. Everything
// then looks broken in a way that points nowhere near the mount: the agent
// asks for a login and a theme as though freshly installed.
//
// Path alone is not enough to detect it, because a renamed and recreated
// directory reuses the path with a different inode, so the marker carries
// both.
func Marker(dir string) (string, error) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return real + ":0", nil
	}
	return fmt.Sprintf("%s:%d", real, sys.Ino), nil
}

// PrepareWorkspace makes the host side of the sandbox ready. It runs whether
// or not a VM has to be started: the workspace lives on the host, and the
// marker written here is what the stale-share check reads back.
//
// Everything below writes into a directory the sandbox has had read-write for
// however long it has been running, so the whole of it goes through one
// [workspaceRoot] rather than through paths joined onto c.Workspace. See
// rootio.go for what that buys.
func (c *Config) PrepareWorkspace() error {
	if strings.ContainsAny(c.Workspace, " \t") {
		return fmt.Errorf("workspace path must not contain spaces: %s", c.Workspace)
	}
	r, err := c.openWorkspace()
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()

	if err := c.seedHostConfig(r); err != nil {
		return err
	}
	if err := c.migrateClaudeState(r); err != nil {
		return err
	}
	if err := c.seedOnboarding(r); err != nil {
		return err
	}
	if err := c.trustGuestCwd(r); err != nil {
		return err
	}
	c.warnStaleCredentials(r)
	marker, err := Marker(c.Workspace)
	if err != nil {
		return err
	}
	return r.writeIfChanged(markerFile, marker+"\n", 0o644)
}

// seedOnboarding makes a fresh workspace usable without an interactive login,
// WITHOUT putting a credential on disk.
//
// A forwarded token authenticates the agent on its own. What a fresh
// workspace still stops for is first-run ONBOARDING, which is a separate
// thing from authentication and unskippable in the guest, since choosing a
// login method there opens a browser the guest does not have. A couple of
// non-secret flags in the agent's own state file settle it.
//
// The file is created only when it is absent. An existing one belongs to the
// agent: the single key brig changes in a file it did not create is the
// per-directory trust key, in trustGuestCwd below.
//
// A symlink at that path is neither: the agent did not put it there, and
// stat-ing through it would have brig create the seed at whatever host path
// the sandbox aimed the link at. That is refused rather than skipped, so the
// attempt is visible.
func (c *Config) seedOnboarding(r *workspaceRoot) error {
	ob := c.Profile.Onboarding
	if ob == nil || ob.File == "" {
		return nil
	}
	// A state file inside a directory needs the directory first, made through
	// the same walk the volume targets take so a symlink on the way is refused.
	if dir := slashDir(slashClean(ob.File)); dir != "." {
		if err := ensureTarget(r, dir, false); err != nil {
			return err
		}
	}
	seeded, err := r.exists("seed", ob.File)
	if err != nil {
		return err
	}
	if seeded {
		return nil
	}
	blob, err := json.Marshal(ob.Seed)
	if err != nil {
		return err
	}
	return r.writeFile(ob.File, append(blob, '\n'), 0o600)
}

// Where Claude Code kept its global state before and after it was pointed at
// its config directory with CLAUDE_CONFIG_DIR.
const (
	legacyClaudeState = ".claude.json"
	claudeState       = ".claude/.claude.json"
)

// migrateClaudeState moves a workspace's .claude.json into .claude, once.
//
// With CLAUDE_CONFIG_DIR set the agent reads its global state from inside its
// config directory, so an existing workspace would otherwise start over:
// onboarding, trust and per-project settings all live in that file. Only for
// a profile whose onboarding file is the new path, only when the old file is
// there and the new one is not, and through the root, so a symlink at either
// end or at .claude is refused rather than followed.
//
// Only in a home brig prepared before, which carries its marker. The file in
// any other directory belongs to whoever put it there. Never in the user's
// own home either: there .claude.json is the host agent's state, and moving
// it would reset the host agent.
func (c *Config) migrateClaudeState(r *workspaceRoot) error {
	ob := c.Profile.Onboarding
	if ob == nil || ob.File != claudeState || isUserHome(c.Workspace) {
		return nil
	}
	old, err := r.exists("move", legacyClaudeState)
	if err != nil || !old {
		return err
	}
	prepared, err := r.exists("move", markerFile)
	if err != nil {
		return err
	}
	if !prepared {
		// Said once: the seed below makes the new file, and from then on
		// the agent reads that one.
		if fresh, err := r.exists("move", claudeState); err == nil && !fresh {
			c.warnf("%s", notice.New(r.path(legacyClaudeState)+" stays where it is").
				Note("brig did not prepare this home, so it moves nothing in it; %s now reads its state from %s",
					c.Profile.Binary, claudeState).
				Do("to keep using it", "mv "+r.path(legacyClaudeState)+" "+r.path(claudeState)))
		}
		return nil
	}
	if err := ensureTarget(r, slashDir(claudeState), false); err != nil {
		return err
	}
	moved, err := r.exists("move", claudeState)
	if err != nil || moved {
		return err
	}
	return r.rename(legacyClaudeState, claudeState)
}

// isUserHome returns whether dir is the user's home directory. It returns
// true when either cannot be read, so a caller about to change files there
// leaves them alone.
func isUserHome(dir string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return true
	}
	a, errA := os.Stat(home)
	b, errB := os.Stat(dir)
	if errA != nil || errB != nil {
		return true
	}
	return os.SameFile(a, b)
}

// warnStaleCredentials points out a credential file in the workspace. brig
// never writes to a host path: a credential it delivers as a file goes into a
// tmpfs the profile declares, which the workspace cannot see. So such a file
// is a real token sitting on disk. An older wrapper may have left it, which
// is how adopting a Homebrew-era workspace gets here. Or a process in the
// sandbox wrote it outside the tmpfs: with claude-code's tmpfs at
// /brig/claude, a claude started without CLAUDE_CONFIG_DIR writes it to the
// home share. Say so rather than deleting somebody's file.
//
// The lstat is against the WORKSPACE, not against the guest. brig writes the
// credential to mountTarget, under the tmpfs, so nothing brig writes reaches
// the directory this reads.
func (c *Config) warnStaleCredentials(r *workspaceRoot) {
	for _, rel := range c.Profile.StaleCredentialFiles {
		rel = filepath.FromSlash(rel)
		path := r.path(rel)
		if _, err := r.lstat(rel); err == nil {
			c.warnf("%s", notice.New(path+" holds a token on disk").
				Note("brig hands credentials to the sandbox in memory and never writes one there, "+
					"so an older wrapper left it or a process in the sandbox wrote it outside brig's tmpfs").
				Do("to delete it", "rm "+path))
		}
	}
}

// TrustKey is the guest directory the agent records trust against.
//
// Not simply the directory the agent starts in: it resolves the key to the
// git repository root when the working directory sits inside one. Keying on
// the cwd therefore writes a key that is never read back -- started in
// myrepo/sub the agent asks for the entry under myrepo -- and the dialog
// stays up.
//
// Only a repository the guest can see counts. The guest has nothing but the
// workspace mounted as its home, so a .git above the workspace is invisible
// in there and the guest keys on the directory itself. The walk stops at the
// workspace for exactly that reason.
func TrustKey(cwd, workspace, guestHome string) string {
	guestCwd := GuestCwd(cwd, workspace, guestHome)
	if guestCwd == guestHome && !under(cwd, workspace) {
		return guestCwd
	}
	dir := cwd
	for {
		// A worktree or submodule checkout has .git as a file, not a
		// directory, so any kind of entry counts.
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return GuestCwd(dir, workspace, guestHome)
		}
		if dir == workspace {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return guestCwd
}

// trustKey is the directory this run has to pre-trust: the guest path the
// agent will actually be in.
//
// With a project mounted that is the project, not anything under the home.
// Otherwise the agent starts in /work/<basename> and asks whether you trust a
// directory brig just handed it, which is the one thing this seeding exists to
// prevent.
//
// The project is passed as its own workspace, because to the guest that is what
// it is: /work/<basename> is the mount, and nothing above it is visible in
// there. So the repository walk has no room to move -- the same reason the
// home's walk stops at the workspace -- and the key is the project's guest
// path whether or not it is a git repository. Routed through TrustKey anyway,
// so one function decides what a trust key is.
func (c *Config) trustKey() string {
	if c.Project != "" {
		return TrustKey(c.Project, c.Project, c.GuestProject)
	}
	return TrustKey(c.Cwd, c.Workspace, c.Profile.GuestHome)
}

func under(cwd, workspace string) bool {
	rel, err := filepath.Rel(workspace, cwd)
	if err != nil {
		return false
	}
	return rel == "." || !strings.HasPrefix(rel, "..")
}

// trustGuestCwd trusts the directory this run starts in, so the agent gets to
// work instead of asking whether you trust the files in the folder.
//
// Trust lives per directory. The top-level flag seeded above answers a
// different question, which is why seeding that alone still left the dialog.
//
// Trust stays on by default. A host would want the opposite, but the guest
// sees only the workspace, so choosing to mount it already answers the
// dialog's question. BRIG_TRUST_WORKSPACE=0 keeps the dialog.
//
// This writes one key, only when it needs to, through a temporary file at the
// workspace root renamed over the state file, so a partial write leaves the
// agent's state intact. It runs before the agent starts and stops once the key
// is set. That narrows the window against a session already writing the same
// file rather than closing it.
//
// A state file the profile hostmounts is written in place instead. The guest
// holds that file through a bind, and a virtio-fs server that tracks files by
// handle, as virtiofsd does, keeps the bind on the old file after a rename:
// the key would never reach the agent, and what the agent wrote next would go
// to a file with no name on the host, lost at shutdown.
//
// The read matters as much as the write, and is the less obvious of the two.
// A rename cannot be made to land outside the workspace, which makes this look
// safe from the writing end -- but reading through a symlink and renaming the
// result back into the guest's home is a copy IN: point the state file at any
// JSON the host user can read, ~/.docker/config.json say, and this hands it to
// the sandbox. Both ends go through the root.
func (c *Config) trustGuestCwd(r *workspaceRoot) error {
	ob := c.Profile.Onboarding
	if !c.TrustWorkspace || ob == nil || ob.TrustKey[0] == "" {
		return nil
	}
	path := r.path(ob.File)
	blob, err := r.readFile(ob.File)
	if err != nil {
		if errors.Is(err, errPlantedSymlink) {
			return err
		}
		return nil // nothing to edit; the agent creates it on first run
	}

	key := c.trustKey()

	// UseNumber keeps every number exactly as written. Decoding into float64
	// would round-trip a millisecond timestamp as 1.7e+12 and quietly rewrite
	// values in a file that is not ours.
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(blob))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil || doc == nil {
		// The agent owns this file, so it owns a parse failure too. Say what
		// happened and leave the file alone.
		c.warnf("%s", notice.New(path+" holds invalid JSON, so brig leaves it as it is").
			Note("%s will ask you to trust %s", c.Profile.Binary, key))
		return nil
	}

	group, _ := doc[ob.TrustKey[0]].(map[string]any)
	if group == nil {
		group = map[string]any{}
	}
	entry, _ := group[key].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
	}
	if trusted, ok := entry[ob.TrustKey[1]].(bool); ok && trusted {
		return nil
	}
	entry[ob.TrustKey[1]] = true
	group[key] = entry
	doc[ob.TrustKey[0]] = group

	// Compact, because the agent writes this file on one line: setting one
	// flag is no reason to reformat someone else's state file.
	out, err := json.Marshal(doc)
	if err != nil {
		return nil
	}
	if c.fileHostMount(ob.File) {
		return r.writeFile(ob.File, out, 0o600)
	}
	tmp, name, err := r.createTemp(".brig-trust-")
	if err != nil {
		return err
	}
	defer func() { _ = r.remove(name) }()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return r.rename(name, ob.File)
}

// fileHostMount returns whether rel is a file the profile hostmounts.
func (c *Config) fileHostMount(rel string) bool {
	for _, h := range c.Profile.HostMounts() {
		if h.File && slashClean(h.Path) == slashClean(rel) {
			return true
		}
	}
	return false
}

// seedHostConfig copies the host's own agent configuration into the workspace.
//
// Copied, not mounted read-only. Read-only looks like the careful choice and
// is the wrong one: agents write inside these directories -- installing a
// plugin, populating a cache -- and a read-only mount turns that into an I/O
// error the agent has no way to handle. The guest gets its own writable copy
// and the host's directory is never touched, which is what read-only was for.
//
// Entry by entry, and only what is missing. Copying the directory wholesale
// would clobber whatever the guest has done since -- a plugin it installed, a
// cache it built -- every time the sandbox started. Per-entry means a skill
// added on the host later still arrives, and nothing the guest owns is
// overwritten. Same rule as seedOnboarding: what is already there belongs to
// the agent.
//
// The destination is the sandbox's home, so a .claude the guest replaced with
// a symlink would otherwise have this create directories and copy the host's
// real configuration to wherever the link pointed. Every destination path goes
// through the root; the source paths stay ordinary, because the host's own
// ~/.claude is outside the workspace on purpose.
func (c *Config) seedHostConfig(r *workspaceRoot) error {
	for _, seed := range c.HostConfig {
		if err := r.mkdirAll(seed.Rel, 0o755); err != nil {
			return err
		}
		entries, err := os.ReadDir(seed.Host)
		if err != nil {
			// The directory was there when the projection was resolved. If it
			// has gone since, that is not worth failing a boot over.
			continue
		}
		for _, e := range entries {
			target := filepath.Join(seed.Rel, e.Name())
			if _, err := r.lstat(target); err == nil {
				continue // the guest's copy wins
			} else if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if err := copyTree(r, filepath.Join(seed.Host, e.Name()), target); err != nil {
				return fmt.Errorf("cannot seed %s: %w", r.path(target), err)
			}
		}
	}
	return nil
}

// copyTree copies a host file or directory into the workspace, following
// neither symlinks nor the host's ownership. Symlinks are recreated as
// symlinks: a skill that points somewhere is the author's business, and
// resolving it here would copy in whatever it aimed at.
//
// src is a host path and read as one. dst is workspace-relative and every
// write to it goes through the root, which is also what keeps a recreated
// symlink harmless: the copy stays a link, and anything brig later writes
// through it is refused rather than followed.
func copyTree(r *workspaceRoot, src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return r.symlink(target, dst)
	case info.IsDir():
		if err := r.mkdirAll(dst, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyTree(r, filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	case !info.Mode().IsRegular():
		// Sockets, devices and the like have no business in a skills
		// directory, and copying one is never what somebody meant.
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := r.openFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
