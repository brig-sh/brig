//go:build !darwin

package runtime

import "os/exec"

// lookHull is exec.LookPath for the hull that decides the default, a variable
// so a test can give the host one or take it away.
var lookHull = func() error { _, err := exec.LookPath("hull"); return err }

// defaultKind is hull when a hull is on PATH, and nerdctl otherwise. hull on
// Linux enforces what nerdctl does through the same urunc shim, and serves
// the policies, routes and published ports of hull on hvi on the same network
// gateway, so a host that carries it gets it. brig-standalone-linux ships
// hull beside nerdctl since v0.1.0-rc22; a host without it keeps nerdctl, and
// BRIG_RUNTIME still names either one.
func defaultKind() string {
	if lookHull() == nil {
		return "hull"
	}
	return "nerdctl"
}
