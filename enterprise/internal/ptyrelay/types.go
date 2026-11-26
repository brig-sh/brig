// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package ptyrelay

import (
	"os"
	"syscall"
	"time"
)

// Options configures Run. The zero value relays os.Stdin and os.Stdout with no
// tee.
type Options struct {
	// In, Out and Err are the user's streams. They default to os.Stdin,
	// os.Stdout and os.Stderr. In is put in raw mode when it is a terminal. The
	// command's stderr is on the pseudo-terminal only when Err is the same
	// terminal device as Out.
	In, Out, Err *os.File

	// Tap, when set, receives what the command writes and the terminal size,
	// in order, from one goroutine. It is fed through a bounded queue: when it
	// falls behind, output is dropped (counted in Result.DroppedBytes) and the
	// Tap is told where, rather than the relay waiting for it. Input typed by
	// the user is never given to the Tap.
	Tap Tap

	// QueueBytes bounds the output waiting for the Tap. Zero means 4 MiB.
	QueueBytes int

	// Exited, when set, is called once, on the goroutine that called Run, as
	// soon as the command has ended and before the relay drains its output. It
	// is not called when the command was not started. A signal that reaches
	// the process after it is one the relay did not forward.
	Exited func()
}

// Tap observes a relayed command. at comes from time.Now, so it carries the
// monotonic clock. A slice passed to a method is the Tap's to keep.
type Tap interface {
	// Output is bytes the command wrote.
	Output(at time.Time, p []byte)
	// Dropped says n bytes of output were lost just before the next Output.
	Dropped(at time.Time, n int64)
	// Resize is the terminal size at start and after each change. It is not
	// called when the relay has no terminal to read a size from.
	Resize(at time.Time, cols, rows uint16)
}

// Result is how the command ended.
type Result struct {
	// ExitCode is the exit status, or 128 plus the signal number when a signal
	// ended the command.
	ExitCode int
	// Signal is the signal that ended the command, or 0.
	Signal syscall.Signal
	// OutputBytes counts the bytes the command wrote.
	OutputBytes int64
	// DroppedBytes counts output bytes not given to Tee because it was behind.
	DroppedBytes int64
}
