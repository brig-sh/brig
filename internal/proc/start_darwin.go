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
