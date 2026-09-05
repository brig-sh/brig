package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// A ref reads the log of the gateway serving that sandbox alone, not the shared
// one. The path is asked of the runtime package for the sandbox name, the same
// way the shared test does, so both follow the writer's rule for where a log
// goes rather than pinning one of their own.
func TestIsolatedGatewayLogsReadsTheFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_GATEWAY_DIR", dir)
	path, err := runtime.IsolatedGatewayLogPath("brig-claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("isolated gateway up\x1b[0m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The shared log holds something else, so reading it instead would be
	// visible rather than an empty pass.
	shared, err := runtime.GatewayLogPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shared, []byte("shared gateway up\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := isolatedGatewayLogs(&buf, logsOptions{tail: -1}, "claude@code", "brig-claude-code"); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), "isolated gateway up\n"; got != want {
		t.Errorf("the isolated gateway log was not read filtered: got %q, want %q", got, want)
	}

	// And bare --gateway still reads the shared one, with an isolated log
	// sitting beside it.
	buf.Reset()
	if err := gatewayLogs(&buf, logsOptions{tail: -1}); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), "shared gateway up\n"; got != want {
		t.Errorf("bare --gateway did not read the shared log: got %q, want %q", got, want)
	}
}

// An isolated gateway's log is removed with the gateway, so an absent one means
// the sandbox's gateway is not running. The message says that and names the ref
// the reader typed -- not the sandbox name, which they did not -- and it is a
// not-found, like the shared one, so a script can tell it from a failure.
func TestIsolatedGatewayLogsMissing(t *testing.T) {
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())

	err := isolatedGatewayLogs(io.Discard, logsOptions{tail: -1}, "claude@code", "brig-claude-code")
	if err == nil {
		t.Fatal("a missing isolated gateway log was reported as success")
	}
	if _, ok := err.(*notFoundError); !ok {
		t.Fatalf("a missing isolated gateway log is not a not-found error: %T: %v", err, err)
	}
	if got := err.Error(); !strings.Contains(got, "claude@code") || !strings.Contains(got, "not running") {
		t.Errorf("the message does not name the ref and say the gateway is not running: %q", got)
	}
}

// --gateway with a ref is no longer refused on the line. It names a sandbox
// whose gateway log is wanted, so whatever it goes on to fail at, it is not the
// usage error that used to reject the ref outright.
func TestGatewayWithRefIsNotAUsageError(t *testing.T) {
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	// A profile that does not exist stops the resolution at its first step, so
	// this touches no runtime: what is asserted is only that the ref got that
	// far, which the old parse-time refusal did not allow.
	err := logsCmd([]string{"--gateway", "no-such-profile-here"})
	if err == nil {
		t.Fatal("`brig logs --gateway <ref>` returned no error for an unknown profile")
	}
	if exitCode(err) == exitUsage {
		t.Errorf("`brig logs --gateway <ref>` is still a usage error: %v", err)
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
		{"a ref that does not parse, with --gateway", []string{"--gateway", "claude@@x"}},
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
