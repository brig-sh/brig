package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"

	"github.com/brig-sh/brig/internal/notice"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/wrap"
)

// A teleported session runs on the host it was moved to, and the verbs that
// act on its sandbox run there too: brig hands the same verb, the same ref
// and the same flags to the brig on that host, over ssh, and that brig does
// what it would do for one of its own sessions. This host resolves nothing
// for it, credentials included: the guest holds what it held, and an exec
// there carries what that host's store has. See docs/teleport.md.
//
// The remote command line is a string a shell on the other host parses
// again, so every word on it is quoted here, the host is checked to be one
// word ssh cannot read as an option, and `--` ends ssh's own options before
// it. Nothing here passes through a local shell.

const (
	// sshSetting names the ssh brig runs, for a test or a wrapper. Unset is
	// the ssh on PATH.
	sshSetting = "BRIG_TELEPORT_SSH"
	// remoteBrigSetting names the brig to run on the other host, for one
	// that is not on the PATH a non-interactive ssh session gets there.
	remoteBrigSetting = "BRIG_TELEPORT_REMOTE_BRIG"
	// remoteEnvSetting is NAME=VALUE words put in front of every remote brig
	// command, for a destination that needs settings its login does not
	// give it: two brigs on one host, each with its own state and store, is
	// the case it exists for.
	remoteEnvSetting = "BRIG_TELEPORT_REMOTE_ENV"
)

// remoteCall is one brig command on another host.
type remoteCall struct {
	host string
	// store is the hull store the session runs in there, handed to that brig
	// as BRIG_HULL_STORE_DIR, or "" for its own default.
	store string
	// tty asks ssh for a terminal on the other end, for a shell or an agent
	// someone is typing into.
	tty bool
	// batch makes ssh fail rather than ask for a password, for the calls
	// brig makes on its own account: nobody is watching for a prompt there.
	batch bool
	// args is the brig command line there, after the binary.
	args []string
}

// envName is what a remote setting's name may be: the shape of a variable,
// so a word cannot carry a second assignment or a command.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// remoteEnv is the NAME=VALUE words of BRIG_TELEPORT_REMOTE_ENV, each
// checked, in order.
func remoteEnv() ([]string, error) {
	var out []string
	for _, word := range strings.Fields(os.Getenv(remoteEnvSetting)) {
		name, _, ok := strings.Cut(word, "=")
		if !ok || !envName.MatchString(name) {
			return nil, fmt.Errorf("%s holds %q, and every word there must be NAME=VALUE", remoteEnvSetting, word)
		}
		out = append(out, word)
	}
	return out, nil
}

// sshArgv is the one place a command for another host is built: the ssh
// binary, its options, `--`, the host, and the remote command as one quoted
// string.
func sshArgv(c remoteCall) ([]string, error) {
	if err := runtime.CheckRemote(c.host); err != nil {
		return nil, usagef("%s", err)
	}
	env, err := remoteEnv()
	if err != nil {
		return nil, err
	}
	if c.store != "" {
		env = append(env, runtime.HullStoreDir+"="+c.store)
	}
	var remote []string
	if len(env) > 0 {
		remote = append(append(remote, "env"), env...)
	}
	brig := os.Getenv(remoteBrigSetting)
	if brig == "" {
		brig = "brig"
	}
	remote = append(append(remote, brig), c.args...)

	ssh := os.Getenv(sshSetting)
	if ssh == "" {
		ssh = "ssh"
	}
	argv := []string{ssh}
	if c.tty {
		argv = append(argv, "-t")
	}
	if c.batch {
		argv = append(argv, "-o", "BatchMode=yes")
	}
	return append(argv, "--", c.host, shellJoin(remote)), nil
}

// safeWord is what a POSIX shell reads as itself, unquoted.
var safeWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuote is one word as a POSIX shell reads it back: bare when nothing
// in it is special, single-quoted otherwise, with each single quote closed,
// escaped and reopened.
func shellQuote(word string) string {
	if safeWord.MatchString(word) {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

func shellJoin(words []string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = shellQuote(w)
	}
	return strings.Join(quoted, " ")
}

// The ways a remote command is run, behind variables so a test can see the
// argv without a process: replaced by ssh, as a child that keeps brig's
// terminal, or as a child whose output brig reads.
var (
	execRemote   = execSSH
	attachRemote = runtime.RunAttached
	outputRemote = outputSSH
)

func execSSH(argv []string) error {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("cannot run %s to reach the session's host: %w", argv[0], err)
	}
	return syscall.Exec(path, argv, os.Environ())
}

// outputSSH runs argv and returns what it printed, with ssh's own error and
// the remote brig's folded into the error when it fails.
func outputSSH(argv []string) ([]byte, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		said := strings.TrimSpace(errb.String())
		if said != "" {
			return out.Bytes(), fmt.Errorf("%w: %s", err, said)
		}
	}
	return out.Bytes(), err
}

// remoteVerbs are the verbs a moved session answers on its host. They act on
// the sandbox, and the sandbox is there.
var remoteVerbs = map[string]bool{"run": true, "sh": true, "stop": true, "info": true}

// remoteVerb answers a verb on a session the index says is away, or says it
// did not. rm and teleport are answered here, by this host: rm removes the
// copy left behind, and teleport asks about the move.
func remoteVerb(verb string, g globals, rest []string, t profile.Profile, loc wrap.Location, wantJSON bool) (bool, error) {
	switch {
	case verb == "rm" || verb == "teleport":
		return false, nil
	case loc.PendingHost != "":
		return true, fmt.Errorf("%s is being moved to %s (move %s), and whether the move finished is not "+
			"known here. `brig teleport %s --status` finds out, and `brig teleport %s --abort` calls it off",
			loc.Ref, loc.PendingHost, loc.TxID, loc.Ref, loc.Ref)
	case !remoteVerbs[verb]:
		return true, fmt.Errorf("%s runs on %s, and `brig %s` acts on this host. Run it there: "+
			"ssh %s brig %s %s", loc.Ref, loc.Host, verb, loc.Host, verb, loc.Ref)
	}
	args := append(globalWords(g), verb)
	args = append(args, rest...)
	interactive := (verb == "sh" || verb == "run") && isTerminal()
	argv, err := sshArgv(remoteCall{host: loc.Host, store: loc.RemoteStore, tty: interactive, args: args})
	if err != nil {
		return true, err
	}
	if verbosity > wrap.Quiet && !wantJSON {
		warnf("%s", notice.Newf("%s runs on %s", loc.Ref, loc.Host).
			Note("brig hands `%s` to the brig there, over ssh", verb))
	}
	if wantJSON && (verb == "run" || verb == "sh") {
		// The brig there prints the Run line. A child, not a handover, so an
		// ssh that never reached it still ends in one Run line from here.
		code, err := attachRemote(argv, os.Environ())
		if err != nil {
			return true, err
		}
		if code == sshFailed {
			return true, fmt.Errorf("ssh to %s failed, so the brig there was not asked", loc.Host)
		}
		return true, &agentExit{code: code}
	}
	return true, execRemote(argv)
}

// sshFailed is the status ssh exits with when it fails itself, rather than
// passing on the remote command's.
const sshFailed = 255

// globalWords is brig's global flags as the line had them, for the brig on
// the other host to read the same way.
func globalWords(g globals) []string {
	var out []string
	if g.quiet {
		out = append(out, "-q")
	}
	if g.verbose {
		out = append(out, "--verbose")
	}
	if g.json {
		out = append(out, "--json")
	}
	return out
}

// remoteLogs answers `brig logs` on a moved session from its host, with the
// line as it was typed.
func remoteLogs(args []string, loc wrap.Location) error {
	if loc.PendingHost != "" {
		_, err := remoteVerb("logs", globals{}, nil, profile.Profile{}, loc, false)
		return err
	}
	argv, err := sshArgv(remoteCall{host: loc.Host, store: loc.RemoteStore,
		args: append(append(globalWords(globals{quiet: verbosity <= wrap.Quiet}), "logs"), args...)})
	if err != nil {
		return err
	}
	return execRemote(argv)
}

// remoteOutput runs one brig command on host on brig's own account and
// returns what it printed.
func remoteOutput(host, store string, args ...string) ([]byte, error) {
	argv, err := sshArgv(remoteCall{host: host, store: store, batch: true, args: args})
	if err != nil {
		return nil, err
	}
	return outputRemote(argv)
}

// removeMovedCopy is `brig rm` on a session that runs on another host. What
// is here is the copy the move left: a guest fenced by its checkpoint, which
// can never run again. rm removes it once the brig on the other host says
// the session runs there, or once hull's own record here says the move
// committed (see staleByHullsRecord), and asks nothing else: it is not the
// session. It then forgets the session on this host, as it does when no copy
// is left here, and leaves the one there alone. One rm leaves this host ready
// to take the session back: a record kept here would only say where it runs,
// and the old copy's removal is what a move back needs.
func removeMovedCopy(cfg *wrap.Config, t profile.Profile, ref string, loc wrap.Location, dryRun bool) error {
	vm := cfg.VMName
	if loc.PendingHost != "" {
		return fmt.Errorf("a move of %s to %s (%s) has not finished, and the copy here may be the only "+
			"one. `brig teleport %s --status` finds out where it stands", ref, loc.PendingHost, loc.TxID, ref)
	}
	present, err := sandboxPresent(cfg.Runtime, vm)
	if err != nil {
		return err
	}
	if !present {
		if dryRun {
			fmt.Printf("would forget %s on this host; it runs on %s, which keeps it\n", ref, loc.Host)
			return nil
		}
		if err := wrap.ForgetRemoteSession(loc.Ref, vm); err != nil {
			return err
		}
		fmt.Printf("forgot %s on this host; it runs on %s, untouched there\n", ref, loc.Host)
		warnf("%s", notice.Newf("ssh %s brig rm %s removes it there", loc.Host, ref))
		return nil
	}
	if !staleByHullsRecord(cfg.Runtime, vm, loc.TxID) {
		if err := confirmRunsThere(loc, t.Name, vm); err != nil {
			return fmt.Errorf("the copy of %s here is kept: %w", ref, err)
		}
	}
	if dryRun {
		fmt.Printf("would remove the copy of %s left on this host (sandbox %s) and forget it here; it runs on %s\n",
			ref, vm, loc.Host)
		return nil
	}
	if err := cfg.Remove(); err != nil {
		return err
	}
	if err := wrap.ForgetRemoteSession(loc.Ref, vm); err != nil {
		return fmt.Errorf("removed the copy of %s left on this host, and could not forget it here: %w", ref, err)
	}
	fmt.Printf("removed the copy of %s left on this host and forgot it here; it runs on %s, untouched there\n",
		ref, loc.Host)
	warnf("%s", notice.Newf("ssh %s brig rm %s removes it there", loc.Host, ref))
	return nil
}

// staleByHullsRecord reports whether hull's own record of the copy here says
// the move txid committed away from this host: the checkpoint is migrated,
// and hull refuses to restore, resume or move it, so it can never run again.
// hull writes that record only after the destination answered the commit, so
// it is proof the copy is not the only one, and rm needs no word from the
// destination. That word cannot always be had: the session may since have
// been removed there, or moved on, or that host may be gone. A record that
// does not read, or says anything else, proves nothing, and rm asks.
func staleByHullsRecord(rt runtime.Runtime, vm, txid string) bool {
	mi, ok := rt.(runtime.MigrationInspector)
	if !ok || txid == "" {
		return false
	}
	m, err := mi.Migration(vm)
	return err == nil && m.State == runtime.MigrationMigrated && m.MoveTxID == txid
}

// confirmRunsThere asks the brig on the session's host whether its sandbox
// runs there. Only a yes lets rm take the copy here: when the move's end is
// in doubt, the copy here may be the only one.
func confirmRunsThere(loc wrap.Location, agent, vm string) error {
	out, err := remoteOutput(loc.Host, loc.RemoteStore, probeVerb, "--json", "--agent", agent, "--sandbox", vm)
	if err != nil {
		return fmt.Errorf("could not ask %s whether %s runs there: %w", loc.Host, vm, err)
	}
	var doc struct {
		Kind string      `json:"kind"`
		Data probeReport `json:"data"`
	}
	if err := json.Unmarshal(out, &doc); err != nil || doc.Kind != "TeleportProbe" || doc.Data.Sandbox == nil {
		return fmt.Errorf("%s did not say whether %s runs there", loc.Host, vm)
	}
	if !doc.Data.Sandbox.Running {
		return fmt.Errorf("%s says %s is not running there", loc.Host, vm)
	}
	return nil
}
