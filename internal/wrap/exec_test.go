package wrap

import (
	"os"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
)

// recordingRuntime captures the spec handed to Replace instead of exec'ing it,
// so a test can see what the guest pty and the consent gate were told.
type recordingRuntime struct {
	runtime.Runtime
	spec runtime.ExecSpec
}

func (r *recordingRuntime) Replace(spec runtime.ExecSpec) error {
	r.spec = spec
	return nil
}

func (r *recordingRuntime) Attach(spec runtime.ExecSpec) (int, error) {
	r.spec = spec
	return 0, nil
}

// Shell forces a pty on because a login shell wants one, but whether hull may
// ask its consent question is a fact about brig's own stdin, not the guest's
// pty. Under test, stdin is not a terminal (a script or CI is the same), so a
// `brig shell -- cmd` must record TTY on and CanAsk off: the split the boot
// gate reads to leave a fresh install suppressed rather than send its first
// event before anyone was asked.
func TestShellSeparatesPtyFromConsent(t *testing.T) {
	rec := &recordingRuntime{}
	c := &Config{VMName: "vm", Runtime: rec}

	if err := c.Shell(creds.Set{}, []string{"ls"}); err != nil {
		t.Fatalf("shell: %v", err)
	}
	if !rec.spec.TTY {
		t.Error("shell dropped the guest pty")
	}
	if rec.spec.CanAsk {
		t.Error("a non-terminal stdin was treated as askable")
	}
}

// shellArgv used to join the caller's words with spaces and hand the result to
// bash -lc, so quoting, spaces, ";" and "$" were re-parsed. The guest must
// receive the original argv under a login shell.
func TestShellArgvPreservesCommandVector(t *testing.T) {
	command := []string{"sh", "-c", "echo FIRST; echo SECOND", "a   b"}
	want := []string{"bash", "-lc", `exec "$@"`, "bash", "sh", "-c", "echo FIRST; echo SECOND", "a   b"}
	if got := shellArgv(command); !equalStrings(got, want) {
		t.Fatalf("shellArgv = %#v, want %#v", got, want)
	}
	if got := shellArgv(nil); !equalStrings(got, []string{"bash", "-l"}) {
		t.Fatalf("empty shellArgv = %#v, want interactive login shell", got)
	}

	rec := &recordingRuntime{}
	c := &Config{VMName: "vm", Runtime: rec}
	if err := c.Shell(creds.Set{}, command); err != nil {
		t.Fatalf("shell: %v", err)
	}
	if !equalStrings(rec.spec.Cmd, want) {
		t.Fatalf("Shell handed guest %#v, want unmodified vector %#v", rec.spec.Cmd, want)
	}

	rec = &recordingRuntime{}
	c = &Config{VMName: "vm", Runtime: rec}
	if _, err := c.ShellAttached(creds.Set{}, command); err != nil {
		t.Fatalf("shell attached: %v", err)
	}
	if !equalStrings(rec.spec.Cmd, want) {
		t.Fatalf("ShellAttached handed guest %#v, want %#v", rec.spec.Cmd, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The Run path already fed the terminal check to both jobs by passing it as the
// tty argument, and that stays true: CanAsk tracks the same signal TTY does
// when the caller computed tty from stdin. This pins that the field is set from
// the real stdin, not left to default, so the Run path's behaviour is unchanged.
func TestExecCanAskTracksStdin(t *testing.T) {
	rec := &recordingRuntime{}
	c := &Config{VMName: "vm", Runtime: rec}

	if err := c.Exec(creds.Set{}, []string{"ls"}, false); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if rec.spec.CanAsk != IsTerminal(os.Stdin) {
		t.Errorf("CanAsk = %v, want the real stdin terminal check", rec.spec.CanAsk)
	}
}
