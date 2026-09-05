package wrap

import "os"

// IsTerminal reports whether a file is a terminal, which decides both whether
// a guest exec allocates a pseudo-terminal and whether there is anyone to
// answer a question.
//
// It asks the terminal driver -- the same ioctl isatty(3) makes -- rather than
// asking the file whether it is a character device. Those are not the same
// question, and the difference is load-bearing: /dev/null is a character
// device, so `brig run < /dev/null` counted as a terminal, and the one check
// that stops a boot -- an image that claims to be ours and fails verification
// -- put its question to a file that answers nothing. It failed closed only by
// accident, because the read that followed hit EOF; a confirm() that took
// silence for consent would have failed open with nothing in the tests to say
// so. /dev/zero is the same mistake with a worse ending: an unbounded read
// that never returns.
//
// golang.org/x/term does exactly this and is not used on purpose. brig has one
// direct dependency and keeps it that way; the whole of the check is one ioctl
// per platform, below.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	return isatty(f.Fd())
}
