package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/brig-sh/brig/internal/notice"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/session"
	"github.com/brig-sh/brig/internal/telemetry"
	"github.com/brig-sh/brig/internal/wrap"
)

// detectRuntime is the seam the CLI tests replace. Everything else goes
// through runtime.Detect, which picks the backend for this host.
var detectRuntime = runtime.Detect

// telemetryUsage is what `brig telemetry --help` prints.
const telemetryUsage = `brig telemetry -- what is counted, and how to stop it

usage:
  brig telemetry status   report whether usage data is being sent
  brig telemetry on       start sending it
  brig telemetry off      stop sending it, on this machine, for good

brig has no account. The answer is kept on this machine, and it also covers
the sandbox runtime underneath, so an opt-out holds whether brig is the caller
or not. DO_NOT_TRACK=1 in your environment turns it off without recording
anything, and beats both.

docs/telemetry.md lists every field an event carries, who receives it, and
what is never collected.
`

// telemetryCmd reports and sets the telemetry answer.
//
// The whole point of the command is that you do not have to know what brig
// drives underneath to opt out, so neither the report nor the help names the
// runtime: a user who reads "local-first, no account" and then wants the
// network call to stop should find the switch under brig's own name. The
// answer is the one the runtime reads too, not a second store of brig's: see
// internal/telemetry.
func telemetryCmd(out io.Writer, args []string) error {
	verb := "status"
	if len(args) > 0 {
		verb = args[0]
	}
	if verb == "flush" {
		// Not in the usage: spawnUploader runs it, a person has no reason to.
		// This process starts no uploader of its own, not even for a crash
		// report of its own, or one panic would start the next for ever.
		telemetry.SpawnUploader = nil
		if len(args) == 2 && args[1] == "--now" {
			telemetry.Upload()
			return nil
		}
		startUploader()
		return nil
	}
	if len(args) > 1 {
		return fmt.Errorf("telemetry takes one word: status, on or off")
	}
	switch verb {
	case "--help", "-h", "help":
		_, err := io.WriteString(out, telemetryUsage)
		return err
	case "status", "on", "off":
	default:
		return fmt.Errorf("unknown telemetry subcommand %q (status, on or off)", verb)
	}

	if verb != "status" {
		if err := telemetry.Set(verb == "on"); err != nil {
			return err
		}
	}
	// Report the state after setting it rather than echoing what was asked
	// for. They differ whenever DO_NOT_TRACK is set: the answer is recorded,
	// and the variable still wins, which is exactly the case a confident
	// "telemetry: on" would misreport.
	answer, setting, err := telemetry.Status()
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, telemetryReport(answer, setting, telemetry.InstallID()))
	return err
}

// telemetryReport is the state in brig's words, laid out as a block, with the
// install id when there is one and the command that changes the state.
func telemetryReport(answer telemetry.Answer, setting, id string) string {
	var b *notice.Block
	switch answer {
	case telemetry.On:
		b = notice.New("telemetry: on").
			Note("each brig command counts once").
			Note("on macOS, a sandbox also reports its boot and how long it ran")
	case telemetry.Off:
		b = notice.New("telemetry: off")
		if setting != "" {
			b.Note("%s in this environment turns it off", setting).
				Note("it beats any answer recorded on this machine")
		} else {
			b.Note("the answer is recorded on this machine")
		}
	default:
		b = notice.New("telemetry: not answered yet").
			Note("nothing goes out until you answer").
			Note("the first `brig run` or `brig sh` on a terminal asks the question")
	}
	if id != "" {
		b.Note("install id: %s", id)
	}
	switch {
	case answer == telemetry.On:
		b.Do("to turn it off", "brig telemetry off")
	case answer == telemetry.Off && setting != "":
		b.Do("to let the recorded answer decide", "unset "+strings.TrimSuffix(setting, "=1"))
	case answer == telemetry.Off:
		b.Do("to turn it on", "brig telemetry on")
	default:
		b.Do("to turn it on", "brig telemetry on").Do("to turn it off", "brig telemetry off")
	}
	return b.String() + "\n"
}

// startTelemetry is nil unless main set it, so a test that drives dispatch
// reads and writes no telemetry state.
var startTelemetry func(command string, interactive bool)

// countedVerbs maps every verb dispatch answers to the command its event
// names, and whether that command may ask the consent question.
//
// The event names one of these and nothing else. A word typed where a verb
// goes can be anything -- an image reference, a path, a mistyped secret -- so
// it is sent as "unknown". telemetry and the completion script are absent:
// the first is how you answer, and the second runs in every new shell.
var countedVerbs = map[string]struct {
	command string
	asks    bool
}{
	"run": {"run", true}, "sh": {"sh", true}, "create": {"create", false}, "exec": {"exec", false},
	"stop": {"stop", false}, "rm": {"rm", false}, "reset": {"reset", false}, "ls": {"ls", false},
	"logs": {"logs", false}, "info": {"info", false}, "env": {"env", false}, "plan": {"plan", false},
	"network": {"network", false}, "agent": {"agent", false}, "agents": {"agents", false},
	"profile": {"profile", false}, "profiles": {"profiles", false}, "template": {"template", false},
	"import": {"import", false}, "export": {"export", false}, "policy": {"policy", false},
	"policies": {"policies", false}, "secret": {"secret", false}, "doctor": {"doctor", false},
	"version": {"version", false}, "--version": {"version", false},
	"help": {"help", false}, "-h": {"help", false}, "--help": {"help", false},
}

// unknownCommand is the command sent for a word that is no verb.
const unknownCommand = "unknown"

// pendingTelemetry is the command of a run or a shell whose telemetry waits
// for its line to be read. See beginTelemetry.
var pendingTelemetry string

// beginTelemetry starts telemetry for verb, the word in the verb position.
//
// Only a run or a shell on a terminal asks the question. That is the command
// someone is watching, and the one whose events brig would otherwise send
// first. Whether it may ask depends on the run line -- a script's --json or
// -q is never interrupted -- so a run or a shell starts in askTelemetry, once
// the line is read and before anything boots.
func beginTelemetry(verb string) {
	pendingTelemetry = ""
	if startTelemetry == nil {
		return
	}
	command, asks := telemetryCommand(verb)
	switch {
	case command == "":
	case asks:
		pendingTelemetry = command
	default:
		startTelemetry(command, false)
	}
}

// askTelemetry starts the telemetry beginTelemetry left waiting, asking the
// question when nobody has answered and brig may ask: a terminal on stdin and
// stderr, brig in its foreground, no CI, and no --json or -q.
func askTelemetry(wantJSON bool) {
	command := pendingTelemetry
	if command == "" || startTelemetry == nil {
		return
	}
	pendingTelemetry = ""
	startTelemetry(command, mayAsk(wantJSON))
}

// mayAsk reports whether this invocation may put the consent question to
// someone. A job in the background that reads the terminal is stopped.
func mayAsk(wantJSON bool) bool {
	return !wantJSON && verbosity > wrap.Quiet && os.Getenv("CI") == "" && atForegroundTerminal()
}

// atForegroundTerminal reports whether stdin and stderr are a terminal and
// brig runs in its foreground. It is a variable so a test can be a terminal.
var atForegroundTerminal = func() bool {
	return wrap.IsTerminal(os.Stdin) && wrap.IsTerminal(os.Stderr) && wrap.InForeground(os.Stdin)
}

// telemetryCommand returns the command an event names for the word in the
// verb position, and whether it may ask. A bare ref is a run. It returns ""
// for a verb that is not counted.
func telemetryCommand(verb string) (command string, asks bool) {
	if verb == "telemetry" || verb == "completion" {
		return "", false
	}
	if v, ok := countedVerbs[verb]; ok {
		return v.command, v.asks
	}
	if ref, err := session.ParseRef(verb); err == nil {
		if _, known := profile.Lookup(ref.Agent); known {
			return "run", true
		}
	}
	return unknownCommand, false
}

// finishTelemetry sends the command event for how this invocation ended. An
// agent's own exit status under --json is the agent's, and brig did its part.
func finishTelemetry(err error) {
	// A run that failed before its line was read still counts, as a
	// command that asked nobody anything.
	if pendingTelemetry != "" && startTelemetry != nil {
		startTelemetry(pendingTelemetry, false)
		pendingTelemetry = ""
	}
	var ae *agentExit
	if err == nil || errors.As(err, &ae) {
		telemetry.Finish("ok", "")
		return
	}
	telemetry.Finish("error", errorClass(exitCode(err)))
}

// errorClass names an exit status for the command event: the class
// docs/cli.md documents for it, never the message.
func errorClass(code int) string {
	switch code {
	case exitUsage:
		return "usage"
	case exitNotFound:
		return "not-found"
	case exitRuntime:
		return "runtime"
	case exitVerify:
		return "verify"
	case exitCredentials:
		return "credentials"
	case exitCapability:
		return "capability"
	default:
		return "other"
	}
}

// handlePanic queues a crash report for a panic on brig's main goroutine and
// then crashes the way Go does: the panic and its stack on stderr, exit 2.
func handlePanic(recovered any) {
	stack := debug.Stack()
	// A run that panics before its line is read has not started telemetry
	// yet. It starts now, without asking, so the crash is not lost.
	if command := pendingTelemetry; command != "" && startTelemetry != nil {
		pendingTelemetry = ""
		startTelemetry(command, false)
	}
	telemetry.CapturePanic(recovered, stack)
	fmt.Fprintf(os.Stderr, "panic: %v\n\n%s", recovered, stack)
	exitOnPanic(2)
}

// exitOnPanic ends the process after a panic, as a variable so a test can
// stay alive.
var exitOnPanic = os.Exit

// spawnUploader uploads the telemetry queues from a process that outlives
// this one. It runs `brig telemetry flush` and waits for it. That process
// starts the uploader, `brig telemetry flush --now`, and exits at once, so
// the uploader has no parent left that could fail to wait for it: not this
// brig, and not the hull or nerdctl a handover execs into its place.
func spawnUploader() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "telemetry", "flush")
	cmd.Dir = "/"
	_ = cmd.Run()
}

// startUploader starts the uploader in a session of its own, with no
// terminal and no stdio, and does not wait for it.
func startUploader() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := uploaderCmd(exe)
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}

// uploaderCmd returns the command startUploader starts.
func uploaderCmd(exe string) *exec.Cmd {
	cmd := exec.Command(exe, "telemetry", "flush", "--now")
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}
