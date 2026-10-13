package wrap

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/brig-sh/brig/internal/notice"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
)

// A teleportable session owns all of its storage. The guest root is a block
// disk, the guest home is a directory inside it, and no host directory is
// shared in: no home share, no project. That is what lets a later release
// move the running sandbox to another host as a whole, because there is
// nothing left behind on this one for it to point at.
//
// Everything else is the same session. The network, the policy and the
// credentials are decided and delivered exactly as for any other: a files:
// credential is fed through hull exec into a tmpfs the profile declares, and
// the tmpfs now sits over a directory of the block disk rather than of a
// share. An environment credential goes to hull run with the rest of the
// boot environment, and hull writes that into /urunit.conf on the root disk.
// On an ordinary session that disk goes with the sandbox; here it is the
// session, so the value outlives a rotation and travels with a move.
// docs/teleport.md says so, and keeping it off the disk is a precondition of
// moving one.
//
// What changes is only where the guest home is, and so which of brig's
// host-side steps have anything to act on. Each one that writes into the host
// home is skipped, and each one a user can ask for by name is refused, so a
// request that would do nothing says so instead.

// teleportChoice is what Load settled about this session's storage before it
// resolved anything else: whether the session is teleportable, what made it
// so, and why this invocation cannot run it, if it cannot.
type teleportChoice struct {
	on bool
	// source names what made it teleportable, for a refusal to quote.
	source string
	// refusal is held, not returned, for the reason Load holds a refused
	// project: every verb loads the session, and stop, rm, info and logs
	// must still reach one whose run is refused. sessionRefusal returns it.
	refusal error
}

// resolveTeleportable decides whether this session is teleportable and
// whether this invocation contradicts that.
//
// Three things make it so, and any one is enough: --teleportable on the line,
// teleportable: true in the profile, or an index entry saying the session was
// started that way. The entry is what keeps the property: a later `brig sh`
// names neither of the other two, and reading its silence as an ordinary
// session would boot a host home over a sandbox whose home is its disk.
//
// There is no way to turn it off on a session that has it. Switching either
// way changes where the guest home is, and the home that is not chosen holds
// the agent's state, so it is a new session rather than a setting: brig rm
// first, and the refusals below say so.
func resolveTeleportable(t profile.Profile, o Options, env Env, ref, vmName string, homeGiven bool) teleportChoice {
	recorded, known := rememberedTeleportable(ref, vmName)
	var tp teleportChoice
	switch {
	case o.Teleportable:
		tp.on, tp.source = true, "--teleportable"
	case t.Teleportable:
		tp.on, tp.source = true, "the profile's teleportable: true"
	case recorded:
		tp.on, tp.source = true, "this session, which was started teleportable"
	default:
		return tp
	}
	// A session brig already knows, started with a host home. One sandbox
	// cannot be both, and either answer here would lose a home: booting it
	// teleportable leaves the host home behind, and keeping it ordinary
	// ignores what was asked for.
	if known && !recorded {
		home, _ := rememberedWorkspace(ref, vmName)
		tp.refusal = fmt.Errorf("%s was started with its guest home on the host, at %s, and %s asks for "+
			"a teleportable session, which keeps its guest home inside its own disk. One sandbox cannot "+
			"be both. Remove it with `brig rm %s` and run it again; rm deletes a guest home brig created, "+
			"so copy out what you want to keep first", ref, home, tp.source, ref)
		return tp
	}
	back := ""
	if recorded {
		back = fmt.Sprintf(". A teleportable session cannot be switched back to an ordinary one; "+
			"remove it with `brig rm %s` first", ref)
	}
	switch {
	case homeGiven:
		from := "--home"
		if o.Workspace == "" {
			from = env.SettingName("WORKSPACE")
		}
		tp.refusal = fmt.Errorf("%s names a guest home on the host, and this session is teleportable (%s): "+
			"its guest home is inside its own disk and no host directory is shared in. Drop %s for this "+
			"session%s", from, tp.source, from, back)
	case o.Project != "":
		tp.refusal = fmt.Errorf("this session is teleportable (%s), so it mounts no project: a project is a "+
			"host directory, and a teleportable session shares none in. Run it without %s and copy the "+
			"work in from inside the sandbox, or run an ordinary session for this project%s",
			tp.source, o.Project, back)
	}
	if tp.refusal != nil {
		return tp
	}
	// BRIG_ROOTFS_TYPE is passed to hull unchecked on an ordinary run. Here
	// the root is the session's storage, so a shared root would put the guest
	// home back on a host path by another route.
	if v, ok := env.Get("ROOTFS_TYPE"); ok && v != "" && v != "block" {
		tp.refusal = fmt.Errorf("%s is %q, and a teleportable session (%s) boots a block root: its guest "+
			"home lives on it. Unset it for this session", env.SettingName("ROOTFS_TYPE"), v, tp.source)
	}
	return tp
}

// teleportSettingsRefusal refuses the two opt-ins that write into the guest
// home from the host. A teleportable session has no host home for them to
// write into, and skipping them would leave a flag that silently does
// nothing, which is worse than a refusal that names it.
func (c *Config) teleportSettingsRefusal(skillsAsked bool, skillsFrom string) error {
	if skillsAsked {
		return fmt.Errorf("%s copies your own agent configuration into the guest home on the host, and "+
			"this session is teleportable (%s): it has no guest home on the host. Run it without %s",
			skillsFrom, c.teleportSource, skillsFrom)
	}
	if c.GitConfig {
		from := c.env.SettingName("GIT_CONFIG")
		return fmt.Errorf("%s writes brig's git helper into the guest home on the host, and this session "+
			"is teleportable (%s): it has no guest home on the host. Unset %s for this session",
			from, c.teleportSource, from)
	}
	return nil
}

// teleportBackendRefusal is backendRefusal's question for a teleportable
// session: whether this runtime and backend can run one at all. A
// CapabilityError, so the refusal exits 7 like a policy nothing enforces.
func (c *Config) teleportBackendRefusal(hypervisor string) error {
	if !c.Teleportable || c.Runtime == nil {
		return nil
	}
	return runtime.RequireTeleportable(c.Runtime.Kind(), hypervisor)
}

// teleportNotice tells the user, once, where a teleportable session keeps its
// home, in place of ephemeralNotice: there is no host directory to name, and
// what `brig rm` takes with it is the whole disk.
func (c *Config) teleportNotice(known bool) {
	if known {
		return
	}
	ref := sessionKey(c.Profile.Name, c.Slug)
	c.warnf("%s", notice.New("the guest home of "+ref+" is inside its own disk, not on the host").
		Note("nothing from the host is shared in; the home lives in the sandbox's disk image, "+
			"not in a host directory").
		Note("`brig rm %s` deletes the disk and every file in it", ref))
}

// prepareGuestHome does inside the guest the part of PrepareWorkspace a
// teleportable session still needs, and nothing else.
//
// Three things, in order, each idempotent because the path that finds the
// sandbox running calls it too:
//
//   - The guest home itself. On a share the runtime makes the mount point; on
//     a block root it is whatever the image has, and an image built for a
//     host home need not carry it.
//   - The onboarding seed, when the profile has one and the file is absent.
//     It is non-secret, and without it an agent stops on a first-run screen
//     the guest cannot complete. Written before the hostmount targets, which
//     would otherwise create the same file empty.
//   - Each hostmount's source path. On a share they are made host-side by
//     ensureTarget; here they are directories of the block disk, and
//     mountVolumes binds from them.
//
// This runs as root in the guest and the home is the agent's, so a path in
// it can be a link the agent planted. That reaches only the guest's own
// filesystem: no host directory is shared in, which is the property the
// session exists for. Each step still leaves an existing path alone, a link
// included, rather than writing through it.
func (c *Config) prepareGuestHome() error {
	home := c.Profile.GuestHome
	user := c.Profile.GuestUser()
	if err := c.guestRoot("sh", "-c", guestHomeScript, "sh", home, user); err != nil {
		return fmt.Errorf("could not make the guest home %s in the sandbox: %w", home, err)
	}
	if ob := c.Profile.Onboarding; ob != nil && ob.File != "" {
		blob, err := json.Marshal(ob.Seed)
		if err != nil {
			return err
		}
		target := c.guestPath(ob.File)
		if err := c.Runtime.Feed(runtime.ExecSpec{
			Name:  c.VMName,
			User:  guestRootUser,
			Cmd:   []string{"sh", "-c", onboardingSeedScript, "sh", target, user},
			Stdin: strings.NewReader(string(append(blob, '\n'))),
		}); err != nil {
			return fmt.Errorf("could not seed %s in the sandbox: %w", ob.File, err)
		}
	}
	for _, h := range profile.MountOrder(c.Profile.Volumes) {
		if h.Kind != profile.VolumeHostMount {
			continue
		}
		kind := "dir"
		if h.File {
			kind = "file"
		}
		if err := c.guestRoot("sh", "-c", guestTargetScript, "sh", c.guestPath(h.Path), user, kind); err != nil {
			return fmt.Errorf("could not make %s in the guest home: %w", h.Path, err)
		}
	}
	return nil
}

// The scripts prepareGuestHome runs. Each takes its paths as positional
// parameters, never spliced into the script, and hands what it creates to the
// guest user only when that account exists: GuestUser is derived from the
// home's last element, and on a profile whose home is not an account's
// (ubuntu's /root/work) there is nobody to hand it to.
const (
	guestHomeScript = `set -e; if [ -e "$1" ] || [ -L "$1" ]; then exit 0; fi; mkdir -p -- "$1"; ` +
		`if [ "$2" != root ] && id -u "$2" >/dev/null 2>&1; then chown "$2" "$1"; fi`
	// The seed arrives on stdin, so it is drained even when the file is
	// already there: an exec whose stdin is never read can fail the Feed.
	onboardingSeedScript = `set -e; if [ -e "$1" ] || [ -L "$1" ]; then cat > /dev/null; exit 0; fi; ` +
		`mkdir -p -- "$(dirname -- "$1")"; umask 077; cat > "$1"; ` +
		`if [ "$2" != root ] && id -u "$2" >/dev/null 2>&1; then chown "$2" "$1"; fi`
	guestTargetScript = `set -e; if [ -e "$1" ] || [ -L "$1" ]; then exit 0; fi; ` +
		`if [ "$3" = file ]; then mkdir -p -- "$(dirname -- "$1")"; : > "$1"; chmod 600 "$1"; ` +
		`else mkdir -p -- "$1"; fi; ` +
		`if [ "$2" != root ] && id -u "$2" >/dev/null 2>&1; then chown "$2" "$1"; fi`
)
