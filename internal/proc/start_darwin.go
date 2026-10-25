package proc

import (
	"strconv"

	"golang.org/x/sys/unix"
)

// startTime is p_starttime from the kernel's record of pid, in seconds and
// microseconds.
func startTime(pid int) (string, error) {
	// A pid with no process gets an empty record, which x/sys reports as
	// EIO.
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	return start(kp), nil
}

func start(kp *unix.KinfoProc) string {
	tv := kp.Proc.P_starttime
	return strconv.FormatInt(int64(tv.Sec), 10) + "." + strconv.FormatInt(int64(tv.Usec), 10)
}

// Entry is one process of the table: its parent and its start time, which
// equals what StartTime returns for it.
type Entry struct {
	PPID  int
	Start string
}

// Table reads every process at once, for a caller that walks a process tree.
func Table() (map[int]Entry, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	table := make(map[int]Entry, len(procs))
	for i := range procs {
		kp := &procs[i]
		table[int(kp.Proc.P_pid)] = Entry{PPID: int(kp.Eproc.Ppid), Start: start(kp)}
	}
	return table, nil
}
