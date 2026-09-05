package runtime

import (
	"path/filepath"
	"strings"
	"testing"
)

// The socket a gateway is reached on names the network it serves, because
// ensureGateway reuses whatever is already listening there without asking what
// it is serving. Moving the subnet while a gateway from the old one is still
// running would otherwise reuse it: brig would hand new guests an address on
// the new network while the process on the other end still routed the old one,
// and the sandbox would come up with no working network and nothing to point
// at. A subnet in the name means a different network is a different socket.
func TestGatewaySocketNamesTheSubnet(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("BRIG_GATEWAY_SOCK", "")

	sock, err := gatewaySocket()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filepath.Base(sock), socketTag(gatewaySubnet)) {
		t.Errorf("the socket name does not carry the subnet %s: %s", gatewaySubnet, sock)
	}
	// The property that matters: a different network is a different socket, so
	// a gateway serving the old one is never picked up for guests on the new.
	if socketTag("10.87.0.0/24") == socketTag(gatewaySubnet) {
		t.Error("two different subnets produce the same socket name")
	}
	// And the tag has to survive being a filename.
	if strings.ContainsAny(socketTag(gatewaySubnet), "/") {
		t.Errorf("the socket tag is not filename-safe: %s", socketTag(gatewaySubnet))
	}
	// An explicit override still wins, unchanged: it is how a test, or someone
	// running two brigs, points at a gateway of their own.
	t.Setenv("BRIG_GATEWAY_SOCK", filepath.Join(dir, "mine.sock"))
	if sock, err = gatewaySocket(); err != nil || filepath.Base(sock) != "mine.sock" {
		t.Errorf("BRIG_GATEWAY_SOCK was not honoured: %s (%v)", sock, err)
	}
}
