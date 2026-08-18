package wrap

import "syscall"

// TIOCGETA is BSD's "get the termios struct", which is what macOS answers for
// a terminal and refuses for anything else.
const ioctlReadTermios = syscall.TIOCGETA
