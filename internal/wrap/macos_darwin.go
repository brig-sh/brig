package wrap

import "syscall"

// macOSVersion reports the host's macOS product version, like "14.5".
//
// Read from the kern.osproductversion sysctl rather than by shelling out to
// sw_vers: it is one standard-library call with no subprocess, and brig keeps
// the number of external commands it runs small on purpose. The value drives
// one decision, preflightHypervisor, which compares only the major component,
// so the SYSTEM_VERSION_COMPAT shim that can report "10.16" to a binary linked
// against an old SDK does not reach a modern Go build like this one.
//
// An error is reported as "" -- the same as being off macOS -- so a host that
// will not answer for its own version lets the run proceed rather than being
// refused over a reading brig could not take.
func macOSVersion() string {
	v, err := syscall.Sysctl("kern.osproductversion")
	if err != nil {
		return ""
	}
	return v
}
