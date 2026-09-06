package exitcode

import (
	"errors"
	"fmt"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/wrap"
)

// One error of each class maps to its own number, and anything unclassified
// falls to Failure. The point is the table and the behaviour cannot drift: this
// is the only producer of these codes for brigd, and cmd/brig/exit.go is the
// other for the CLI.
func TestOfClassifiesEachErrorClass(t *testing.T) {
	// A cause wrapped one layer down, to prove Of reads the class through a wrap
	// rather than off the top-level message.
	wrapped := func(err error) error { return fmt.Errorf("while doing the thing: %w", err) }

	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil is success", nil, OK},
		{"an unclassified error is a general failure", errors.New("something broke"), Failure},
		{"a usage error", &UsageError{Err: errors.New("unknown op")}, Usage},
		{"a not-found error", &NotFoundError{Err: errors.New("no such profile")}, NotFound},
		{"a missing runtime", runtime.ErrNoRuntime, Runtime},
		{"a broken runtime", runtime.ErrBadRuntime, Runtime},
		{"a refused verification", &wrap.VerifyRefusedError{Err: errors.New("bad signature")}, Verify},
		{"a credential failure", &creds.MissingSecretsError{}, Credentials},
		{"a wrapped runtime failure keeps its class", wrapped(runtime.ErrBadRuntime), Runtime},
		{"a wrapped verify failure keeps its class",
			wrapped(&wrap.VerifyRefusedError{Err: errors.New("mismatch")}), Verify},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Of(c.err); got != c.want {
				t.Errorf("Of(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}
