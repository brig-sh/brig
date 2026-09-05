package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/wrap"
)

// TestExitCode pins the mapping from an error to the process exit status. Every
// code in the README table has a case here, so a documented code that nothing
// returns is a test failure rather than a surprise for a script.
func TestExitCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil is success", nil, 0},
		{"a bare error is the general failure", errors.New("boom"), 1},
		{"a usage error", usagef("bad flag"), 2},
		{"a missing profile", notFoundf("unknown profile %q", "nope"), 3},
		{"no runtime on PATH", runtime.ErrNoRuntime, 4},
		{"a broken runtime", runtime.ErrBadRuntime, 4},
		{"a verification refusal", &wrap.VerifyRefusedError{Err: errors.New("refused")}, 5},
		{"an unresolved credential", &creds.MissingSecretsError{
			Sandbox: "brig-claude-code",
			Profile: "claude-code",
			Missing: []creds.Missing{{Name: "TOKEN"}},
		}, 6},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := exitCode(c.err); got != c.want {
				t.Fatalf("exitCode = %d, want %d", got, c.want)
			}
		})
	}
}

// TestExitCodeThroughWrapping checks that the mapping reads the cause, not the
// outermost message: an error handed up the stack is wrapped more than once, and
// a code that only matched a bare sentinel would fall back to 1 the moment a
// caller added context.
func TestExitCodeThroughWrapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"wrapped usage", fmt.Errorf("context: %w", usagef("bad flag")), 2},
		{"wrapped not-found", fmt.Errorf("context: %w", notFoundf("unknown profile %q", "x")), 3},
		{"wrapped no-runtime", fmt.Errorf("context: %w", runtime.ErrNoRuntime), 4},
		{"wrapped bad-runtime", fmt.Errorf("context: %w", runtime.ErrBadRuntime), 4},
		{"wrapped verify", fmt.Errorf("context: %w", &wrap.VerifyRefusedError{Err: errors.New("no")}), 5},
		{"wrapped credentials", fmt.Errorf("context: %w", &creds.MissingSecretsError{Missing: []creds.Missing{{Name: "T"}}}), 6},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := exitCode(c.err); got != c.want {
				t.Fatalf("exitCode = %d, want %d", got, c.want)
			}
		})
	}
}
