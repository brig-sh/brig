package runtime

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/brig-sh/brig/internal/telemetry"
)

// stubTelemetryHull writes a stand-in for the hull binary that records the
// telemetry environment of every invocation but --version.
//
// Recording the environment is the whole point: what decides whether hull
// sends anything is one variable in the child's environment, and the only
// honest way to assert on it is to be the child and write it down.
func stubTelemetryHull(t *testing.T) (*hull, func() string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in is not portable to windows")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "env.log")
	// It answers --version as a hull that takes a suppress list.
	script := `#!/bin/sh
if [ "$1" = --version ]; then
  echo "hull 0.1.0-rc31 (stub)"
  exit 0
fi
{
  printf 'verb=%s\n' "$1"
  printf 'HULL_TELEMETRY_SUPPRESS=%s\n' "${HULL_TELEMETRY_SUPPRESS-<unset>}"
  printf 'HULL_TELEMETRY_PRODUCT=%s\n' "${HULL_TELEMETRY_PRODUCT-<unset>}"
  printf 'HULL_TELEMETRY_VERSION=%s\n' "${HULL_TELEMETRY_VERSION-<unset>}"
  printf 'DO_NOT_TRACK=%s\n' "${DO_NOT_TRACK-<unset>}"
} >> "$STUB_LOG"
exit 0
`
	bin := filepath.Join(dir, "hull")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STUB_LOG", log)
	return &hull{bin: bin}, func() string {
		b, err := os.ReadFile(log)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// counting starts brig's telemetry in a home of the test's own, with a yes on
// file when answered is true and nothing on file otherwise. It also gives the
// boots and stops the tests drive a home: they release the gateway under the
// sandbox's name, which would otherwise be the real ~/.brig.
func counting(t *testing.T, answered bool) {
	t.Helper()
	scratchHome(t)
	for _, v := range []string{"DO_NOT_TRACK", "HULL_TELEMETRY_DISABLED", "HULL_TELEMETRY_DEBUG",
		"HULL_TELEMETRY_SUPPRESS", "HULL_TELEMETRY_ENDPOINT", "HULL_TELEMETRY_PRODUCT", "HULL_TELEMETRY_VERSION"} {
		t.Setenv(v, "")
	}
	// A build with no endpoint queues nothing. Nothing listens on this one.
	t.Setenv("HULL_TELEMETRY_ENDPOINT", "http://127.0.0.1:1")
	if answered {
		if err := telemetry.Set(true); err != nil {
			t.Fatal(err)
		}
	}
	telemetry.Start("run", false)
	// The next test starts from a client that sends nothing.
	t.Cleanup(func() {
		_ = telemetry.Set(false)
		telemetry.Start("run", false)
	})
}

func boot(t *testing.T, h *hull) {
	t.Helper()
	if err := h.Run(RunSpec{Name: "vm", Image: "img", Mem: 2048, CPUs: 2, Counted: true}); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// hull defaults telemetry on when it cannot prompt, and the boot hands it no
// terminal. A fresh install must not send events for its first boot before
// anyone has been asked anything, with brig's name on them.
func TestFreshInstallDoesNotCountTheBoot(t *testing.T) {
	counting(t, false)
	h, logged := stubTelemetryHull(t)

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

// The other half of the same rule: once there is an answer, the boot reports
// what only hull sees. brig counts the command itself, so hull is told to send
// everything but its own command event, and to report brig's version.
func TestRecordedConsentCountsTheBoot(t *testing.T) {
	counting(t, true)
	h, logged := stubTelemetryHull(t)

	boot(t, h)

	got := logged()
	if !strings.Contains(got, "HULL_TELEMETRY_SUPPRESS=command\n") {
		t.Errorf("a recorded yes did not count the boot, or let hull count the command twice:\n%s", got)
	}
	if !strings.Contains(got, "HULL_TELEMETRY_VERSION="+telemetry.Version()+"\n") {
		t.Errorf("the boot's events would carry hull's version, not brig's:\n%s", got)
	}
}

// DO_NOT_TRACK is the cross-tool opt-out, and it wins over everything: over
// an answer recorded on this machine and over brig's own attribution. It also
// has to reach the child untouched, because the tool that honours it is the
// one brig spawns.
func TestDoNotTrackWins(t *testing.T) {
	counting(t, true)
	t.Setenv("DO_NOT_TRACK", "1")
	telemetry.Start("run", false)
	h, logged := stubTelemetryHull(t)

	boot(t, h)

	got := logged()
	if !strings.Contains(got, "DO_NOT_TRACK=1") {
		t.Errorf("DO_NOT_TRACK did not reach the runtime:\n%s", got)
	}
	if !strings.Contains(got, "HULL_TELEMETRY_SUPPRESS=1") {
		t.Errorf("DO_NOT_TRACK was set and the boot was still counted:\n%s", got)
	}
	answer, setting, err := telemetry.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if answer != telemetry.Off {
		t.Errorf("answer = %q, want off", answer)
	}
	// Reported, because "off" with no explanation is the state a user cannot
	// then turn back on: the answer is in the shell, not on the disk.
	if setting != "DO_NOT_TRACK=1" {
		t.Errorf("setting = %q, want DO_NOT_TRACK=1", setting)
	}
}

// A shell handover always gives the guest a pty, and brig asked its question
// before anything booted, so hull is never left to ask one. Under a script
// with nothing answered, the handover is suppressed and the pty is unchanged.
func TestShellHandoverSeparatesPtyFromConsent(t *testing.T) {
	counting(t, false)
	h, _ := stubTelemetryHull(t)

	argv, env, err := h.replaceCmd(ExecSpec{Name: "vm", Cmd: []string{"bash", "-lc", "ls"}, Counted: true, TTY: true})
	if err != nil {
		t.Fatal(err)
	}
	if line := strings.Join(argv, " "); !strings.Contains(line, "exec -t") {
		t.Errorf("the guest lost its pty: %s", line)
	}
	if got := strings.Join(env, " "); !strings.Contains(got, "HULL_TELEMETRY_SUPPRESS=1") {
		t.Errorf("a shell on an unanswered install was counted: %s", got)
	}
}

// Plumbing stays suppressed whatever the answer is: one user command counts
// once.
func TestPlumbingStaysSuppressedWithConsent(t *testing.T) {
	counting(t, true)

	if got := strings.Join(telemetryEnv(false), " "); !strings.Contains(got, "HULL_TELEMETRY_SUPPRESS=1") {
		t.Errorf("plumbing counted: %s", got)
	}
}

// The handover replaces brig's process, so brig's command event has to be
// queued before the exec: after it there is no brig left. It is queued and not
// sent, so the agent's terminal does not wait on the network. The stub stands
// in for the exec and checks what is on disk and what the collector has.
func TestHandoverQueuesTheCommandEventFirst(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		_, _ = b.ReadFrom(r.Body)
		mu.Lock()
		bodies = append(bodies, b.String())
		mu.Unlock()
	}))
	defer srv.Close()
	counting(t, true)
	t.Setenv("HULL_TELEMETRY_ENDPOINT", srv.URL)
	h, _ := stubTelemetryHull(t)

	var queued []string
	var sent int
	prev := execHandover
	execHandover = func(string, []string, []string) error {
		queued, _ = filepath.Glob(filepath.Join(telemetry.StateDir(), "outbox", "*.json"))
		mu.Lock()
		sent = len(bodies)
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { execHandover = prev })

	if err := h.Replace(ExecSpec{Name: "vm", Cmd: []string{"claude"}, Counted: true, TTY: true}); err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 {
		t.Fatalf("the outbox held %v when the exec ran, want brig's command event", queued)
	}
	if sent != 0 {
		t.Errorf("the handover waited on the network: %d sent before the exec", sent)
	}
	body, err := os.ReadFile(queued[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"event":"command"`, `"outcome":"ok"`, `"product":"brig"`, `"command":"run"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the queued event lacks %s: %s", want, body)
		}
	}
}

// A hull before 0.1.0-rc31 reads a suppress list as no suppression at all and
// would send its own command event next to brig's. Such a hull gets "1", and
// so does a build from source, which may be one.
func TestOlderHullIsNotCountedTwice(t *testing.T) {
	counting(t, true)
	for version, want := range map[string]string{
		"hull 0.1.0-rc30 (go1.26.4, darwin/arm64)":                                         "HULL_TELEMETRY_SUPPRESS=1",
		"hull 0.1.0-rc29-main.20260930191353.cc1def9":                                      "HULL_TELEMETRY_SUPPRESS=1",
		"hull 0.1.0-rc31 (go1.26.4, darwin/arm64)":                                         "HULL_TELEMETRY_SUPPRESS=command",
		"hull 0.1.0 (go1.26.4, darwin/arm64)":                                              "HULL_TELEMETRY_SUPPRESS=command",
		"hull v0.1.0-rc30.0.20261004171943-6c40ea424cbf (6c40ea4, go1.26.5, darwin/arm64)": "HULL_TELEMETRY_SUPPRESS=1",
		"hull dev": "HULL_TELEMETRY_SUPPRESS=1",
	} {
		h := &hull{bin: "unused"}
		h.versionOnce.Do(func() { h.versionOut = version })
		if got := strings.Join(h.countedEnv(true), " "); !strings.Contains(got, want) {
			t.Errorf("%s: env %q, want %s", version, got, want)
		}
	}
}

// An exec that returns did not replace brig, so the run failed. The event
// Handover queued says ok, so it is taken back, and the exit path sends the
// failure instead.
func TestAFailedHandoverIsNotReportedAsOK(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		_, _ = b.ReadFrom(r.Body)
		mu.Lock()
		bodies = append(bodies, b.String())
		mu.Unlock()
	}))
	defer srv.Close()
	counting(t, true)
	t.Setenv("HULL_TELEMETRY_ENDPOINT", srv.URL)
	telemetry.Start("run", false)
	h, _ := stubTelemetryHull(t)

	prev := execHandover
	execHandover = func(string, []string, []string) error { return errors.New("exec: no such file") }
	t.Cleanup(func() { execHandover = prev })

	if err := h.Replace(ExecSpec{Name: "vm", Cmd: []string{"claude"}, Counted: true, TTY: true}); err == nil {
		t.Fatal("a failed exec returned no error")
	}
	if queued, _ := filepath.Glob(filepath.Join(telemetry.StateDir(), "outbox", "*.json")); len(queued) != 0 {
		t.Fatalf("the ok event stayed queued after the exec failed: %v", queued)
	}
	telemetry.Finish("error", "runtime")
	telemetry.Upload()

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"outcome":"error"`) {
		t.Fatalf("the collector got %q, want one failed command", bodies)
	}
}
