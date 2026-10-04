package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/telemetry"
	"github.com/brig-sh/brig/internal/wrap"
)

// quietRuntime is a runtime that answers nothing, so a test that reaches one
// of its methods panics.
type quietRuntime struct{ runtime.Runtime }

func withRuntime(t *testing.T, rt runtime.Runtime) {
	t.Helper()
	prev := detectRuntime
	detectRuntime = func() (runtime.Runtime, error) { return rt, nil }
	t.Cleanup(func() { detectRuntime = prev })
}

// telemetryHome gives a test a home of its own, so the answer it records is
// not the one on the machine running it, and clears the opt-out variables the
// machine may have set.
func telemetryHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, v := range []string{"DO_NOT_TRACK", "HULL_TELEMETRY_DISABLED", "HULL_TELEMETRY_DEBUG",
		"HULL_TELEMETRY_SUPPRESS", "HULL_TELEMETRY_ENDPOINT", "HULL_TELEMETRY_PRODUCT", "HULL_TELEMETRY_VERSION"} {
		t.Setenv(v, "")
	}
	// A build with no endpoint queues nothing. Nothing listens on this one.
	t.Setenv("HULL_TELEMETRY_ENDPOINT", "http://127.0.0.1:1")
}

func telemetryOutput(t *testing.T, args ...string) string {
	t.Helper()
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
	telemetryHome(t)
	for _, step := range [][]string{{"status"}, {"on"}, {"off"}} {
		got := telemetryOutput(t, step...)
		if !strings.HasPrefix(got, "telemetry: ") {
			t.Errorf("%q does not start with the state", got)
		}
		if strings.Contains(strings.ToLower(got), "hull") {
			t.Errorf("the report names the runtime: %q", got)
		}
	}
	t.Setenv("DO_NOT_TRACK", "1")
	if got := telemetryOutput(t, "status"); strings.Contains(strings.ToLower(got), "hull") {
		t.Errorf("the report names the runtime: %q", got)
	}
	if strings.Contains(strings.ToLower(telemetryUsage), "hull") {
		t.Errorf("the help names the runtime:\n%s", telemetryUsage)
	}
}

// status is the default verb, because `brig telemetry` on its own is a
// question, not a change.
func TestTelemetryDefaultsToStatus(t *testing.T) {
	telemetryHome(t)

	got := telemetryOutput(t)

	if !strings.Contains(got, "not answered yet") {
		t.Errorf("state not reported: %q", got)
	}
	if answer, _, err := telemetry.Status(); err != nil || answer != telemetry.Unanswered {
		t.Errorf("a bare `brig telemetry` changed the answer: %q, %v", answer, err)
	}
	// Asking is not answering: no install id is minted on a machine that has
	// never sent anything.
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".hull")); !os.IsNotExist(err) {
		t.Errorf("a bare `brig telemetry` created the telemetry state: %v", err)
	}
}

func TestTelemetryOnAndOffRecordTheAnswer(t *testing.T) {
	telemetryHome(t)

	if got := telemetryOutput(t, "off"); !strings.Contains(got, "telemetry: off") {
		t.Errorf("off did not report off: %q", got)
	}
	if answer, _, _ := telemetry.Status(); answer != telemetry.Off {
		t.Errorf("off recorded %q", answer)
	}
	if got := telemetryOutput(t, "on"); !strings.Contains(got, "telemetry: on") {
		t.Errorf("on did not report on: %q", got)
	}
	if answer, _, _ := telemetry.Status(); answer != telemetry.On {
		t.Errorf("on recorded %q", answer)
	}
}

// The report is the effective state, not an echo of the verb. Recording a yes
// while DO_NOT_TRACK is set leaves telemetry off, and saying "on" there would
// be a plain lie about what the machine will do.
func TestTelemetryOnReportsTheVariableThatStillWins(t *testing.T) {
	telemetryHome(t)
	t.Setenv("DO_NOT_TRACK", "1")

	got := telemetryOutput(t, "on")

	if !strings.Contains(got, "telemetry: off") {
		t.Errorf("a recorded yes was reported as on while DO_NOT_TRACK was set: %q", got)
	}
	if !strings.Contains(got, "DO_NOT_TRACK=1") {
		t.Errorf("the variable that decided the state was not named: %q", got)
	}
}

// brig sends its own events, so the runtime underneath does not decide the
// answer. A runtime that collects nothing, which is what nerdctl is, is never
// asked: the report is the answer on this machine, and `on` turns brig's own
// events on there too.
func TestTelemetryOnARuntimeThatCollectsNothing(t *testing.T) {
	telemetryHome(t)
	withRuntime(t, quietRuntime{})

	if got := telemetryOutput(t, "on"); !strings.Contains(got, "telemetry: on") {
		t.Errorf("on a runtime that collects nothing, on reported %q", got)
	}
}

func TestTelemetryRejectsAnUnknownVerb(t *testing.T) {
	telemetryHome(t)
	var out bytes.Buffer
	if err := telemetryCmd(&out, []string{"enable"}); err == nil {
		t.Fatal("an unknown verb was accepted")
	}
}

// The word in the verb position can be anything someone typed: an image
// reference, a path, a secret pasted into the wrong terminal. Only a verb, or
// a ref to an agent brig has, may name the command. Everything else is sent as
// unknown.
func TestTelemetryCommandNeverSendsAStrayWord(t *testing.T) {
	cases := []struct {
		word    string
		command string
		asks    bool
	}{
		{"run", "run", true},
		{"sh", "sh", true},
		{"create", "create", false},
		{"exec", "exec", false},
		{"ls", "ls", false},
		{"--version", "version", false},
		{"claude", "run", true},
		{"claude@refactor", "run", true},
		{"ghcr.io/example/private-thing:v1", unknownCommand, false},
		{"/Users/someone/project", unknownCommand, false},
		{"sk-ant-api03-not-a-verb", unknownCommand, false},
		{"rnu", unknownCommand, false},
		{"telemetry", "", false},
		{"completion", "", false},
	}
	for _, tc := range cases {
		command, asks := telemetryCommand(tc.word)
		if command != tc.command || asks != tc.asks {
			t.Errorf("telemetryCommand(%q) = %q, %v; want %q, %v", tc.word, command, asks, tc.command, tc.asks)
		}
	}
}

// A test drives dispatch without main, so nothing starts telemetry and no
// answer is read or written.
func TestDispatchStartsNoTelemetryUnderTest(t *testing.T) {
	if startTelemetry != nil || telemetry.SpawnUploader != nil {
		t.Fatal("startTelemetry or the uploader is set outside main")
	}
	called := false
	prev := startTelemetry
	startTelemetry = func(string, bool) { called = true }
	t.Cleanup(func() { startTelemetry = prev })
	withVerbosity(t, wrap.Normal)
	beginTelemetry("ls")
	if !called {
		t.Fatal("a counted verb did not start telemetry")
	}
	called = false
	beginTelemetry("telemetry")
	if called {
		t.Fatal("`brig telemetry` started telemetry, and could ask on its way to an answer")
	}
}

// The class follows the exit code docs/cli.md documents, so it never carries
// the message, which can hold a path or a profile name.
func TestTelemetryErrorClassFollowsTheExitCode(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{usagef("unexpected argument %q", "/Users/someone/secret"), "usage"},
		{notFoundf("unknown profile %q", "my-private-agent"), "not-found"},
		{runtime.ErrNoRuntime, "runtime"},
		{errors.New("open /Users/someone/.brig/state.json: permission denied"), "other"},
	}
	for _, tc := range cases {
		if got := errorClass(exitCode(tc.err)); got != tc.want {
			t.Errorf("errorClass(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// recordStarts makes startTelemetry record its calls for the test.
func recordStarts(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	prev, prevPending := startTelemetry, pendingTelemetry
	startTelemetry = func(command string, interactive bool) {
		calls = append(calls, fmt.Sprintf("%s:%v", command, interactive))
	}
	t.Cleanup(func() { startTelemetry, pendingTelemetry = prev, prevPending })
	return &calls
}

// Whether a run may ask depends on its line: --json and -q after the ref are
// read only by parse. So a run waits for askTelemetry, and --json never asks.
func TestARunAsksOnlyOnceItsLineIsRead(t *testing.T) {
	calls := recordStarts(t)
	withVerbosity(t, wrap.Normal)

	beginTelemetry("run")
	if len(*calls) != 0 {
		t.Fatalf("a run started telemetry before its line was read: %v", *calls)
	}
	askTelemetry(true)
	if len(*calls) != 1 || (*calls)[0] != "run:false" {
		t.Fatalf("starts = %v, want one that does not ask", *calls)
	}
}

// A run that fails before its line is read still counts, and asks nobody.
func TestARunRefusedEarlyStillCounts(t *testing.T) {
	calls := recordStarts(t)
	beginTelemetry("sh")

	finishTelemetry(usagef("unexpected argument"))

	if len(*calls) != 1 || (*calls)[0] != "sh:false" {
		t.Fatalf("starts = %v, want one that does not ask", *calls)
	}
}

// At a foreground terminal, --json, -q and CI still never ask.
func TestMayAskNeverForJSONOrQuiet(t *testing.T) {
	prev := atForegroundTerminal
	atForegroundTerminal = func() bool { return true }
	t.Cleanup(func() { atForegroundTerminal = prev })
	t.Setenv("CI", "")

	withVerbosity(t, wrap.Normal)
	if !mayAsk(false) {
		t.Fatal("a plain run at a terminal may not ask, so the cases below prove nothing")
	}
	if mayAsk(true) {
		t.Error("--json may ask")
	}
	t.Setenv("CI", "true")
	if mayAsk(false) {
		t.Error("CI may ask")
	}
	t.Setenv("CI", "")
	withVerbosity(t, wrap.Quiet)
	if mayAsk(false) {
		t.Error("-q may ask")
	}
}

// A panic in a run before its line is read still starts telemetry, without
// asking, so the crash report has a client to go to.
func TestAPanicBeforeTheLineIsReadStartsTelemetry(t *testing.T) {
	calls := recordStarts(t)
	beginTelemetry("run")
	prev := exitOnPanic
	exitOnPanic = func(int) {}
	t.Cleanup(func() { exitOnPanic = prev })

	handlePanic("boom")

	if len(*calls) != 1 || (*calls)[0] != "run:false" {
		t.Fatalf("starts = %v, want one that does not ask", *calls)
	}
}

// The uploader outlives brig: it leads a session of its own, so the exit and
// the exec of a handover do not take it down, and it holds no directory of
// the user's.
func TestTheUploaderIsDetached(t *testing.T) {
	cmd := uploaderCmd("/opt/brig/bin/brig")
	if got := strings.Join(cmd.Args, " "); got != "/opt/brig/bin/brig telemetry flush --now" {
		t.Errorf("the uploader runs %q", got)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Error("the uploader shares brig's session")
	}
	if cmd.Dir != "/" {
		t.Errorf("the uploader runs in %q", cmd.Dir)
	}
}

// The uploader prints nothing, counts nothing, and starts no uploader of its
// own: a panic in it would otherwise start the next one for ever.
func TestTelemetryFlushStartsNoUploader(t *testing.T) {
	telemetryHome(t)
	prev := telemetry.SpawnUploader
	telemetry.SpawnUploader = func() { t.Error("the uploader started an uploader") }
	t.Cleanup(func() { telemetry.SpawnUploader = prev })

	if got := telemetryOutput(t, "flush", "--now"); got != "" {
		t.Errorf("flush printed %q", got)
	}
	if telemetry.SpawnUploader != nil {
		t.Error("the uploader can still start an uploader")
	}
	if command, _ := telemetryCommand("telemetry"); command != "" {
		t.Errorf("brig telemetry counts as %q", command)
	}
}

// The report names the install id, so you can find your own events, or ask
// for them to be deleted. It is the id on file, and asking creates none.
func TestTelemetryStatusNamesTheInstallID(t *testing.T) {
	telemetryHome(t)
	if got := telemetryOutput(t, "status"); strings.Contains(got, "install id") {
		t.Errorf("a machine with no id on file reported one: %q", got)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".hull")); !os.IsNotExist(err) {
		t.Errorf("asking created the state: %v", err)
	}

	telemetryOutput(t, "off")
	data, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".hull", "telemetry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		InstallID string `json:"install_id"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.InstallID == "" {
		t.Fatalf("no install id on file: %s", data)
	}
	if got := telemetryOutput(t, "status"); !strings.Contains(got, "install id: "+st.InstallID+"\n") {
		t.Errorf("the report does not name the id on file, %s:\n%s", st.InstallID, got)
	}
}

// Before a newer hull or brig moves the state, the id is in the default
// store's file. That is the id hull sends under, and the one the move keeps.
func TestTelemetryStatusReadsTheLegacyInstallID(t *testing.T) {
	telemetryHome(t)
	legacy := filepath.Join(os.Getenv("HOME"), ".hull", "store")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	const id = "11111111-2222-4333-8444-555555555555"
	if err := os.WriteFile(filepath.Join(legacy, "telemetry.json"), []byte(`{"install_id":"`+id+`","consent":true,"consent_version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := telemetryOutput(t, "status"); !strings.Contains(got, "install id: "+id) {
		t.Errorf("the report does not name the legacy id:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".hull", "telemetry.json")); !os.IsNotExist(err) {
		t.Errorf("asking moved the state: %v", err)
	}
}

// Each state says what to type to change it, laid out as brig's other blocks
// are: the facts as notes, the command as an action.
func TestTelemetryReportSaysWhatToType(t *testing.T) {
	cases := []struct {
		answer  telemetry.Answer
		setting string
		want    string
	}{
		{telemetry.On, "", "  → to turn it off:  brig telemetry off\n"},
		{telemetry.Off, "", "  → to turn it on:  brig telemetry on\n"},
		{telemetry.Off, "HULL_TELEMETRY_DISABLED=1", "  → to let the recorded answer decide:  unset HULL_TELEMETRY_DISABLED\n"},
		{telemetry.Unanswered, "", "  → to turn it on:   brig telemetry on\n  → to turn it off:  brig telemetry off\n"},
	}
	for _, tc := range cases {
		got := telemetryReport(tc.answer, tc.setting, "12345678-90ab-4cde-8f01-234567890abc")
		if !strings.HasSuffix(got, tc.want) {
			t.Errorf("%s %s: the report ends\n%s\nwant\n%s", tc.answer, tc.setting, got, tc.want)
		}
		if !strings.Contains(got, "\n  ↳ install id: 12345678-90ab-4cde-8f01-234567890abc\n") {
			t.Errorf("%s: the install id is not a note:\n%s", tc.answer, got)
		}
	}
}
