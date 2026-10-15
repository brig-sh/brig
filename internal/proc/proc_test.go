package proc

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// The start time of a process is the same whatever locale reads it, a later
// process has another one, and a process that exited has none.
func TestStartTimeIgnoresTheLocaleAndTellsProcessesApart(t *testing.T) {
	self, err := StartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	for _, loc := range []string{"C", "el_GR.UTF-8", "de_DE.UTF-8"} {
		t.Setenv("LC_ALL", loc)
		t.Setenv("LC_TIME", loc)
		again, err := StartTime(os.Getpid())
		if err != nil || again != self {
			t.Errorf("under %s the start time is %q (%v), want %q", loc, again, err, self)
		}
	}
	// Linux counts the start in clock ticks of 10 ms.
	time.Sleep(50 * time.Millisecond)
	child := exec.Command("sleep", "5")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	other, err := StartTime(child.Process.Pid)
	_ = child.Process.Kill()
	_ = child.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if other == self {
		t.Errorf("a later process has the same start time %q", other)
	}
	if got, err := StartTime(child.Process.Pid); err == nil {
		t.Errorf("a process that exited has the start time %q", got)
	}
	if _, err := StartTime(0); err == nil {
		t.Error("pid 0 has a start time")
	}
}
