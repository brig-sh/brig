// Package clirun runs a host CLI that prints a credential, such as aws or
// gcloud, for the packages that broker one. It bounds the call, caps what the
// CLI prints, and keeps stdout, where the credential is, out of every error.
// Each caller keeps its own mapping of the CLI's stderr to an error the user
// can act on.
package clirun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// MaxOutput caps stdout and stderr, far above any credential document.
const MaxOutput = 64 << 10

// maxStderr is how much of the CLI's stderr an error repeats.
const maxStderr = 400

// Runner runs bin with args in exactly env, and returns its stdout and stderr.
// Exec is the real one; a test replaces it.
type Runner func(ctx context.Context, bin string, args, env []string) (stdout, stderr []byte, err error)

// ErrTimedOut is the error of Output when its own timeout fired, and not a
// deadline or cancellation of the caller's context.
var ErrTimedOut = errors.New("timed out")

// Output runs run, or Exec when run is nil, bounded by timeout. On success it
// returns stdout. On failure it returns no stdout, the CLI's stderr for the
// caller to map, and the error: ErrTimedOut when the bound fired, the
// caller's context error when the caller gave up first, or the runner's.
func Output(ctx context.Context, run Runner, bin string, args, env []string, timeout time.Duration) (stdout, stderr []byte, err error) {
	if run == nil {
		run = Exec
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout, stderr, err = run(tctx, bin, args, env)
	if err == nil {
		return stdout, stderr, nil
	}
	// A caller that gave up first gets its own error: Exec returns the kill's
	// exit status ("signal: killed"), which would hide why the CLI stopped.
	// Stdout is dropped here as on every failure.
	if ctx.Err() != nil {
		return nil, stderr, ctx.Err()
	}
	// Only the bound this function set.
	if errors.Is(tctx.Err(), context.DeadlineExceeded) {
		return nil, stderr, ErrTimedOut
	}
	return nil, stderr, err
}

// Clip is stderr as an error repeats it: trimmed, and cut at 400 bytes.
func Clip(stderr []byte) string {
	msg := strings.TrimSpace(string(stderr))
	if len(msg) > maxStderr {
		msg = msg[:maxStderr] + "..."
	}
	return msg
}

// CmdName is the subcommand for an error: "sts assume-role".
func CmdName(args []string) string {
	if len(args) >= 2 {
		return args[0] + " " + args[1]
	}
	return strings.Join(args, " ")
}

// Exec is the real Runner. Both streams are capped so a misbehaving binary
// cannot fill the caller's memory.
func Exec(ctx context.Context, bin string, args, env []string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	if env == nil {
		// A nil Env makes os/exec inherit the parent's.
		env = []string{}
	}
	cmd.Env = env
	// CommandContext kills the CLI but not what the CLI started. A
	// credential_process helper, an SSO browser flow or a helper gcloud runs
	// can keep the output pipes open, and Run waits for them to close, so the
	// timeout would not release the caller. After the kill, WaitDelay closes
	// the pipes and Run returns.
	cmd.WaitDelay = 2 * time.Second
	out, errb := &capped{limit: MaxOutput}, &capped{limit: MaxOutput}
	cmd.Stdout, cmd.Stderr = out, errb
	err := cmd.Run()
	if out.over {
		// The cap first, as the cause: the signal the process died of is
		// not what the reader needs.
		return nil, errb.buf.Bytes(), fmt.Errorf("stdout is over %d bytes", MaxOutput)
	}
	return out.buf.Bytes(), errb.buf.Bytes(), err
}

// capped is an io.Writer that stops at a limit; the write error tears the
// command down instead of reading it to the end.
type capped struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); len(p) > room {
		c.buf.Write(p[:room])
		c.over = true
		return 0, errors.New("output over the limit")
	}
	return c.buf.Write(p)
}
