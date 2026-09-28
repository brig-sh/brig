package runtime

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The table is the answer every refusal reads. A run path brig drives with no
// row in it falls through to unknown and refuses every policy there. That
// fails closed, and it is still a run path nobody wrote an answer for.
func TestEgressContractAnswersEveryRunPath(t *testing.T) {
	for _, tt := range []struct {
		path RunPath
		want Capability
	}{
		{RunPath{"hull", "hvi"}, Enforced},
		{RunPath{"hull", "vz"}, CannotEnforce},
		{RunPath{"hull", "qemu"}, CannotEnforce},
		{RunPath{"nerdctl", "io.containerd.urunc.v2"}, CannotEnforce},
		{RunPath{"nerdctl", "runc"}, CannotEnforce},
		{RunPath{"docker", "io.containerd.urunc.v2"}, CannotEnforce},
		{RunPath{"docker", "runc"}, CannotEnforce},
		// A backend brig was never taught about is not waved through.
		{RunPath{"hull", "krun"}, Unknown},
		{RunPath{"other", "vz"}, Unknown},
	} {
		got := Answer(EgressPolicy, tt.path)
		if got.State != tt.want {
			t.Errorf("%s answers %q, want %q", tt.path, got.State, tt.want)
		}
		if got.Path != tt.path {
			t.Errorf("the answer for %s names %s", tt.path, got.Path)
		}
		if got.State != Enforced && (got.Why == "" || got.Remedy == "") {
			t.Errorf("the refusal on %s has nothing to say why or what to do: %+v", tt.path, got)
		}
	}
	// One row for each run path brig drives, and no row for one it does not.
	var have []string
	for _, r := range Records(EgressPolicy) {
		have = append(have, r.Path.String())
	}
	want := []string{"hull on hvi", "hull on vz", "hull on qemu", "nerdctl", "docker"}
	if strings.Join(have, ",") != strings.Join(want, ",") {
		t.Errorf("the egress records cover %q, want %q", have, want)
	}
}

// A record that holds for every backend has no backend to name. Records is
// exported for a caller that prints the table, and "nerdctl on " with nothing
// after it reads as a backend that went missing.
func TestRunPathWithNoBackendNamesTheRuntimeAlone(t *testing.T) {
	for _, tt := range []struct {
		path RunPath
		want string
	}{
		{RunPath{"nerdctl", anyBackend}, "nerdctl"},
		{RunPath{"docker", anyBackend}, "docker"},
		{RunPath{"hull", "hvi"}, "hull on hvi"},
	} {
		if got := tt.path.String(); got != tt.want {
			t.Errorf("%#v prints %q, want %q", tt.path, got, tt.want)
		}
	}
}

// Each backend booted through its adapter, against a runtime double that logs
// every call. A policy boots only where the contract answers enforced, and a
// refusal reaches no runtime command beyond the probe.
func TestEgressContractDecidesTheBootOnEveryBackend(t *testing.T) {
	policy := Egress{Default: "deny", Allow: []Rule{{Host: "example.com"}}}
	for _, tt := range []struct {
		name  string
		hv    string
		help  string // what `network-gateway --help` prints
		exit  string // how it exits
		want  Capability
		probe bool
	}{
		{"hvi with a gateway that takes rules", "hvi", egressHelp, "0", Enforced, true},
		{"hvi with a gateway too old for rules", "hvi", "   --socket string\n", "0", CannotEnforce, true},
		{"hvi with a probe that fails", "hvi", egressHelp, "2", Unknown, true},
		{"vz", "vz", egressHelp, "0", CannotEnforce, false},
		{"qemu", "qemu", egressHelp, "0", CannotEnforce, false},
		{"a backend with no record", "krun", egressHelp, "0", Unknown, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := newHullDouble(t, tt.help, tt.exit)
			h := &hull{bin: d.bin}
			spec := RunSpec{Name: "brig-cap", Image: "img", Hypervisor: tt.hv,
				Net: "shared", Egress: policy}
			err := h.Run(spec)
			calls := d.calls()

			if tt.want == Enforced {
				if err != nil {
					t.Fatalf("a policy was refused on the backend that enforces it: %v", err)
				}
				if !d.called("run ") {
					t.Fatalf("nothing booted: %q", calls)
				}
				if !d.called("--egress-default deny") {
					t.Errorf("the rules did not reach the gateway: %q", calls)
				}
				return
			}
			var ce *CapabilityError
			if !errors.As(err, &ce) {
				t.Fatalf("want a capability refusal, got %v", err)
			}
			if ce.State != tt.want || ce.Property != EgressPolicy ||
				ce.Path != (RunPath{"hull", tt.hv}) {
				t.Errorf("refused as %q on %s for %q, want %q on hull on %s for %q",
					ce.State, ce.Path, ce.Property, tt.want, tt.hv, EgressPolicy)
			}
			for _, want := range []string{string(EgressPolicy), "hull", tt.hv, string(tt.want)} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
			// Exactly the probe, or nothing at all. A check for the boot alone
			// passes a refusal that started some other command first.
			want := ""
			if tt.probe {
				want = "network-gateway --help\n"
			}
			if calls != want {
				t.Errorf("a refused run reached the runtime: %q, want %q", calls, want)
			}
			// The join path asks CanRun, which reads the same table. Only the
			// probe is left to the boot on hvi.
			if !tt.probe && canRunRefusal(h, spec) == nil {
				t.Error("CanRun waved through what Run refused")
			}
		})
	}
}

// The probe failure keeps what #237 asks it to name: the binary, the probe
// command and its error, under the unknown state.
func TestAFailedProbeAnswersUnknown(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "not-a-runtime")
	err := gatewayEnforces(bin)
	var ce *CapabilityError
	if !errors.As(err, &ce) {
		t.Fatalf("want a capability refusal, got %v", err)
	}
	if ce.State != Unknown || ce.Path != (RunPath{"hull", "hvi"}) || ce.Err == nil {
		t.Errorf("a probe that did not run answered %+v", ce)
	}
	assertProbeRefusal(t, err, bin)
}

// nerdctl reads no policy into the run on any shim, so the answer is cannot
// enforce whatever BRIG_CONTAINERD_RUNTIME names, and the refusal names the
// shim the run asked for.
func TestNerdctlAnswersCannotEnforceOnEveryShim(t *testing.T) {
	for _, shim := range []string{"", "io.containerd.runc.v2"} {
		t.Setenv("BRIG_CONTAINERD_RUNTIME", shim)
		d := newHullDouble(t, egressHelp, "0")
		n := &nerdctl{bin: d.bin}
		spec := RunSpec{Name: "brig-cap", Image: "img", Net: "shared",
			Egress: Egress{Default: "deny"}}
		err := n.Run(spec)
		var ce *CapabilityError
		if !errors.As(err, &ce) {
			t.Fatalf("want a capability refusal, got %v", err)
		}
		want := RunPath{"nerdctl", containerdRuntime()}
		if ce.State != CannotEnforce || ce.Path != want {
			t.Errorf("refused as %q on %s, want %q on %s", ce.State, ce.Path, CannotEnforce, want)
		}
		if !strings.Contains(err.Error(), containerdRuntime()) {
			t.Errorf("the refusal does not name the shim: %v", err)
		}
		if calls := d.calls(); calls != "" {
			t.Errorf("a refused run reached the runtime: %q", calls)
		}
	}
}

// The same adapter drives docker when nerdctl is not installed. A refusal
// that named nerdctl there sent a docker user to read about a runtime they
// never installed, the mistake Kind was fixed for.
func TestDockerRefusalNamesDocker(t *testing.T) {
	t.Setenv("BRIG_CONTAINERD_RUNTIME", "")
	d := newHullDouble(t, egressHelp, "0")
	docker := filepath.Join(filepath.Dir(d.bin), "docker")
	if err := os.Rename(d.bin, docker); err != nil {
		t.Fatal(err)
	}
	n := &nerdctl{bin: docker}
	spec := RunSpec{Name: "brig-cap", Image: "img", Net: "shared",
		Egress: Egress{Default: "deny"}}
	err := n.Run(spec)
	var ce *CapabilityError
	if !errors.As(err, &ce) {
		t.Fatalf("want a capability refusal, got %v", err)
	}
	want := RunPath{"docker", containerdRuntime()}
	if ce.State != CannotEnforce || ce.Path != want {
		t.Errorf("refused as %q on %s, want %q on %s", ce.State, ce.Path, CannotEnforce, want)
	}
	if strings.Contains(err.Error(), "nerdctl") {
		t.Errorf("the refusal on docker names nerdctl: %v", err)
	}
	if !strings.Contains(err.Error(), "docker on "+containerdRuntime()) {
		t.Errorf("the refusal does not name docker and the shim: %v", err)
	}
	if calls := d.calls(); calls != "" {
		t.Errorf("a refused run reached the runtime: %q", calls)
	}
}

// A run with no policy asks nothing: no probe, no refusal, on every backend,
// including one the table has no record for. An offline run with a policy
// asks nothing either, because it reaches no network.
func TestARunThatNeedsNoEnforcementAsksNothing(t *testing.T) {
	policy := Egress{Default: "deny"}
	for _, tt := range []struct {
		name string
		hv   string
		net  string
		e    Egress
	}{
		{"hvi, no policy", "hvi", "shared", Egress{}},
		{"vz, no policy", "vz", "shared", Egress{}},
		{"qemu, no policy", "qemu", "shared", Egress{}},
		{"krun, no policy", "krun", "shared", Egress{}},
		{"hvi, offline with a policy", "hvi", "none", policy},
		{"vz, offline with a policy", "vz", "none", policy},
		{"qemu, offline with a policy", "qemu", "none", policy},
		{"krun, offline with a policy", "krun", "none", policy},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := newHullDouble(t, egressHelp, "0")
			err := (&hull{bin: d.bin}).Run(RunSpec{Name: "brig-cap", Image: "img",
				Hypervisor: tt.hv, Net: tt.net, Egress: tt.e})
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if d.called("network-gateway --help") {
				t.Errorf("a run that enforces nothing ran the probe: %q", d.calls())
			}
			if !d.called("run ") {
				t.Errorf("nothing booted: %q", d.calls())
			}
		})
	}
	for _, e := range []struct {
		net string
		e   Egress
	}{{"shared", Egress{}}, {"none", policy}} {
		d := newHullDouble(t, egressHelp, "0")
		err := (&nerdctl{bin: d.bin}).Run(RunSpec{Name: "brig-cap", Image: "img",
			Net: e.net, Egress: e.e})
		if err != nil {
			t.Errorf("nerdctl refused a run on %s with %+v: %v", e.net, e.e, err)
		}
	}
}

// canRunRefusal is the refusal the join path gets, which reads the table and
// never probes.
func canRunRefusal(r RunChecker, spec RunSpec) error {
	err := r.CanRun(spec)
	var ce *CapabilityError
	if errors.As(err, &ce) {
		return err
	}
	return nil
}

const egressHelp = "   --egress-default string   verdict\n"

// hullDouble is a runtime binary that logs every call. Its gateway writes the
// socket it was told to serve and exits, and the test listens there in its
// place, so the boot sees a gateway come up without python or a real hull.
type hullDouble struct {
	bin, log string
}

func newHullDouble(t *testing.T, help, exit string) *hullDouble {
	t.Helper()
	scratchIsolatedDir(t)
	dir := t.TempDir()
	d := &hullDouble{bin: filepath.Join(dir, "hull"), log: filepath.Join(dir, "calls")}
	ready := filepath.Join(dir, "gateway")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> '" + d.log + "'\n" +
		"if [ \"$1\" = network-gateway ]; then\n" +
		"  case \" $* \" in *\" --help \"*) cat <<'EOF'\n" + help + "EOF\n  exit " + exit + ";; esac\n" +
		"  prev=''; for a in \"$@\"; do [ \"$prev\" = --qemu-socket ] && echo \"$a\" > '" + ready + "'; prev=\"$a\"; done\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(d.bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	t.Cleanup(func() { close(stop); <-done })
	go func() {
		defer close(done)
		var l net.Listener
		defer func() {
			if l != nil {
				_ = l.Close()
			}
		}()
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			if l != nil {
				continue
			}
			blob, err := os.ReadFile(ready)
			if err != nil {
				continue
			}
			l, err = net.Listen("unix", strings.TrimSpace(string(blob)))
			if err != nil {
				continue
			}
			go func(l net.Listener) {
				for {
					c, err := l.Accept()
					if err != nil {
						return
					}
					_ = c.Close()
				}
			}(l)
		}
	}()
	return d
}

func (d *hullDouble) calls() string {
	blob, _ := os.ReadFile(d.log)
	return string(blob)
}

func (d *hullDouble) called(s string) bool { return strings.Contains(d.calls(), s) }
