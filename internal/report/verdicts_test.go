package report

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/runtime"
)

// The console of the 6 Nov 2026 hang, cut to the kill.
const oomConsole = `urunit-agent: listening on vsock port 1024
[17538.451690] claude invoked oom-killer: gfp_mask=0x140cca(GFP_HIGHUSER_MOVABLE|__GFP_COMP), order=0, oom_score_adj=0
[17538.451898] oom-kill:constraint=CONSTRAINT_NONE,nodemask=(null),cpuset=/,mems_allowed=0,global_oom,task_memcg=/,task=node,pid=21272,uid=0
[17538.451910] Out of memory: Killed process 21272 (node) total-vm:13653136kB, anon-rss:1052680kB, file-rss:276kB
`

// The same kill as dmesg prints it in colour, which is what the guest gave.
const oomDmesg = "\x1b[32m[17538.451910] \x1b[0m\x1b[33mOut of memory: \x1b[0m\x1b[31mKilled process 21272 (node) total-vm:13653136kB, anon-rss:1052680kB, file-rss:276kB\x1b[0m\n"

func answering(console string, answers map[string]string) inspectingRuntime {
	return inspectingRuntime{
		fakeRuntime: &fakeRuntime{console: console, output: func(spec runtime.ExecSpec) (string, error) {
			script := spec.Cmd[len(spec.Cmd)-1]
			for key, out := range answers {
				if strings.Contains(script, key) {
					return out, nil
				}
			}
			return "", nil
		}},
		vm: runtime.VM{MemMiB: 4096, CPUs: 4, Backend: "hvi", Status: "running"},
	}
}

func verdictNamed(t *testing.T, r Report, name string) Verdict {
	t.Helper()
	for _, v := range r.Verdicts {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no %q verdict in %+v", name, r.Verdicts)
	return Verdict{}
}

func TestVerdictOOM(t *testing.T) {
	r := Collect(context.Background(), answering(oomConsole, map[string]string{"dmesg": oomDmesg}), running(), io.Discard)
	v := verdictNamed(t, r, "oom")
	if !strings.Contains(v.Evidence, "Killed process 21272 (node)") {
		t.Errorf("evidence = %q", v.Evidence)
	}
	for _, want := range []string{"brig stop claude", "4096 MiB", "1 kill"} {
		if !strings.Contains(v.Fix, want) {
			t.Errorf("fix %q does not say %q", v.Fix, want)
		}
	}
	// The console and dmesg hold the same kill.
	if r.OOMKills != 1 {
		t.Errorf("OOMKills = %d, want 1", r.OOMKills)
	}
}

func TestVerdictThrash(t *testing.T) {
	psi := "memory:\nsome avg10=80.00 avg60=60.00 avg300=20.00 total=1\nfull avg10=42.00 avg60=30.00 avg300=10.00 total=1\n" +
		"cpu:\nsome avg10=1.00 avg60=1.00 avg300=1.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"
	r := Collect(context.Background(), answering("", map[string]string{"pressure": psi}), running(), io.Discard)
	v := verdictNamed(t, r, "thrash")
	if !strings.Contains(v.Evidence, "full avg10=42.00") || !strings.Contains(v.Fix, "4096 MiB") {
		t.Errorf("thrash verdict = %+v", v)
	}
}

func TestVerdictThrashNotOnLowPressure(t *testing.T) {
	psi := "memory:\nsome avg10=3.00 avg60=1.00 avg300=0.50 total=1\nfull avg10=2.00 avg60=1.00 avg300=0.50 total=1\n"
	r := Collect(context.Background(), answering("", map[string]string{"pressure": psi}), running(), io.Discard)
	for _, v := range r.Verdicts {
		if v.Name == "thrash" {
			t.Fatalf("low pressure produced %+v", v)
		}
	}
}

func TestVerdictUnresponsiveSaysIdle(t *testing.T) {
	r := Collect(context.Background(), inspectingRuntime{&fakeRuntime{}, runtime.VM{Backend: "hvi"}}, running(), io.Discard)
	r.vmmCPU, r.haveCPU = 0, true
	r.Verdicts = match(&r)
	v := verdictNamed(t, r, "unresponsive")
	if !strings.HasSuffix(v.Fix, "hvi is idle.") {
		t.Errorf("fix = %q, want it to end saying hvi is idle", v.Fix)
	}
	r.vmmCPU = 99.5
	v = verdictNamed(t, Report{Probes: r.Probes, VM: r.VM, HaveVM: true, vmmCPU: 99.5, haveCPU: true, in: r.in, Verdicts: match(&r)}, "unresponsive")
	if !strings.HasSuffix(v.Fix, "hvi is using 99.5% CPU.") {
		t.Errorf("fix = %q, want it to give the CPU figure", v.Fix)
	}
}

func TestVerdictTmpfsFull(t *testing.T) {
	df := "Filesystem Size Used Avail Use% Mounted on\ntmpfs 512M 512M 0 100% /brig/claude\n"
	in := running()
	in.Tmpfs = []string{"/brig/claude"}
	r := Collect(context.Background(), answering("", map[string]string{"df -h": df}), in, io.Discard)
	want := "The tmpfs at /brig/claude is full. Its size is set by the size: of that volume in the profile."
	if v := verdictNamed(t, r, "tmpfs"); v.Fix != want {
		t.Errorf("fix = %q, want %q", v.Fix, want)
	}
}

// Each profile mounts its tmpfs somewhere of its own. The df probe asks about
// the ones this profile has, and only those count as full.
func TestVerdictTmpfsFollowsTheProfile(t *testing.T) {
	df := "Filesystem Size Used Avail Use% Mounted on\n" +
		"tmpfs 64M 64M 0 100% /root/.codex\n" +
		"tmpfs 512M 512M 0 100% /dev/shm\n"
	var script string
	rt := answering("", nil)
	rt.output = func(spec runtime.ExecSpec) (string, error) {
		if s := spec.Cmd[len(spec.Cmd)-1]; strings.Contains(s, "df -h") {
			script = s
			return df, nil
		}
		return "", nil
	}
	in := running()
	in.Tmpfs = []string{"/root/.codex", "/brig/it's"}
	r := Collect(context.Background(), rt, in, io.Discard)
	for _, want := range []string{"'/root/.codex'", `'/brig/it'\''s'`} {
		if !strings.Contains(script, want) {
			t.Errorf("df script %q does not ask about %s", script, want)
		}
	}
	if strings.Contains(script, "/brig/claude") {
		t.Errorf("df script %q names a mount this profile does not have", script)
	}
	v := verdictNamed(t, r, "tmpfs")
	if !strings.Contains(v.Evidence, "/root/.codex") {
		t.Errorf("evidence = %q, want the profile's mount, not /dev/shm", v.Evidence)
	}
}

// A guest can answer the first probes and stop on a later one. That is the
// same silent guest, and the verdict names the probe it stopped on.
func TestVerdictUnresponsiveOnALaterProbe(t *testing.T) {
	rt := answering("", nil)
	rt.output = func(spec runtime.ExecSpec) (string, error) {
		switch s := spec.Cmd[len(spec.Cmd)-1]; {
		case strings.Contains(s, "dmesg"):
			return "", nil
		case strings.Contains(s, "/proc/pressure"):
			return "memory:\n", nil
		}
		<-spec.Ctx.Done()
		return "", spec.Ctx.Err()
	}
	r := Collect(context.Background(), rt, running(), io.Discard)
	v := verdictNamed(t, r, "unresponsive")
	if v.Evidence != "probe free -m timed out" {
		t.Errorf("evidence = %q, want the third probe named", v.Evidence)
	}
}

// A pid file that is there but cannot be read says nothing about the
// gateway, and the verdict does not claim it is down.
func TestVerdictGatewayPidFileUnreadable(t *testing.T) {
	in := running()
	in.GatewayPIDFile = t.TempDir() // a directory: reading it fails
	in.GatewayAlways = true
	r := Collect(context.Background(), answering("", nil), in, io.Discard)
	v := verdictNamed(t, r, "gateway")
	if strings.Contains(v.Fix, "is not running") || !strings.Contains(v.Fix, "could not be read") {
		t.Errorf("fix = %q, want it to say the pid file could not be read", v.Fix)
	}
}

func TestVerdictNone(t *testing.T) {
	r := Collect(context.Background(), answering("", nil), running(), io.Discard)
	if len(r.Verdicts) != 1 || r.Verdicts[0].Name != "none" || r.Verdicts[0].Fix != "No known pattern matched." {
		t.Errorf("Verdicts = %+v, want only none", r.Verdicts)
	}
}

// hull runs a gateway for every isolated sandbox; nerdctl only when a policy
// filters egress. A missing pid file is a fault on the first and normal on
// the second.
func TestVerdictGateway(t *testing.T) {
	for _, tc := range []struct {
		name   string
		always bool
		want   bool
	}{{"hull", true, true}, {"nerdctl", false, false}} {
		in := running()
		in.GatewayPIDFile = filepath.Join(t.TempDir(), "absent.pid")
		in.GatewayAlways = tc.always
		r := Collect(context.Background(), answering("", nil), in, io.Discard)
		got := false
		for _, v := range r.Verdicts {
			got = got || v.Name == "gateway"
		}
		if got != tc.want {
			t.Errorf("%s: gateway verdict %t, want %t (%+v)", tc.name, got, tc.want, r.Verdicts)
		}
	}
}

// A hung mount makes df wait, not the guest. When the guest answered every
// other probe, the verdict names the probe and what it waits on, and does
// not call the guest unresponsive.
func TestVerdictStalledMountIsNotASilentGuest(t *testing.T) {
	rt := answering("", map[string]string{"dmesg": "[1.0] booted\n"})
	inner := rt.output
	rt.output = func(spec runtime.ExecSpec) (string, error) {
		if strings.Contains(spec.Cmd[len(spec.Cmd)-1], "df -h") {
			<-spec.Ctx.Done()
			return "", spec.Ctx.Err()
		}
		return inner(spec)
	}
	r := Collect(context.Background(), rt, running(), io.Discard)
	for _, v := range r.Verdicts {
		if v.Name == "unresponsive" {
			t.Errorf("a hung df made the guest unresponsive: %+v", v)
		}
	}
	v := verdictNamed(t, r, "stalled")
	if v.Evidence != "probe df -h timed out" || !strings.Contains(v.Fix, "/work") {
		t.Errorf("stalled = %+v, want df named and the workspace mount suggested", v)
	}
	if p, _ := r.probe("dmesg"); p == "" {
		t.Error("dmesg was not asked")
	}
}

// A live process at the recorded pid is not the gateway unless the runtime
// says so: a gateway that died gave its pid back.
func TestVerdictGatewayPidReused(t *testing.T) {
	in := running()
	in.GatewayPIDFile = filepath.Join(t.TempDir(), "gw.pid")
	writeFile(t, in.GatewayPIDFile, fmt.Sprintf("%d\n", os.Getpid()))
	in.GatewayAlways = true
	in.GatewayOwns = func(int) bool { return false }
	r := Collect(context.Background(), answering("", nil), in, io.Discard)
	if v := verdictNamed(t, r, "gateway"); !strings.Contains(v.Evidence, "another process") {
		t.Errorf("gateway verdict = %+v, want the pid named as another process", v)
	}
	in.GatewayOwns = func(int) bool { return true }
	r = Collect(context.Background(), answering("", nil), in, io.Discard)
	for _, v := range r.Verdicts {
		if v.Name == "gateway" {
			t.Errorf("a gateway the runtime owns was reported down: %+v", v)
		}
	}
}
