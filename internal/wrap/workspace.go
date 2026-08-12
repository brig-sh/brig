package wrap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// markerFile identifies the workspace from inside the guest. See Marker.
const markerFile = ".brig-workspace"

func (c *Config) warnf(format string, a ...any) {
	fmt.Fprintf(c.Err, "brig: "+format+"\n", a...)
}

func (c *Config) sayf(format string, a ...any) {
	fmt.Fprintf(c.Out, "brig: "+format+"\n", a...)
}

// writeIfChanged leaves a file alone when it already holds exactly this
// content, so a regenerated-every-run file does not churn its mtime.
func writeIfChanged(path, content string, mode os.FileMode) error {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == content {
		return os.Chmod(path, mode)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
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
func (c *Config) PrepareWorkspace() error {
	if strings.ContainsAny(c.Workspace, " \t") {
		return fmt.Errorf("workspace path must not contain spaces: %s", c.Workspace)
	}
	if err := os.MkdirAll(c.Workspace, 0o755); err != nil {
		return err
	}
	if err := c.seedOnboarding(); err != nil {
		return err
	}
	if err := c.trustGuestCwd(); err != nil {
		return err
	}
	c.warnStaleCredentials()
	marker, err := Marker(c.Workspace)
	if err != nil {
		return err
	}
	return writeIfChanged(filepath.Join(c.Workspace, markerFile), marker+"\n", 0o644)
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
func (c *Config) seedOnboarding() error {
	ob := c.Agent.Onboarding
	if ob == nil || ob.File == "" {
		return nil
	}
	path := filepath.Join(c.Workspace, ob.File)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	blob, err := json.Marshal(ob.Seed)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(blob, '\n'), 0o600)
}

// warnStaleCredentials points out a credential an older wrapper wrote into
// the workspace. brig forwards credentials as environment and writes none, so
// such a file is a real token sitting on disk that nothing reads any more --
// and adopting a Homebrew-era workspace is exactly how one gets here. Say so
// rather than deleting somebody's file.
func (c *Config) warnStaleCredentials() {
	for _, rel := range c.Agent.StaleCredentialFiles {
		path := filepath.Join(c.Workspace, filepath.FromSlash(rel))
		if _, err := os.Stat(path); err == nil {
			c.warnf("note: %s holds a token on disk and is no longer used -- "+
				"credentials are passed as environment only. Delete it when convenient.",
				path)
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
// This writes one key, only when it needs to, through a temporary file in the
// same directory, so a partial write leaves the agent's state intact. It runs
// before the agent starts and stops once the key is set. That narrows the
// window against a session already writing the same file rather than closing
// it.
func (c *Config) trustGuestCwd() error {
	ob := c.Agent.Onboarding
	if !c.TrustWorkspace || ob == nil || ob.TrustKey[0] == "" {
		return nil
	}
	path := filepath.Join(c.Workspace, ob.File)
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil // nothing to edit; the agent creates it on first run
	}

	key := TrustKey(c.Cwd, c.Workspace, c.Agent.GuestHome)

	// UseNumber keeps every number exactly as written. Decoding into float64
	// would round-trip a millisecond timestamp as 1.7e+12 and quietly rewrite
	// values in a file that is not ours.
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(blob))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil || doc == nil {
		// The agent owns this file, so it owns a parse failure too. Say what
		// happened and leave the file alone.
		c.warnf("%s holds invalid JSON, so it stays as it is and %s will ask you to "+
			"trust %s.", path, c.Agent.Binary, key)
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
	tmp, err := os.CreateTemp(c.Workspace, ".brig-trust-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
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
	return os.Rename(tmpName, path)
}
