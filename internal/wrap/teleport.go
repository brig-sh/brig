package wrap

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/brig-sh/brig/internal/notice"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
)

// A teleportable session owns all of its storage. The guest root is a block
// disk, the guest home is a directory inside it, and no host directory is
// shared in: no home share, no project. That is what lets a later release
// move the running sandbox to another host as a whole, because there is
// nothing left behind on this one for it to point at.
//
// Everything else is the same session, credentials included, with one
// difference in when they arrive. A files: credential is fed through hull
// exec into a tmpfs the profile declares, as on any session; the tmpfs now
// sits over a directory of the block disk rather than of a share. An
// environment credential is not handed to hull run. hull writes the boot
// environment, values included, into /urunit.conf on the root disk or
// /urunc-env in the boot initrd, and here the disk is the session: a value
// there would outlive a rotation and travel with a move. So the boot carries
// only the names hull reads for itself, the profile's literals and brig's
// plumbing (runtime.RunSpec.Teleportable), and every exec, the agent's
// included, hands the credentials in by name as it always has. A profile
// whose workload reads credentials at boot cannot be teleportable, and
// resolveTeleportable refuses it.
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
	// A workload that reads its credentials at boot can only get them from
	// the boot environment, and a teleportable boot carries none: hull would
	// write them into the disk. Refused before the backend is asked, because
	// no backend changes it. A profile that says teleportable: true is
	// refused for the same reason when it loads; this is --teleportable on a
	// profile that does not.
	if t.CredentialsAtBoot() {
		tp.refusal = fmt.Errorf("%s asks for a teleportable session, and %s cannot be one. %s. "+
			"Run it as an ordinary session", tp.source, t.Name, profile.TeleportableBootCredentials)
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
// This runs as root in the guest on every run, reuse included, and the home
// is the agent's by then, so any path under it can be a link the agent
// planted. Following one as root is what a custom profile whose agent is not
// root has to be protected from: the seed, an empty target file or a chown
// landing wherever the link points, and mountVolumes then binding a
// root-owned path into the home. So every path under the home goes through
// guestPathScript, which walks it one component at a time, refuses a link at
// any of them, creates what is missing one directory at a time, and refuses a
// path whose last component is not the kind the profile declared. The walk
// is also the check mountVolumes needs before it binds from a hostmount's
// source as root: it runs just before delivery, on both paths that reach it.
//
// What it does not close is a race: an agent swapping a checked directory
// for a link between the check and the write. The file it creates is opened
// with noclobber, so a link at the last component fails the write instead of
// being followed, and the chown that hands a new path over is -h, so a link
// swapped in after the create is changed itself rather than its target. The shipped profiles run the agent as root, for which none
// of this widens anything: it already owns the whole guest, and nothing from
// the host is shared in.
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
		if err := c.Runtime.Feed(runtime.ExecSpec{
			Name:  c.VMName,
			User:  guestRootUser,
			Cmd:   []string{"sh", "-c", guestPathScript, "sh", home, ob.File, "seed", user},
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
		if err := c.guestRoot("sh", "-c", guestPathScript, "sh", home, h.Path, kind, user); err != nil {
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
	// The home itself, made by root above the agent's reach: the
	// components over it are the image's, not the agent's.
	guestHomeScript = `set -e; if [ -e "$1" ] || [ -L "$1" ]; then exit 0; fi; mkdir -p -- "$1"; ` +
		`if [ "$2" != root ] && id -u "$2" >/dev/null 2>&1; then chown "$2" "$1"; fi`

	// guestPathScript HOME REL KIND USER makes or checks HOME/REL. KIND is
	// dir, file, or seed (a file whose content arrives on stdin). An empty,
	// "." or ".." component is refused with the rest: a profile path is
	// relative to the home and stays inside it. The seed is drained even when
	// the file is already there, because an exec whose stdin is never read
	// can fail the Feed.
	guestPathScript = `set -e; home=$1; rel=$2; kind=$3; user=$4
[ "$kind" = seed ] && trap 'cat > /dev/null' EXIT
no() { echo "brig: $*" >&2; exit 1; }
own() { if [ "$user" != root ] && id -u "$user" >/dev/null 2>&1; then chown -h "$user" "$1"; fi; }
[ -L "$home" ] && no "the guest home $home is a symlink"
[ -d "$home" ] || no "the guest home $home is not a directory"
p=$home; rest=$rel
while :; do
  case $rest in */*) part=${rest%%/*}; rest=${rest#*/} ;; *) part=$rest; rest= ;; esac
  case $part in ''|.|..) no "$rel is not a plain path under the guest home" ;; esac
  p=$p/$part
  [ -L "$p" ] && no "$p is a symlink; brig will not write through it"
  [ -z "$rest" ] && break
  if [ ! -e "$p" ]; then mkdir -- "$p"; own "$p"; elif [ ! -d "$p" ]; then no "$p is not a directory"; fi
done
case $kind in
  dir)
    if [ -e "$p" ]; then [ -d "$p" ] || no "$p is not a directory"; else mkdir -- "$p"; own "$p"; fi ;;
  file)
    if [ -e "$p" ]; then [ -f "$p" ] || no "$p is not a regular file"
    else (umask 077; set -C; : > "$p"); own "$p"; fi ;;
  seed)
    if [ -e "$p" ]; then [ -f "$p" ] || no "$p is not a regular file"
    else trap - EXIT; (umask 077; set -C; cat > "$p"); own "$p"; fi ;;
  *) no "unknown kind $kind" ;;
esac`
)

// readTeleportState fills teleportState from the runtime's migration record,
// for brig info. The run envelope does not ask: it has no use for a second
// call to hull before every boot. A run that reuses a running sandbox reads
// the record for itself, to refuse a parked guest; see
// refuseParkedTeleportable.
//
// Read on a teleportable session only, since no other can be checkpointed.
// Every failure leaves it empty: info answers when the runtime is broken,
// and the row it would print is the one fact it could not establish.
func (c *Config) readTeleportState() {
	c.teleportState = runtime.Migration{}
	c.machineSeen = ""
	if !c.Teleportable || c.Runtime == nil {
		return
	}
	if mi, ok := c.Runtime.(runtime.MachineInspector); ok {
		if m, err := mi.Machine(c.VMName); err == nil {
			c.machineSeen = m
		}
	}
	mi, ok := c.Runtime.(runtime.MigrationInspector)
	if !ok {
		return
	}
	if m, err := mi.Migration(c.VMName); err == nil {
		c.teleportState = m
	}
}

// machine is the machine profile a boot of this session asks hvi for: the
// portable one for a teleportable session on an arm64 host, so its
// checkpoint restores on a Mac and on a Linux host alike, and "" for hvi's
// native machine otherwise. One function, so the run, the plan, info and the
// boot asset resolve cannot come to disagree.
func (c *Config) machine() string {
	if !c.Teleportable {
		return ""
	}
	return runtime.TeleportableMachine()
}

// machineName is machine as a row names it.
func (c *Config) machineName() string {
	if m := c.machine(); m != "" {
		return m
	}
	return runtime.MachineNative
}

// machineLine is the MACHINE row of a teleportable session: the machine it
// boots, and for info the machine hull's record says it runs when that
// differs, which is a session booted before brig chose the portable one.
func (c *Config) machineLine() string {
	want := c.machineName()
	note := "the same machine on a Mac and on a Linux arm64 host"
	if want == runtime.MachineNative {
		note = "this host's own; there is no portable machine on " + runtime.GuestArch()
	}
	if seen := c.machineSeen; seen != "" && seen != want {
		return fmt.Sprintf("%s, as hull records the boot (a teleportable session boots %s now)", seen, want)
	}
	return fmt.Sprintf("%s (%s)", want, note)
}

// infoMachine is the MACHINE row as data: what hull's record says when it
// says, and what a boot asks for otherwise.
func (c *Config) infoMachine() string {
	if !c.Teleportable {
		return ""
	}
	if c.machineSeen != "" {
		return c.machineSeen
	}
	return c.machineName()
}

// teleportLine is the TELEPORT row: hull's state and the bundle it names, or
// "" when there is no record.
//
// A move adds its other host, and a session moved here the host it came
// from, so the row says where the session was or is going.
func (c *Config) teleportLine() string {
	m := c.teleportState
	from := ""
	if a := c.arrival; a != nil {
		from = fmt.Sprintf("arrived from %s (move %s)", a.From, a.TxID)
	}
	switch {
	case m.State == "":
		return from
	case m.Peer != "" && from == "":
		return fmt.Sprintf("%s (move %s, %s)", m.State, m.TxID, m.Peer)
	case m.BundleDir != "" && from == "":
		return fmt.Sprintf("%s (checkpoint in %s)", m.State, m.BundleDir)
	case from != "":
		return m.State + ", " + from
	}
	return m.State
}

// MoveNetwork is the network a move carries: the posture and the slot the
// guest's address is on, and the rules its gateway enforces, by value.
type MoveNetwork struct {
	Net      string
	Slot     int
	Egress   policy.Egress
	Policies []string
}

// MoveNetwork reads the network this session's sandbox runs on, for a move
// to carry. It refuses when what this host would resolve for the session
// differs from what the sandbox booted with: the rules a move carries must
// be the ones the guest is under, and either answer would carry the wrong
// ones.
func (c *Config) MoveNetwork() (MoveNetwork, error) {
	if c.postureChanged() || c.networkStale() {
		change, _ := c.postureChange()
		return MoveNetwork{}, fmt.Errorf("%s, so the rules %s runs under are not the ones brig "+
			"resolves for it now, and a move would carry the wrong ones. Put the network and policy "+
			"back as the sandbox was started", change, c.VMName)
	}
	net, slot, ok := runtime.SandboxAddress(c.VMName)
	if !ok {
		return MoveNetwork{}, fmt.Errorf("brig has no record of the network %s was booted on, and a "+
			"moved guest keeps the address it has, so the destination could not give it the same one",
			c.VMName)
	}
	policies := c.Policies
	if c.arrival != nil && c.arrival.Egress.Default != "" && len(c.arrival.Policies) > 0 {
		policies = c.arrival.Policies
	}
	return MoveNetwork{Net: net, Slot: slot, Egress: c.Egress, Policies: policies}, nil
}
