package runtime

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/egress"
)

// Trimmed from `nerdctl network inspect --mode native` of a brig sandbox
// network, nerdctl v2.3.5.
const nativeInspect = `[
  {
    "CNI": {
      "cniVersion": "1.0.0",
      "name": "brig-ubuntu-egpoc",
      "plugins": [
        {
          "type": "bridge",
          "bridge": "br-ebd625d78bb2",
          "isGateway": true,
          "ipMasq": true,
          "ipam": {
            "ranges": [[{"gateway": "10.4.1.1", "subnet": "10.4.1.0/24"}]],
            "routes": [{"dst": "0.0.0.0/0"}],
            "type": "host-local"
          }
        },
        {"type": "portmap", "capabilities": {"portMappings": true}},
        {"type": "firewall", "backend": "iptables", "ingressPolicy": "same-bridge"},
        {"type": "tuning"}
      ]
    },
    "NerdctlID": "ebd625d78bb2db12a713ca34b864ebfb0c2ab3960440c510be517e960101c4a1"
  }
]`

func TestParseBridge(t *testing.T) {
	n, err := parseBridge([]byte(nativeInspect))
	if err != nil {
		t.Fatal(err)
	}
	if n.Bridge != "br-ebd625d78bb2" || n.Gateway.String() != "10.4.1.1" || n.Subnet.String() != "10.4.1.0/24" {
		t.Errorf("got %+v", n)
	}
	// A gateway outside the subnet would put the resolver where the guest's
	// packets never arrive.
	outside := strings.Replace(nativeInspect, `"gateway": "10.4.1.1"`, `"gateway": "10.9.9.1"`, 1)
	if _, err := parseBridge([]byte(outside)); err == nil {
		t.Error("parseBridge took a gateway outside the subnet")
	}
	// host-local defaults the gateway to the first address of the subnet.
	noGW := strings.Replace(nativeInspect, `"gateway": "10.4.1.1", `, "", 1)
	if n, err := parseBridge([]byte(noGW)); err != nil || n.Gateway.String() != "10.4.1.1" {
		t.Errorf("no gateway: %+v, %v", n, err)
	}
	for name, bad := range map[string]string{
		"empty":     `[]`,
		"no bridge": `[{"CNI": {"plugins": [{"type": "macvlan"}]}}]`,
		"v6 only":   `[{"CNI": {"plugins": [{"type": "bridge", "bridge": "br-x", "ipam": {"ranges": [[{"subnet": "fd00::/64"}]]}}]}}]`,
		"garbage":   `not json`,
	} {
		if _, err := parseBridge([]byte(bad)); err == nil {
			t.Errorf("%s: parseBridge succeeded", name)
		}
	}
}

func TestEgressSpecIgnoresOrderAndKeepsSides(t *testing.T) {
	a := Egress{Default: "deny", Allow: []Rule{{Host: "b.com"}, {CIDR: "10.0.0.0/8"}}}
	b := Egress{Default: "deny", Allow: []Rule{{CIDR: "10.0.0.0/8"}, {Host: "b.com"}}}
	if egressSpec(a) != egressSpec(b) {
		t.Error("two orderings of one policy differ")
	}
	moved := Egress{Default: "deny", Allow: []Rule{{CIDR: "10.0.0.0/8"}}, Deny: []Rule{{Host: "b.com"}}}
	if egressSpec(a) == egressSpec(moved) {
		t.Error("a rule moved from allow to deny compares equal")
	}
	if egressSpec(Egress{}) != "" {
		t.Error("an unfiltered run has a spec")
	}
}

func TestOrphanTables(t *testing.T) {
	listing := "table inet brig_egress_claude-a\ntable inet brig_egress_claude-b\n" +
		"table inet firewalld\ntable ip nat\n"
	got := orphanTables(listing, map[string]bool{"brig-claude-a": true})
	if len(got) != 1 || got[0] != "brig_egress_claude-b" {
		t.Errorf("orphanTables = %q", got)
	}
}

func TestUpstreamsReadsIPv4Nameservers(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "resolv.conf")
	_ = os.WriteFile(conf, []byte("search lan\nnameserver 10.0.2.3\nnameserver fd00::1\nnameserver 192.168.42.1\n"), 0o600)
	got, err := containerNetns{resolvConf: conf}.upstreams()
	if err != nil || len(got) != 2 || got[0].String() != "10.0.2.3" || got[1].String() != "192.168.42.1" {
		t.Errorf("upstreams = %v, %v", got, err)
	}
	_ = os.WriteFile(conf, []byte("nameserver fd00::1\n"), 0o600)
	if _, err := (containerNetns{resolvConf: conf}).upstreams(); err == nil {
		t.Error("an IPv6-only resolv.conf gave upstreams")
	}
}

// NetworkStale on nerdctl compares the recorded rules, and reads a recorded
// policy as stale when its resolver is gone. It never compares the posture,
// which wrap records and compares itself.
func TestNerdctlNetworkStale(t *testing.T) {
	scratchIsolatedDir(t)
	n := &nerdctl{bin: "nerdctl"}
	policy := Egress{Default: "deny", Allow: []Rule{{Host: "example.com"}}}

	if n.NetworkStale("brig-s", "", "isolated", Egress{}) {
		t.Error("a sandbox with no record and no policy reads stale")
	}
	if !n.NetworkStale("brig-s", "", "isolated", policy) {
		t.Error("a policy attached since the boot does not read stale")
	}

	dir, _ := egressDir()
	_ = os.MkdirAll(dir, 0o700)
	spec, _ := egressFile("brig-s", ".spec")
	_ = os.WriteFile(spec, []byte(egressSpec(policy)+"\n"), 0o600)
	// No resolver behind the record: the rules are enforced by nothing.
	if !n.NetworkStale("brig-s", "", "isolated", policy) {
		t.Error("a recorded policy with no resolver does not read stale")
	}
	if !n.NetworkStale("brig-s", "", "shared", Egress{}) {
		t.Error("a policy detached since the boot does not read stale")
	}
	if !n.NetworkStale("brig-s", "", "isolated", Egress{Default: "allow"}) {
		t.Error("a changed policy does not read stale")
	}
	// An offline run asks for no rules.
	_ = os.Remove(spec)
	if n.NetworkStale("brig-s", "", "none", policy) {
		t.Error("an offline sandbox reads stale over a policy")
	}
}

// The guest's resolver is a run flag, so it has to land among the flags and
// not after the image, where nerdctl would pass it to the guest's command.
func TestWithDNSGoesBeforeTheImage(t *testing.T) {
	n := &nerdctl{bin: "nerdctl"}
	args, _, err := n.runArgs(RunSpec{Name: "brig-s", Image: "img", Net: "isolated", Mem: 512, CPUs: 1})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(withDNS(args, netip.MustParseAddr("10.4.1.1")), " ")
	if !strings.HasPrefix(got, "run --dns 10.4.1.1 --detach ") || !strings.HasSuffix(got, " img sleep infinity") {
		t.Errorf("run args = %s", got)
	}
}

// Zero is the mark of every untagged connection. Every other value is kept,
// so two boots share a mark once in 65535 rather than once in 32768.
func TestBootMark(t *testing.T) {
	for in, want := range map[[2]byte]uint16{{0, 0}: 1, {0x12, 0x34}: 0x1234, {0xff, 0xfe}: 0xfffe} {
		if got := bootMark(in); got != want {
			t.Errorf("bootMark(%x) = %#x, want %#x", in, got, want)
		}
	}
}

// nerdctlDouble is a nerdctl that logs its calls. `ps` lists the sandbox
// named in the file running while it exists, and a verb fails while the file
// fail-<verb> exists. There is no rootless containerd beside it, so brig
// finds no namespace and touches no real table.
type nerdctlDouble struct{ bin, dir string }

func newNerdctlDouble(t *testing.T) *nerdctlDouble {
	t.Helper()
	scratchIsolatedDir(t)
	t.Setenv("ROOTLESSKIT_STATE_DIR", "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	prev := rootless
	t.Cleanup(func() { rootless = prev })
	rootless = func() bool { return true }
	dir := t.TempDir()
	d := &nerdctlDouble{bin: filepath.Join(dir, "nerdctl"), dir: dir}
	sh := "#!/bin/sh\n" +
		"echo \"$*\" >> '" + dir + "/calls'\n" +
		"[ -e '" + dir + "/fail-'\"$1\" ] && exit 1\n" +
		"if [ \"$1\" = ps ] && [ -e '" + dir + "/running' ]; then cat '" + dir + "/running'; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(d.bin, []byte(sh), 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

func (d *nerdctlDouble) set(t *testing.T, file, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(d.dir, file), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (d *nerdctlDouble) calls() string {
	blob, _ := os.ReadFile(filepath.Join(d.dir, "calls"))
	return string(blob)
}

// plantEgress writes the records a boot under a policy leaves. The pid is
// no process's, so the resolver reads as gone.
func plantEgress(t *testing.T, name string) {
	t.Helper()
	dir, err := egressDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for ext, content := range map[string]string{".pid": "999999999\n", ".spec": "default=deny\n", ".log": ""} {
		p, _ := egressFile(name, ext)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// While a sandbox runs, its table is all that filters it. A stop or a remove
// that failed leaves it running, and its rules stay.
func TestAFailedStopOrRemoveKeepsTheRules(t *testing.T) {
	for _, verb := range []string{"stop", "rm"} {
		d := newNerdctlDouble(t)
		n := &nerdctl{bin: d.bin}
		plantEgress(t, "brig-live")
		d.set(t, "running", "brig-live\n")
		d.set(t, "fail-"+verb, "")
		if verb == "stop" {
			_ = n.Stop("brig-live")
		} else {
			_ = n.Remove("brig-live")
		}
		if !hasEgressRecord("brig-live") {
			t.Errorf("a failed %s removed the rules of a running sandbox", verb)
		}
	}
}

func TestAStoppedSandboxLosesItsRules(t *testing.T) {
	d := newNerdctlDouble(t)
	plantEgress(t, "brig-done")
	if err := (&nerdctl{bin: d.bin}).Stop("brig-done"); err != nil {
		t.Fatal(err)
	}
	if hasEgressFiles("brig-done") {
		t.Error("a stopped sandbox kept its egress records")
	}
}

// Another brig can boot the sandbox between wrap's look and this run. Its
// rules are not this run's to replace.
func TestABootLeavesARunningSandboxAlone(t *testing.T) {
	d := newNerdctlDouble(t)
	plantEgress(t, "brig-live")
	d.set(t, "running", "brig-live\n")
	defer func(prev func(*nerdctl, string, Egress) (egressBoot, error)) { egressStart = prev }(egressStart)
	egressStart = func(*nerdctl, string, Egress) (egressBoot, error) {
		t.Error("the boot started egress over a running sandbox")
		return egressBoot{}, errors.New("refused")
	}
	err := (&nerdctl{bin: d.bin}).Run(RunSpec{Name: "brig-live", Image: "img", Net: "isolated",
		Egress: Egress{Default: "deny"}})
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("want a refusal naming the running sandbox, got %v", err)
	}
	if !hasEgressRecord("brig-live") {
		t.Error("the boot removed a running sandbox's rules")
	}
	if strings.Contains(d.calls(), "run ") {
		t.Errorf("the boot reached `nerdctl run`: %q", d.calls())
	}
}

func TestARunGivesTheGuestTheResolver(t *testing.T) {
	d := newNerdctlDouble(t)
	defer func(prev func(*nerdctl, string, Egress) (egressBoot, error)) { egressStart = prev }(egressStart)
	egressStart = func(*nerdctl, string, Egress) (egressBoot, error) {
		return egressBoot{resolver: netip.MustParseAddr("10.4.9.1"), up: func() error { return nil }}, nil
	}
	err := (&nerdctl{bin: d.bin}).Run(RunSpec{Name: "brig-s", Image: "img", Net: "isolated",
		Egress: Egress{Default: "deny"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.calls(), "run --dns 10.4.9.1 --detach --name brig-s") {
		t.Errorf("the guest was not given the resolver: %q", d.calls())
	}
}

// The rules match the bridge by name in the namespace brig found. A
// container whose bridge is somewhere else runs unfiltered, so it is removed
// and the boot refused.
func TestABootWhoseBridgeIsElsewhereIsRefused(t *testing.T) {
	d := newNerdctlDouble(t)
	defer func(prev func(*nerdctl, string, Egress) (egressBoot, error)) { egressStart = prev }(egressStart)
	egressStart = func(*nerdctl, string, Egress) (egressBoot, error) {
		return egressBoot{resolver: netip.MustParseAddr("10.4.9.1"),
			up: func() error { return errors.New("the bridge br-x is not there") }}, nil
	}
	err := (&nerdctl{bin: d.bin}).Run(RunSpec{Name: "brig-s", Image: "img", Net: "isolated",
		Egress: Egress{Default: "deny"}})
	var ce *CapabilityError
	if !errors.As(err, &ce) || ce.State != Unknown {
		t.Fatalf("want a capability refusal, got %v", err)
	}
	if !strings.Contains(d.calls(), "rm -f brig-s") {
		t.Errorf("the unfiltered container was left running: %q", d.calls())
	}
}

func TestARunWithoutAPolicyDropsThePreviousRules(t *testing.T) {
	d := newNerdctlDouble(t)
	plantEgress(t, "brig-s")
	if err := (&nerdctl{bin: d.bin}).Run(RunSpec{Name: "brig-s", Image: "img", Net: "isolated"}); err != nil {
		t.Fatal(err)
	}
	if hasEgressFiles("brig-s") {
		t.Error("a boot without a policy kept the previous boot's records")
	}
}

// A prune works from a list of sandboxes read before it, and a sandbox can
// boot in between. Its rules stay.
func TestPruneLeavesASandboxThatBootedSince(t *testing.T) {
	d := newNerdctlDouble(t)
	n := &nerdctl{bin: d.bin}
	plantEgress(t, "brig-late")
	d.set(t, "running", "brig-late\n")
	n.pruneEgress(nil)
	if !hasEgressRecord("brig-late") {
		t.Error("prune removed the rules of a running sandbox")
	}
	if err := os.Remove(filepath.Join(d.dir, "running")); err != nil {
		t.Fatal(err)
	}
	n.pruneEgress(nil)
	if hasEgressFiles("brig-late") {
		t.Error("prune kept the rules of a sandbox that is gone")
	}
}

// The probe runs nft in the namespace. An nft that refuses there refuses the
// boot with what it said.
func TestNerdctlRefusesWhenNFTFailsInTheNamespace(t *testing.T) {
	d := newNerdctlDouble(t)
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "child_pid"), []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROOTLESSKIT_STATE_DIR", state)
	bin := t.TempDir()
	for name, sh := range map[string]string{
		"nsenter": "#!/bin/sh\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n",
		"nft":     "#!/bin/sh\necho 'Error: Could not process rule: Operation not permitted' >&2\nexit 1\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(sh), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	err := (&nerdctl{bin: d.bin}).Run(RunSpec{Name: "brig-s", Image: "img", Net: "isolated",
		Egress: Egress{Default: "deny"}})
	var ce *CapabilityError
	if !errors.As(err, &ce) || ce.State != Unknown {
		t.Fatalf("want a capability refusal, got %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "list tables` failed") || !strings.Contains(msg, "Operation not permitted") {
		t.Errorf("the refusal does not name the probe and what nft said: %v", err)
	}
	if strings.Contains(d.calls(), "run ") {
		t.Errorf("a refused boot reached `nerdctl run`: %q", d.calls())
	}
}

// A resolver that exits before it serves fails the boot with what it said,
// and leaves no record that would read as a live resolver.
func TestAResolverThatExitsFailsTheBoot(t *testing.T) {
	scratchIsolatedDir(t)
	nft := filepath.Join(t.TempDir(), "nft")
	sh := "#!/bin/sh\necho 'Error: Could not process rule: Operation not permitted' >&2\nexit 1\n"
	if err := os.WriteFile(nft, []byte(sh), 0o700); err != nil {
		t.Fatal(err)
	}
	bridge := egress.Network{Bridge: "br-x", Gateway: netip.MustParseAddr("127.0.0.1"),
		Subnet: netip.MustParsePrefix("127.0.0.0/8"), Redirect: []netip.Addr{netip.MustParseAddr("10.0.2.3")}}
	err := startResolver("brig-x", "brig_egress_x", bridge, 7, containerNetns{}, nft, Egress{Default: "deny"})
	if err == nil || !strings.Contains(err.Error(), "Operation not permitted") {
		t.Errorf("want the resolver's own error, got %v", err)
	}
	if hasEgressRecord("brig-x") {
		t.Error("a resolver that never served was recorded")
	}
}
