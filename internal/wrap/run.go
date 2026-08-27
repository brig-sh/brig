package wrap

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
)

// BuildEnv resolves everything the guest is handed for this invocation.
//
// It is called once per command and never by Stop: stopping a sandbox needs
// nothing but the instance name, and reading the host credential for it would
// raise a keychain approval prompt for a command that needs no credential.
func (c *Config) BuildEnv() (creds.Set, error) {
	// What building the binding list dropped, said before anything can fail:
	// that decision was made when the list was built, and holding it back
	// because a later step failed would tell the user half of what happened to
	// their BRIG_FORWARD_ENV. Whatever fails below still prints after it.
	for _, w := range c.envWarnings {
		c.warnf("%s", w)
	}

	// Resolved before anything else touches the sandbox, and returned as an
	// error rather than a warning: a run whose secret cannot be resolved must
	// fail, saying which secret is missing and how to create it, instead of
	// creating a sandbox that will fail later and less legibly. Both callers
	// (cmd/brig and cmd/brigd) return this before EnsureRunning.
	res, err := c.resolveSecrets()
	if err != nil {
		return creds.Set{}, err
	}
	c.secrets = res
	for _, w := range res.Warnings {
		// Multi-line by construction: each of these is a sentence about what
		// will happen plus the command that changes it, and warnf prefixes
		// every line so a copied one still reads as brig's.
		for _, line := range strings.Split(w, "\n") {
			c.warnf("%s", line)
		}
	}
	set := creds.Bind(c.Profile, c.Env, res.Values, os.LookupEnv, creds.Options{
		AllowRefs:   c.AllowRefs,
		AllowDenied: c.AllowDenied,
	})
	for _, w := range set.Warnings {
		c.warnf("%s", w)
	}

	// Fall back to the host's own login when the environment supplied no
	// token, so the sandbox works out of the box without anyone having to
	// mint one. Read fresh every invocation: the token is short-lived, the
	// host renews it as a matter of course, and nothing in the guest has to
	// refresh anything.
	//
	// Only a profile still carrying the deprecated hostCredential: gets here,
	// and no built-in carries one any more. The keychain is the only backend
	// this reads: BRIG_CREDENTIALS_CMD, which used to point it at a command of
	// the user's own, is gone, and Load fails a run that still sets it rather
	// than booting a sandbox without the login the variable used to supply.
	if hc := c.Profile.HostCredential; hc != nil && !set.Has(hc.TargetVar) {
		if host := creds.ReadHost(hc, c.ReadKeychain); host != nil {
			c.HostCred = host
			c.admitHostCredential(hc, host, &set)
		}
	}

	// Never let git block on an interactive credential prompt: inside an
	// agent session there is no one to answer it, so git hangs and the agent
	// looks wedged. Fail fast instead. Deliberately unconditional -- a
	// missing token is exactly when git wants to prompt. GIT_TERMINAL_PROMPT=1
	// on the host opts back in.
	prompt := os.Getenv("GIT_TERMINAL_PROMPT")
	if prompt == "" {
		prompt = "0"
	}
	set.AddPlumbing("GIT_TERMINAL_PROMPT", prompt)

	if c.GitIdentity {
		if c.GitName != "" {
			set.AddPlumbing("GIT_AUTHOR_NAME", c.GitName)
			set.AddPlumbing("GIT_COMMITTER_NAME", c.GitName)
		}
		if c.GitEmail != "" {
			set.AddPlumbing("GIT_AUTHOR_EMAIL", c.GitEmail)
			set.AddPlumbing("GIT_COMMITTER_EMAIL", c.GitEmail)
		}
	}

	if err := c.SetupGit(&set); err != nil {
		return set, err
	}
	c.warnExpiredSecrets()
	return set, nil
}

// admitHostCredential decides whether the credential brig read off the host
// reaches the guest, and forwards it if so.
//
// Two decisions, and neither of them used to be made here. The value went
// straight into the set, so it passed neither the denylist nor the
// unresolved-reference guard that every environment-sourced value passes --
// see creds.AdmitHost -- and an expired token was forwarded and merely warned
// about.
//
// An expired credential is not forwarded. It cannot authenticate anything: the
// expiry is the host's own record of when its access token died, so sending it
// in buys the sandbox nothing and puts a real, if dead, credential inside a
// machine with unrestricted egress -- a token that is worthless to the agent
// is not worthless to whatever else is in there. The run continues rather than
// failing: refusing outright would break a session mid-flight over a
// credential the user may not need for what they are doing, and the sandbox
// asking for a login is a legible outcome, which the warning says out loud.
// BRIG_ALLOW_EXPIRED=1 forwards it anyway, for a host whose clock or whose
// expiry field cannot be trusted.
func (c *Config) admitHostCredential(hc *profile.HostCredential, host *creds.HostCredential, set *creds.Set) {
	if warning, ok := creds.AdmitHost(c.Profile, hc.TargetVar, host.Token, creds.Options{
		AllowRefs:   c.AllowRefs,
		AllowDenied: c.AllowDenied,
	}); !ok {
		c.warnf("%s (from %s)", warning, host.Source)
		return
	}
	if host.Expired(time.Now().UnixMilli()) && !c.AllowExpired {
		c.warnf("NOT forwarding the host's credential: it has expired, so it would "+
			"authenticate nothing and the sandbox would ask you to log in anyway. "+
			"%s, or set BRIG_ALLOW_EXPIRED=1 to send it as it is", hc.RenewHint)
		return
	}
	if host.Expired(time.Now().UnixMilli()) {
		// Quote what the user set, not a hardcoded =1: with the strict reading
		// BRIG_ALLOW_EXPIRED=true is what turned this on, and echoing a value
		// they never wrote sends them looking for a variable that is not there.
		c.warnf("the host's credential has expired and %s is set, "+
			"so it is being forwarded anyway -- %s", c.env.setting("ALLOW_EXPIRED"), hc.RenewHint)
	}
	// AddSecret, not Add: this token came out of the host's keychain, so it
	// wants the same argv exemption a store secret gets. BRIG_ENV_ARGV is a
	// debugging hatch, and no debugging is worth writing a keychain token into
	// a log file that outlives the sandbox.
	set.AddSecret(hc.TargetVar, host.Token, hc.TargetVar+"(host)")
}

// resolveSecrets reads what this run needs out of the store, opening it only
// when something needs it -- so a run that needs no secret raises no keychain
// prompt, the same property BuildEnv already protects for the host credential.
//
// The decision to open is creds.Needed's rather than a length check here, and
// it is a better one: a profile declaring secrets that this run's environment
// already answers no longer opens the store either.
//
// The list is read off the profile rather than copied onto Config: two fields
// holding one list is two fields that can disagree, and the one that would then
// decide whether the store is opened at all is not the one resolution reads.
//
// The sandbox name goes into the error rather than the profile name: the user
// asked to create this sandbox, and that is the name every other brig command
// takes.
func (c *Config) resolveSecrets() (creds.Resolution, error) {
	open := c.OpenStore
	if open == nil {
		open = openStore
	}
	return creds.ResolveSecrets(c.Profile, c.VMName, open, os.LookupEnv)
}

// EnsureRunning brings the sandbox up if it is not already, and makes sure
// the one that is up is mounting this workspace.
func (c *Config) EnsureRunning(set creds.Set) error {
	// BRIG_HYPERVISOR (or BRIG_<PROFILE>_HYPERVISOR) beats what the profile
	// asked for, which is the same order every other setting follows. Resolved
	// once here so the preflight below and the spec built later cannot disagree
	// about which backend this run wants.
	hypervisor := c.env.String("HYPERVISOR", c.Profile.Hypervisor)
	// Refuse a backend the host cannot boot before the workspace is prepared or
	// an image is pulled, so the floor lands as one sentence that names the way
	// past it rather than as the runtime dying at boot with no name. See
	// preflightHypervisor.
	if err := c.preflightHypervisor(hypervisor); err != nil {
		return err
	}
	// Said on the run, not in BuildEnv: BuildEnv resolves the set for every
	// verb, brig env included, so a warning from there prints on a preview that
	// spawns nothing and lands twice on env next to reportArgv. Here it is the
	// run, once, before the workspace is prepared or a value reaches a command
	// line. The set is the whole of what the runtime will be handed: the git
	// plumbing and SetupGit have both added to it.
	c.warnArgvExposure(set)
	if err := c.PrepareWorkspace(); err != nil {
		return err
	}
	// Before the sandbox exists, not only before delivery: a container runtime
	// takes its mounts at create time, so a hostmount whose host path is not
	// there yet is a boot that fails or a directory the runtime invents
	// somewhere brig never looked. Symlink-safe, through the workspace root.
	if err := c.prepareVolumeTargets(); err != nil {
		return err
	}

	if c.Runtime.Running(c.VMName) {
		if c.guestMountsWorkspace() {
			// The guest has confirmed this workspace, so record it. Nothing has
			// changed for a session brig already knows about; for one created
			// before the index existed, this is where its entry appears.
			c.rememberWorkspace()
			// Running a graphical agent again is how you get back to its
			// window, so the focus is not part of the boot -- it belongs on
			// every path that leaves a sandbox running.
			if c.Profile.IsGUI() {
				focusWindow()
			}
			// Delivered on this path too: the tmpfs dies with the sandbox but
			// a running one may have been booted before the secret existed,
			// and rewriting a file the agent already read is how a rotated
			// credential reaches a live session.
			return c.deliverSecretFiles()
		}
		// Recreate rather than fail: all persistent state lives in the
		// workspace on the host, so restarting costs nothing but the boot.
		c.warnf("the running sandbox is not mounting %s -- its share went stale (the "+
			"directory was renamed or replaced, or the workspace changed). Restarting "+
			"it; any other session using this sandbox will be disconnected.", c.Workspace)
		_ = c.Runtime.Stop(c.VMName)
		_ = c.Runtime.Remove(c.VMName)
	}
	// Clear a stale stopped instance holding the name.
	_ = c.Runtime.Remove(c.VMName)

	// Verify before boot, not after: the point is to decide whether to run
	// this image at all.
	if err := c.verifyImage(); err != nil {
		// Tagged so a caller can map a verification refusal to its own exit code:
		// every non-nil return from verifyImage is a refusal to boot, never an
		// incidental error, so the whole thing is the class.
		return &VerifyRefusedError{Err: err}
	}
	// And the kernel the sandbox boots, for a profile that boots a downloaded
	// bundle rather than its own image. Checked here, beside the image, so the
	// two refusals happen at the same point in the boot and under the same
	// setting.
	if err := c.verifyBootAssets(); err != nil {
		return &VerifyRefusedError{Err: err}
	}

	// The share below is a path, and the runtime resolves it again on its own
	// time -- after the image pull, after the VM starts. PrepareWorkspace
	// checked the path much earlier and nothing rechecked it since, so a guest
	// owning a parent component did not even need to win a race: it had a whole
	// boot to swap the component and get the target mounted read-write as its
	// home. Re-checking here, holding a directory handle, is what makes the
	// path we hand over mean what it meant when we looked.
	//
	// This does not make the handover atomic -- nothing can, short of passing a
	// descriptor the runtime accepts -- but it moves the window from "the whole
	// boot" to "between these two statements".
	ws, err := c.openWorkspace()
	if err != nil {
		return err
	}
	defer func() { _ = ws.Close() }()
	if err := ws.verifyStillOurs(); err != nil {
		return err
	}

	c.progressf("starting sandbox %s...", c.VMName)
	// What a runtime that cannot mount after boot needs handed to it now
	// instead. Empty for hull, which execs as root and does the three-phase
	// mount itself; see createTimeVolumes.
	tmpfs, volumeShares := c.createTimeVolumes()
	spec := runtime.RunSpec{
		Name: c.VMName,
		// The tag stays as the image; the resolved digest rides alongside it and
		// the runtime boots Image@Digest when it can pin one. Empty Digest boots
		// the tag, which is the hull path and any run that resolved no digest.
		Image:  c.Image,
		Digest: c.BootDigest,
		Pull:   c.Pull,
		Net:    c.Network.RuntimeNet(),
		Mem:    c.Mem,
		CPUs:   c.CPUs,
		// The workspace, which is the guest's home. The host's agent
		// configuration is copied into it rather than mounted, so it needs no
		// share of its own -- see seedHostConfig. volumeShares is empty on
		// hull and carries the profile's hostmounts on a container runtime.
		Shares: append([]runtime.Share{{Host: ws.dir, Guest: c.Profile.GuestHome}},
			volumeShares...),
		Tmpfs:    tmpfs,
		Env:      set.Vars,
		GUI:      c.Profile.IsGUI(),
		GUITitle: c.env.String("TITLE", c.Profile.GUITitle),
		// How the root is shared and whether the image needs a kernel are
		// facts about the profile, so they travel with it rather than being
		// decided in the runtime adapter.
		RootfsType:  c.env.String("ROOTFS_TYPE", c.Profile.RootfsType),
		GenericBoot: c.Profile.GenericBoot,
		// Resolved once at the top of EnsureRunning, where the preflight also
		// read it, so the backend this spec boots is the one that was checked.
		Hypervisor: hypervisor,
		Counted:    true,
	}
	if err := c.Runtime.Run(spec); err != nil {
		return fmt.Errorf("could not start the sandbox: %w", err)
	}
	// The share is bound now and cannot be changed on a live sandbox, so this
	// is the moment the path becomes a fact about the instance. Recorded before
	// the readiness wait for that reason: a sandbox that boots and never answers
	// still has this workspace, and the next command has to resolve the same one
	// to find out.
	c.rememberWorkspace()
	if !c.waitReady() {
		return fmt.Errorf("sandbox did not become ready; check '%s'",
			c.Runtime.LogsHint(c.VMName))
	}
	if c.Profile.IsGUI() {
		focusWindow()
	}
	return c.deliverSecretFiles()
}

// preflightHypervisor refuses a run whose hypervisor the host cannot boot,
// before the runtime is asked to start anything. It is the host-version
// sibling of runtime.supports, which refuses a graphical profile on a
// console-less backend: same idea, a floor turned into a sentence that names
// the way past it, moved ahead of the runtime so the refusal lands before a
// workspace is prepared or an image is pulled.
//
// It is not the first thing the command does. BuildEnv has already resolved
// credentials by the time EnsureRunning runs, so a keychain prompt, or a
// managed-gitconfig write under BRIG_GIT_CONFIG, can happen before this. What
// it stays ahead of is the runtime's own work, where the unnamed crash lived.
//
// The one floor that exists today is hvi on macOS. The hvi backend drives
// Apple's in-kernel interrupt controller, the hv_gic_* family, which Apple
// shipped first in macOS 15. On macOS 14 those symbols are absent and the VMM
// dies at start with `dyld: missing symbol called` -- a failure with no name
// on it, which reads like a brig bug rather than an OS floor. That is the
// report in #4, seen on 14.5. Six of the eight shipped profiles ask for hvi,
// so the default first run on macOS 14 hits this, and it is worth catching
// here rather than leaving to an unnamed crash.
//
// It refuses rather than quietly falling back to vz. The profile named hvi for
// a reason, and a downgrade nobody chose is its own surprise; naming
// BRIG_HYPERVISOR=vz leaves that choice with the person who can weigh it.
//
// The version is read through c.MacOSVersion, a seam a test can pin. Off macOS
// it reports "", and there is nothing to refuse: hvi is a macOS backend, and
// the Linux runtime ignores the field entirely. A version brig cannot parse
// also proceeds -- blocking a run over a version string we could not read
// would trade the boot crash for a refusal that is just as opaque.
func (c *Config) preflightHypervisor(hv string) error {
	if hv != "hvi" {
		return nil
	}
	version := ""
	if c.MacOSVersion != nil {
		version = c.MacOSVersion()
	}
	if version == "" {
		return nil
	}
	if major, ok := majorVersion(version); !ok || major >= 15 {
		return nil
	}
	return fmt.Errorf("the hvi hypervisor needs macOS 15 or newer (this is %s): "+
		"its in-kernel interrupt controller does not exist here. "+
		"Set BRIG_HYPERVISOR=vz for this run, or upgrade macOS", version)
}

// majorVersion pulls the major number out of a "15.4.1"-style version string,
// reporting false when there is no number to read.
func majorVersion(v string) (int, bool) {
	major, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(strings.TrimSpace(major))
	if err != nil {
		return 0, false
	}
	return n, true
}

// waitReady waits for the in-guest agent.
//
// A running instance is not yet a reachable one: the runtime marks an
// instance running as soon as the VMM process starts, while the guest agent
// binds its listener a few seconds later. Everything that asks the guest a
// question waits here first.
func (c *Config) waitReady() bool {
	deadline := time.Now().Add(c.ReadyTimeout)
	for {
		if c.Runtime.Probe(runtime.ExecSpec{Name: c.VMName, Cmd: []string{"/bin/true"}}) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// guestMountsWorkspace asks the guest which workspace it actually has.
//
// Only a reachable guest can answer for its own share. An exec that cannot
// land says nothing about the mount, and treating that silence as "stale" is
// how a second invocation would destroy a VM the first one is still booting.
func (c *Config) guestMountsWorkspace() bool {
	// Through the root like every other workspace access: the marker is read
	// back out of the guest's own home, and following a symlink there would
	// compare the guest's answer against some unrelated host file.
	r, err := c.openWorkspace()
	if err != nil {
		return false
	}
	defer func() { _ = r.Close() }()
	want, err := r.readFile(markerFile)
	if err != nil {
		return false
	}
	if !c.waitReady() {
		return false
	}
	seen, err := c.Runtime.Output(runtime.ExecSpec{
		Name: c.VMName,
		Cmd:  []string{"cat", c.Profile.GuestHome + "/" + markerFile},
	})
	if err != nil {
		return false
	}
	return strings.TrimSpace(seen) == strings.TrimSpace(string(want))
}

// Stop shuts the sandbox down and leaves it there.
//
// The wrapper this was ported from removed the instance as well, because it
// had no other verb. Now that `brig rm` exists, stop is the reversible half:
// the sandbox keeps its name and its identity, and starting it again is a
// boot rather than a fresh creation. A sandbox that was not running is not a
// failure worth reporting -- the end state is the one that was asked for.
//
// A sandbox that would not stop IS worth reporting, and used to be swallowed:
// `brig stop` discarded the runtime's error and exited 0, so a VM still
// running with a forwarded credential in it read as one that was gone. The end
// state is still what decides -- an instance that is no longer running when
// the runtime complains was already in the state that was asked for -- so the
// runtime is asked again before its error is believed, which is also what
// keeps "it was not running to begin with" from becoming a failure.
func (c *Config) Stop() error {
	err := c.Runtime.Stop(c.VMName)
	if err == nil || !c.Runtime.Running(c.VMName) {
		return nil
	}
	return fmt.Errorf("the sandbox %s is still running and would not stop (%s): %w",
		c.VMName, c.Runtime.LogsHint(c.VMName), err)
}

// Remove stops the sandbox and clears the instance holding its name. The
// workspace is untouched: it lives on the host and holds your work. Only the
// index entry goes, so the next sandbox to take this name resolves its
// workspace the ordinary way instead of inheriting one chosen for a sandbox
// that no longer exists.
//
// Pruned whether or not the runtime could remove the instance, which is how
// hull releases the gateway address it hands out: the entry describes a sandbox
// the user has asked to be rid of either way, and a removal that failed is
// reported on its own.
func (c *Config) Remove() error {
	_ = c.Runtime.Stop(c.VMName)
	err := c.Runtime.Remove(c.VMName)
	ForgetWorkspace(c.VMName)
	return err
}

// Exec hands the terminal to a command inside the sandbox. It does not return
// on success.
func (c *Config) Exec(set creds.Set, argv []string, tty bool) error {
	return c.Runtime.Replace(runtime.ExecSpec{
		Name: c.VMName,
		Cmd:  argv,
		Cwd:  c.GuestCwd,
		TTY:  tty,
		// Whether hull may ask its consent question is a fact about brig's own
		// stdin, not about the guest's pty. Shell forces TTY on so a login shell
		// gets its terminal, but a `brig shell -- cmd` from a script still has
		// no one to answer, so it must not read as askable. Compute it here,
		// once, from the real stdin rather than reusing tty. See telemetryEnvFor.
		CanAsk:  IsTerminal(os.Stdin),
		Env:     set.Vars,
		Counted: true,
	})
}

// Shell opens a login shell in the sandbox, or runs one command in it.
//
// The trailing words are joined into a single string before the shell sees
// them, so they land as one argument -- the script text for -c -- rather than
// one per word. Passed individually, bash takes the first as the script and
// the rest as $0, $1, ...
func (c *Config) Shell(set creds.Set, command []string) error {
	argv := []string{"bash", "-l"}
	if len(command) > 0 {
		argv = []string{"bash", "-lc", strings.Join(command, " ")}
	}
	return c.Exec(set, argv, true)
}
