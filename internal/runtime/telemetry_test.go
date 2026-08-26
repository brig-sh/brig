package runtime

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubTelemetryHull writes a stand-in for the hull binary that answers `telemetry
// status` with the state a test names, and records the telemetry environment
// every other invocation was given.
//
// Recording the environment is the whole point: what decides whether a fresh
// install sends anything is one variable in the child's environment, and the
// only honest way to assert on it is to be the child and write it down.
func stubTelemetryHull(t *testing.T, state string) (*hull, func() string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in is not portable to windows")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "env.log")
	script := `#!/bin/sh
if [ "$1" = telemetry ]; then
  printf 'telemetry-verb=%s\n' "$2" >> "$STUB_LOG"
  [ "$2" = status ] && printf 'telemetry: %s\n' "$STUB_TELEMETRY"
  exit 0
fi
{
  printf 'verb=%s\n' "$1"
  printf 'HULL_TELEMETRY_SUPPRESS=%s\n' "${HULL_TELEMETRY_SUPPRESS-<unset>}"
  printf 'HULL_TELEMETRY_PRODUCT=%s\n' "${HULL_TELEMETRY_PRODUCT-<unset>}"
  printf 'DO_NOT_TRACK=%s\n' "${DO_NOT_TRACK-<unset>}"
} >> "$STUB_LOG"
exit 0
`
	bin := filepath.Join(dir, "hull")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STUB_LOG", log)
	t.Setenv("STUB_TELEMETRY", state)
	return &hull{bin: bin}, func() string {
		b, err := os.ReadFile(log)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func boot(t *testing.T, h *hull) {
	t.Helper()
	if err := h.Run(RunSpec{Name: "vm", Image: "img", Mem: 2048, CPUs: 2, Counted: true}); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// The gap this closes: hull defaults telemetry on when it cannot prompt, and
// the boot hands it no terminal, so a fresh install used to send events for
// its first boot before anyone had been asked anything -- with brig's name on
// them. Nothing may go out until an answer exists.
func TestFreshInstallDoesNotCountTheBoot(t *testing.T) {
	h, logged := stubTelemetryHull(t, "not configured (on by default; interactive runs will be asked)")

	boot(t, h)
	if err := h.Stop("vm"); err != nil {
		t.Fatalf("stop: %v", err)
	}

	got := logged()
	if strings.Count(got, "HULL_TELEMETRY_SUPPRESS=1") != 2 {
		t.Errorf("an unanswered install counted an operation:\n%s", got)
	}
	// Attribution still travels, so that whatever is eventually sent says brig
	// rather than reading as hull's own usage.
	if !strings.Contains(got, "HULL_TELEMETRY_PRODUCT=brig") {
		t.Errorf("attribution missing:\n%s", got)
	}
}

// The other half of the same rule: once there is an answer, the operations the
// user asked for count again. A gate that never opens is not a consent gate,
// it is an opt-out brig imposed on the user's behalf.
func TestRecordedConsentCountsTheBoot(t *testing.T) {
	h, logged := stubTelemetryHull(t, "enabled")

	boot(t, h)

	if got := logged(); !strings.Contains(got, "HULL_TELEMETRY_SUPPRESS=\n") {
		t.Errorf("a recorded yes did not count the boot:\n%s", got)
	}
}

// DO_NOT_TRACK is the cross-tool opt-out, and it wins over everything: over
// the boot gate, over an answer recorded on this machine, over brig's own
// attribution. It also has to reach the child untouched, because the tool that
// honours it is the one brig spawns.
func TestDoNotTrackWins(t *testing.T) {
	t.Setenv("DO_NOT_TRACK", "1")
	h, logged := stubTelemetryHull(t, "disabled (DO_NOT_TRACK=1)")

	boot(t, h)

	got := logged()
	if !strings.Contains(got, "DO_NOT_TRACK=1") {
		t.Errorf("DO_NOT_TRACK did not reach the runtime:\n%s", got)
	}
	if !strings.Contains(got, "HULL_TELEMETRY_SUPPRESS=1") {
		t.Errorf("DO_NOT_TRACK was set and the boot was still counted:\n%s", got)
	}
	state, err := h.TelemetryStatus()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if state.State != TelemetryOff {
		t.Errorf("state = %q, want off", state.State)
	}
	// Reported, because "off" with no explanation is the state a user cannot
	// then turn back on: the answer is in the shell, not on the disk.
	if state.Setting != "DO_NOT_TRACK=1" {
		t.Errorf("setting = %q, want DO_NOT_TRACK=1", state.Setting)
	}
}

// The exception, and the reason the gate does not simply suppress everything:
// the exec that hands over the terminal is where hull can ask the question.
// Suppressing there would mean the prompt never appears and the answer is
// never recorded, so telemetry would be off for good with nobody having
// chosen that.
func TestTerminalHandoverStaysAskable(t *testing.T) {
	h, _ := stubTelemetryHull(t, "not configured (on by default; interactive runs will be asked)")

	withTerminal := strings.Join(h.telemetryEnvFor(true, true), " ")
	if strings.Contains(withTerminal, "HULL_TELEMETRY_SUPPRESS=1") {
		t.Errorf("the invocation that can ask was suppressed: %s", withTerminal)
	}
	// Same invocation without a terminal on stdin: nobody to ask, so the boot
	// rule applies.
	withoutTerminal := strings.Join(h.telemetryEnvFor(true, false), " ")
	if !strings.Contains(withoutTerminal, "HULL_TELEMETRY_SUPPRESS=1") {
		t.Errorf("a non-interactive handover was counted: %s", withoutTerminal)
	}
}

// A shell handover always gives the guest a pty, but only a real terminal on
// brig's own stdin means hull has someone to answer its consent question. The
// two are separate signals: TTY drives `hull exec -t`, CanAsk drives the boot
// gate. A scripted `brig shell -- cmd` sets TTY (a login shell wants a pty)
// without CanAsk, and on a fresh install that must still suppress -- the exact
// case the boot gate closes and which reading TTY for both jobs had left open.
func TestShellHandoverSeparatesPtyFromConsent(t *testing.T) {
	h, _ := stubTelemetryHull(t, "not configured (on by default; interactive runs will be asked)")

	argv, env := h.replaceCmd(ExecSpec{Name: "vm", Cmd: []string{"bash", "-lc", "ls"}, Counted: true, TTY: true, CanAsk: false})
	if line := strings.Join(argv, " "); !strings.Contains(line, "exec -t") {
		t.Errorf("the guest lost its pty when brig's stdin was not a terminal: %s", line)
	}
	if got := strings.Join(env, " "); !strings.Contains(got, "HULL_TELEMETRY_SUPPRESS=1") {
		t.Errorf("a scripted shell on a fresh install was counted: %s", got)
	}

	// Same handover with a terminal on brig's stdin: hull can put the question
	// to a person, so the gate leaves it unsuppressed, and the pty is unchanged.
	argv, env = h.replaceCmd(ExecSpec{Name: "vm", Cmd: []string{"bash", "-l"}, Counted: true, TTY: true, CanAsk: true})
	if got := strings.Join(env, " "); strings.Contains(got, "HULL_TELEMETRY_SUPPRESS=1") {
		t.Errorf("an interactive shell could not be asked the consent question: %s", got)
	}
	if line := strings.Join(argv, " "); !strings.Contains(line, "exec -t") {
		t.Errorf("an interactive shell lost its pty: %s", line)
	}
}

// Plumbing stays suppressed whatever the answer is: one user command counts
// once, which is the rule that predates the gate.
func TestPlumbingStaysSuppressedWithConsent(t *testing.T) {
	h, _ := stubTelemetryHull(t, "enabled")

	if got := strings.Join(h.telemetryEnvFor(false, true), " "); !strings.Contains(got, "HULL_TELEMETRY_SUPPRESS=1") {
		t.Errorf("plumbing counted: %s", got)
	}
}

func TestSetTelemetryRecordsTheAnswer(t *testing.T) {
	h, logged := stubTelemetryHull(t, "disabled")

	if err := h.SetTelemetry(false); err != nil {
		t.Fatalf("off: %v", err)
	}
	if err := h.SetTelemetry(true); err != nil {
		t.Fatalf("on: %v", err)
	}

	got := logged()
	for _, want := range []string{"telemetry-verb=off", "telemetry-verb=on"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s not driven:\n%s", want, got)
		}
	}
}

// Every phrase hull documents, plus the two that matter most: a state this
// version has not seen must not read as "on", and an answer given to a
// narrower disclosure than today's is not an answer to today's.
func TestParseTelemetry(t *testing.T) {
	for _, tt := range []struct {
		out     string
		want    TelemetryState
		setting string
	}{
		{"telemetry: enabled\n", TelemetryOn, ""},
		{"telemetry: disabled\n", TelemetryOff, ""},
		{"telemetry: disabled (DO_NOT_TRACK=1)\n", TelemetryOff, "DO_NOT_TRACK=1"},
		{"telemetry: disabled (HULL_TELEMETRY_DISABLED=1)\n", TelemetryOff, "HULL_TELEMETRY_DISABLED=1"},
		{"telemetry: enabled (HULL_TELEMETRY_ENABLED=1)\n", TelemetryOn, "HULL_TELEMETRY_ENABLED=1"},
		{"telemetry: not configured (on by default; interactive runs will be asked)\n", TelemetryUnanswered, ""},
		{"telemetry: enabled for an older schema (interactive runs will be re-asked)\n", TelemetryUnanswered, ""},
		{"", TelemetryUnknown, ""},
		{"telemetry: something this version has never heard of\n", TelemetryUnknown, ""},
	} {
		got := parseTelemetry(tt.out)
		if got.State != tt.want || got.Setting != tt.setting {
			t.Errorf("parseTelemetry(%q) = (%q, %q), want (%q, %q)",
				tt.out, got.State, got.Setting, tt.want, tt.setting)
		}
	}
}

// An older runtime with no telemetry command at all, or one that will not
// answer, is not an answer either. The gate must close, not open, and asking
// must not take the whole run down with it.
func TestUnansweredRuntimeSuppresses(t *testing.T) {
	h := &hull{bin: filepath.Join(t.TempDir(), "not-there")}

	if _, err := h.TelemetryStatus(); err == nil {
		t.Fatal("a missing runtime reported a telemetry state")
	}
	if got := strings.Join(h.telemetryEnvFor(true, false), " "); !strings.Contains(got, "HULL_TELEMETRY_SUPPRESS=1") {
		t.Errorf("a runtime that would not answer counted the operation: %s", got)
	}
}
