//go:build !linux || !(amd64 || arm64)

package main

// hardenSandbox does nothing where the broker has no seccomp filter. A broker
// for one sandbox is a Linux node daemon; elsewhere --sandbox only limits it
// to one registration and one policy.
func hardenSandbox() error { return nil }
