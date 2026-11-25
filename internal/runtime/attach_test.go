package runtime

import (
	"os"
	"reflect"
	"syscall"
	"testing"
	"time"
)

// Replace and Attach must be the same exec asked for two ways: the terminal
// handover and the --json child differ only in whether brig survives them, never
// in what they run or the environment they run it in. A drift there is a
// credential forwarded to one path and not the other, or a --json run that
// behaves unlike the interactive one it is meant to report on.
//
// The two handovers are captured through the package seams rather than actually
// exec'd -- syscall.Exec would replace the test binary and never return -- so
// what is compared is exactly the argv and env each method handed down.
func TestReplaceAndAttachBuildTheSameCommand(t *testing.T) {
	spec := ExecSpec{
		Name: "brig-x",
		Cmd:  []string{"agent", "--flag", "v"},
		Cwd:  "/work/proj",
		TTY:  true,
		Env:  []Var{{Name: "GH_TOKEN", Value: "t", Secret: true}},
	}

	for _, tc := range []struct {
		name    string
		replace func() ([]string, []string)
		attach  func() ([]string, []string)
	}{
		{
			name: "hull",
			replace: func() ([]string, []string) {
				return captureHandover(t, func() { _ = (&hull{bin: "/opt/hull"}).Replace(spec) },
					func() (int, error) { return 0, nil })
			},
			attach: func() ([]string, []string) {
				return captureAttach(t, func() { _, _ = (&hull{bin: "/opt/hull"}).Attach(spec) })
			},
		},
		{
			name: "nerdctl",
			replace: func() ([]string, []string) {
				return captureHandover(t, func() { _ = (&nerdctl{bin: "/usr/bin/nerdctl"}).Replace(spec) },
					func() (int, error) { return 0, nil })
			},
			attach: func() ([]string, []string) {
				return captureAttach(t, func() { _, _ = (&nerdctl{bin: "/usr/bin/nerdctl"}).Attach(spec) })
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rArgv, rEnv := tc.replace()
			aArgv, aEnv := tc.attach()
			if !reflect.DeepEqual(rArgv, aArgv) {
				t.Errorf("argv differs between Replace and Attach:\n  replace: %v\n  attach:  %v", rArgv, aArgv)
			}
			if !reflect.DeepEqual(rEnv, aEnv) {
				t.Errorf("env differs between Replace and Attach:\n  replace: %v\n  attach:  %v", rEnv, aEnv)
			}
		})
	}
}

// captureHandover runs fn with execHandover stubbed to record the argv and env
// it is given rather than replacing the process, restoring the seam after.
func captureHandover(t *testing.T, fn func(), _ func() (int, error)) (argv, env []string) {
	t.Helper()
	prev := execHandover
	execHandover = func(_ string, a, e []string) error { argv, env = a, e; return nil }
	t.Cleanup(func() { execHandover = prev })
	fn()
	return argv, env
}

// captureAttach is captureHandover for the child path.
func captureAttach(t *testing.T, fn func()) (argv, env []string) {
	t.Helper()
	prev := attachHandover
	attachHandover = func(a, e []string) (int, error) { argv, env = a, e; return 0, nil }
	t.Cleanup(func() { attachHandover = prev })
	fn()
	return argv, env
}

// A child that exits with a status hands that status straight back, so a --json
// run can report the agent's own code as brig's.
func TestRunAttachedReturnsTheChildExitStatus(t *testing.T) {
	code, err := RunAttached([]string{"/bin/sh", "-c", "exit 7"}, nil)
	if err != nil {
		t.Fatalf("RunAttached: %v", err)
	}
	if code != 7 {
		t.Errorf("exit status = %d, want 7", code)
	}
}

// A child brig cannot even start is brig's own failure, returned as an error
// rather than folded into a status a script would read as the agent's.
func TestRunAttachedReturnsAnErrorWhenTheChildCannotStart(t *testing.T) {
	if _, err := RunAttached([]string{"/no/such/binary/at/all"}, nil); err == nil {
		t.Fatal("a child that could not be started returned no error")
	}
}

// SIGTERM reaches brig alone -- a kill, a closed session -- not through the tty,
// so the child never sees it unless brig forwards it. brig forwards it, the
// child dies of it, and brig's status is 128 plus the signal number, the shell's
// convention. Without the forwarding a `kill` of a --json run would leave the
// agent running.
func TestRunAttachedForwardsSIGTERM(t *testing.T) {
	go func() {
		// After RunAttached has registered its handler and started the child.
		time.Sleep(300 * time.Millisecond)
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	}()
	code, err := RunAttached([]string{"/bin/sh", "-c", "sleep 30"}, nil)
	if err != nil {
		t.Fatalf("RunAttached: %v", err)
	}
	if want := 128 + int(syscall.SIGTERM); code != want {
		t.Errorf("exit status = %d, want %d (128 + SIGTERM)", code, want)
	}
}

// SIGINT is the opposite case: the tty delivers it to the child directly, so a
// second copy from brig -- or brig exiting and orphaning the child -- is the
// bug. brig catches SIGINT, drops it, and goes on waiting. The child here never
// receives it (the test signals brig alone, not the child's group), so a brig
// that exited early or forwarded it would end the wait before the child was
// stopped some other way.
func TestRunAttachedDoesNotExitEarlyOnSIGINT(t *testing.T) {
	start := time.Now()
	go func() {
		time.Sleep(200 * time.Millisecond)
		// Dropped by brig: the child keeps sleeping, brig keeps waiting.
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		time.Sleep(400 * time.Millisecond)
		// What actually ends the wait, proving SIGINT did not.
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	}()
	code, err := RunAttached([]string{"/bin/sh", "-c", "sleep 30"}, nil)
	if err != nil {
		t.Fatalf("RunAttached: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
		t.Errorf("returned after %v, too soon -- SIGINT cut the wait short", elapsed)
	}
	if want := 128 + int(syscall.SIGTERM); code != want {
		t.Errorf("exit status = %d, want %d -- SIGINT, not SIGTERM, ended it", code, want)
	}
}

// A Relay on the spec runs the attached exec in place of the child brig
// would start, and gets exactly the argv and env the child would have had.
// Its status is Attach's. The child path is not taken at all.
func TestHullAttachRunsTheRelayWithTheChildCommand(t *testing.T) {
	spec := ExecSpec{
		Name: "brig-x",
		Cmd:  []string{"agent", "--flag", "v"},
		TTY:  true,
		Env:  []Var{{Name: "GH_TOKEN", Value: "t", Secret: true}},
	}
	wantArgv, wantEnv := captureAttach(t, func() { _, _ = (&hull{bin: "/opt/hull"}).Attach(spec) })

	prev := attachHandover
	attachHandover = func([]string, []string) (int, error) {
		t.Error("Attach started its own child although a relay was set")
		return 0, nil
	}
	t.Cleanup(func() { attachHandover = prev })

	var gotArgv, gotEnv []string
	spec.Relay = func(argv, env []string) (int, error) { gotArgv, gotEnv = argv, env; return 42, nil }
	code, err := (&hull{bin: "/opt/hull"}).Attach(spec)
	if err != nil || code != 42 {
		t.Fatalf("Attach = %d, %v; want 42, nil", code, err)
	}
	if !reflect.DeepEqual(gotArgv, wantArgv) || !reflect.DeepEqual(gotEnv, wantEnv) {
		t.Errorf("relay got argv %v env %v; the child would have got argv %v env %v", gotArgv, gotEnv, wantArgv, wantEnv)
	}
}

// A relay that declines gets the process handed over as Replace hands it over:
// the same argv and env, through the same exec, and no child.
func TestHullAttachHandsOverWhenTheRelayDeclines(t *testing.T) {
	spec := ExecSpec{
		Name: "brig-x",
		Cmd:  []string{"agent", "--flag", "v"},
		TTY:  true,
		Env:  []Var{{Name: "GH_TOKEN", Value: "t", Secret: true}},
	}
	wantArgv, wantEnv := captureHandover(t, func() { _ = (&hull{bin: "/opt/hull"}).Replace(spec) }, nil)

	prev := attachHandover
	attachHandover = func([]string, []string) (int, error) {
		t.Error("Attach started a child although the relay declined")
		return 0, nil
	}
	t.Cleanup(func() { attachHandover = prev })

	asked := 0
	spec.Relay = func([]string, []string) (int, error) { asked++; return 0, ErrRelayDeclined }
	var gotArgv, gotEnv []string
	execPrev := execHandover
	execHandover = func(_ string, a, e []string) error { gotArgv, gotEnv = a, e; return nil }
	t.Cleanup(func() { execHandover = execPrev })
	if _, err := (&hull{bin: "/opt/hull"}).Attach(spec); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if asked != 1 || !reflect.DeepEqual(gotArgv, wantArgv) || !reflect.DeepEqual(gotEnv, wantEnv) {
		t.Errorf("relay asked %d; exec got argv %v env %v, Replace gives argv %v env %v", asked, gotArgv, gotEnv, wantArgv, wantEnv)
	}
}

// A run that cannot build its command never reaches the relay, so nothing the
// relay does (asking a deployment, writing a file, printing a notice) happens
// for a run that never starts.
func TestHullAttachDoesNotCallTheRelayWhenTheCommandCannotBeBuilt(t *testing.T) {
	spec := ExecSpec{
		Name: "brig-x",
		Cmd:  []string{"agent"},
		Env:  []Var{{Name: "HOME", Value: "/h", Secret: true}}, // a stored secret the runtime reads for itself
	}
	spec.Relay = func([]string, []string) (int, error) {
		t.Error("the relay was called for a run whose command could not be built")
		return 0, nil
	}
	if _, err := (&hull{bin: "/opt/hull"}).Attach(spec); err == nil {
		t.Fatal("Attach built a command that splitEnv refuses")
	}
}
