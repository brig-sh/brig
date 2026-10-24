//go:build !darwin && !linux

package proc

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// startTime is what ps(1) prints for pid, in the C locale so that every
// reader prints it alike.
func startTime(pid int) (string, error) {
	cmd := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.Join(strings.Fields(string(out)), " "), nil
}
