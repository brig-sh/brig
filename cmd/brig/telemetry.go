package main

import (
	"fmt"
	"io"

	"github.com/brig-sh/brig/internal/runtime"
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

brig has no collector and no account. The counting is done by the sandbox
runtime underneath, and these verbs set the answer it keeps, so an opt-out
holds whether brig is the caller or not. DO_NOT_TRACK=1 in your environment
turns it off without recording anything, and beats both.

The telemetry section of the README lists every field an event carries, who
receives it, and what is never collected.
`

// telemetryCmd reports and sets the telemetry answer.
//
// The whole point of the command is that you do not have to know what brig
// drives underneath to opt out, so neither the report nor the help names the
// runtime: a user who reads "local-first, no account" and then wants the
// network call to stop should find the switch under brig's own name. The state
// is the runtime's, though, not a second store of brig's -- see SetTelemetry.
func telemetryCmd(out io.Writer, args []string) error {
	verb := "status"
	if len(args) > 0 {
		verb = args[0]
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

	rt, err := detectRuntime()
	if err != nil {
		return err
	}
	reporter, ok := rt.(runtime.TelemetryReporter)
	if !ok {
		// A backend that collects nothing has nothing to report and nothing to
		// turn off, and saying so is more use than an error: the question the
		// user asked -- is anything being sent -- has an answer here, and it is
		// no.
		_, err := io.WriteString(out, "telemetry: off\n"+
			"The sandbox runtime on this host sends nothing, so there is nothing\n"+
			"to turn off.\n")
		return err
	}
	if verb != "status" {
		if err := reporter.SetTelemetry(verb == "on"); err != nil {
			return err
		}
	}
	// Report the state after setting it rather than echoing what was asked
	// for. They differ whenever DO_NOT_TRACK is set: the answer is recorded,
	// and the variable still wins, which is exactly the case a confident
	// "telemetry: on" would misreport.
	state, err := reporter.TelemetryStatus()
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, telemetryReport(state))
	return err
}

// telemetryReport is the state in brig's words, with the sentence that tells
// the reader what to do about it.
func telemetryReport(state runtime.Telemetry) string {
	switch state.State {
	case runtime.TelemetryOn:
		return "telemetry: on\n" +
			"A sandbox boot and the command that takes your terminal each count\n" +
			"once. `brig telemetry off` stops it.\n"
	case runtime.TelemetryOff:
		if state.Setting != "" {
			return "telemetry: off\n" +
				state.Setting + " in this environment turns it off, and beats any\n" +
				"answer recorded on this machine.\n"
		}
		return "telemetry: off\n" +
			"The answer is recorded on this machine. `brig telemetry on` reverses it.\n"
	case runtime.TelemetryUnanswered:
		return "telemetry: not answered yet\n" +
			"Nothing goes out for a sandbox boot until you answer, and the first\n" +
			"command that hands your terminal to an agent asks the question.\n" +
			"`brig telemetry on` or `brig telemetry off` answers it now.\n"
	default:
		return "telemetry: cannot tell\n" +
			"The sandbox runtime did not report a state this version of brig\n" +
			"recognises. Boots are not counted while that is true.\n"
	}
}
