package runtime

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

// capsHull writes a stand-in hull whose `capabilities --json` is body (a shell
// fragment), and which logs every invocation, one argv per line, to the file
// it returns. Any other verb is logged and succeeds, so a case can tell what
// was asked from what was booted.
func capsHull(t *testing.T, body string) (bin, log string) {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("shell stand-in is not portable to windows")
	}
	dir := t.TempDir()
	bin = filepath.Join(dir, "hull")
	log = filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> '" + log + "'\n" +
		"if [ \"$1\" = --version ]; then echo 'hull version 0.1.0-rc29'; exit 0; fi\n" +
		"if [ \"$1\" = capabilities ]; then\n" + body + "\nfi\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func readLog(t *testing.T, log string) string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

const (
	capsSupported   = `printf '{"schemaVersion":1,"nestedVirt":{"supported":true,"backend":"hvi","detail":"Hypervisor.framework reports EL2"}}\n'; exit 0`
	capsUnsupported = `printf '{"schemaVersion":1,"nestedVirt":{"supported":false,"backend":"hvi","detail":"Hypervisor.framework reports no EL2"}}\n'; exit 0`
	// What hull 0.1.0-rc29, released before the probe, does with the question:
	// a usage page on stderr and exit 1.
	capsOldHull = `printf 'Incorrect Usage: flag provided but not defined: -json\n' >&2; exit 1`
)

// Absent is the default, and a test holds it: a guest given EL2 runs things brig's view of the guest does not reach, so the
// flag must never ride along on a run that did not ask for it.
func TestRunArgsNestedVirtOffByDefault(t *testing.T) {
	for _, hv := range []string{"vz", "hvi", "qemu"} {
		got := argv(t, RunSpec{Name: "s", Image: "img", Mem: 1, CPUs: 1}, hv, "shared", "", "")
		if strings.Contains(got, "--nested-virt") {
			t.Errorf("%s: a run that did not ask for nested virtualization carries the flag: %s", hv, got)
		}
	}
}

// Asked for, it reaches hull, and ahead of the image: hull reads its own flags
// before the positional, so one after the image would be handed to the guest.
func TestRunArgsNestedVirtReachesHull(t *testing.T) {
	args, _, err := runArgs(RunSpec{Name: "s", Image: "img", NestedVirt: true}, "hvi", "shared", "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	at := -1
	for i, a := range args {
		if a == "--nested-virt" {
			if at >= 0 {
				t.Fatalf("--nested-virt appears twice: %v", args)
			}
			at = i
		}
	}
	if at < 0 {
		t.Fatalf("--nested-virt did not reach the command line: %v", args)
	}
	if args[len(args)-1] != "img" {
		t.Errorf("the image is no longer the final argument: %v", args)
	}
	if at >= len(args)-1 {
		t.Errorf("--nested-virt comes after the image, where hull would not read it: %v", args)
	}
}

// Only hvi can give the guest EL2. The refusal names the variable a person can
// change, in the words every layer of the stack agreed on.
func TestSupportsRefusesNestedOffHvi(t *testing.T) {
	spec := RunSpec{Name: "s", Image: "img", NestedVirt: true}
	for _, hv := range []string{"vz", "qemu"} {
		err := supports(spec, hv)
		if err == nil {
			t.Fatalf("nested virtualization was accepted on %s", hv)
		}
		want := `nested virtualization (capability kvm) needs the hvi backend (BRIG_HYPERVISOR is "` + hv + `")`
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: the refusal is %q, want it to contain %q", hv, err, want)
		}
	}
	// The unset backend is vz, and is refused as vz.
	if err := supports(spec, hypervisor(spec)); err == nil || !strings.Contains(err.Error(), `"vz"`) {
		t.Errorf("the default backend was not refused as vz: %v", err)
	}
	if err := supports(spec, "hvi"); err != nil {
		t.Errorf("hvi refused nested virtualization on the spec alone: %v", err)
	}
}

// On the wrong backend the answer is in the spec, so hull is not asked: the
// refusal costs no subprocess and names the variable a person can change.
func TestCanRunRefusesNestedOffHviWithoutAskingHull(t *testing.T) {
	bin, log := capsHull(t, capsSupported)
	h := &hull{bin: bin}
	err := h.CanRun(RunSpec{Name: "s", Image: "img", Hypervisor: "vz", NestedVirt: true})
	if err == nil {
		t.Fatal("a kvm run on vz was accepted")
	}
	if got := readLog(t, log); got != "" {
		t.Errorf("hull was asked something for a run refused on the spec alone: %q", got)
	}
}

// On hvi CanRun asks hull nothing about capabilities, even on a host that
// cannot nest. The note in supports says why.
func TestCanRunNeverAsksHullAboutCapabilities(t *testing.T) {
	bin, log := capsHull(t, capsUnsupported)
	h := &hull{bin: bin}
	for _, nested := range []bool{false, true} {
		if err := h.CanRun(RunSpec{Name: "s", Image: "img", Hypervisor: "hvi", NestedVirt: nested}); err != nil {
			t.Errorf("NestedVirt=%v on hvi was refused on the spec: %v", nested, err)
		}
	}
	if got := readLog(t, log); got != "" {
		t.Errorf("CanRun asked hull something: %q", got)
	}
}

// A hull released before the command answers with its CLI library's usage
// error. That says nothing about the host, so it is not reported as the host
// being unable: it is a hull to upgrade, named by the version it reports.
func TestCapabilitiesOldHullIsOutdated(t *testing.T) {
	bin, _ := capsHull(t, capsOldHull)
	got := (&hull{bin: bin}).NestedVirt()
	if got.Supported || !got.Outdated {
		t.Fatalf("an old hull's usage error: %+v, want outdated and not supported", got)
	}
	want := "this hull (0.1.0-rc29) predates nested virtualization; upgrade hull"
	if got.Detail != want {
		t.Errorf("detail = %q, want %q", got.Detail, want)
	}
}

// Anything that is not a readable schema-1 answer is not supported. The stub
// runtime in smoke answers an unknown verb with exit 0 and nothing on stdout,
// which is the first of these.
func TestCapabilitiesUnreadableAnswerIsNotSupported(t *testing.T) {
	for _, tt := range []struct{ name, body string }{
		{"empty", `exit 0`},
		{"not json", `printf 'nested virtualization: supported (hvi)\n'; exit 0`},
		{"no schema", `printf '{"nestedVirt":{"supported":true}}\n'; exit 0`},
		{"no nestedVirt", `printf '{"schemaVersion":1}\n'; exit 0`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bin, _ := capsHull(t, tt.body)
			got := (&hull{bin: bin}).NestedVirt()
			if got.Supported || got.Outdated || got.Detail == "" {
				t.Errorf("%s: %+v, want not supported with a reason", tt.name, got)
			}
		})
	}
}

// The version quoted in a message about an outdated hull comes from hull's own
// output, and an escape byte in it would act on the terminal it is printed to.
func TestVersionLabelDropsControlBytes(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("shell stand-in is not portable to windows")
	}
	bin := filepath.Join(t.TempDir(), "hull")
	script := "#!/bin/sh\nprintf 'hull version 0.1.0-rc29\\033]52;c;aGk=\\007\\n'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	got := (&hull{bin: bin}).versionLabel()
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Errorf("a control byte reached the label: %q", got)
	}
	if !strings.HasPrefix(got, "0.1.0-rc29") {
		t.Errorf("the label lost the version: %q", got)
	}
}

// Only schema 1 is read. A field of the same name could mean something else
// in another schema, and a wrong "supported" is the answer that costs a
// sandbox.
func TestCapabilitiesReadsOnlySchemaOne(t *testing.T) {
	bin, _ := capsHull(t, `printf '{"schemaVersion":2,"nestedVirt":{"supported":true,"backend":"hvi"}}\n'`)
	got := (&hull{bin: bin}).NestedVirt()
	if got.Supported {
		t.Errorf("a schema-2 answer was read as supported: %+v", got)
	}
	if want := "hull answered schemaVersion 2; this brig reads 1"; got.Detail != want {
		t.Errorf("detail = %q, want %q", got.Detail, want)
	}
}

// Asked once per process, and as plumbing: brig info reads it twice from one
// Config, and every brig info must not become a telemetry event.
func TestCapabilitiesAskedOnceAndUncounted(t *testing.T) {
	bin, log := capsHull(t, `printf 'suppress=%s\n' "$HULL_TELEMETRY_SUPPRESS" >> "$(dirname "$0")/env.log"; `+capsSupported)
	h := &hull{bin: bin}
	h.NestedVirt()
	h.NestedVirt()
	if n := strings.Count(readLog(t, log), "capabilities --json"); n != 1 {
		t.Errorf("hull was asked %d times, want once", n)
	}
	env := readLog(t, filepath.Join(filepath.Dir(bin), "env.log"))
	if strings.TrimSpace(env) != "suppress=1" {
		t.Errorf("the probe was not marked as plumbing: %q", env)
	}
}

// A hull that never answers is not supported within the bound, never a hang:
// brig info and brig doctor both ask, and both are what someone runs against
// a wedged runtime.
func TestCapabilitiesTimeoutIsNotSupported(t *testing.T) {
	old := capabilitiesTimeout
	capabilitiesTimeout = 200 * time.Millisecond
	t.Cleanup(func() { capabilitiesTimeout = old })
	bin, _ := capsHull(t, `sleep 300`)

	done := make(chan NestedSupport, 1)
	go func() { done <- (&hull{bin: bin}).NestedVirt() }()
	select {
	case got := <-done:
		if got.Supported || !strings.Contains(got.Detail, "did not answer") {
			t.Errorf("a hull that never answered: %+v", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("NestedVirt never returned from a hull that does not answer")
	}
}

// The detail is printed to a terminal, so hull's words arrive as one plain
// line and an escape sequence in them is not interpreted.
func TestCapabilitiesDetailIsOneLine(t *testing.T) {
	bin, _ := capsHull(t, `printf '{"schemaVersion":1,"nestedVirt":{"supported":false,"detail":"no EL2\\u001b]52;c;aGk=\\u0007\\nsecond"}}\n'`)
	got := (&hull{bin: bin}).NestedVirt()
	if strings.ContainsAny(got.Detail, "\x1b\x07\n") {
		t.Errorf("a control character reached the detail: %q", got.Detail)
	}
	if !strings.HasPrefix(got.Detail, "no EL2") {
		t.Errorf("the detail lost its words: %q", got.Detail)
	}
}

// The boot carries the flag, and the run path puts no capabilities question to
// hull first.
func TestRunPassesNestedVirtWithoutAskingCapabilities(t *testing.T) {
	bin, log := capsHull(t, capsSupported)
	h := &hull{bin: bin}
	if err := h.Run(RunSpec{Name: "brig-s", Image: "img", Hypervisor: "hvi", Net: "none",
		Mem: 1, CPUs: 1, NestedVirt: true}); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	got := readLog(t, log)
	booted := false
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "capabilities") {
			t.Errorf("the run path asked hull about capabilities: %q", line)
		}
		if strings.HasPrefix(line, "run ") {
			booted = true
			if !strings.Contains(line, "--nested-virt") {
				t.Errorf("the boot does not carry --nested-virt: %q", line)
			}
		}
	}
	if !booted {
		t.Errorf("hull was never told to boot: %q", got)
	}
}

// A hull released before --nested-virt rejects it in its CLI library's words,
// which read like a mistake in brig. brig names the hull and says to upgrade
// it instead.
func TestRunSaysAnOldHullPredatesNestedVirt(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("shell stand-in is not portable to windows")
	}
	bin := filepath.Join(t.TempDir(), "hull")
	script := "#!/bin/sh\ncase \"$1\" in\n" +
		"--version) echo 'hull v0.1.0-rc29 (e54923f, 2026-09-25, go1.27.1, darwin/arm64)' ;;\n" +
		"run) echo 'Incorrect Usage: flag provided but not defined: -nested-virt' >&2\n" +
		"  echo 'error: flag provided but not defined: -nested-virt' >&2; exit 1 ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	err := (&hull{bin: bin}).Run(RunSpec{Name: "brig-s", Image: "img", Hypervisor: "hvi", Net: "none",
		Mem: 1, CPUs: 1, NestedVirt: true})
	if err == nil {
		t.Fatal("a boot an old hull refused was reported as started")
	}
	if !strings.Contains(err.Error(), "this hull (0.1.0-rc29) predates nested virtualization; upgrade hull") {
		t.Errorf("the refusal does not name the hull to upgrade: %v", err)
	}
	if strings.Contains(err.Error(), "flag provided but not defined") {
		t.Errorf("the CLI library's words reached the user: %v", err)
	}
}

// hull's own refusal on a host that cannot nest reaches the person running
// brig, in hull's words: the phrase every layer uses and hull's detail.
func TestRunSurfacesHullsNestedRefusal(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("shell stand-in is not portable to windows")
	}
	bin := filepath.Join(t.TempDir(), "hull")
	script := "#!/bin/sh\nif [ \"$1\" = run ]; then\n" +
		"  echo 'error: nested virtualization requested but not supported by this host: hvi reports no EL2' >&2\n" +
		"  exit 1\nfi\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	err := (&hull{bin: bin}).Run(RunSpec{Name: "brig-s", Image: "img", Hypervisor: "hvi", Net: "none",
		Mem: 1, CPUs: 1, NestedVirt: true})
	if err == nil {
		t.Fatal("a boot hull refused was reported as started")
	}
	if !strings.Contains(err.Error(), "nested virtualization requested but not supported by this host: hvi reports no EL2") {
		t.Errorf("hull's refusal did not reach the error: %v", err)
	}
}

// The Linux runtime has no /dev/kvm to hand a guest, and refuses a kvm run.
// Booting one would leave the guest without the device its profile asked for.
func TestNerdctlRefusesNestedVirt(t *testing.T) {
	n := &nerdctl{bin: filepath.Join(t.TempDir(), "nerdctl-not-there")}
	spec := RunSpec{Name: "brig-s", Image: "img", Net: "shared", NestedVirt: true}
	err := n.CanRun(spec)
	if err == nil {
		t.Fatal("the Linux runtime accepted a kvm profile")
	}
	for _, want := range []string{"kvm capability", "hvi backend", "does not pass /dev/kvm through"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not contain %q", err, want)
		}
	}
	// Run refuses before reaching the binary, which is not there to reach.
	if err := n.Run(spec); err == nil || !strings.Contains(err.Error(), "kvm capability") {
		t.Errorf("Run did not refuse the same way: %v", err)
	}
	spec.NestedVirt = false
	if err := n.CanRun(spec); err != nil {
		t.Errorf("an ordinary run was refused: %v", err)
	}
}

// hull answers the question and nerdctl does not claim to: a runtime that
// passes no /dev/kvm through has nothing to report, and a stub saying "no"
// would be an answer to a question nobody put to it.
func TestOnlyHullProbesCapabilities(t *testing.T) {
	var _ CapabilityProber = &hull{}
	var rt Runtime = &nerdctl{bin: "nerdctl"}
	if _, ok := rt.(CapabilityProber); ok {
		t.Error("nerdctl claims to answer nested virtualization, which it does not pass through")
	}
}
