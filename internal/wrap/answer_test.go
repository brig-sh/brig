package wrap

import (
	"errors"
	"math"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// question returns the command ask wrapped in answerScript or
// answerScriptHeld, and whether cmd was wrapped at all. The guest fakes use it
// to answer the way the guest's shell does.
func question(cmd []string) ([]string, bool) {
	if len(cmd) > 4 && cmd[0] == "sh" && cmd[1] == "-c" &&
		(cmd[2] == answerScript || cmd[2] == answerScriptHeld) {
		return cmd[4:], true
	}
	return cmd, false
}

// answered returns what answerScript prints for a command that succeeded with
// out. TestAnswerScriptUnderARealShell holds it to what a real shell prints.
func answered(out string) string { return out + "\n" + answerEnd + "\n" }

// noPause skips the waits between tries for the length of a test, and returns
// the waits ask asked for.
func noPause(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	old := answerSleep
	answerSleep = func(d time.Duration) { waits = append(waits, d) }
	t.Cleanup(func() { answerSleep = old })
	return &waits
}

// The runtime loses output in bursts that last longer than one quick retry,
// so each wait doubles and the tries span several seconds.
func TestAskWaitsLongerBeforeEachTry(t *testing.T) {
	waits := noPause(t)
	g := newGuestFake()
	c := deliveryConfig(t, g)
	g.lose["cat /proc/self/mountinfo"] = &loss{times: math.MaxInt}
	if err := c.deliverSecretFiles(); err == nil {
		t.Fatal("delivery went on without a mount table")
	}
	ms := time.Millisecond
	want := []time.Duration{100 * ms, 200 * ms, 400 * ms, 800 * ms,
		time.Second, time.Second, time.Second, time.Second, time.Second, time.Second, time.Second}
	if !slices.Equal(*waits, want) {
		t.Errorf("waited %v between tries, want %v", *waits, want)
	}
}

// Only a try after a lost answer holds the process open. A runtime that
// loses nothing never pays for the hold.
func TestOnlyATryAfterALostAnswerIsHeldOpen(t *testing.T) {
	noPause(t)
	g := newGuestFake()
	c := deliveryConfig(t, g)
	g.lose["cat /proc/self/mountinfo"] = &loss{times: 2}
	if err := c.deliverSecretFiles(); err != nil {
		t.Fatalf("deliverSecretFiles: %v", err)
	}
	var held []bool
	for _, line := range g.log {
		if strings.Contains(line, "cat /proc/self/mountinfo") {
			held = append(held, strings.Contains(line, "sleep"))
		}
	}
	if len(held) < 3 || held[0] || !held[1] || !held[2] {
		t.Errorf("mount table asks held open: %v, want the first plain and the retries held", held)
	}
	for _, line := range g.log {
		if strings.Contains(line, "stat -f") && strings.Contains(line, "sleep") {
			t.Errorf("a question answered on the first try was held open: %q", line)
		}
	}
}

// The fakes answer through answered, so it has to be what the guest's shell
// actually prints, for an answer with and without its own trailing newline,
// and for both scripts. A command that fails keeps its exit status and prints
// no answerEnd.
func TestAnswerScriptUnderARealShell(t *testing.T) {
	for _, script := range []string{answerScript, answerScriptHeld} {
		for _, out := range []string{"tmpfs\n", "no newline", ""} {
			got, err := exec.Command("sh", "-c", script, "sh", "printf", "%s", out).Output()
			if err != nil {
				t.Fatalf("%s: printf %q: %v", script, out, err)
			}
			if string(got) != answered(out) {
				t.Errorf("%s: printf %q printed %q, want %q", script, out, got, answered(out))
			}
		}
		got, err := exec.Command("sh", "-c", script, "sh", "sh", "-c", "echo partial; exit 3").Output()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 3 {
			t.Fatalf("%s: a failing question exited with %v, want its own status 3", script, err)
		}
		if string(got) != "partial\n" {
			t.Errorf("%s: a failing question printed %q; answerEnd must not follow a failure",
				script, got)
		}
	}
	// A guest with no sleep still answers under the held script.
	cmd := exec.Command("sh", "-c", answerScriptHeld, "sh", "printf", "%s", "tmpfs\n")
	cmd.Env = []string{"PATH=/nonexistent"}
	got, err := cmd.Output()
	if err != nil || string(got) != answered("tmpfs\n") {
		t.Errorf("with no sleep on PATH the held script printed %q, %v", got, err)
	}
}
