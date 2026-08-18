// Package ttytest hands a test a real terminal.
//
// It exists because brig makes two decisions on whether it is talking to one
// -- whether there is anybody to answer a confirmation, and whether a value is
// about to land in somebody's scrollback -- and a test that fakes the answer
// tests the fake. /dev/null was the stand-in before, on the strength of being
// a character device, and it is exactly what a real check has to reject: the
// suite could not tell a confirmation that fails closed from one that fails
// open, because neither branch was ever reached.
//
// A pseudo-terminal is the only thing that answers the ioctl the way a
// terminal does, so the tests allocate one. Nothing outside a test uses this,
// and it adds no dependency: the two ioctls are in syscall on both operating
// systems brig ships for.
package ttytest

import (
	"os"
	"testing"
)

// Pair allocates a pseudo-terminal and returns both ends: master is what a
// test writes the user's answer into, slave is the terminal the code under
// test sees. Both are closed when the test finishes.
//
// A platform with no pseudo-terminal skips the test rather than failing it.
// The alternative is a suite that cannot be run on that platform at all, and
// the property being checked is about brig's own logic, not about that
// machine.
func Pair(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, s, err := open()
	if err != nil {
		t.Skipf("no pseudo-terminal on this machine: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		_ = m.Close()
	})
	return m, s
}

// AsStdin points os.Stdin at a real terminal for the duration of the test, and
// returns the end the test types into.
func AsStdin(t *testing.T) *os.File {
	t.Helper()
	master, slave := Pair(t)
	old := os.Stdin
	os.Stdin = slave
	t.Cleanup(func() { os.Stdin = old })
	return master
}
