package cli

import (
	"io"
	"testing"

	"github.com/brig-sh/brig/internal/runtime"
)

// relayRuntime is a jsonRuntime that behaves as hull's Attach does: a Relay on
// the spec runs the attached exec, and its status is Attach's. It keeps the
// specs it was given, so a case can assert on what reached the runtime.
type relayRuntime struct {
	jsonRuntime
	kind  string
	specs []runtime.ExecSpec
}

func (r *relayRuntime) Kind() string { return r.kind }

func (r *relayRuntime) Replace(spec runtime.ExecSpec) error {
	r.specs = append(r.specs, spec)
	return r.jsonRuntime.Replace(spec)
}

func (r *relayRuntime) Attach(spec runtime.ExecSpec) (int, error) {
	r.specs = append(r.specs, spec)
	r.attached++
	if spec.Relay != nil {
		return spec.Relay([]string{"hull", "exec"}, []string{"K=v"})
	}
	return 0, nil
}

// attachHost registers a build with the given hook and points a full run at rt.
func attachHost(t *testing.T, rt *relayRuntime, hook func(AttachedRun) func(argv, env []string) (int, error)) {
	t.Helper()
	jsonRunHost(t, rt)
	registerForTest(t, Options{Edition: "enterprise", Attach: hook})
}

// A build with no hook, and a hook that declines, leave the run exactly as it
// was: the terminal is handed over with Replace, the child path is never
// taken, and the runtime is given no relay.
func TestARunWithNoRelayIsUnchanged(t *testing.T) {
	asked := 0
	for name, hook := range map[string]func(AttachedRun) func(argv, env []string) (int, error){
		"no hook":    nil,
		"nil answer": func(AttachedRun) func(argv, env []string) (int, error) { asked++; return nil },
	} {
		for _, args := range [][]string{{"run", "faker"}, {"sh", "faker"}, {"run", "faker", "--", "ls"}, {"run", "ubuntu"}, {"run", "ubuntu", "--", "ls"}} {
			rt := &relayRuntime{kind: "hull"}
			attachHost(t, rt, hook)
			if _, err := captureStdout(t, func() error { return run(args) }); err != nil {
				t.Fatalf("%s: brig %v: %v", name, args, err)
			}
			if rt.replaced != 1 || rt.attached != 0 {
				t.Errorf("%s: brig %v: Replace %d, Attach %d; want 1 and 0", name, args, rt.replaced, rt.attached)
			}
			for _, s := range rt.specs {
				if s.Relay != nil {
					t.Errorf("%s: brig %v gave the runtime a relay", name, args)
				}
			}
		}
	}
	if asked == 0 {
		t.Error("the declining hook was never asked, so the case proves nothing")
	}
}

// A hook that returns a function makes that function the one that runs the
// attached exec, and its status is brig's, for the agent and for a shell.
func TestAHookRelayRunsTheAttachedExecAndItsStatusIsTheRuns(t *testing.T) {
	for _, args := range [][]string{{"run", "faker"}, {"sh", "faker"}, {"run", "faker", "--", "ls"}, {"run", "ubuntu"}, {"run", "ubuntu", "--", "ls"}} {
		rt := &relayRuntime{kind: "hull"}
		var ran int
		attachHost(t, rt, func(AttachedRun) func(argv, env []string) (int, error) {
			return func(argv, env []string) (int, error) { ran++; return 9, nil }
		})
		_, err := captureStdout(t, func() error { return run(args) })
		if got := exitCode(err); got != 9 {
			t.Errorf("brig %v: exit %d (err %v), want the relay's 9", args, got, err)
		}
		if ran != 1 || rt.attached != 1 || rt.replaced != 0 {
			t.Errorf("brig %v: relay ran %d, Attach %d, Replace %d; want 1, 1, 0", args, ran, rt.attached, rt.replaced)
		}
		if len(rt.specs) != 1 || rt.specs[0].Relay == nil {
			t.Errorf("brig %v: the runtime was given no relay", args)
		}
	}
}

// The hook is asked only for hull, and is told what the run is.
func TestTheHookIsAskedOnlyForHullAndGetsTheRun(t *testing.T) {
	asked := 0
	rt := &relayRuntime{kind: "nerdctl"}
	attachHost(t, rt, func(AttachedRun) func(argv, env []string) (int, error) {
		asked++
		return func(argv, env []string) (int, error) { return 1, nil }
	})
	if _, err := captureStdout(t, func() error { return run([]string{"run", "faker"}) }); err != nil {
		t.Fatal(err)
	}
	if asked != 0 || rt.replaced != 1 || rt.attached != 0 {
		t.Errorf("nerdctl: hook asked %d, Replace %d, Attach %d; want 0, 1, 0", asked, rt.replaced, rt.attached)
	}

	var got AttachedRun
	rt = &relayRuntime{kind: "hull"}
	attachHost(t, rt, func(r AttachedRun) func(argv, env []string) (int, error) {
		got = r
		return nil
	})
	if _, err := captureStdout(t, func() error { return run([]string{"run", "faker@work"}) }); err != nil {
		t.Fatal(err)
	}
	if got.Ref != "faker@work" || got.Profile != "faker" || got.Runtime != "hull" || got.Sandbox == "" ||
		got.Version == "" || got.Cols <= 0 || got.Rows <= 0 {
		t.Errorf("the hook was told %+v", got)
	}
}

// A command may opt out of telemetry, as brig telemetry does; one that does not
// is counted under its own verb, as login is.
func TestAnUncountedCommandIsNotCounted(t *testing.T) {
	registerForTest(t, Options{Edition: "enterprise", Commands: []Command{
		{Verb: "quiet", Uncounted: true, Run: func(io.Writer, []string) error { return nil }},
		{Verb: "loud", Run: func(io.Writer, []string) error { return nil }},
	}})
	if c, _ := telemetryCommand("quiet"); c != "" {
		t.Errorf("an uncounted command is counted as %q", c)
	}
	if c, _ := telemetryCommand("loud"); c == "" {
		t.Error("a command that did not opt out is not counted")
	}
}
