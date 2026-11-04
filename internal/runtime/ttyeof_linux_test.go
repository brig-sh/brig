package runtime

import (
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/brig-sh/brig/internal/ttytest"
)

// sendEOF writes the terminal's EOF character into master, as script(1) does
// when its stdin ends, and waits until the slave's line discipline has queued it.
func sendEOF(t *testing.T, master, slave *os.File) {
	t.Helper()
	if _, err := master.Write([]byte{4}); err != nil {
		t.Fatal(err)
	}
	waitReadable(t, slave, time.Second)
}

// waitReadable fails the test unless f becomes readable within d.
func waitReadable(t *testing.T, f *os.File, d time.Duration) {
	t.Helper()
	pfd := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLIN}}
	if n, err := unix.Poll(pfd, int(d/time.Millisecond)); err != nil || n != 1 {
		t.Fatalf("the slave never became readable: n=%d err=%v", n, err)
	}
}

// makeRawLikeNerdctl is what term.MakeRaw does: TCSETS with ICANON and ECHO off.
func makeRawLikeNerdctl(t *testing.T, f *os.File) {
	t.Helper()
	tio, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	tio.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	tio.Oflag &^= unix.OPOST
	tio.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	tio.Cc[unix.VMIN], tio.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(int(f.Fd()), unix.TCSETS, tio); err != nil {
		t.Fatal(err)
	}
}

// readAvailable returns what a raw-mode reader gets within d.
func readAvailable(t *testing.T, f *os.File, d time.Duration) []byte {
	t.Helper()
	pfd := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLIN}}
	if n, _ := unix.Poll(pfd, int(d/time.Millisecond)); n != 1 {
		return nil
	}
	buf := make([]byte, 64)
	n, _ := unix.Read(int(f.Fd()), buf)
	return buf[:n]
}

// eofMarkPending reports whether f holds a lone EOF mark: readable, with
// nothing that TIOCINQ counts. It is the state consumePendingEOF looks for.
func eofMarkPending(t *testing.T, f *os.File) bool {
	t.Helper()
	pfd := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLIN}}
	n, err := unix.Poll(pfd, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		return false
	}
	queued, err := unix.IoctlGetInt(int(f.Fd()), unix.TIOCINQ)
	if err != nil {
		t.Fatal(err)
	}
	return queued == 0
}

// The kernel behaviour the fix exists for: an EOF queued in canonical mode is
// read as a 0x00 byte once the terminal is raw.
func TestPendingEOFBecomesNULWhenRaw(t *testing.T) {
	master, slave := ttytest.Pair(t)
	sendEOF(t, master, slave)
	makeRawLikeNerdctl(t, slave)
	if got := readAvailable(t, slave, 200*time.Millisecond); string(got) != "\x00" {
		t.Fatalf("raw read after a queued EOF = %q, want a lone NUL", got)
	}
}

func TestConsumePendingEOFLeavesNoNUL(t *testing.T) {
	master, slave := ttytest.Pair(t)
	sendEOF(t, master, slave)
	consumePendingEOF(slave)
	makeRawLikeNerdctl(t, slave)
	if got := readAvailable(t, slave, 200*time.Millisecond); len(got) != 0 {
		t.Fatalf("raw read after consumePendingEOF = %q, want nothing", got)
	}
}

func TestConsumePendingEOFKeepsTypedLine(t *testing.T) {
	master, slave := ttytest.Pair(t)
	if _, err := master.Write([]byte("ls\n")); err != nil {
		t.Fatal(err)
	}
	waitReadable(t, slave, time.Second)
	consumePendingEOF(slave)
	makeRawLikeNerdctl(t, slave)
	if got := readAvailable(t, slave, 200*time.Millisecond); string(got) != "ls\n" {
		t.Fatalf("typed line after consumePendingEOF = %q, want %q", got, "ls\n")
	}
}

func TestConsumePendingEOFDoesNotBlock(t *testing.T) {
	_, slave := ttytest.Pair(t)
	done := make(chan struct{})
	go func() { consumePendingEOF(slave); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("consumePendingEOF blocked on a terminal with nothing queued")
	}
}

// The call sites. The stub stands where nerdctl's own raw switch would run, so
// what the stub reads on stdin is what the guest would have been sent.
func TestReplaceConsumesPendingEOFBeforeTheHandover(t *testing.T) {
	master := ttytest.AsStdin(t)
	sendEOF(t, master, os.Stdin)
	ran := false
	var got []byte
	prev := execHandover
	execHandover = func(string, []string, []string) error {
		ran = true
		makeRawLikeNerdctl(t, os.Stdin)
		got = readAvailable(t, os.Stdin, 200*time.Millisecond)
		return nil
	}
	t.Cleanup(func() { execHandover = prev })
	spec := ExecSpec{Name: "vm", Cmd: []string{"bash"}, TTY: true}
	if err := (&nerdctl{bin: "/usr/bin/nerdctl"}).Replace(spec); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("Replace did not reach execHandover")
	}
	if len(got) != 0 {
		t.Fatalf("the guest would have been sent %q, want nothing", got)
	}
}

func TestAttachConsumesPendingEOFBeforeTheHandover(t *testing.T) {
	master := ttytest.AsStdin(t)
	sendEOF(t, master, os.Stdin)
	ran := false
	var got []byte
	prev := attachHandover
	attachHandover = func([]string, []string) (int, error) {
		ran = true
		makeRawLikeNerdctl(t, os.Stdin)
		got = readAvailable(t, os.Stdin, 200*time.Millisecond)
		return 0, nil
	}
	t.Cleanup(func() { attachHandover = prev })
	spec := ExecSpec{Name: "vm", Cmd: []string{"bash"}, TTY: true}
	if _, err := (&nerdctl{bin: "/usr/bin/nerdctl"}).Attach(spec); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("Attach did not reach attachHandover")
	}
	if len(got) != 0 {
		t.Fatalf("the guest would have been sent %q, want nothing", got)
	}
}

// The negative case. An exec without a pty is `exec -i` alone: nerdctl never
// switches to raw, and the EOF has to stay queued so that it closes the guest's
// stdin.
func TestReplaceLeavesStdinAloneWithoutATTY(t *testing.T) {
	master := ttytest.AsStdin(t)
	sendEOF(t, master, os.Stdin)
	ran := false
	pending := false
	prev := execHandover
	execHandover = func(string, []string, []string) error {
		ran = true
		pending = eofMarkPending(t, os.Stdin)
		return nil
	}
	t.Cleanup(func() { execHandover = prev })
	spec := ExecSpec{Name: "vm", Cmd: []string{"bash"}, TTY: false}
	if err := (&nerdctl{bin: "/usr/bin/nerdctl"}).Replace(spec); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("Replace did not reach execHandover")
	}
	if !pending {
		t.Fatal("the EOF mark was consumed although the exec has no tty")
	}
}
