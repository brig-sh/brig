package wrap

import (
	"os"
	"os/exec"
	"slices"
	"strings"
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
// `brig sh <ref> cmd` must record TTY on and CanAsk off: the split the boot
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

// sh runs the words it was given as one argument vector. Joining them into a
// single -c script let bash re-split them, so `brig sh x sh -c 'echo FIRST;
// echo SECOND'` ran `sh -c echo FIRST` and then `echo SECOND`. The words must
// reach the guest as positional parameters, which the login shell never
// re-parses, on both the handover and the --json child path.
func TestShellPassesWordsThrough(t *testing.T) {
	command := []string{"sh", "-c", "echo FIRST; echo SECOND"}
	want := []string{"bash", "-lc", `exec "$@"`, "bash", "sh", "-c", "echo FIRST; echo SECOND"}

	rec := &recordingRuntime{}
	c := &Config{VMName: "vm", Runtime: rec}
	if err := c.Shell(creds.Set{}, command); err != nil {
		t.Fatalf("shell: %v", err)
	}
	if !slices.Equal(rec.spec.Cmd, want) {
		t.Errorf("Shell ran %q, want %q", rec.spec.Cmd, want)
	}

	rec = &recordingRuntime{}
	c = &Config{VMName: "vm", Runtime: rec}
	if _, err := c.ShellAttached(creds.Set{}, command); err != nil {
		t.Fatalf("shell attached: %v", err)
	}
	if !slices.Equal(rec.spec.Cmd, want) {
		t.Errorf("ShellAttached ran %q, want %q", rec.spec.Cmd, want)
	}
}

// With no words sh is an interactive login shell, and passing words through
// must not turn it into anything else.
func TestShellWithoutCommandIsLoginShell(t *testing.T) {
	rec := &recordingRuntime{}
	c := &Config{VMName: "vm", Runtime: rec}
	if err := c.Shell(creds.Set{}, nil); err != nil {
		t.Fatalf("shell: %v", err)
	}
	if want := []string{"bash", "-l"}; !slices.Equal(rec.spec.Cmd, want) {
		t.Errorf("Shell ran %q, want %q", rec.spec.Cmd, want)
	}
}

// Comparing the argv shellArgv builds says nothing about whether bash keeps
// the words apart when it runs it. This runs that argv under the host's bash,
// with the command swapped for a printf that echoes each argument it receives,
// and checks every word comes back byte for byte: runs of spaces, a `;`, a
// quote, a `$` and a glob included. -l is dropped so the host's own profile is
// not sourced into the test.
func TestShellArgvSurvivesBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on this host")
	}
	words := []string{"a   b", "echo FIRST; echo SECOND", "it's", `"quoted"`, "$HOME", "*", "a\nb", ""}

	argv := shellArgv(append([]string{"printf", `%s\0`}, words...))
	if argv[1] != "-lc" {
		t.Fatalf("shellArgv = %q, want bash -lc", argv)
	}
	out, err := exec.Command(bash, append([]string{"-c"}, argv[2:]...)...).Output()
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if !slices.Equal(got, words) {
		t.Errorf("guest received %q, want %q", got, words)
	}
}
