package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The hvi backend brings no network of its own. Its built-in stack answers
// ARP and ICMP and resolves DNS through the host, but TCP is observed and
// never forwarded -- a guest that looks online right up until its first
// connection, which is the worst shape a network failure can take. Real
// egress comes from hull's user-mode gateway (gvisor netstack), so on hvi
// brig owns that gateway's lifecycle rather than leaving it to the user.
//
// There are two kinds of gateway here, one per posture:
//
//   - shared: one gateway for the host, serving every sandbox that asked for
//     the shared network. What brig has always done, unchanged.
//   - isolated: a gateway per sandbox, each on a /30 of its own out of the
//     same space. No other sandbox is on that network, whatever the backend
//     would have done with a shared one.
//
// isolated is also what a policy needs. An egress policy belongs to a gateway:
// it covers every member of that gateway's network and no rule can name one
// member, so a sandbox that is to answer to rules of its own needs a gateway
// of its own. hull's gateway documentation says exactly that. A sandbox
// carrying a policy is therefore isolated whether or not it asked to be.
//
// An isolated gateway costs one process per running sandbox, measured at
// 28.7 MB resident with no member attached (hull 0.1.0-rc21, arm64). Against
// a guest's gigabytes that is the cheap half of a sandbox, and it is only
// spent while the sandbox runs: `brig stop` and `brig rm` take it with them.
// A sandbox that dies without either leaves its gateway behind, which is
// bounded and self-clearing rather than unbounded: the socket is named after
// the sandbox, so the next run of that sandbox reuses that gateway or replaces
// it, and a sandbox that never runs again has its gateway stopped by the
// `brig rm` that removes it.
//
// vz needs none of this: it gets NAT from vmnet directly.

const (
	// gatewayReadyTimeout bounds the wait for a gateway we just started. It
	// is a local process opening two unix sockets, so this is generous.
	gatewayReadyTimeout = 10 * time.Second
	gatewayPollInterval = 100 * time.Millisecond

	// gatewayStopTimeout bounds the wait for one to go away. Nothing is
	// flushed on the way out, so this only covers process teardown.
	gatewayStopTimeout = 3 * time.Second

	// The virtual network the shared gateway serves. brig passes it explicitly
	// rather than leaning on hull's default: brig also hands out the addresses
	// on it, and an allocator working from a different subnet than the gateway
	// is a failure nobody would find quickly. formatGatewayCIDR derives guest
	// addresses from this rather than repeating it, so the two cannot drift.
	//
	// 198.18.0.0/15 is the range RFC 2544 reserves for network benchmarking.
	// It is never routed on the public internet and almost nothing claims it,
	// which is the property that matters here: a sandbox network that collides
	// with something real takes the workspace's own traffic with it. The
	// obvious candidates all lose. 10.0.0.0/8 is where corporate VPNs and
	// cloud VPCs live, and the previous 10.87.0.0/24 sat one octet from
	// Podman's default 10.88.0.0/16. 172.16.0.0/12 is Docker's, which walks up
	// from 172.17 as networks are created. 192.168.0.0/16 is home routers, and
	// on macOS vmnet already uses 192.168.64.0/24. 100.64.0.0/10 looks unused
	// until you notice Tailscale lives there.
	//
	// The sibling 198.19.0.0/16 is left alone: OrbStack uses it. The isolated
	// networks are the adjacent /24; see isolatedSpace.
	gatewaySubnet = "198.18.0.0/24"
	gatewayAddr   = "198.18.0.1"
	gatewayPrefix = 24

	// Guests start at .2. The gateway is the network address plus one, which
	// is how hull derives it from whatever --gateway-cidr it is given.
	firstGuestHost = 2
	lastGuestHost  = 254
)

// gatewayDir is where the sockets, the logs and the network map live.
//
// BRIG_GATEWAY_SOCK named the single shared socket. It still names it, because
// that gateway still exists; the isolated ones are siblings beside it, and
// BRIG_GATEWAY_DIR moves the lot.
func gatewayDir() (string, error) {
	if d := os.Getenv("BRIG_GATEWAY_DIR"); d != "" {
		return d, nil
	}
	if s := os.Getenv("BRIG_GATEWAY_SOCK"); s != "" {
		return filepath.Dir(s), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory to place the gateway sockets in: %w", err)
	}
	return filepath.Join(home, ".brig"), nil
}

// gatewaySocket is the control socket of the shared gateway.
//
// One gateway serves every sandbox that asked for the shared network. That is
// not only cheaper than one per VM: members of a single virtual network can
// reach each other, which is what makes two sandboxes able to talk at all.
//
// The network it serves is part of the name. ensureGateway reuses whatever is
// already listening here without asking what it serves, so a gateway left over
// from a different subnet would be reused for guests that are not on it: brig
// would hand out an address the process on the other end does not route, and
// the sandbox would come up with no network and nothing pointing at the cause.
// A different network is a different socket, which also lets sandboxes from
// before a subnet move keep the gateway they were booted against until they
// are removed.
func gatewaySocket() (string, error) {
	if s := os.Getenv("BRIG_GATEWAY_SOCK"); s != "" {
		return s, nil
	}
	dir, err := gatewayDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "gateway-"+socketTag(gatewaySubnet)+".sock"), nil
}

// GatewayLogPath is where the shared gateway's own output is written, beside
// its socket, and IsolatedGatewayLogPath is the same for the gateway serving
// one sandbox alone. ensureGateway and ensureIsolatedGateway open them, and
// `brig logs --gateway` points at them: the gateway logs a boot's network
// failure to a file no command named until now, so a network that never came up
// was invisible unless you already knew the file was there. Both are derived
// from the socket by the same rule startGateway uses, so the writer and the
// reader cannot drift onto two paths.
//
// The shared log stays put; an isolated one does not. clearGatewayRecord
// removes it with the gateway it belongs to, once that gateway is confirmed
// stopped, which is what keeps ~/.brig from growing a file for every sandbox
// ever isolated. So an isolated log is there while its gateway runs -- and
// after one that failed to start, which is the case a reader most needs -- and
// gone once the sandbox is stopped.
func GatewayLogPath() (string, error) {
	sock, err := gatewaySocket()
	if err != nil {
		return "", err
	}
	return gatewayLogPath(sock), nil
}

func IsolatedGatewayLogPath(name string) (string, error) {
	sock, err := isolatedSocket(name)
	if err != nil {
		return "", err
	}
	return gatewayLogPath(sock), nil
}

// socketTag turns a subnet into something that can sit in a filename.
func socketTag(subnet string) string {
	return strings.NewReplacer("/", "_", ".", "-").Replace(subnet)
}

// sockaddrUnMax is what a unix socket path fits in: 104 bytes on macOS,
// counting the trailing NUL. Exceeding it fails at bind, and what the user
// would see is a gateway that never came up after a ten second wait, with
// nothing about the length in the message.
//
// The shared socket never had to think about this, because its name is fixed.
// One named after the sandbox does, because the name is not brig's to bound.
const sockaddrUnMax = 103

// isolatedSocket is the control socket of the gateway serving one sandbox.
//
// Named after the sandbox rather than placed in a directory of its own: the
// path is charged against the limit above, and a directory level costs more of
// that budget than it buys in tidiness.
//
// A name too long to fit is hashed rather than refused. The path stays
// readable for every name that fits, a name that does not still gets a path of
// its own, and it is the same path on every call so the sandbox finds the
// gateway it had.
//
// The subnet is not in the name, as it is for the shared socket. The hazard it
// guards against there is real and unchanged, but an isolated gateway is
// checked against a record of everything it was started to serve -- its
// network and its rules both -- and a name can only carry one of the two. See
// gatewaySpec.
func isolatedSocket(name string) (string, error) {
	dir, err := gatewayDir()
	if err != nil {
		return "", err
	}
	sock := filepath.Join(dir, "sandbox-"+name+".sock")
	if len(qemuGatewaySocket(sock)) <= sockaddrUnMax {
		return sock, nil
	}
	sum := sha256.Sum256([]byte(name))
	sock = filepath.Join(dir, "sandbox-"+hex.EncodeToString(sum[:4])+".sock")
	if len(qemuGatewaySocket(sock)) > sockaddrUnMax {
		return "", fmt.Errorf("the gateway directory %s is too deep for a unix socket: "+
			"a path there exceeds the %d bytes one fits in. Set BRIG_GATEWAY_DIR to "+
			"somewhere shorter", dir, sockaddrUnMax)
	}
	return sock, nil
}

// qemuGatewaySocket mirrors hull's own derivation. hull takes the control
// socket on --gateway-sock and pre-flights this path, so the two have to
// agree; keeping the convention in one function on each side is the best
// available version of that.
func qemuGatewaySocket(controlSock string) string { return controlSock + ".qemu" }

// The files kept beside a socket. The pid and the spec belong to an isolated
// gateway -- which process is serving it, and what that process was started to
// serve; the log belongs to every gateway.
//
// A log per socket rather than one for the lot. There is a gateway per
// isolated sandbox now, and a single file would interleave them, which costs
// exactly what a log is for: the run that failed is the one you cannot pick
// out of it. It is also what the "did not come up" error points at, and
// pointing every sandbox at the same file would send a reader to the wrong
// gateway's output.
func gatewayPIDPath(sock string) string  { return strings.TrimSuffix(sock, ".sock") + ".pid" }
func gatewaySpecPath(sock string) string { return strings.TrimSuffix(sock, ".sock") + ".spec" }
func gatewayLogPath(sock string) string  { return strings.TrimSuffix(sock, ".sock") + ".log" }

// ensureGateway returns the control socket of the shared gateway, starting one
// if nothing answers on it.
//
// Readiness is judged on the QEMU stream socket rather than the control
// socket, because that is the one hull dials before boot and the one hvi
// shuttles frames over. A gateway whose control socket exists but whose
// stream socket does not is not yet usable.
func ensureGateway(bin string) (string, error) {
	sock, err := gatewaySocket()
	if err != nil {
		return "", err
	}
	if gatewayReachable(sock) {
		return sock, nil
	}
	return startGateway(bin, sock, gatewaySubnet, gatewayAddr, Egress{}, "")
}

// ensureIsolatedGateway returns the control socket of the gateway serving this
// sandbox alone, starting one if nothing answers on it.
//
// A gateway already running to serve something else is replaced rather than
// reused. Its network and its rules are both read once, when it starts, so a
// running one cannot be told a new rule or moved to another subnet -- and
// reusing it would enforce the policy as it stood when the sandbox last
// booted, or route guests on a network the process on the other end does not
// serve. Restarting is safe here because this runs before the guest attaches.
func ensureIsolatedGateway(bin, name string, index int, policy Egress) (string, error) {
	sock, err := isolatedSocket(name)
	if err != nil {
		return "", err
	}
	want := gatewaySpec(index, policy)
	if gatewayReachable(sock) && recordedSpec(sock) == want {
		return sock, nil
	}
	// Below the reuse check, so the common path -- the same sandbox restarted
	// under the same policy -- does not pay for a probe process on every boot.
	if policy.Filtered() {
		if err := gatewayEnforces(bin); err != nil {
			return "", err
		}
	}
	// A gateway serving something else has to be gone before the replacement
	// starts, and gone is not the same as asked to go. shutDownGateway is best
	// effort: the record can be missing, the pid can belong to something else,
	// hull can outlive the grace period. Any of those leaves the old process
	// listening -- and startGateway judges success on the socket answering, so
	// the old one would satisfy it while the new one died unseen on "socket is
	// in use". The boot would then run under the previous rules with the new
	// ones recorded beside it, which NetworkStale reads as current, so it would
	// never be corrected.
	if gatewayReachable(sock) {
		shutDownGateway(name)
		if gatewayReachable(sock) {
			return "", fmt.Errorf("the gateway serving %s is still running and could not be "+
				"stopped, so it cannot be replaced with one that serves the network and rules "+
				"this run asks for. Stop the sandbox with `brig stop %s`, or kill the process "+
				"holding %s", name, name, qemuGatewaySocket(sock))
		}
	}
	return startGateway(bin, sock, sandboxSubnet(index), sandboxGatewayIP(index), policy, want)
}

// startGateway runs one gateway and waits for it to answer.
//
// spec is what it was started to serve, recorded beside the socket for an
// isolated gateway and empty for the shared one -- which is never replaced,
// because its network is in its name and it carries no rules.
func startGateway(bin, sock, subnet, gatewayIP string, policy Egress, spec string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return "", fmt.Errorf("could not create the gateway directory: %w", err)
	}

	// A stale socket file left by a crashed gateway is hull's problem, not
	// ours: it claims the path and removes a file nothing is listening on.
	logPath := gatewayLogPath(sock)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", fmt.Errorf("could not open the gateway log: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	args := []string{"network-gateway",
		"--socket", sock,
		"--qemu-socket", qemuGatewaySocket(sock),
		// Explicit, though the shared pair are hull's defaults: brig hands out
		// the addresses on this network, so the two must agree by construction
		// rather than by both happening to default the same way.
		"--subnet", subnet,
		"--gateway-ip", gatewayIP,
	}
	args = append(args, policy.args()...)

	cmd := exec.Command(bin, args...)
	cmd.Env = mergeEnv(telemetryEnv(false))
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// The gateway outlives this brig invocation: it serves a sandbox that is
	// still running when we exit. Put it in its own session so it does not
	// take our terminal's signals with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// A start failure is not reported here. Two brig processes racing to boot
	// the first sandbox will both find no gateway and both try to start one;
	// the loser exits with "socket is in use by a running gateway", which is
	// success from where we stand. What matters is whether a gateway is
	// reachable when the wait is over, so that is the only thing checked.
	pid := 0
	if err := cmd.Start(); err == nil {
		pid = cmd.Process.Pid
		go func() { _ = cmd.Wait() }()
	}

	deadline := time.Now().Add(gatewayReadyTimeout)
	for time.Now().Before(deadline) {
		if gatewayReachable(sock) {
			// Recorded only once something is answering, and only for the
			// process that is answering: a record written before the wait
			// would claim a gateway that never came up, and one written for a
			// pid that lost the race would name a process that has exited.
			if spec != "" && pid != 0 && ownsGateway(pid, sock) {
				writeGatewayRecord(sock, pid, spec)
			}
			return sock, nil
		}
		time.Sleep(gatewayPollInterval)
	}
	return "", fmt.Errorf("the network gateway did not come up at %s within %s; see %s",
		sock, gatewayReadyTimeout, logPath)
}

// gatewayEnforces checks that this runtime's gateway takes egress rules at
// all, and is called only when a policy was asked for.
//
// The flags arrived after hull 0.1.0-rc21, so a brig new enough to enforce a
// policy can meet a hull that cannot. Without the check the rules reach a
// gateway that rejects the flag it does not know, and what the user sees is a
// sandbox whose network did not come up, with the real reason in a log file
// they have no reason to open. Asked of the binary rather than derived from
// its version string: what matters is whether this hull takes the flag.
func gatewayEnforces(bin string) error {
	out, err := exec.Command(bin, "network-gateway", "--help").CombinedOutput()
	if err != nil {
		// Nothing is concluded from a failed probe. If this runtime is broken
		// enough not to print its own help, the boot below will say so with
		// far better context than a guess made here.
		return nil
	}
	if strings.Contains(string(out), "--egress-default") {
		return nil
	}
	return fmt.Errorf("a policy applies to this sandbox, but %s cannot enforce one "+
		"(its network-gateway has no --egress-default). Upgrade the runtime, or detach "+
		"the policy -- brig will not boot a sandbox that reports a policy nothing "+
		"enforces", bin)
}

// args is the policy as the gateway's command line takes it. Empty when no
// filtering was asked for, which leaves the gateway's own default: no filter
// at all rather than an empty allowlist.
//
// This is the one place a rule is turned into the gateway's wire spelling, so
// a rule class added to the document has one place to reach the enforcer
// through rather than several that can disagree.
func (e Egress) args() []string {
	if !e.Filtered() {
		return nil
	}
	args := []string{"--egress-default", e.Default}
	for _, r := range e.Allow {
		args = append(args, "--egress-allow", r.arg())
	}
	for _, r := range e.Deny {
		args = append(args, "--egress-deny", r.arg())
	}
	return args
}

func (r Rule) arg() string {
	if r.CIDR != "" {
		return "cidr=" + r.CIDR
	}
	return "host=" + r.Host
}

// gatewaySpec is everything a running gateway was started to serve: the
// network, and the rules on it. An isolated gateway is reused only when this
// matches what is being asked for now.
//
// The rules are sorted, so that reordering two allow lines in a policy
// document is not treated as a different policy. They are prefixed per list,
// so that moving a rule from allow to deny is.
func gatewaySpec(index int, e Egress) string {
	parts := []string{"subnet=" + sandboxSubnet(index), "gateway=" + sandboxGatewayIP(index)}
	if !e.Filtered() {
		return strings.Join(append(parts, "unfiltered"), "\n")
	}
	rules := []string{"default=" + e.Default}
	for _, r := range e.Allow {
		rules = append(rules, "allow "+r.arg())
	}
	for _, r := range e.Deny {
		rules = append(rules, "deny "+r.arg())
	}
	slices.Sort(rules[1:])
	return strings.Join(append(parts, rules...), "\n")
}

// writeGatewayRecord records which process serves this socket and what it was
// started to serve, so a later brig can tell a gateway it may reuse from one
// it must replace, and can stop the right process.
//
// Best effort. Losing the record costs a gateway that is replaced when it need
// not have been, or one left running after its sandbox is gone -- both
// recoverable, neither worth failing a boot over.
func writeGatewayRecord(sock string, pid int, spec string) {
	_ = os.WriteFile(gatewayPIDPath(sock), []byte(strconv.Itoa(pid)+"\n"), 0o600)
	_ = os.WriteFile(gatewaySpecPath(sock), []byte(spec+"\n"), 0o600)
}

// recordedSpec is what the running gateway was started to serve, or the empty
// string when there is no record.
//
// An unrecorded gateway never matches, so it is replaced rather than trusted.
// That is the safe direction: a gateway nobody can say what it serves is one
// nobody can say is the right one.
func recordedSpec(sock string) string {
	blob, err := os.ReadFile(gatewaySpecPath(sock))
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(string(blob), "\n")
}

// shutDownGateway stops the isolated gateway serving a sandbox that has gone
// away, and clears what was recorded about it.
//
// Best effort, like releasing the network: a gateway left behind holds a
// socket and 28.7 MB, but nothing about stopping or removing the sandbox
// depends on it, and there is no answer to "the kill failed" worth
// interrupting a `brig rm` with. The shared gateway is never stopped here: it
// serves sandboxes this one knows nothing about.
func shutDownGateway(name string) {
	sock, err := isolatedSocket(name)
	if err != nil {
		return
	}
	stopGatewayAt(sock)
}

// stopGatewayAt is shutDownGateway once the socket is known, so the pruner --
// which works from the records on disk rather than from a sandbox name -- does
// not have to repeat it.
// Returns whether the gateway is gone, so a caller that must replace one can
// refuse rather than start a second on the same socket.
//
// The record is removed only once the process is confirmed gone. Removing it
// unconditionally was worse than leaving it: PruneNetworks sweeps by the .pid
// files, so a record deleted while its gateway still ran put that gateway
// beyond the reach of `brig reset`, of a later `brig stop`, and of this
// function -- holding a socket and 28.7 MB until the login session ended, with
// nothing left on disk to find it by.
//
// SIGTERM, then SIGKILL. The first is what a gateway should need; the second is
// what makes "gone" a fact rather than a request, and this process has no state
// to flush that would make killing it costly.
func stopGatewayAt(sock string) bool {
	pid, ok := gatewayPID(sock)
	if !ok || !ownsGateway(pid, sock) {
		// Nothing of ours is running: either it never was, or the pid now
		// belongs to something else, which is not brig's to signal. The record
		// is stale either way, and keeping it would have every later sweep
		// re-examine a process it must not touch.
		clearGatewayRecord(sock)
		return !gatewayReachable(sock)
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		_ = syscall.Kill(pid, sig)
		deadline := time.Now().Add(gatewayStopTimeout)
		for time.Now().Before(deadline) {
			if !ownsGateway(pid, sock) {
				clearGatewayRecord(sock)
				return true
			}
			time.Sleep(gatewayPollInterval)
		}
	}
	// Still there after a kill. Leave the record: it is the only handle
	// anything has on this process.
	return false
}

// clearGatewayRecord drops what was written beside a socket, once there is no
// gateway of ours behind it. The log goes too -- it is this gateway's, and
// nothing else will ever read it -- which is what keeps ~/.brig from growing a
// file for every sandbox that was ever isolated.
func clearGatewayRecord(sock string) {
	_ = os.Remove(gatewayPIDPath(sock))
	_ = os.Remove(gatewaySpecPath(sock))
	_ = os.Remove(gatewayLogPath(sock))
}

func gatewayPID(sock string) (int, bool) {
	blob, err := os.ReadFile(gatewayPIDPath(sock))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(blob)))
	if err != nil || pid <= 1 {
		return 0, false
	}
	return pid, true
}

// ownsGateway reports whether a process is the gateway that was recorded for
// this socket.
//
// The recorded pid alone is not enough to kill on. A gateway that died takes
// its pid back into circulation, and the process holding it next is one brig
// has no business signalling. The argv is what settles it: this gateway was
// started with the socket path on its command line, and nothing else on the
// host has a reason to carry that string.
func ownsGateway(pid int, sock string) bool {
	out, err := exec.Command("ps", "-ww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	argv := string(out)
	return strings.Contains(argv, "network-gateway") && strings.Contains(argv, sock)
}

// gatewayReachable reports whether a gateway is accepting members.
//
// Connecting and closing is exactly hull's own pre-flight, so it costs the
// gateway nothing it does not already handle.
func gatewayReachable(controlSock string) bool {
	conn, err := net.DialTimeout("unix", qemuGatewaySocket(controlSock), 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
