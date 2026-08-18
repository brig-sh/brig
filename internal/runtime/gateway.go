package runtime

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
// vz needs none of this: it gets NAT from vmnet directly.

const (
	// gatewayReadyTimeout bounds the wait for a gateway we just started. It
	// is a local process opening two unix sockets, so this is generous.
	gatewayReadyTimeout = 10 * time.Second
	gatewayPollInterval = 100 * time.Millisecond

	// The virtual network the gateway serves. hull defaults to exactly this,
	// but brig passes it explicitly: brig also hands out the addresses on it,
	// and an allocator working from a different subnet than the gateway is a
	// failure nobody would find quickly.
	gatewaySubnet = "10.87.0.0/24"
	gatewayAddr   = "10.87.0.1"
	gatewayPrefix = 24

	// Guests start at .2. The gateway is the network address plus one, which
	// is how hull derives it from whatever --gateway-cidr it is given.
	firstGuestHost = 2
	lastGuestHost  = 254
)

// gatewaySocket is the control socket of the shared gateway.
//
// One gateway serves every sandbox on the host. That is not only cheaper than
// one per VM: members of a single virtual network can reach each other, which
// is what makes two sandboxes able to talk at all.
func gatewaySocket() (string, error) {
	if s := os.Getenv("BRIG_GATEWAY_SOCK"); s != "" {
		return s, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory to place the gateway socket in: %w", err)
	}
	return filepath.Join(home, ".brig", "gateway.sock"), nil
}

// qemuGatewaySocket mirrors hull's own derivation. hull takes the control
// socket on --gateway-sock and pre-flights this path, so the two have to
// agree; keeping the convention in one function on each side is the best
// available version of that.
func qemuGatewaySocket(controlSock string) string { return controlSock + ".qemu" }

// ensureGateway returns the control socket of a running gateway, starting one
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
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return "", fmt.Errorf("could not create the gateway directory: %w", err)
	}

	// A stale socket file left by a crashed gateway is hull's problem, not
	// ours: it claims the path and removes a file nothing is listening on.
	logPath := filepath.Join(filepath.Dir(sock), "gateway.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", fmt.Errorf("could not open the gateway log: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	cmd := exec.Command(bin, "network-gateway",
		"--socket", sock,
		"--qemu-socket", qemuGatewaySocket(sock),
		// Explicit, though these are hull's defaults: brig hands out the
		// addresses on this network, so the two must agree by construction
		// rather than by both happening to default the same way.
		"--subnet", gatewaySubnet,
		"--gateway-ip", gatewayAddr,
	)
	cmd.Env = mergeEnv(telemetryEnv(false))
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// The gateway outlives this brig invocation: it serves sandboxes that are
	// still running when we exit. Put it in its own session so it does not
	// take our terminal's signals with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// A start failure is not reported here. Two brig processes racing to boot
	// the first sandbox will both find no gateway and both try to start one;
	// the loser exits with "socket is in use by a running gateway", which is
	// success from where we stand. What matters is whether a gateway is
	// reachable when the wait is over, so that is the only thing checked.
	if err := cmd.Start(); err == nil {
		go func() { _ = cmd.Wait() }()
	}

	deadline := time.Now().Add(gatewayReadyTimeout)
	for time.Now().Before(deadline) {
		if gatewayReachable(sock) {
			return sock, nil
		}
		time.Sleep(gatewayPollInterval)
	}
	return "", fmt.Errorf("the network gateway did not come up at %s within %s; see %s",
		sock, gatewayReadyTimeout, logPath)
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
