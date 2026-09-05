//go:build !darwin

package main

import "os"

// probeVirtualization reports whether this host can back a guest with a kernel
// of its own, and how it can tell.
//
// /dev/kvm is that answer on Linux: the container runtime brig drives boots a
// microVM through urunc, which needs the KVM device present and openable. Its
// mere existence is not enough -- a host can carry the node while the current
// user lacks access to it -- so the probe opens it, which is the same thing the
// boot will do, and reports the fact rather than gating on it, per #9.
func probeVirtualization() (bool, string) {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return false, "/dev/kvm could not be opened: " + err.Error()
	}
	_ = f.Close()
	return true, "/dev/kvm is present and openable"
}
