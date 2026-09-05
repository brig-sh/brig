package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
)

// jsonRuntime is a runtime a full `brig run` can drive with none on PATH. It
// boots trivially -- nothing is running, so a first boot is recorded and the
// readiness probe passes -- and records which handover the run reached, which is
// the fact the tests below turn on. attachFn is what Attach runs, so a case can
// hand back a real exit status.
type jsonRuntime struct {
	runtime.Runtime
	replaced int
	attached int
	attachFn func(runtime.ExecSpec) (int, error)
}

func (r *jsonRuntime) Kind() string                 { return "hull" }
func (r *jsonRuntime) Bin() string                  { return "hull" }
func (r *jsonRuntime) Running(string) (bool, error) { return false, nil }
func (r *jsonRuntime) Run(runtime.RunSpec) error    { return nil }
func (r *jsonRuntime) Probe(runtime.ExecSpec) bool  { return true }
func (r *jsonRuntime) Output(runtime.ExecSpec) (string, error) {
	return "", nil
}
func (r *jsonRuntime) Stop(string) error                  { return nil }
func (r *jsonRuntime) Remove(string) error                { return nil }
func (r *jsonRuntime) PinsDigest() bool                   { return false }
func (r *jsonRuntime) LocalDigest(string) (string, error) { return "", nil }
func (r *jsonRuntime) LogsHint(name string) string        { return "hull logs " + name }

// Replace records the handover and returns rather than exec'ing -- a real
// syscall.Exec would replace the test binary. That it is a no-op here is the
// whole point: the default path must reach it and not Attach.
func (r *jsonRuntime) Replace(runtime.ExecSpec) error {
	r.replaced++
	return nil
}

func (r *jsonRuntime) Attach(spec runtime.ExecSpec) (int, error) {
	r.attached++
	if r.attachFn != nil {
		return r.attachFn(spec)
	}
	return 0, nil
}

// jsonRunHost points brig's whole environment at scratch directories and hands
// run() the given fake runtime through the detection seam, so a full run drives
// the fake with nothing on the host. It writes a minimal agent profile named
// "faker" -- no genericBoot, no secrets -- so the boot needs neither boot assets
// nor a keychain.
func jsonRunHost(t *testing.T, rt runtime.Runtime) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	t.Setenv("BRIG_STATE_DIR", t.TempDir())
	t.Setenv("BRIG_WORKSPACE", t.TempDir())
	t.Setenv("BRIG_VERIFY", "off")
	t.Setenv("BRIG_RUNTIME", "")
	emptyPath(t)
	body := "name: faker\nimage: img\nguestHome: /home/faker\nbinary: agent\nmem: 1024\ncpus: 1\n"
	if err := os.WriteFile(filepath.Join(dir, "faker.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	prev := detectRuntimeFor
	detectRuntimeFor = func(runtime.Preference) (runtime.Runtime, error) { return rt, nil }
	t.Cleanup(func() { detectRuntimeFor = prev })
}

// lastJSONLine parses the final non-empty line of out as a Run object. brig
// prints its object after the agent's own inherited output, so a script's rule
// is "the last line of stdout is brig's" -- this is that rule, in a test.
func lastJSONLine(t *testing.T, out string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	last := lines[len(lines)-1]
	var doc map[string]any
	if err := json.Unmarshal([]byte(last), &doc); err != nil {
		t.Fatalf("last stdout line is not JSON: %q\n(full stdout: %q)", last, out)
	}
	return doc
}

// The test that protects every interactive user: a run WITHOUT --json hands the
// terminal over with Replace and never touches the child path. A regression here
// is an agent that has lost its tty, so this goes first.
func TestRunWithoutJSONReachesReplaceNeverAttach(t *testing.T) {
	rt := &jsonRuntime{}
	jsonRunHost(t, rt)

	if _, err := captureStdout(t, func() error { return run([]string{"run", "faker"}) }); err != nil {
		t.Fatalf("run faker: %v", err)
	}
	if rt.replaced != 1 {
		t.Errorf("Replace called %d times, want 1", rt.replaced)
	}
	if rt.attached != 0 {
		t.Errorf("a default run reached the child path %d times, want 0", rt.attached)
	}
}

// Under --json the agent runs as a child, and its own exit status is brig's. The
// child here really exits 7, through the production RunAttached, so this pins the
// whole chain: Attach's status, the Run object, and the status brig returns.
func TestRunJSONPropagatesTheAgentExitStatus(t *testing.T) {
	rt := &jsonRuntime{attachFn: func(runtime.ExecSpec) (int, error) {
		return runtime.RunAttached([]string{"/bin/sh", "-c", "exit 7"}, nil)
	}}
	jsonRunHost(t, rt)

	out, err := captureStdout(t, func() error { return run([]string{"--json", "run", "faker"}) })
	if rt.attached != 1 {
		t.Errorf("the agent reached the child path %d times, want 1", rt.attached)
	}
	if got := exitCode(err); got != 7 {
		t.Errorf("brig exit = %d, want 7", got)
	}
	doc := lastJSONLine(t, out)
	if doc["kind"] != "Run" {
		t.Errorf("kind = %v, want Run", doc["kind"])
	}
	data, _ := doc["data"].(map[string]any)
	if data["stage"] != "agent" {
		t.Errorf("stage = %v, want agent", data["stage"])
	}
	if data["exit"] != float64(7) {
		t.Errorf("exit = %v, want 7", data["exit"])
	}
	if _, ok := data["error"]; ok {
		t.Errorf("an agent that ran carried an error field: %v", data["error"])
	}
}

// A brig refusal under --json is one line too, with stage "brig" and the
// message, and nothing else on stdout: the object is what a script parses to
// tell "brig would not start this" from "the agent exited non-zero", which the
// exit status alone cannot say.
func TestRunJSONReportsABrigRefusal(t *testing.T) {
	// No runtime needed: an unknown profile is refused before detection.
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}

	var err error
	out, _ := captureStdout(t, func() error { err = run([]string{"--json", "run", "nosuchagent"}); return nil })
	if got := exitCode(err); got != exitNotFound {
		t.Errorf("brig exit = %d, want %d (not found)", got, exitNotFound)
	}
	if lines := strings.Count(strings.TrimRight(out, "\n"), "\n"); lines != 0 {
		t.Errorf("stdout has more than one line:\n%s", out)
	}
	doc := lastJSONLine(t, out)
	data, _ := doc["data"].(map[string]any)
	if data["stage"] != "brig" {
		t.Errorf("stage = %v, want brig", data["stage"])
	}
	if msg, _ := data["error"].(string); msg == "" || !strings.Contains(msg, "nosuchagent") {
		t.Errorf("error = %q, want it to name the refused agent", data["error"])
	}
	if data["exit"] != float64(exitNotFound) {
		t.Errorf("exit = %v, want %d", data["exit"], exitNotFound)
	}
}

// -d starts the sandbox and stops. Under --json it says so as the object, with
// the sandbox name a script would attach to; the text form still prints the bare
// name and nothing more.
func TestDetachJSONPrintsTheObjectAndTextPrintsTheName(t *testing.T) {
	rt := &jsonRuntime{}
	jsonRunHost(t, rt)

	out, err := captureStdout(t, func() error { return run([]string{"--json", "run", "faker", "-d"}) })
	if err != nil {
		t.Fatalf("run -d --json: %v", err)
	}
	doc := lastJSONLine(t, out)
	data, _ := doc["data"].(map[string]any)
	if data["stage"] != "detached" {
		t.Errorf("stage = %v, want detached", data["stage"])
	}
	if data["sandbox"] != "brig-faker" {
		t.Errorf("sandbox = %v, want brig-faker", data["sandbox"])
	}
	if rt.replaced != 0 || rt.attached != 0 {
		t.Errorf("-d ran the agent: replaced=%d attached=%d", rt.replaced, rt.attached)
	}

	rt2 := &jsonRuntime{}
	jsonRunHost(t, rt2)
	textOut, err := captureStdout(t, func() error { return run([]string{"run", "faker", "-d"}) })
	if err != nil {
		t.Fatalf("run -d: %v", err)
	}
	if strings.TrimSpace(textOut) != "brig-faker" {
		t.Errorf("text -d printed %q, want the bare sandbox name", textOut)
	}
}
