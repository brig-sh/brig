package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubLogs is a nerdctl whose --tail fails, so a test that passes has not relied
// on it, and that records nothing else: the log is all it knows. Its streams
// interleave as a guest's do, with a pause between them so their order is the
// one it wrote.
func stubLogs(t *testing.T) *nerdctl {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "nerdctl")
	script := `#!/bin/sh
case " $* " in
  *" --follow "*) printf 'argv: %s\n' "$*"; exit 0 ;;
  *" --tail "*) echo "--tail reached nerdctl" >&2; exit 3 ;;
  *" gone "*) echo "no such container" >&2; exit 1 ;;
esac
printf 'one\nstart\n'
sleep 0.2
printf 'panic: X\n' >&2
sleep 0.2
printf 'exit\n'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &nerdctl{bin: bin}
}

// ordered records what each stream was given, in the order it was given.
type ordered struct {
	mu   sync.Mutex
	seen []string
}

func (o *ordered) stream(name string) *orderedStream { return &orderedStream{o, name} }

type orderedStream struct {
	o    *ordered
	name string
}

func (s *orderedStream) Write(p []byte) (int, error) {
	s.o.mu.Lock()
	defer s.o.mu.Unlock()
	s.o.seen = append(s.o.seen, s.name+":"+string(p))
	return len(p), nil
}

// A real nerdctl answered --tail 3 with a carriage return for a log that held
// the reason its guest died. nerdctl counts both streams together, and the panic
// on stderr stays between the lines around it.
func TestNerdctlLogsCutsTheTailItself(t *testing.T) {
	var o ordered
	if err := stubLogs(t).Logs(LogsSpec{Name: "x", Tail: 3, Out: o.stream("out"), Err: o.stream("err")}); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(o.seen, ""), "out:start\nerr:panic: X\nout:exit\n"; got != want {
		t.Errorf("the tail = %q, want %q", got, want)
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
	if got, want := out.String(), "one\nstart\nexit\n"; got != want {
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
		{5, "a\nb", "a\nb"},
		{0, "a\nb\n", ""},
		{2, "a\r\nb\r\nc\r\n", "b\r\nc\r\n"},
		{2, "", ""},
		{2, "no newline", "no newline"},
	} {
		l := &logTail{n: tc.n}
		// In two writes: a chunk does not end on a line.
		mid := len(tc.in) / 2
		_, _ = l.stream(0).Write([]byte(tc.in[:mid]))
		_, _ = l.stream(0).Write([]byte(tc.in[mid:]))
		var out, errOut bytes.Buffer
		if err := l.replay(&out, &errOut, false); err != nil {
			t.Fatal(err)
		}
		if got := out.String(); got != tc.want {
			t.Errorf("last %d of %q = %q, want %q", tc.n, tc.in, got, tc.want)
		}
	}
}

// Each write costs its own length. A log of one very long line, or of many
// lines with a large tail, once rescanned the whole buffer on every write.
func TestLogTailIsLinear(t *testing.T) {
	chunk := bytes.Repeat([]byte("x"), 4096)
	line := append(bytes.Repeat([]byte("y"), 99), '\n')
	start := time.Now()
	long := &logTail{n: 10}
	for range 16 << 10 { // 64 MiB with no newline
		_, _ = long.stream(0).Write(chunk)
	}
	many := &logTail{n: 10000}
	for range 1 << 20 { // 100 MiB of short lines
		_, _ = many.stream(0).Write(line)
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Errorf("164 MiB took %s", took)
	}
	var out bytes.Buffer
	if err := many.replay(&out, &out, false); err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(out.Bytes(), []byte("\n")); got != 10000 {
		t.Errorf("the tail holds %d lines, want 10000", got)
	}
}
