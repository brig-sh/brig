package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/brig-sh/brig/internal/runtime"
)

// logRuntime records the one request `brig logs` makes of the runtime. The
// interface is embedded rather than implemented, the repo's pattern: a call to
// any other method is a call `brig logs` has no business making, and the nil
// panic says that louder than a stub returning nothing.
type logRuntime struct {
	runtime.Runtime
	spec runtime.LogsSpec
	err  error
}

func (r *logRuntime) Logs(spec runtime.LogsSpec) error {
	r.spec = spec
	return r.err
}

// The filter is the part most likely to be wrong, so it gets a table of its
// own: what a runtime replays into a log versus what a reader should see.
func TestStripControl(t *testing.T) {
	cases := []struct {
		name     string
		in, want string
	}{
		{"plain text is untouched", "hello world\n", "hello world\n"},
		{"newline tab and return survive", "a\tb\r\nc\n", "a\tb\r\nc\n"},
		{"a CSI colour sequence goes", "a\x1b[31mred\x1b[0m", "ared"},
		{"a cursor move goes", "before\x1b[2Kafter", "beforeafter"},
		{"an OSC title ends at BEL", "x\x1b]0;my title\x07y", "xy"},
		{"an OSC title ends at ST", "x\x1b]0;my title\x1b\\y", "xy"},
		{"a charset selection goes", "a\x1b(Bb", "ab"},
		{"a bare escape pair goes", "a\x1bcb", "ab"},
		{"stray control bytes are dropped", "a\x00\x07b", "ab"},
		{"DEL is dropped", "a\x7fb", "ab"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := string(stripControl([]byte(c.in))); got != c.want {
				t.Errorf("stripControl(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// A sequence split across two writes -- which streaming makes ordinary -- is
// still filtered whole: the state rides on the writer, not on one Write.
func TestControlFilterAcrossWrites(t *testing.T) {
	var buf bytes.Buffer
	f := &controlFilter{w: &buf}
	if _, err := f.Write([]byte("a\x1b[")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("31mb")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "ab" {
		t.Errorf("a split escape was not filtered whole: got %q, want %q", got, "ab")
	}
}

// The whole input must read as written even though bytes were dropped, or
// io.Copy from a runtime's pipe reports ErrShortWrite.
func TestControlFilterReportsFullWrite(t *testing.T) {
	in := []byte("a\x1b[31mb")
	n, err := (&controlFilter{w: io.Discard}).Write(in)
	if err != nil || n != len(in) {
		t.Errorf("Write(%q) = %d, %v; want %d, nil", in, n, err, len(in))
	}
}

// --follow and --tail reach the adapter with the values the line carried, and
// the sandbox name resolved for it.
func TestStreamLogsPassesFollowAndTail(t *testing.T) {
	rt := &logRuntime{}
	if err := streamLogs(rt, "brig-claude-code", logsOptions{follow: true, tail: 100}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if rt.spec.Name != "brig-claude-code" {
		t.Errorf("the adapter got name %q, want %q", rt.spec.Name, "brig-claude-code")
	}
	if !rt.spec.Follow {
		t.Error("--follow did not reach the adapter")
	}
	if rt.spec.Tail != 100 {
		t.Errorf("the adapter got tail %d, want 100", rt.spec.Tail)
	}
}

// By default the writer handed to the adapter filters control sequences; --raw
// hands the bytes through untouched.
func TestStreamLogsFiltersUnlessRaw(t *testing.T) {
	dirty := []byte("a\x1b[31mb")

	var filteredBuf bytes.Buffer
	rt := &logRuntime{}
	if err := streamLogs(rt, "brig-x", logsOptions{tail: -1}, &filteredBuf); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.spec.Out.Write(dirty); err != nil {
		t.Fatal(err)
	}
	if got := filteredBuf.String(); got != "ab" {
		t.Errorf("the default writer did not filter: got %q, want %q", got, "ab")
	}

	var rawBuf bytes.Buffer
	rt = &logRuntime{}
	if err := streamLogs(rt, "brig-x", logsOptions{tail: -1, raw: true}, &rawBuf); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.spec.Out.Write(dirty); err != nil {
		t.Fatal(err)
	}
	if got := rawBuf.Bytes(); !bytes.Equal(got, dirty) {
		t.Errorf("--raw filtered the bytes: got %q, want %q", got, dirty)
	}
}

// --gateway reads the log named for the socket, filtering it like any other
// log. The path is asked of the runtime package rather than spelled here, so
// the test follows the writer's rule for where the log goes instead of pinning
// a second one.
func TestGatewayLogsReadsTheFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_GATEWAY_SOCK", filepath.Join(dir, "gateway-test.sock"))
	path, err := runtime.GatewayLogPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("gateway up\x1b[0m\nlistening\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := gatewayLogs(&buf, logsOptions{tail: -1}); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), "gateway up\nlistening\n"; got != want {
		t.Errorf("the gateway log was not read filtered: got %q, want %q", got, want)
	}
}

// A gateway log that does not exist yet is not a fault: it appears only once a
// sandbox has needed the host gateway, and the reader is told that, not handed
// a bare open error. It is a not-found, so a script can tell it from a failure.
func TestGatewayLogsMissing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_GATEWAY_SOCK", filepath.Join(dir, "gateway-test.sock"))

	err := gatewayLogs(io.Discard, logsOptions{tail: -1})
	if err == nil {
		t.Fatal("a missing gateway log was reported as success")
	}
	if _, ok := err.(*notFoundError); !ok {
		t.Errorf("a missing gateway log is not a not-found error: %T: %v", err, err)
	}
}

func TestLastLines(t *testing.T) {
	data := []byte("one\ntwo\nthree\n")
	cases := []struct {
		n    int
		want string
	}{
		{0, ""},
		{1, "three\n"},
		{2, "two\nthree\n"},
		{5, "one\ntwo\nthree\n"},
	}
	for _, c := range cases {
		if got := string(lastLines(data, c.n)); got != c.want {
			t.Errorf("lastLines(_, %d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// logsCmd's own guards, before any runtime or profile is touched.
func TestLogsCmdArgErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no ref and no gateway", nil},
		{"a ref that does not parse", []string{"claude@@x"}},
		{"a gateway with a ref", []string{"--gateway", "claude"}},
		{"an unknown flag", []string{"--nope", "claude"}},
		{"tail without a number", []string{"--tail", "claude"}},
		{"a second ref", []string{"claude", "extra"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := logsCmd(c.args)
			if err == nil {
				t.Fatalf("logsCmd(%q) returned no error", c.args)
			}
			// Every one of these is a mistake on the line, so every one is the
			// usage class -- the ref that does not parse included, which is the
			// class run already gives it.
			if exitCode(err) != exitUsage {
				t.Errorf("logsCmd(%q) exits %d, want %d: %v", c.args, exitCode(err), exitUsage, err)
			}
		})
	}
}
