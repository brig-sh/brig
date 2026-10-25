package proc

import (
	"os"
	"testing"
)

// The table names each process's parent and the start time StartTime reads.
func TestTableAgreesWithStartTime(t *testing.T) {
	table, err := Table()
	if err != nil {
		t.Fatal(err)
	}
	self, err := StartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	e, ok := table[os.Getpid()]
	if !ok || e.Start != self || e.PPID != os.Getppid() {
		t.Errorf("the table has %+v (%v) for this process, want parent %d, start %q", e, ok, os.Getppid(), self)
	}
}
