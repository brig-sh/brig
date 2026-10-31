package runtime

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A runtime that never finishes must not hold a caller past its deadline, even
// when a child of it keeps the pipes open.
func TestLogsStopsAtTheDeadline(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "runtime")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, rt := range map[string]Runtime{"nerdctl": &nerdctl{bin: bin}, "hull": &hull{bin: bin}} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			start := time.Now()
			err := rt.Logs(LogsSpec{Name: "x", Tail: -1, Out: io.Discard, Err: io.Discard, Ctx: ctx})
			if err == nil {
				t.Error("a read past its deadline reported success")
			}
			if took := time.Since(start); took > 10*time.Second {
				t.Errorf("Logs returned after %s, with a 50ms deadline", took)
			}
		})
	}
}
