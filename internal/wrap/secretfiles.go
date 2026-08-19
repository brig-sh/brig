package wrap

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
)

// persistRoot is where a hostmount is pinned while the tmpfs goes over it.
//
// Under /run rather than anywhere in the workspace, and that is the point: /run
// is root-owned in the guest and the agent runs as an ordinary user, so nothing
// the sandbox can write to sits between a privileged mount and its source.
const persistRoot = "/run/brig/persist"

// guestRootUser is who the mounting execs run as. The mount syscall is the
// only privileged thing here; every target is created under a directory root
// already owns, so nothing else needs the privilege.
const guestRootUser = "root"

// deliverSecretFiles mounts the profile's volumes: and writes its files: into
// the guest.
//
// The order is the design. volumes: covers the agent's config directory with
// a tmpfs, so a credential written there cannot reach host disk. files: then
// writes it into that tmpfs as an ordinary file, never a bind mount on its own
// path: agents rewrite a credential file atomically (temp file, then rename),
// and rename onto a mountpoint returns EBUSY.
//
// Called on the already-running path as well as after a fresh boot, so every
// step below is idempotent: a mount is skipped when the path is already a
// mountpoint, and rewriting a credential the agent already read is how a
// rotated one reaches a live session.
//
// It clears the resolved values on the way out. Defence in depth rather than
// a control -- a Go process memory dump is already game over -- but retaining
// a plaintext refresh token for the process lifetime is the wrong default to
// write down.
func (c *Config) deliverSecretFiles() error {
	defer func() { c.secrets = creds.Resolution{} }()
	if len(c.Profile.Volumes) == 0 && len(c.Profile.Files) == 0 {
		return nil
	}
	if err := c.mountVolumes(); err != nil {
		return err
	}
	if err := c.verifyVolumes(); err != nil {
		return err
	}
	if err := c.writeSecretFiles(); err != nil {
		return err
	}
	return c.releaseTmpfs()
}

// releaseTmpfs hands each covered directory to the guest user.
//
// Last, and unconditionally. The cover leaves the directory root-owned so that
// nothing can be planted in it while brig is writing a credential there, which
// also means the agent cannot write its own state into it until this runs --
// including on a profile with no files: binding at all, where nothing else
// would ever hand it over.
func (c *Config) releaseTmpfs() error {
	var roots []string
	for _, t := range c.Profile.Tmpfs() {
		roots = append(roots, c.guestPath(t.Path))
	}
	return c.chownGuest(roots, c.Profile.GuestUser())
}

// prepareVolumeTargets creates each hostmount's host-side path, before
// anything covers it.
//
// Host-side and through the workspace root, which is what makes it safe. Every
// one of these paths is inside the workspace, which the sandbox has had write
// access to for weeks; a root-run `mount --bind` that followed a planted
// symlink here would be an arbitrary-write primitive reaching whatever the
// link named on the host. os.Root refuses an escaping link outright and
// workspaceRoot.exists refuses one that stays inside, so a link anywhere on
// the path fails the run naming the path rather than being repaired.
//
// Run before the sandbox is created, not only before delivery: a container
// runtime takes its mounts at create time, so a source that does not exist yet
// is a boot that fails or a directory the runtime invents.
func (c *Config) prepareVolumeTargets() error {
	if len(c.Profile.Volumes) == 0 {
		return nil
	}
	r, err := c.openWorkspace()
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	for _, v := range profile.MountOrder(c.Profile.Volumes) {
		wantFile := v.Kind == profile.VolumeHostMount && v.File
		if err := ensureTarget(r, v.Path, wantFile); err != nil {
			return err
		}
	}
	return nil
}

// ensureTarget creates one volume path in the workspace as the right kind,
// refusing a symlink at any component rather than writing through it.
//
// What is already there wins over what the profile guessed: a bind mount fails
// when the target is a directory and the source a file, and the workspace is
// where that state actually lives. file: is consulted only for a path that
// does not exist yet, which is the one case nothing else can answer.
func ensureTarget(r *workspaceRoot, rel string, wantFile bool) error {
	parts := strings.Split(slashClean(rel), "/")
	for i := range parts {
		prefix := strings.Join(parts[:i+1], "/")
		present, err := r.exists("mount", prefix)
		if err != nil {
			return err
		}
		last := i == len(parts)-1
		if present {
			if !last {
				continue
			}
			// A leaf that is there decides its own kind, and a mismatch is
			// worth naming: it is the difference between the bind working and
			// the run failing with the kernel's own opaque EINVAL.
			info, err := r.lstat(prefix)
			if err != nil {
				return err
			}
			if info.IsDir() == wantFile {
				return fmt.Errorf("volume %s is a %s in the workspace but the profile "+
					"declares it as a %s; a bind mount onto the wrong kind of target fails",
					r.path(prefix), kindWord(!info.IsDir()), kindWord(wantFile))
			}
			return nil
		}
		if last && wantFile {
			// 0600 rather than the agent's own default: brig is creating a
			// file the agent has not written yet, and the narrower mode is
			// the one that cannot be a mistake.
			return r.writeFile(prefix, nil, 0o600)
		}
		if err := r.mkdirAll(prefix, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func kindWord(file bool) string {
	if file {
		return "file"
	}
	return "directory"
}

// mountVolumes does the three phases, in the one order that works.
//
// Once tmpfs covers the directory the host copies underneath are unreachable,
// so every hostmount has to be pinned first -- bound to a path outside the
// directory about to be covered -- and bound back in afterwards. Proven in a
// guest in this order; any other loses the state it was meant to keep.
func (c *Config) mountVolumes() error {
	mounted, err := c.guestMountpoints()
	if err != nil {
		return err
	}
	p := c.Profile

	// A tmpfs that is already there is either a re-delivery or a container
	// runtime that took it at create time. Either way the host copies below it
	// are already gone, so pinning is not something to retry -- it is
	// something that had to happen before this moment.
	var cover []profile.Volume
	for _, v := range p.Tmpfs() {
		if !mounted[c.guestPath(v.Path)] {
			cover = append(cover, v)
		}
	}
	pinning := func(h profile.Volume) bool {
		for _, t := range cover {
			if profile.Under(h.Path, t.Path) {
				return true
			}
		}
		return false
	}

	if len(cover) > 0 {
		// Phase 1: pin, before anything covers the directory.
		for _, h := range p.HostMounts() {
			if !pinning(h) || mounted[pinPath(h.Path)] {
				continue
			}
			if err := c.createGuestTarget(pinPath(h.Path), h.File); err != nil {
				return err
			}
			if err := c.guestRoot("mount", "--bind", c.guestPath(h.Path), pinPath(h.Path)); err != nil {
				return fmt.Errorf("could not pin %s before covering it: %w", h.Path, err)
			}
		}
		// Phase 2: cover. Left root-owned deliberately, and handed to the
		// guest user only by releaseTmpfs at the very end: a directory the
		// agent cannot create in is a directory where a symlink cannot be
		// planted at a credential's path, so the race has no window rather
		// than a small one.
		for _, t := range cover {
			if err := c.guestRoot("mount", "-t", "tmpfs", "-o", t.TmpfsOptions(),
				"tmpfs", c.guestPath(t.Path)); err != nil {
				return fmt.Errorf("could not make %s ephemeral: %w", t.Path, err)
			}
		}
		// Phase 3: bind the pinned paths back in.
		for _, h := range p.HostMounts() {
			if !pinning(h) {
				continue
			}
			if err := c.createGuestTarget(c.guestPath(h.Path), h.File); err != nil {
				return err
			}
			if err := c.guestRoot("mount", "--bind", pinPath(h.Path), c.guestPath(h.Path)); err != nil {
				return fmt.Errorf("could not bind %s back in after covering it: %w", h.Path, err)
			}
		}
		if mounted, err = c.guestMountpoints(); err != nil {
			return err
		}
	}

	// Every hostmount must be a mountpoint by now, whoever made it. A
	// hostmount that is not one is a path the agent writes to believing it
	// persists, under a tmpfs that throws it away at shutdown -- silent, and
	// only noticed when someone goes looking for last week's sessions.
	for _, h := range p.HostMounts() {
		if !mounted[c.guestPath(h.Path)] {
			return fmt.Errorf("%s is not mounted and the ephemeral directory above it "+
				"already is, so what the sandbox writes there would be lost at shutdown. "+
				"Stop the sandbox and run again: brig rm %s", h.Path, c.VMName)
		}
	}
	return nil
}

// verifyVolumes fails the run on anything that is not what it must be.
//
// Each check carries one guarantee: the covered directory really is tmpfs,
// each hostmount survived the cover (it reads as the host share, so "not
// tmpfs" is the assertion), and there is no swap for the tmpfs to be paged out
// to. Checked rather than assumed -- from the host, a mount that silently did
// not happen looks exactly like one that did.
func (c *Config) verifyVolumes() error {
	for _, v := range c.Profile.Tmpfs() {
		fstype, err := c.guestFstype(c.guestPath(v.Path))
		if err != nil {
			return err
		}
		if fstype != "tmpfs" {
			return fmt.Errorf("%s should be ephemeral and reads as %s, so anything written "+
				"there reaches host disk; refusing to hand the sandbox a credential",
				v.Path, fstype)
		}
	}
	for _, v := range c.Profile.HostMounts() {
		fstype, err := c.guestFstype(c.guestPath(v.Path))
		if err != nil {
			return err
		}
		if fstype == "tmpfs" {
			return fmt.Errorf("%s should be kept on the host and reads as tmpfs, so the "+
				"ephemeral mount covered it and the sandbox would lose that state at "+
				"shutdown", v.Path)
		}
	}
	// A tmpfs page that reached swap is a credential on a disk brig never
	// wrote to. There is no swap in the guest today, so this is not a
	// mitigation but a tripwire: if one ever appears, the run stops rather
	// than the guarantee quietly weakening.
	swaps, err := c.guestOutput("cat", "/proc/swaps")
	if err != nil {
		return fmt.Errorf("could not check the guest for swap: %w", err)
	}
	if lines := nonEmptyLines(swaps); len(lines) > 1 {
		return fmt.Errorf("the guest has swap enabled (%s), so an ephemeral mount can be "+
			"paged to disk; refusing to hand the sandbox a credential", lines[1])
	}
	return nil
}

// writeSecretFiles puts each resolved secret where its binding says.
//
// An ordinary file, owned by the guest user, with the binding's mode, and the
// value on stdin -- never in argv, because hull durably logs every exec's argv
// to a host file that outlives the sandbox.
func (c *Config) writeSecretFiles() (err error) {
	var pending []profile.FileBinding
	for _, b := range c.Profile.Files {
		r, refErr := profile.ParseRef(b.Ref)
		if refErr != nil {
			return fmt.Errorf("file %s: %w", b.Path, refErr)
		}
		if _, ok := c.secrets.Values[r.Name]; !ok {
			// An unresolved binding leaves NOTHING behind: no file, and above
			// all no empty one. An empty file at a credential path is
			// indistinguishable from a real leak to whoever finds it, and it
			// is a login prompt the agent cannot explain.
			continue
		}
		pending = append(pending, b)
	}
	if len(pending) == 0 {
		return nil
	}

	// Hold the directories root-owned across the write. The agent runs as the
	// guest user and could otherwise plant a symlink at a credential's path
	// between brig checking it and brig writing it -- and root writing through
	// one is the arbitrary-write primitive this whole file is careful about.
	// A directory root owns cannot have anything created in it by the agent,
	// so the race has no window rather than a small one.
	dirs := c.credentialDirs(pending)
	if err := c.chownGuest(dirs, guestRootUser); err != nil {
		return err
	}
	defer func() {
		if release := c.chownGuest(dirs, c.Profile.GuestUser()); err == nil {
			err = release
		}
	}()

	for _, b := range pending {
		if err := c.writeSecretFile(b); err != nil {
			return err
		}
	}
	return nil
}

// writeSecretFile creates one file, checks it, and only then fills it.
//
// Three execs rather than one: the file is created empty with O_EXCL (`set -C`
// is what makes the shell's > use it), checked to be a regular file of the
// right ownership on a tmpfs, and only then handed the value. Checking after
// the write would be checking where a live token had already gone.
func (c *Config) writeSecretFile(b profile.FileBinding) error {
	r, err := profile.ParseRef(b.Ref)
	if err != nil {
		return err
	}
	mode, err := b.FileMode()
	if err != nil {
		return fmt.Errorf("file %s: %w", b.Path, err)
	}
	target := c.guestPath(b.Path)
	user := c.Profile.GuestUser()

	// rm -f removes the link rather than following it; set -C then refuses to
	// create through anything that reappeared, so a race loses loudly.
	create := `set -e; rm -f -- "$1"; umask 077; set -C; : > "$1"; chown "$2" "$1"; chmod "$3" "$1"`
	if err := c.guestRoot("sh", "-c", create, "sh", target, user,
		fmt.Sprintf("%04o", mode.Perm())); err != nil {
		return fmt.Errorf("could not create %s in the sandbox: %w", b.Path, err)
	}
	if err := c.verifySecretFile(b, target, user, mode); err != nil {
		return err
	}
	value := c.secrets.Values[r.Name]
	if err := c.Runtime.Feed(runtime.ExecSpec{
		Name: c.VMName,
		User: guestRootUser,
		// The value goes in on stdin. Never argv: see runtime.Var.Secret for
		// why brig treats a stored credential in a log file as a different
		// severity of leak from an ambient one.
		Cmd:   []string{"sh", "-c", `cat > "$1"`, "sh", target},
		Stdin: strings.NewReader(value),
	}); err != nil {
		return fmt.Errorf("could not write %s in the sandbox: %w", b.Path, err)
	}
	// Re-checked after the write for size alone: a Feed that reported success
	// while delivering nothing would leave the agent with an empty credential
	// and no way to say so.
	size, err := c.guestOutput("stat", "-c", "%s", target)
	if err != nil || strings.TrimSpace(size) == "0" {
		return fmt.Errorf("%s is empty after delivery, so the sandbox has no credential "+
			"where it expects one", b.Path)
	}
	return nil
}

// verifySecretFile is the check that runs BEFORE the value is written.
func (c *Config) verifySecretFile(b profile.FileBinding, target, user string, mode fs.FileMode) error {
	// stat does not follow a symlink without -L, so "regular file" here also
	// answers "not a link somebody aimed at the workspace".
	got, err := c.guestOutput("stat", "-c", "%F|%U|%a", target)
	if err != nil {
		return fmt.Errorf("could not check %s in the sandbox: %w", b.Path, err)
	}
	// "regular empty file" and "regular file" are both stat's answer for what
	// this check wants -- the file was just created, so it is the empty
	// spelling that turns up. What is being refused is "symbolic link" and
	// "directory": stat does not follow a link without -L, so this is also
	// what answers "not something the sandbox aimed at the workspace".
	want := fmt.Sprintf("%s|%o", user, mode.Perm())
	kind, rest, _ := strings.Cut(strings.TrimSpace(got), "|")
	if !strings.HasPrefix(kind, "regular ") || rest != want {
		return fmt.Errorf("%s is %q in the sandbox and must be a regular file %q before a "+
			"credential goes into it", b.Path, strings.TrimSpace(got), want)
	}
	fstype, err := c.guestFstype(target)
	if err != nil {
		return err
	}
	if fstype != "tmpfs" {
		return fmt.Errorf("%s sits on %s rather than an ephemeral mount, so writing a "+
			"credential there would put it on host disk", b.Path, fstype)
	}
	return nil
}

// credentialDirs is every directory between a tmpfs root and a credential's
// own parent, deepest last.
//
// The whole chain rather than just the parent: locking .claude while
// .claude/x is the agent's to write leaves the same window one level down.
// Validation guarantees each of these is inside a tmpfs and outside any
// hostmount, so nothing here is a host path.
func (c *Config) credentialDirs(files []profile.FileBinding) []string {
	var out []string
	for _, t := range c.Profile.Tmpfs() {
		out = append(out, c.guestPath(t.Path))
		for _, b := range files {
			if !profile.Under(b.Path, t.Path) {
				continue
			}
			for dir := slashDir(b.Path); profile.Under(dir, t.Path); dir = slashDir(dir) {
				if guest := c.guestPath(dir); !slices.Contains(out, guest) {
					out = append(out, guest)
				}
			}
		}
	}
	return out
}

// chownGuest hands a set of directories to one user, shallowest first so a
// parent is never handed over while a child below it is still being written.
func (c *Config) chownGuest(dirs []string, user string) error {
	ordered := slices.Clone(dirs)
	slices.SortStableFunc(ordered, func(a, b string) int {
		return strings.Count(a, "/") - strings.Count(b, "/")
	})
	if user != guestRootUser {
		slices.Reverse(ordered)
	}
	for _, dir := range ordered {
		if err := c.guestRoot("chown", user, dir); err != nil {
			return fmt.Errorf("could not hand %s to %s in the sandbox: %w", dir, user, err)
		}
	}
	return nil
}

// createGuestTarget makes a mount target of the right kind under a directory
// root already owns -- /run/brig/persist, or a tmpfs brig has not handed over
// yet. Neither is writable by the agent, so no symlink can be waiting.
func (c *Config) createGuestTarget(target string, file bool) error {
	if file {
		return c.guestRoot("sh", "-c", `set -e; mkdir -p -- "$(dirname "$1")"; `+
			`[ -e "$1" ] || : > "$1"`, "sh", target)
	}
	return c.guestRoot("mkdir", "-p", target)
}

// guestMountpoints reads the guest's own mount table.
//
// /proc/self/mountinfo rather than `mountpoint -q`: a minimal image may not
// carry the binary, and a missing binary reads as "not mounted", which would
// stack a second mount on every run until the guest ran out. The kernel's own
// table cannot be absent.
func (c *Config) guestMountpoints() (map[string]bool, error) {
	out, err := c.guestOutput("cat", "/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("could not read the sandbox's mount table: %w", err)
	}
	points := map[string]bool{}
	for _, line := range nonEmptyLines(out) {
		fields := strings.Fields(line)
		if len(fields) > 4 {
			points[fields[4]] = true
		}
	}
	return points, nil
}

// guestFstype is `stat -f -c %T`: tmpfs for what brig mounted, fuseblk for the
// host share underneath it.
func (c *Config) guestFstype(target string) (string, error) {
	out, err := c.guestOutput("stat", "-f", "-c", "%T", target)
	if err != nil {
		return "", fmt.Errorf("could not check what %s sits on in the sandbox: %w", target, err)
	}
	return strings.TrimSpace(out), nil
}

// guestPath is one volume or file path as the guest sees it: guestHome plus
// the profile's relative path, always slash-separated.
func (c *Config) guestPath(rel string) string { return path(c.Profile.GuestHome, rel) }

// slashClean and slashDir are the two path helpers this file needs, spelled
// out because the package already has its own `path` function and importing
// the standard one would shadow it.
func slashClean(p string) string { return filepath.ToSlash(filepath.Clean(p)) }

func slashDir(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return "."
	}
	return p[:i]
}

// pinPath is where one hostmount waits out the cover. The path is escaped
// rather than flattened: two distinct volume paths must not collide on one pin
// and quietly swap each other's contents.
func pinPath(rel string) string {
	escaped := strings.ReplaceAll(rel, "%", "%25")
	return persistRoot + "/" + strings.ReplaceAll(escaped, "/", "%2F")
}

func (c *Config) guestRoot(argv ...string) error {
	_, err := c.guestOutput(argv...)
	return err
}

func (c *Config) guestOutput(argv ...string) (string, error) {
	return c.Runtime.Output(runtime.ExecSpec{Name: c.VMName, User: guestRootUser, Cmd: argv})
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// createTimeVolumes is what a runtime that cannot mount after boot needs at
// create time instead.
//
// hull execs into the guest as root, so brig mounts there in the order the
// design requires -- pin, cover, bind back -- and passing these to it would
// mount the same paths twice by two mechanisms. A container runtime has no
// privileged exec to mount with, so the tmpfs and the hostmounts have to be
// part of the create request; runc orders mounts by destination depth, which
// is the same rule MountOrder applies.
func (c *Config) createTimeVolumes() (tmpfs []string, shares []runtime.Share) {
	if c.Runtime.Kind() == "hull" {
		return nil, nil
	}
	for _, v := range profile.MountOrder(c.Profile.Volumes) {
		switch v.Kind {
		case profile.VolumeTmpfs:
			tmpfs = append(tmpfs, c.guestPath(v.Path)+":"+v.TmpfsOptions())
		case profile.VolumeHostMount:
			shares = append(shares, runtime.Share{
				Host:  filepath.Join(c.Workspace, filepath.FromSlash(v.Path)),
				Guest: c.guestPath(v.Path),
			})
		}
	}
	return tmpfs, shares
}
