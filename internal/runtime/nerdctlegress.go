package runtime

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/brig-sh/brig/internal/egress"
)

// Egress on the container network.
//
// A sandbox on this runtime reaches the network through the bridge of its own
// isolated network, and the kernel routes it out from there. brig enforces a
// policy at that hop: an nftables table in the network namespace that holds
// the bridge, and a resolver on the bridge address that answers the guest's
// DNS. internal/egress has both halves. This file starts and stops them with
// the sandbox.
//
// Rootless nerdctl keeps its bridges in rootlesskit's network namespace, not
// the host's. brig enters it with nsenter, the way nerdctl itself does, and
// the user owns that namespace, so this needs no privilege. A rootful nerdctl
// keeps them in the host's namespace, and brig is root there already.
//
// The rules are read once, when the sandbox boots, as on hull's gateway. The
// record beside the resolver says what it was started with, and NetworkStale
// compares it with what a run asks for.
//
// While the sandbox runs, its table is all that filters it. Removing the
// table opens the network, where stopping hull's gateway cuts it. So the
// table goes only once the container is confirmed stopped, under a lock per
// sandbox that a boot holds through `nerdctl run`.

// egressStartTimeout bounds the wait for the resolver to install the table and
// bind its sockets.
var egressStartTimeout = 10 * time.Second

// egressProbeTimeout bounds each nft command brig runs in the namespace.
var egressProbeTimeout = 15 * time.Second

// containerNetns is the namespace nerdctl's bridges live in, and how to run a
// command in it.
type containerNetns struct {
	// enter is the argv prefix that runs a command in the namespace. Empty for
	// the host's own.
	enter []string
	// resolvConf is the resolver configuration the containers get, read for the
	// upstream servers.
	resolvConf string
}

// rootless reports whether nerdctl runs rootless here. nerdctl decides it by
// the effective uid, and so does this.
var rootless = func() bool { return os.Geteuid() != 0 }

// findContainerNetns returns the namespace of this host's nerdctl bridges.
func findContainerNetns() (containerNetns, error) {
	if !rootless() {
		return containerNetns{resolvConf: "/etc/resolv.conf"}, nil
	}
	dir := os.Getenv("ROOTLESSKIT_STATE_DIR")
	if dir == "" {
		run := os.Getenv("XDG_RUNTIME_DIR")
		if run == "" {
			return containerNetns{}, errors.New("XDG_RUNTIME_DIR is not set, so the rootless containerd state cannot be found")
		}
		dir = filepath.Join(run, "containerd-rootless")
	}
	blob, err := os.ReadFile(filepath.Join(dir, "child_pid"))
	if err != nil {
		return containerNetns{}, fmt.Errorf("rootless containerd is not running here: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(blob)))
	if err != nil || pid <= 0 {
		return containerNetns{}, fmt.Errorf("%s holds no pid", filepath.Join(dir, "child_pid"))
	}
	nsenter, err := lookSbin("nsenter")
	if err != nil {
		return containerNetns{}, err
	}
	enter := []string{nsenter, "-U", "--preserve-credentials", "-t", strconv.Itoa(pid)}
	// rootlesskit's detach-netns mode leaves the child in the host's network
	// namespace and bind-mounts the containers' one here. The mount is in the
	// child's mount namespace, so the host reaches it through /proc.
	if _, err := os.Stat(filepath.Join(dir, "netns")); err == nil {
		enter = append(enter, "--net=/proc/"+strconv.Itoa(pid)+"/root"+filepath.Join(dir, "netns"))
	} else {
		enter = append(enter, "-n")
	}
	return containerNetns{enter: append(enter, "--"), resolvConf: filepath.Join(dir, "resolv.conf")}, nil
}

// lookSbin finds a binary on PATH, then in the sbin directories a user's PATH
// often lacks.
func lookSbin(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	for _, dir := range []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"} {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s is not installed", name)
}

// nftArgv is the command that runs nft in the namespace.
func (ns containerNetns) nftArgv() ([]string, string, error) {
	nft, err := lookSbin("nft")
	if err != nil {
		return nil, "", err
	}
	return append(append([]string(nil), ns.enter...), nft), nft, nil
}

// output runs argv in the namespace and returns its standard output.
func (ns containerNetns) output(argv ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), egressProbeTimeout)
	defer cancel()
	full := append(append([]string(nil), ns.enter...), argv...)
	cmd := exec.CommandContext(ctx, full[0], full[1:]...)
	cmd.WaitDelay = time.Second
	return cmd.Output()
}

// requireInterface confirms that the namespace has an interface called name.
//
// The rules match the bridge by name in the namespace brig found. A
// BRIG_RUNTIME_BIN that reaches another containerd puts the bridge somewhere
// else, and the rules would match nothing.
func (ns containerNetns) requireInterface(name string) error {
	var out []byte
	var err error
	if len(ns.enter) == 0 {
		out, err = os.ReadFile("/proc/net/dev")
	} else {
		var cat string
		if cat, err = lookSbin("cat"); err == nil {
			out, err = ns.output(cat, "/proc/net/dev")
		}
	}
	if err != nil {
		return fmt.Errorf("could not list the interfaces of the container network: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if iface, _, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(iface) == name {
			return nil
		}
	}
	return fmt.Errorf("the bridge %s is not in the network namespace brig put its rules in", name)
}

// upstreams reads the nameservers the containers are given.
//
// They are also the resolvers rootless nerdctl writes into a container's
// resolv.conf ahead of any --dns, so DNS sent to them is redirected to
// brig's resolver.
func (ns containerNetns) upstreams() ([]netip.Addr, error) {
	f, err := os.Open(ns.resolvConf)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []netip.Addr
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		// The resolver forwards over the sandbox network's IPv4 path only.
		if a, err := netip.ParseAddr(fields[1]); err == nil && a.Is4() {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s lists no IPv4 nameserver", ns.resolvConf)
	}
	return out, nil
}

// nerdctlPath is the egress capability record for this runtime, under the
// shim the run uses.
func nerdctlPath() RunPath { return RunPath{"nerdctl", containerdRuntime()} }

// egressRefusal is the refusal of a boot whose rules brig could not put in
// place. The record answers Enforced, so what failed is this host, and the
// answer for it is unknown.
func egressRefusal(why, remedy string, err error) error {
	return &CapabilityError{Property: EgressPolicy, Path: nerdctlPath(), State: Unknown,
		Why: why, Remedy: remedy, Err: err}
}

const egressSetupRemedy = "Install nftables and util-linux's nsenter, or detach the policy"

// egressEnforces confirms, at boot, the table's answer for this runtime: that
// nft runs in the namespace the bridges live in. A probe that fails answers
// unknown and refuses the boot, as the gateway probe does on hvi.
func egressEnforces() (containerNetns, error) {
	ns, err := findContainerNetns()
	if err != nil {
		return containerNetns{}, egressRefusal("brig cannot find the network namespace of the container network",
			egressSetupRemedy, err)
	}
	argv, _, err := ns.nftArgv()
	if err != nil {
		return containerNetns{}, egressRefusal("brig enforces a policy here with nftables", egressSetupRemedy, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), egressProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], append(argv[1:], "list", "tables")...)
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("no answer within %s", egressProbeTimeout)
		} else if msg := strings.TrimSpace(string(out)); msg != "" {
			err = fmt.Errorf("%w: %s", err, firstLines(msg, 3))
		}
		return containerNetns{}, egressRefusal(fmt.Sprintf("the probe `%s list tables` failed", strings.Join(argv, " ")),
			egressSetupRemedy, err)
	}
	return ns, nil
}

// bridgeOf reads the bridge and its address from the CNI configuration of a
// sandbox network.
func (n *nerdctl) bridgeOf(network string) (egress.Network, error) {
	out, err := exec.Command(n.bin, "network", "inspect", "--mode", "native", network).Output()
	if err != nil {
		return egress.Network{}, fmt.Errorf("could not inspect the network %s: %w", network, err)
	}
	return parseBridge(out)
}

// parseBridge reads the bridge plugin out of `nerdctl network inspect --mode
// native`.
func parseBridge(out []byte) (egress.Network, error) {
	var nets []struct {
		CNI struct {
			Plugins []struct {
				Type   string `json:"type"`
				Bridge string `json:"bridge"`
				IPAM   struct {
					Ranges [][]struct {
						Gateway string `json:"gateway"`
						Subnet  string `json:"subnet"`
					} `json:"ranges"`
				} `json:"ipam"`
			} `json:"plugins"`
		} `json:"CNI"`
	}
	if err := json.Unmarshal(out, &nets); err != nil {
		return egress.Network{}, fmt.Errorf("unreadable network inspect output: %w", err)
	}
	if len(nets) != 1 {
		return egress.Network{}, fmt.Errorf("network inspect returned %d networks", len(nets))
	}
	for _, p := range nets[0].CNI.Plugins {
		if p.Type != "bridge" {
			continue
		}
		for _, r := range p.IPAM.Ranges {
			for _, rr := range r {
				subnet, err := netip.ParsePrefix(rr.Subnet)
				if err != nil || !subnet.Addr().Is4() {
					continue
				}
				// host-local's default gateway is the first address.
				gw := subnet.Masked().Addr().Next()
				if rr.Gateway != "" {
					if gw, err = netip.ParseAddr(rr.Gateway); err != nil || !subnet.Contains(gw) {
						continue
					}
				}
				if p.Bridge == "" {
					break
				}
				return egress.Network{Bridge: p.Bridge, Gateway: gw, Subnet: subnet.Masked()}, nil
			}
		}
		return egress.Network{}, errors.New("the bridge plugin names no bridge or no IPv4 subnet")
	}
	return egress.Network{}, errors.New("the network has no bridge plugin")
}

// The files kept for a sandbox's resolver: its pid, what it was started to
// enforce, its log, and the lock that orders the boots and stops that touch
// them.
func egressDir() (string, error) {
	dir, err := gatewayDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "egress"), nil
}

func egressFile(name, ext string) (string, error) {
	dir, err := egressDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+ext), nil
}

// egressLock takes the sandbox's egress lock. The lock file stays when the
// records go: a lock file removed while another brig waits on it would let a
// third take a second lock under the same name.
func egressLock(name string) (func(), error) {
	path, err := egressFile(name, "")
	if err != nil {
		return nil, err
	}
	unlock, err := flock(path)
	if err != nil {
		return nil, fmt.Errorf("could not lock the egress records of %s: %w", name, err)
	}
	return unlock, nil
}

// egressSpec is what a sandbox's egress was started to enforce. It reuses the
// gateway's spelling, sorted, so that two orderings of one policy compare
// equal and a rule moved from allow to deny does not.
func egressSpec(e Egress) string {
	if !e.Filtered() {
		return ""
	}
	rules := []string{}
	for _, r := range e.Allow {
		rules = append(rules, "allow "+r.arg())
	}
	for _, r := range e.Deny {
		rules = append(rules, "deny "+r.arg())
	}
	slices.Sort(rules)
	return strings.Join(append([]string{"default=" + e.Default}, rules...), "\n")
}

// egressBoot is what a run needs from startEgress: the resolver the guest is
// given, and the check, once the container is up, that its bridge is where
// the rules are.
type egressBoot struct {
	resolver netip.Addr
	up       func() error
}

// egressStart starts a sandbox's egress. A variable so a test can boot
// without nft.
var egressStart = (*nerdctl).startEgress

// startEgress starts the resolver for one sandbox, which installs its table,
// before the sandbox boots. The caller holds the sandbox's egress lock and
// has seen that the sandbox is not running.
func (n *nerdctl) startEgress(name string, e Egress) (egressBoot, error) {
	if !stopEgress(name) {
		return egressBoot{}, egressRefusal("the egress resolver of this sandbox's previous boot is still running",
			"Stop that process, or detach the policy", nil)
	}
	ns, err := egressEnforces()
	if err != nil {
		return egressBoot{}, err
	}
	refuse := func(why string, err error) (egressBoot, error) {
		return egressBoot{}, egressRefusal(why, "See docs/troubleshooting.md, or detach the policy", err)
	}
	table, err := egress.TableName(name)
	if err != nil {
		return refuse("brig cannot name the sandbox's nftables table", err)
	}
	bridge, err := n.bridgeOf(sandboxNetwork(name))
	if err != nil {
		return refuse("brig cannot find the bridge of the sandbox's network", err)
	}
	upstream, err := ns.upstreams()
	if err != nil {
		return refuse("brig has no upstream resolver for the egress resolver", err)
	}
	bridge.Redirect = upstream
	nftArgv, nftBin, err := ns.nftArgv()
	if err != nil {
		return refuse("brig enforces a policy here with nftables", err)
	}
	var b [2]byte
	_, _ = rand.Read(b[:])
	if err := startResolver(name, table, bridge, bootMark(b), ns, nftBin, e); err != nil {
		// The resolver installs the table before it binds its sockets, so
		// one that failed may leave the table behind.
		_ = egress.RunNFT(context.Background(), nftArgv, egress.DeleteScript(table))
		return refuse("brig could not start the egress resolver", err)
	}
	return egressBoot{resolver: bridge.Gateway, up: func() error { return ns.requireInterface(bridge.Bridge) }}, nil
}

// bootMark is the connection mark of one boot, from two random bytes. Zero is
// what every untagged connection carries, so it becomes one. Every other value
// stays, so two boots of a sandbox share a mark once in 65535.
func bootMark(b [2]byte) uint16 {
	if mark := binary.BigEndian.Uint16(b[:]); mark != 0 {
		return mark
	}
	return 1
}

// withDNS returns the run arguments with the guest's resolver set to addr.
func withDNS(args []string, addr netip.Addr) []string {
	// args[0] is the run verb, and nerdctl reads its flags up to the image.
	return slices.Concat(args[:1], []string{"--dns", addr.String()}, args[1:])
}

// startResolver starts the resolver in the namespace and waits until it has
// installed the table and bound its sockets. The pid and the spec are
// written only once it has.
func startResolver(name, table string, bridge egress.Network, mark uint16, ns containerNetns, nftBin string, e Egress) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not find the brig executable to run the egress resolver: %w", err)
	}
	dir, err := egressDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("could not create %s: %w", dir, err)
	}
	logPath, _ := egressFile(name, ".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("could not open the egress resolver log: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	args := append([]string(nil), ns.enter...)
	args = append(args, self, egress.Verb,
		"--table", table, "--bridge", bridge.Bridge, "--listen", bridge.Gateway.String(),
		"--subnet", bridge.Subnet.String(), "--mark", strconv.Itoa(int(mark)), "--nft", nftBin)
	for _, u := range bridge.Redirect {
		args = append(args, "--upstream", netip.AddrPortFrom(u, 53).String(), "--redirect", u.String())
	}
	args = append(args, e.args()...)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	// The resolver needs nothing from brig's environment, which can hold the
	// credentials a run forwards. It runs for as long as the sandbox does.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	cmd.Dir = "/"
	// The resolver serves the sandbox after this brig exits.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not start the egress resolver: %w", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

	deadline := time.After(egressStartTimeout)
	for {
		if blob, _ := os.ReadFile(logPath); strings.Contains(string(blob), "egress resolver on ") {
			break
		}
		select {
		case <-exited:
			blob, _ := os.ReadFile(logPath)
			return fmt.Errorf("the egress resolver for %s exited: %s", name, firstLines(strings.TrimSpace(string(blob)), 3))
		case <-deadline:
			_ = cmd.Process.Kill()
			<-exited
			return fmt.Errorf("the egress resolver for %s did not start within %s; see %s", name, egressStartTimeout, logPath)
		case <-time.After(50 * time.Millisecond):
		}
	}
	pidPath, _ := egressFile(name, ".pid")
	specPath, _ := egressFile(name, ".spec")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("could not record the egress resolver: %w", err)
	}
	if err := os.WriteFile(specPath, []byte(egressSpec(e)+"\n"), 0o600); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("could not record the egress resolver: %w", err)
	}
	return nil
}

// resolverPID returns the pid of the sandbox's resolver when that process is
// still the resolver brig started for it.
func resolverPID(name string) (int, bool) {
	pidPath, err := egressFile(name, ".pid")
	if err != nil {
		return 0, false
	}
	blob, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(blob)))
	if err != nil || pid <= 1 {
		return 0, false
	}
	table, err := egress.TableName(name)
	if err != nil {
		return 0, false
	}
	argv, ok := procArgv(pid)
	if !ok || !strings.Contains(argv, egress.Verb) || !strings.Contains(argv, "--table "+table+" ") {
		return 0, false
	}
	return pid, true
}

// stopEgress stops a sandbox's resolver and removes its table and records,
// and reports whether the resolver is gone. The caller has confirmed that the
// sandbox is not running.
//
// A resolver that survives a kill keeps its records and its table. The
// records are the only handle anything has on it, and a second resolver
// could not bind its address while it runs.
func stopEgress(name string) bool {
	if pid, ok := resolverPID(name); ok {
		if !signalGateway(pid, func() bool { _, ok := resolverPID(name); return ok }) {
			return false
		}
	}
	if table, err := egress.TableName(name); err == nil && hasEgressRecord(name) {
		if ns, err := findContainerNetns(); err == nil {
			if argv, _, err := ns.nftArgv(); err == nil {
				_ = egress.RunNFT(context.Background(), argv, egress.DeleteScript(table))
			}
		}
	}
	for _, ext := range []string{".pid", ".spec", ".log"} {
		if p, err := egressFile(name, ext); err == nil {
			_ = os.Remove(p)
		}
	}
	return true
}

// hasEgressRecord reports whether a resolver was recorded for the sandbox.
func hasEgressRecord(name string) bool {
	return hasEgressFile(name, ".pid", ".spec")
}

// hasEgressFiles reports whether the sandbox has anything of an egress setup
// on disk, the log of a resolver that failed to start included.
func hasEgressFiles(name string) bool {
	return hasEgressFile(name, ".pid", ".spec", ".log")
}

func hasEgressFile(name string, exts ...string) bool {
	for _, ext := range exts {
		if p, err := egressFile(name, ext); err == nil {
			if _, err := os.Stat(p); err == nil {
				return true
			}
		}
	}
	return false
}

// dropEgress removes a sandbox's egress rules and resolver once the sandbox
// is confirmed stopped. A stop or a remove that failed can leave it running,
// and the rules are then all that filters it.
func (n *nerdctl) dropEgress(name string) {
	if !hasEgressFiles(name) {
		return
	}
	unlock, err := egressLock(name)
	if err != nil {
		return
	}
	defer unlock()
	n.dropEgressLocked(name)
}

// dropEgressLocked is dropEgress for a caller that holds the lock.
func (n *nerdctl) dropEgressLocked(name string) {
	if running, err := n.Running(name); err == nil && !running {
		stopEgress(name)
	}
}

// recordedEgress returns what the sandbox's egress was started to enforce,
// or "" when it was started with none.
func recordedEgress(name string) string {
	p, err := egressFile(name, ".spec")
	if err != nil {
		return ""
	}
	blob, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(string(blob), "\n")
}

// egressLive reports whether a sandbox's resolver is running and its table is
// installed.
func egressLive(name string) bool {
	if _, ok := resolverPID(name); !ok {
		return false
	}
	table, err := egress.TableName(name)
	if err != nil {
		return false
	}
	ns, err := findContainerNetns()
	if err != nil {
		return false
	}
	nft, err := lookSbin("nft")
	if err != nil {
		return false
	}
	_, err = ns.output(nft, "list", "table", "inet", table)
	return err == nil
}

// NetworkStale reports whether the running sandbox is under different egress
// rules than the run asks for, or under rules nothing enforces any more.
//
// The posture is not compared here. wrap records it and compares it itself.
// A sandbox asked about with no rules, and with a record of rules, reads
// stale: that is how wrap tells a sandbox a policy isolated from a shared one.
func (n *nerdctl) NetworkStale(name, _, net string, e Egress) bool {
	want := ""
	if net != "none" {
		want = egressSpec(e)
	}
	have := recordedEgress(name)
	if want != have {
		return true
	}
	// A resolver that died leaves new connections refused, and a table gone
	// with a restarted rootless containerd leaves nothing enforced. Both
	// need a boot.
	return want != "" && !egressLive(name)
}

// pruneEgress removes the resolvers, records and tables of sandboxes that are
// gone. A sandbox that died without a stop leaves all three behind.
//
// inUse was read before this runs, so a sandbox can have booted since. Each
// one is checked again under its lock before anything of it is removed.
func (n *nerdctl) pruneEgress(inUse []string) {
	live := make(map[string]bool, len(inUse))
	for _, name := range inUse {
		live[name] = true
	}
	// gone maps each sandbox name to its table when only the table was found.
	gone := map[string]string{}
	if dir, err := egressDir(); err == nil {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			name := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
			if strings.HasPrefix(name, sandboxPrefix) && !live[name] && hasEgressFiles(name) {
				gone[name] = ""
			}
		}
	}
	ns, nsErr := findContainerNetns()
	var nftArgv []string
	if nsErr == nil {
		if argv, nft, err := ns.nftArgv(); err == nil {
			nftArgv = argv
			if out, err := ns.output(nft, "list", "tables", "inet"); err == nil {
				for _, table := range orphanTables(string(out), live) {
					gone[sandboxPrefix+strings.TrimPrefix(table, egress.TablePrefix)] = table
				}
			}
		}
	}
	for name, table := range gone {
		unlock, err := egressLock(name)
		if err != nil {
			continue
		}
		if running, err := n.Running(name); err == nil && !running {
			stopEgress(name)
			if table != "" && nftArgv != nil {
				_ = egress.RunNFT(context.Background(), nftArgv, egress.DeleteScript(table))
			}
		}
		unlock()
	}
}

// orphanTables picks, from `nft list tables inet`, brig's tables whose
// sandbox is not in live.
func orphanTables(listing string, live map[string]bool) []string {
	var out []string
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != "table" || fields[1] != "inet" {
			continue
		}
		rest, ok := strings.CutPrefix(fields[2], egress.TablePrefix)
		if ok && !live[sandboxPrefix+rest] {
			out = append(out, fields[2])
		}
	}
	return out
}
