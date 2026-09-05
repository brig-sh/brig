package wrap

import "syscall"

// TCGETS is Linux's spelling of the same request TIOCGETA makes on darwin.
const ioctlReadTermios = syscall.TCGETS
