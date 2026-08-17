package wrap

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/brig-sh/brig/internal/creds"
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
	secrets, err := c.resolveSecrets()
	if err != nil {
		return creds.Set{}, err
	}
	set := creds.Bind(c.Profile, c.Env, secrets, os.LookupEnv, creds.Options{
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
	if hc := c.Profile.HostCredential; hc != nil && !set.Has(hc.TargetVar) {
		host, err := creds.ReadHost(hc, c.CredentialsCmd)
		if err != nil {
			c.warnf("%s", err)
		} else if host != nil {
			c.HostCred = host
			// AddSecret, not Add: this token came out of the host's
			// keychain, so it wants the same argv exemption a store
			// secret gets. BRIG_ENV_ARGV is a debugging hatch, and no
			// debugging is worth writing a keychain token into a log
			// file that outlives the sandbox.
			set.AddSecret(hc.TargetVar, host.Token, hc.TargetVar+"(host)")
			if host.Expired(time.Now().UnixMilli()) {
				// An expired token means the host has not run the agent in a
				// while. The sandbox would ask for a login and the reason
				// would not be obvious from inside it.
				c.warnf("the host's credential has expired -- %s, or the sandbox will "+
					"ask you to log in", hc.RenewHint)
			}
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
	return set, nil
}

// resolveSecrets reads the profile's requirement list, opening the store only if
// there is one -- so a run that needs no secret raises no keychain prompt, the
// same property BuildEnv already protects for the host credential.
//
// The list is read off the profile rather than copied onto Config: two fields
// holding one list is two fields that can disagree, and the one that would then
// decide whether the store is opened at all is not the one resolution reads.
//
// The sandbox name goes into the error rather than the profile name: the user
// asked to create this sandbox, and that is the name every other brig command
// takes.
func (c *Config) resolveSecrets() (map[string]string, error) {
	if len(c.Profile.Secrets) == 0 {
		return nil, nil
	}
	open := c.OpenStore
	if open == nil {
		open = openStore
	}
	store, err := open()
	if err != nil {
		return nil, err
	}
	return creds.ResolveSecrets(c.Profile, c.VMName, store)
}

// EnsureRunning brings the sandbox up if it is not already, and makes sure
// the one that is up is mounting this workspace.
func (c *Config) EnsureRunning(set creds.Set) error {
	if err := c.PrepareWorkspace(); err != nil {
		return err
	}

	if c.Runtime.Running(c.VMName) {
		if c.guestMountsWorkspace() {
			// Running a graphical agent again is how you get back to its
			// window, so the focus is not part of the boot -- it belongs on
			// every path that leaves a sandbox running.
			if c.Profile.IsGUI() {
				focusWindow()
			}
			return nil
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
		return err
	}

	fmt.Fprintf(c.Err, "brig: starting sandbox %s...\n", c.VMName)
	spec := runtime.RunSpec{
		Name:  c.VMName,
		Image: c.Image,
		Pull:  c.Pull,
		Net:   "shared",
		Mem:   c.Mem,
		CPUs:  c.CPUs,
		// One share: the workspace, which is the guest's home. The host's
		// agent configuration is copied into it rather than mounted, so it
		// needs no share of its own -- see seedHostConfig.
		Shares:   []runtime.Share{{Host: c.Workspace, Guest: c.Profile.GuestHome}},
		Env:      set.Vars,
		GUI:      c.Profile.IsGUI(),
		GUITitle: c.env.String("TITLE", c.Profile.GUITitle),
		// How the root is shared and whether the image needs a kernel are
		// facts about the profile, so they travel with it rather than being
		// decided in the runtime adapter.
		RootfsType:  c.env.String("ROOTFS_TYPE", c.Profile.RootfsType),
		GenericBoot: c.Profile.GenericBoot,
		// BRIG_HYPERVISOR (or BRIG_<PROFILE>_HYPERVISOR) beats what the
		// profile asked for, which is the same order every other setting
		// follows.
		Hypervisor: c.env.String("HYPERVISOR", c.Profile.Hypervisor),
		Counted:    true,
	}
	if err := c.Runtime.Run(spec); err != nil {
		return fmt.Errorf("could not start the sandbox: %w", err)
	}
	if !c.waitReady() {
		return fmt.Errorf("sandbox did not become ready; check '%s'",
			c.Runtime.LogsHint(c.VMName))
	}
	if c.Profile.IsGUI() {
		focusWindow()
	}
	return nil
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
func (c *Config) Stop() error {
	_ = c.Runtime.Stop(c.VMName)
	return nil
}

// Remove stops the sandbox and clears the instance holding its name. The
// workspace is untouched: it lives on the host and holds your work.
func (c *Config) Remove() error {
	_ = c.Runtime.Stop(c.VMName)
	return c.Runtime.Remove(c.VMName)
}

// Exec hands the terminal to a command inside the sandbox. It does not return
// on success.
func (c *Config) Exec(set creds.Set, argv []string, tty bool) error {
	return c.Runtime.Replace(runtime.ExecSpec{
		Name:    c.VMName,
		Cmd:     argv,
		Cwd:     c.GuestCwd,
		TTY:     tty,
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
