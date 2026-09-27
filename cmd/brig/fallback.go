package main

import "github.com/brig-sh/brig/internal/runtime"

// sayFallback prints the runtime's note when it drives a binary nobody named,
// which today is docker taken from PATH because nerdctl was not there.
//
// It is called after each place the CLI detects a runtime, and not inside the
// detectRuntime seams: the tests replace those, and a note printed from inside
// one is a note no CLI test can see. It goes through warnf, so it stays in the
// default output and -q drops it.
func sayFallback(rt runtime.Runtime) {
	f, ok := rt.(runtime.FallbackReporter)
	if !ok {
		return
	}
	if note := f.Fallback(); note != "" {
		warnf("%s", note)
	}
}
