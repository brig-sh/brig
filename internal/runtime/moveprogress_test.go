package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A printer that stops does not hold hull up. The fake hull prints far more
// than a pipe holds, then its closing line, then leaves a mark that it got
// to the end, while the printer is held: a Progress that printed on the
// goroutine reading hull's pipe would leave hull blocked on the full pipe
// and the move would not end. The report still reads every line, the
// closing one among them, and the Echo prints what it kept, then how many
// it dropped.
func TestAPrinterThatStopsDoesNotHoldHullUp(t *testing.T) {
	const n = 20000
	dir := t.TempDir()
	mark := filepath.Join(dir, "hull-finished")
	h, _ := migrateHull(t, "i=0; while [ $i -lt "+fmt.Sprint(n)+" ]; do "+
		"echo \"move: disk round 0: $i blocks so far, 17.1 MiB/s (1% of 4393)\" >&2; i=$((i+1)); done; "+
		"echo 'move: 109.5 MiB in 5.4 s; downtime about 358 ms' >&2; echo committed > RECORD; : > "+mark)
	release := make(chan struct{})
	var printed []string
	echo := NewEcho(func(line string) {
		<-release
		printed = append(printed, line)
	})
	var r MoveReport
	moved := make(chan error, 1)
	go func() {
		_, err := h.Migrate("s", "me@box", MigrateSpec{TxID: tx1, Progress: func(line string) {
			echo.Say(line)
			r.Add(line)
		}})
		moved <- err
	}()
	select {
	case err := <-moved:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		close(release)
		t.Fatal("hull did not finish while brig's printer was held")
	}
	if _, err := os.Stat(mark); err != nil {
		t.Errorf("hull did not get to its end: %v", err)
	}
	if r.DowntimeSeconds != 0.358 {
		t.Errorf("the report missed hull's closing line: %+v", r)
	}
	close(release)
	echo.Stop()
	last := printed[len(printed)-1]
	var dropped int
	if _, err := fmt.Sscanf(last, "%d of hull's lines were not shown", &dropped); err != nil || dropped == 0 {
		t.Fatalf("the Echo did not say it dropped lines: %q", last)
	}
	if len(printed)-1+dropped != n+1 {
		t.Errorf("printed %d and dropped %d of %d lines", len(printed)-1, dropped, n+1)
	}
	if !strings.HasPrefix(printed[0], "move: disk round 0: 0 blocks so far") {
		t.Errorf("the Echo did not print in order: %q", printed[0])
	}
}

// An Echo that keeps up prints every line, in order, by the time Stop
// returns, and says nothing of dropped lines.
func TestAnEchoThatKeepsUpPrintsEveryLine(t *testing.T) {
	var printed []string
	e := NewEcho(func(line string) { printed = append(printed, line) })
	for i := range 100 {
		e.Say(fmt.Sprint(i))
	}
	e.Stop()
	if len(printed) != 100 || printed[0] != "0" || printed[99] != "99" {
		t.Errorf("printed %d lines: %q", len(printed), printed)
	}
}
