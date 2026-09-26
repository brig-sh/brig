package wrap

import (
	"errors"
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
	want := []string{"bash", "-lc", `"$@"`, "bash", "sh", "-c", "echo FIRST; echo SECOND"}

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

// runShellArgv runs what shellArgv builds for command under the host's bash,
// with -l dropped from its flags so the host's own profile is not sourced into
// the test, and returns the exit status.
func runShellArgv(t *testing.T, command ...string) int {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on this host")
	}
	argv := shellArgv(command)
	flags := strings.Replace(argv[1], "l", "", 1)
	err = exec.Command(bash, append([]string{flags}, argv[2:]...)...).Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.ExitCode()
	}
	t.Fatalf("bash: %v", err)
	return 0
}

// The first word is the command, even when it starts with a dash. Under
// `exec "$@"` bash read `-l` as exec's own option, ran nothing and exited 0,
// and `-a foo bar` ran bar under another name.
func TestShellLeadingDashIsACommand(t *testing.T) {
	if got := runShellArgv(t, "-l"); got != 127 {
		t.Errorf("a first word of -l exited %d, want 127 for a command not found", got)
	}
}

// sh runs under a login shell so the command gets what that shell sets up,
// and that includes the shell's own builtins and the functions its profile
// defines: `brig sh x ulimit -n` and `brig sh x nvm use 20` ran before the
// words were passed through and have to keep running. shopt is the builtin
// checked because it has no binary of the same name on any host, where
// ulimit and type do on macOS.
func TestShellRunsABuiltin(t *testing.T) {
	if got := runShellArgv(t, "shopt", "-q", "sourcepath"); got != 0 {
		t.Errorf("shopt exited %d, want 0", got)
	}
}

// -c as the first word is sh's own -c: the next word is a script for the login
// shell, parsed in the guest, so pipes, ~ and $VAR mean what they mean there.
// It is the one spelling that asks for the parse the plain form never does.
func TestShellScriptFlag(t *testing.T) {
	rec := &recordingRuntime{}
	c := &Config{VMName: "vm", Runtime: rec}
	if err := c.Shell(creds.Set{}, []string{"-c", "ls /work | wc -l"}); err != nil {
		t.Fatalf("shell: %v", err)
	}
	if want := []string{"bash", "-lc", "ls /work | wc -l"}; !slices.Equal(rec.spec.Cmd, want) {
		t.Errorf("Shell ran %q, want %q", rec.spec.Cmd, want)
	}
}

// Words after the script are its $0, $1 and on, the way sh -c takes them, and
// the script itself is parsed by bash rather than looked up as a command.
func TestShellScriptFlagUnderBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on this host")
	}
	argv := shellArgv([]string{"-c", `echo "$0:$1" | tr a-z A-Z`, "x", "y"})
	if argv[1] != "-lc" {
		t.Fatalf("shellArgv = %q, want bash -lc", argv)
	}
	out, err := exec.Command(bash, append([]string{"-c"}, argv[2:]...)...).Output()
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "X:Y" {
		t.Errorf("script printed %q, want X:Y", got)
	}
}

// -c takes the flags people put in front of it with sh -c: -ec to stop at the
// first failure, -xc to trace, -uc for unset variables. They apply to the
// script alone, set at its start, not to the login shell: bash -lxc traced
// every line of /etc/profile and ~/.bashrc before the first line of the
// script, and -e or -u there would stop on a profile's own habits.
func TestShellScriptFlagCombined(t *testing.T) {
	if got, want := shellArgv([]string{"-ec", "false; echo no"}), []string{"bash", "-lc", "set -e; false; echo no"}; !slices.Equal(got, want) {
		t.Errorf("shellArgv = %q, want %q", got, want)
	}
	if got, want := shellArgv([]string{"-lc", "x"}), []string{"bash", "-lc", "x"}; !slices.Equal(got, want) {
		t.Errorf("shellArgv = %q, want %q", got, want)
	}
	if got := runShellArgv(t, "-ec", "false; echo reached"); got != 1 {
		t.Errorf("-ec kept going past a failure: exit %d, want 1", got)
	}
	// Anything else that starts with a dash is still a command name.
	if got := runShellArgv(t, "-zc", "echo x"); got != 127 {
		t.Errorf("-zc exited %d, want 127 for a command not found", got)
	}
}

// A script flag with no script, or an empty one, is refused rather than handed
// to bash: bash -lc exits 0 on an empty script, and a caller whose $SCRIPT was
// unset reads that as success. The check lives beside shellArgv so every
// caller of Shell gets it, not only the verbs dispatch remembers to check.
func TestShellScriptFlagNeedsAScript(t *testing.T) {
	for _, command := range [][]string{{"-c"}, {"-c", ""}, {"-c", "  \n"}, {"-ec"}} {
		if ShellCommandError(command) == nil {
			t.Errorf("ShellCommandError(%q) = nil, want a refusal", command)
		}
		rec := &recordingRuntime{}
		c := &Config{VMName: "vm", Runtime: rec}
		if err := c.Shell(creds.Set{}, command); err == nil {
			t.Errorf("Shell(%q) ran %q, want a refusal", command, rec.spec.Cmd)
		}
		if _, err := c.ShellAttached(creds.Set{}, command); err == nil {
			t.Errorf("ShellAttached(%q) ran, want a refusal", command)
		}
	}
	for _, command := range [][]string{nil, {"ls"}, {"-c", "ls"}, {"-l"}} {
		if err := ShellCommandError(command); err != nil {
			t.Errorf("ShellCommandError(%q) = %v, want nil", command, err)
		}
	}
}
