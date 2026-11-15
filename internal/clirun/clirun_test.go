package clirun

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// block is a Runner that prints a credential and waits for its context.
func block(ctx context.Context, _ string, _, _ []string) ([]byte, []byte, error) {
	<-ctx.Done()
	return []byte("secret"), []byte("stderr"), ctx.Err()
}

func TestOutputTimesOutOnItsOwnBound(t *testing.T) {
	stdout, stderr, err := Output(context.Background(), block, "cli", nil, nil, 20*time.Millisecond)
	if !errors.Is(err, ErrTimedOut) {
		t.Fatalf("err = %v, want ErrTimedOut", err)
	}
	if stdout != nil || string(stderr) != "stderr" {
		t.Errorf("stdout = %q, stderr = %q", stdout, stderr)
	}
}

// A caller that gives up first gets its own error, not a timeout it did not
// set.
func TestOutputLeavesTheCallersCancellationAlone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	_, _, err := Output(ctx, block, "cli", nil, nil, time.Minute)
	if errors.Is(err, ErrTimedOut) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// The real runner reports the kill ("signal: killed"), not the cancellation;
// Output must still hand the caller its own error.
func TestOutputLeavesTheCallersCancellationAloneWithExec(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, _, err = Output(ctx, nil, sh, []string{"-c", "sleep 5"}, []string{"PATH=/usr/bin:/bin"}, time.Minute)
	if errors.Is(err, ErrTimedOut) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestOutputDropsStdoutOnFailure(t *testing.T) {
	fail := func(context.Context, string, []string, []string) ([]byte, []byte, error) {
		return []byte("secret"), []byte("boom"), errors.New("exit status 1")
	}
	stdout, stderr, err := Output(context.Background(), fail, "cli", nil, nil, time.Minute)
	if err == nil || stdout != nil || string(stderr) != "boom" {
		t.Errorf("stdout = %q, stderr = %q, err = %v", stdout, stderr, err)
	}
}

func TestExecCapsStdout(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	script := "head -c " + "70000" + " /dev/zero"
	stdout, _, err := Exec(context.Background(), sh, []string{"-c", script}, []string{"PATH=/usr/bin:/bin"})
	if err == nil || !strings.Contains(err.Error(), "stdout is over") {
		t.Fatalf("err = %v", err)
	}
	if stdout != nil {
		t.Errorf("stdout returned with the error: %d bytes", len(stdout))
	}
}

func TestClipCutsStderr(t *testing.T) {
	got := Clip([]byte("  " + strings.Repeat("x", maxStderr+50) + "\n"))
	if want := strings.Repeat("x", maxStderr) + "..."; got != want {
		t.Errorf("Clip = %d bytes, want %d", len(got), len(want))
	}
	if got := Clip([]byte(" short \n")); got != "short" {
		t.Errorf("Clip = %q", got)
	}
}
