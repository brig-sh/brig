// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package recording

import "syscall"

// mask sets the process's umask and returns the one it replaced. The spool
// test runs with 0, so a 0700 or 0600 that holds is the code's own mode and
// not the umask's doing.
func mask(m int) int { return syscall.Umask(m) }
