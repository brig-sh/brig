package proc

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"strings"
)

// startTime is field 22 of /proc/<pid>/stat: the clock ticks from boot to
// the start of pid.
func startTime(pid int) (string, error) {
	blob, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	// Field 2 is the command name in parentheses, and the name can hold
	// spaces and parentheses itself, so the fields after it start past the
	// last one. That is field 3, which puts field 22 at index 19.
	i := bytes.LastIndexByte(blob, ')')
	if i < 0 {
		return "", errors.New("/proc stat has no command name")
	}
	f := strings.Fields(string(blob[i+1:]))
	if len(f) < 20 {
		return "", errors.New("/proc stat is too short")
	}
	return f[19], nil
}
