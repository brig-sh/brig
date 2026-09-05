//go:build darwin

package main

import "syscall"

// probeVirtualization reports whether this Mac can back a microVM, and how it
// can tell.
//
// kern.hv_support is the cheap, honest probe: the kernel sets it to 1 exactly
// when Hypervisor.framework is usable on this hardware, which is what hull needs
// to boot a guest with a kernel of its own. It is read the same way
// macOSVersion reads its sysctl -- one standard-library call, no subprocess --
// and the fact is reported rather than gated on, per #9: doctor names the
// boundary, it does not refuse to run without one.
func probeVirtualization() (bool, string) {
	v, err := syscall.SysctlUint32("kern.hv_support")
	if err != nil {
		return false, "could not read kern.hv_support: " + err.Error()
	}
	if v == 1 {
		return true, "Hypervisor.framework available"
	}
	return false, "Hypervisor.framework unavailable (kern.hv_support=0)"
}
