package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// brokenPipeHelper names the run of this test binary that writes to a
// stderr with no reader, as brig does when whoever read its stderr has gone.
const brokenPipeHelper = "BRIG_TEST_BROKEN_PIPE"

// A stderr whose reader has gone does not kill brig while it catches the
// signal: the writes fail and brig goes on, as it must between hull's
// commit and the record of it. Without catchBrokenPipe the same writes kill
// the process, which is what the second run shows, so the first run's
// survival is the catching and not the platform.
func TestABrokenStderrDoesNotKillBrigWhileItCatchesThePipe(t *testing.T) {
	if mode := os.Getenv(brokenPipeHelper); mode != "" {
		if mode == "catch" {
			defer catchBrokenPipe()()
		}
		for range 100 {
			_, _ = os.Stderr.WriteString("brig: hull: move: ram-round-0: 74.0 MiB in 2.88 s, 25.7 MiB/s\n")
		}
		_, _ = os.Stdout.WriteString("still running\n")
		return
	}
	for _, c := range []struct {
		mode    string
		survive bool
	}{{"catch", true}, {"default", false}} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Close()
		cmd := exec.Command(os.Args[0], "-test.run=^TestABrokenStderrDoesNotKillBrigWhileItCatchesThePipe$")
		cmd.Env = append(os.Environ(), brokenPipeHelper+"="+c.mode)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, w
		err = cmd.Run()
		_ = w.Close()
		survived := strings.Contains(out.String(), "still running")
		var exit *exec.ExitError
		killed := errors.As(err, &exit) && exit.Sys().(syscall.WaitStatus).Signal() == syscall.SIGPIPE
		if survived != c.survive || killed == c.survive {
			t.Errorf("%s: survived %v, killed by SIGPIPE %v (%v, stdout %q)", c.mode, survived, killed, err, out.String())
		}
	}
}
