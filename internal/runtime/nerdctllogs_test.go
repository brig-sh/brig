package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// stubLogs is a nerdctl whose --tail fails, so a test that passes has not relied
// on it, and that records nothing else: the log is all it knows.
func stubLogs(t *testing.T) *nerdctl {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "nerdctl")
	script := `#!/bin/sh
case " $* " in
  *" --follow "*) printf 'argv: %s\n' "$*"; exit 0 ;;
  *" --tail "*) echo "--tail reached nerdctl" >&2; exit 3 ;;
  *" gone "*) echo "no such container" >&2; exit 1 ;;
esac
printf 'one\nexec sleep failed\nreboot\n'
printf 'warn\ninfo\n' >&2
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &nerdctl{bin: bin}
}

// A real nerdctl answered --tail 3 with a carriage return for a log that held
// the reason its guest died.
func TestNerdctlLogsCutsTheTailItself(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := stubLogs(t).Logs(LogsSpec{Name: "x", Tail: 2, Out: &out, Err: &errOut}); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "exec sleep failed\nreboot\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if got, want := errOut.String(), "warn\ninfo\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

// Following is the one path that still hands the flag to nerdctl.
func TestNerdctlLogsPassesTheTailWhenFollowing(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := stubLogs(t).Logs(LogsSpec{Name: "x", Follow: true, Tail: 2, Out: &out, Err: &errOut}); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "argv: logs --follow --tail 2 x\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

// --tail 0 asks for none of the log. A failure still has to say why.
func TestNerdctlLogsKeepsItsOwnErrorAtTailZero(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := stubLogs(t).Logs(LogsSpec{Name: "gone", Tail: 0, Out: &out, Err: &errOut}); err == nil {
		t.Fatal("a failed read returned no error")
	}
	if got := errOut.String(); got != "no such container\n" {
		t.Errorf("stderr = %q", got)
	}
	errOut.Reset()
	if err := stubLogs(t).Logs(LogsSpec{Name: "x", Tail: 0, Out: &out, Err: &errOut}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Errorf("--tail 0 printed %q and %q", out.String(), errOut.String())
	}
}

func TestNerdctlLogsWithoutATailIsTheWholeLog(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := stubLogs(t).Logs(LogsSpec{Name: "x", Tail: -1, Out: &out, Err: &errOut}); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "one\nexec sleep failed\nreboot\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestLogTail(t *testing.T) {
	for _, tc := range []struct {
		n        int
		in, want string
	}{
		{1, "a\nb\n", "b\n"},
		{1, "a\nb", "b"},
		{5, "a\nb\n", "a\nb\n"},
		{0, "a\nb\n", ""},
	} {
		l := &logTail{n: tc.n}
		// In two writes: a chunk does not end on a line.
		mid := len(tc.in) / 2
		_, _ = l.Write([]byte(tc.in[:mid]))
		_, _ = l.Write([]byte(tc.in[mid:]))
		if got := string(l.buf); got != tc.want {
			t.Errorf("last %d of %q = %q, want %q", tc.n, tc.in, got, tc.want)
		}
	}
}
