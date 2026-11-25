// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

// Package ptyrelay runs a command on a pseudo-terminal that brig owns, copies
// bytes between the user's terminal and that pseudo-terminal, and hands every
// byte the command writes to an optional tap. The command's stderr goes on the
// pseudo-terminal only when the user's stderr is the same terminal as stdout.
//
// It exists so brig can stay the parent of an interactive exec (instead of
// replacing itself with syscall.Exec) and still see the terminal output. The
// tap carries output only; input is never copied to it.
package ptyrelay

// Job control: the command runs as the leader of its own session, so its
// process group is orphaned and the kernel discards a stop signal sent to it.
// Ctrl-Z typed by the user therefore does nothing to the command. A shell
// inside the sandbox that offers job control for its own children is
// unaffected.
