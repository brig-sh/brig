package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/runtime"
)

// fakeRuntime is a runtime that reports telemetry and nothing else. The
// embedded interface supplies the sandbox methods, which this command never
// calls: a nil method that panics if it is ever reached is a better test than
// a dozen stubs that quietly do nothing.
type fakeRuntime struct {
	runtime.Runtime
	state runtime.Telemetry
	set   []bool
	// sticky is a state a recorded answer does not move, which is what a
	// variable in the environment is.
	sticky bool
}

func (f *fakeRuntime) TelemetryStatus() (runtime.Telemetry, error) { return f.state, nil }

func (f *fakeRuntime) SetTelemetry(on bool) error {
	f.set = append(f.set, on)
	if f.sticky {
		return nil
	}
	f.state = runtime.Telemetry{State: runtime.TelemetryOff}
	if on {
		f.state = runtime.Telemetry{State: runtime.TelemetryOn}
	}
	return nil
}

// quiet is a runtime that collects nothing, which is what nerdctl is.
type quietRuntime struct{ runtime.Runtime }

func withRuntime(t *testing.T, rt runtime.Runtime) {
	t.Helper()
	prev := detectRuntime
	detectRuntime = func() (runtime.Runtime, error) { return rt, nil }
	t.Cleanup(func() { detectRuntime = prev })
}

func telemetryOutput(t *testing.T, rt runtime.Runtime, args ...string) string {
	t.Helper()
	withRuntime(t, rt)
	var out bytes.Buffer
	if err := telemetryCmd(&out, args); err != nil {
		t.Fatalf("telemetry %v: %v", args, err)
	}
	return out.String()
}

// The command exists so that opting out does not require knowing what brig
// drives underneath. A report that named it would defeat that: the user would
// still be looking up another tool's documentation to understand their own
// answer.
func TestTelemetryStatusDoesNotNameTheRuntime(t *testing.T) {
	for _, state := range []runtime.Telemetry{
		{State: runtime.TelemetryOn},
		{State: runtime.TelemetryOff},
		{State: runtime.TelemetryOff, Setting: "DO_NOT_TRACK=1"},
		{State: runtime.TelemetryUnanswered},
		{State: runtime.TelemetryUnknown},
	} {
		got := telemetryOutput(t, &fakeRuntime{state: state}, "status")
		if !strings.HasPrefix(got, "telemetry: ") {
			t.Errorf("%q does not start with the state", got)
		}
		if strings.Contains(strings.ToLower(got), "hull") {
			t.Errorf("the report names the runtime: %q", got)
		}
	}
	if strings.Contains(strings.ToLower(telemetryUsage), "hull") {
		t.Errorf("the help names the runtime:\n%s", telemetryUsage)
	}
}

// status is the default verb, because `brig telemetry` on its own is a
// question, not a change.
func TestTelemetryDefaultsToStatus(t *testing.T) {
	rt := &fakeRuntime{state: runtime.Telemetry{State: runtime.TelemetryUnanswered}}

	got := telemetryOutput(t, rt)

	if len(rt.set) != 0 {
		t.Errorf("a bare `brig telemetry` changed the answer: %v", rt.set)
	}
	if !strings.Contains(got, "not answered yet") {
		t.Errorf("state not reported: %q", got)
	}
}

func TestTelemetryOnAndOffRecordTheAnswer(t *testing.T) {
	rt := &fakeRuntime{state: runtime.Telemetry{State: runtime.TelemetryUnanswered}}

	if got := telemetryOutput(t, rt, "off"); !strings.Contains(got, "telemetry: off") {
		t.Errorf("off did not report off: %q", got)
	}
	if got := telemetryOutput(t, rt, "on"); !strings.Contains(got, "telemetry: on") {
		t.Errorf("on did not report on: %q", got)
	}
	if len(rt.set) != 2 || rt.set[0] || !rt.set[1] {
		t.Errorf("answers recorded = %v, want [false true]", rt.set)
	}
}

// The report is the effective state, not an echo of the verb. Recording a yes
// while DO_NOT_TRACK is set leaves telemetry off, and saying "on" there would
// be a plain lie about what the machine will do.
func TestTelemetryOnReportsTheVariableThatStillWins(t *testing.T) {
	rt := &fakeRuntime{
		state:  runtime.Telemetry{State: runtime.TelemetryOff, Setting: "DO_NOT_TRACK=1"},
		sticky: true,
	}

	got := telemetryOutput(t, rt, "on")

	if !strings.Contains(got, "telemetry: off") {
		t.Errorf("a recorded yes was reported as on while DO_NOT_TRACK was set: %q", got)
	}
	if !strings.Contains(got, "DO_NOT_TRACK=1") {
		t.Errorf("the variable that decided the state was not named: %q", got)
	}
}

// A backend that sends nothing answers the question the user actually asked
// rather than failing, because "is anything being sent" has an answer on Linux
// too, and it is no.
func TestTelemetryOnARuntimeThatCollectsNothing(t *testing.T) {
	got := telemetryOutput(t, quietRuntime{}, "status")

	if !strings.Contains(got, "telemetry: off") {
		t.Errorf("a runtime that collects nothing reported %q", got)
	}
}

func TestTelemetryRejectsAnUnknownVerb(t *testing.T) {
	withRuntime(t, &fakeRuntime{})
	var out bytes.Buffer
	if err := telemetryCmd(&out, []string{"enable"}); err == nil {
		t.Fatal("an unknown verb was accepted")
	}
}
