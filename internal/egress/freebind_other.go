//go:build !linux

package egress

import "syscall"

// freebind does nothing off Linux, where nothing starts the resolver.
func freebind(_, _ string, _ syscall.RawConn) error { return nil }
