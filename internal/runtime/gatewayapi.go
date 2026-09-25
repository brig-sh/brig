package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The gateway's HTTP API, which is how a port is published on a gateway that
// is already serving guests.
//
// The alternative was --forward at startup, which the gateway has always
// taken. It cannot be used here. The shared gateway serves every sandbox on
// the shared network, so restarting it to add one sandbox's port would drop
// the network of every other -- and even for an isolated gateway the guest is
// already attached by the time anyone asks to publish.

// gatewayAPITimeout bounds a call. The gateway is a local process answering
// over a unix socket; a call that takes longer than this is one that is not
// coming back.
const gatewayAPITimeout = 5 * time.Second

// ErrNoForwardAPI is what a gateway too old to publish a port answers with. It
// is kept apart so the caller can say which half of the install is behind
// rather than quoting a 404 at someone.
var ErrNoForwardAPI = errors.New("this runtime's gateway cannot publish a port")

// gatewayAPISocket is the API socket beside a gateway's control socket. The
// gateway derives nothing here; brig passes both paths explicitly, and this is
// the one place the second is named.
//
// It is shorter than the control socket it is derived from, so the length
// check isolatedSocket makes against the QEMU socket covers this one too.
func gatewayAPISocket(controlSock string) string {
	return strings.TrimSuffix(controlSock, ".sock") + ".api"
}

// gatewayForward is one forward as the gateway's API renders it.
type gatewayForward struct {
	Protocol string `json:"protocol"`
	Local    string `json:"local"`
	Remote   string `json:"remote"`
}

// gatewayAPI calls one endpoint on a gateway's API socket.
//
// The host in the URL is a placeholder: the dialer ignores it and connects to
// the socket. Every call goes through here so the timeout and the socket
// plumbing are written once.
func gatewayAPI(sock, method, path string, body any) ([]byte, int, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}
	// The connection goes back to this transport's idle pool when the body is
	// closed, and an idle connection keeps its reader and writer goroutines
	// and its descriptor. A transport per call would leak all three in a
	// process that outlives one command: brigd drives a reconcile on every
	// sandbox it starts.
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: gatewayAPITimeout}
	var payload io.Reader
	if body != nil {
		blob, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		payload = bytes.NewReader(blob)
	}
	req, err := http.NewRequest(method, "http://gateway"+path, payload)
	if err != nil {
		return nil, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("the gateway at %s did not answer: %w", sock, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	// A gateway that predates the endpoint answers 404 for the path itself,
	// which is indistinguishable from a 404 the endpoint returns. They are
	// told apart by the method: this endpoint only answers 404 to a DELETE.
	if resp.StatusCode == http.StatusNotFound && method != http.MethodDelete {
		return out, resp.StatusCode, ErrNoForwardAPI
	}
	return out, resp.StatusCode, nil
}

// gatewayForwards is every forward a gateway is running.
func gatewayForwards(sock string) ([]gatewayForward, error) {
	body, status, err := gatewayAPI(gatewayAPISocket(sock), http.MethodGet, "/forwards", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("the gateway would not list its published ports: %s", message(body, status))
	}
	var out []gatewayForward
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("the gateway's published ports could not be read: %w", err)
	}
	return out, nil
}

// exposeForward publishes one port on a gateway, and unexposeForward takes it
// away.
//
// A conflict names what is already there. The caller turns the address into a
// sandbox; see sandboxAt.
func exposeForward(sock string, f gatewayForward) error {
	body, status, err := gatewayAPI(gatewayAPISocket(sock), http.MethodPost, "/forwards", f)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusCreated, http.StatusOK:
		return nil
	case http.StatusConflict:
		return &forwardConflict{local: f.Local, detail: message(body, status)}
	default:
		return fmt.Errorf("the gateway would not publish %s: %s", f.Local, message(body, status))
	}
}

func unexposeForward(sock string, f gatewayForward) error {
	q := url.Values{"protocol": {f.Protocol}, "local": {f.Local}}
	body, status, err := gatewayAPI(gatewayAPISocket(sock), http.MethodDelete, "/forwards?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	switch status {
	// Nothing published there is the state being asked for, so it is not a
	// failure. It is also the state a gateway that was restarted is in.
	case http.StatusOK, http.StatusNotFound:
		return nil
	default:
		return fmt.Errorf("the gateway would not withdraw %s: %s", f.Local, message(body, status))
	}
}

// forwardConflict is a host address something is already published on.
type forwardConflict struct {
	local  string
	detail string
}

func (e *forwardConflict) Error() string {
	return fmt.Sprintf("%s is already published: %s", e.local, e.detail)
}

// message is the gateway's own words, or the status when it had none. The
// gateway writes a plain-text reason on every failure, so this is usually the
// sentence a reader wants and the status is the fallback.
func message(body []byte, status int) string {
	if s := strings.TrimSpace(string(body)); s != "" {
		return s
	}
	return "HTTP " + strconv.Itoa(status)
}

// publishOn offers one guest port on the host through this gateway.
func publishOn(sock, guestIP string, p Publication) error {
	return exposeForward(sock, gatewayForward{
		Protocol: p.Proto(), Local: p.Local(), Remote: p.Remote(guestIP),
	})
}

// withdrawFrom takes one publication away again.
func withdrawFrom(sock, guestIP string, p Publication) error {
	return unexposeForward(sock, gatewayForward{
		Protocol: p.Proto(), Local: p.Local(), Remote: p.Remote(guestIP),
	})
}

// publishedOn is what this guest is offering on this gateway, read back from
// the gateway itself.
//
// A gateway serving the shared network carries other sandboxes' forwards too,
// so the guest's own address is what picks this sandbox's out. That is exact
// and needs no bookkeeping inside the gateway: a forward carries to exactly
// one guest, and brig hands out the addresses.
func publishedOn(sock, guestIP string) ([]Publication, error) {
	all, err := gatewayForwards(sock)
	if err != nil {
		return nil, err
	}
	var out []Publication
	for _, f := range all {
		host, port, err := net.SplitHostPort(f.Remote)
		if err != nil || host != guestIP {
			continue
		}
		p, err := publicationOf(f, port)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// publisherElsewhere returns the sandbox whose forward on a brig gateway other
// than sock holds p's host port, or "" when none does.
//
// The shared network has one gateway, and every isolated sandbox has its own.
// A port one of them holds is taken for all the others.
func publisherElsewhere(sock string, p Publication) string {
	var socks []string
	if shared, err := gatewaySocket(); err == nil {
		socks = append(socks, shared)
	}
	if alloc, err := isolatedNets(); err == nil {
		for name := range alloc.read() {
			if s, err := isolatedSocket(name); err == nil {
				socks = append(socks, s)
			}
		}
	}
	for _, s := range socks {
		if s == sock || !gatewayReachable(s) {
			continue
		}
		forwards, err := gatewayForwards(s)
		if err != nil {
			continue
		}
		for _, f := range forwards {
			guest, port, err := net.SplitHostPort(f.Remote)
			if err != nil {
				continue
			}
			if q, err := publicationOf(f, port); err == nil && q.Overlaps(p) {
				return sandboxAt(guest)
			}
		}
	}
	return ""
}

// publicationOf turns a gateway forward back into the publication that asked
// for it.
func publicationOf(f gatewayForward, guestPort string) (Publication, error) {
	addr, hostPort, err := net.SplitHostPort(f.Local)
	if err != nil {
		return Publication{}, err
	}
	host, err := strconv.Atoi(hostPort)
	if err != nil {
		return Publication{}, err
	}
	guest, err := strconv.Atoi(guestPort)
	if err != nil {
		return Publication{}, err
	}
	p := Publication{HostAddr: addr, HostPort: host, GuestPort: guest}
	if f.Protocol != "tcp" {
		p.Protocol = f.Protocol
	}
	if p.HostAddr == loopback {
		p.HostAddr = ""
	}
	return p, nil
}

// reconcilePublications makes the gateway serve exactly the ports recorded for
// this sandbox, and nothing else of this sandbox's.
//
// It runs at boot, which is the only place the two can have drifted: a gateway
// that was restarted has none of them, and a publication withdrawn while the
// sandbox was stopped was recorded and never reached a gateway. Forwards
// belonging to other guests on a shared gateway are left alone.
//
// A publication that cannot be installed fails the boot. The alternative is a
// sandbox that comes up with the envelope naming a port nothing is listening
// on, and the person in front of it debugging their dev server.
func reconcilePublications(sock, guestIP string, want []Publication) error {
	have, err := publishedOn(sock, guestIP)
	if err != nil {
		if len(want) == 0 {
			// Nothing to publish, so there is no disagreement to correct and
			// no reason for this to have an opinion about the gateway at all.
			//
			// Whatever the error was. A gateway too old for the endpoint
			// answers 404, and one that was started without --api -- a stub, a
			// gateway brig did not start, anything predating this -- has no
			// socket to dial and fails to connect. Neither is a reason to
			// refuse a boot that publishes nothing, and reading only the first
			// of them failed every such boot behind a gateway of the second
			// kind.
			return nil
		}
		return unpublishable(err)
	}
	for _, p := range have {
		if !contains(want, p) {
			if err := withdrawFrom(sock, guestIP, p); err != nil {
				return err
			}
		}
	}
	for _, p := range want {
		if !contains(have, p) {
			if err := publishOn(sock, guestIP, p); err != nil {
				return publishError(sock, err, p)
			}
		}
	}
	return nil
}

// unpublishable names a gateway that cannot be asked about forwards at all,
// for the run that wanted one published.
//
// Two shapes reach here, and they have different fixes. A gateway that answers
// 404 predates the /forwards endpoint, so the runtime needs an upgrade. A
// gateway with no API socket to dial was started without --api, by a brig from
// before ports could be published. It keeps running while any sandbox uses it,
// so it is replaced only once they have all stopped.
func unpublishable(err error) error {
	switch {
	case errors.Is(err, ErrNoForwardAPI):
		return fmt.Errorf("the network gateway serving this sandbox cannot publish a port "+
			"(%w). Upgrade the runtime, or withdraw the ports with "+
			"`brig network unpublish`", err)
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("the network gateway serving this sandbox was started by an older "+
			"brig, without the socket a port is published through (%w). It is replaced "+
			"once every sandbox on it has stopped: `brig ls` lists them, and `brig stop` "+
			"stops one. Or withdraw the ports with `brig network unpublish`", err)
	}
	return err
}

func contains(set []Publication, p Publication) bool {
	for _, q := range set {
		if q.Same(p) && q.GuestPort == p.GuestPort {
			return true
		}
	}
	return false
}

// sandboxGateway is the gateway serving a sandbox and the address that
// sandbox has on it.
//
// Which gateway a sandbox is behind is not recorded anywhere, and does not
// need to be: an isolated sandbox has a gateway named after it, so a socket
// answering under that name is the answer, and every other sandbox with an
// address is on the shared one. Reading it this way rather than from the
// posture a command resolved means `brig network publish` finds the gateway
// the sandbox is actually attached to, which is the one that has to be told.
func sandboxGateway(name string) (sock, guestIP string, err error) {
	if sock, err := isolatedSocket(name); err == nil && gatewayReachable(sock) {
		index, ok := lookupSandboxNet(name)
		if !ok {
			return "", "", fmt.Errorf("the gateway serving %s is running, but brig has no record "+
				"of the address that sandbox has on it. Restart it with `brig run`", name)
		}
		return sock, addrOf(sandboxCIDR(index)), nil
	}
	alloc, err := sharedIPs()
	if err != nil {
		return "", "", err
	}
	host, ok := alloc.lookup(name)
	if !ok {
		return "", "", fmt.Errorf("%s is not on a network brig owns, so brig cannot publish a "+
			"port from it. That is every sandbox on the vz backend, and any sandbox run "+
			"with --network offline", name)
	}
	sock, err = gatewaySocket()
	if err != nil {
		return "", "", err
	}
	if !gatewayReachable(sock) {
		return "", "", fmt.Errorf("the shared network gateway is not running, so there is "+
			"nothing to publish %s's port on. Start the sandbox with `brig run`", name)
	}
	return sock, addrOf(sharedCIDR(host)), nil
}

// addrOf is the address half of a CIDR. Both allocators hand out the guest's
// address with its prefix, which is the form hull's --gateway-cidr takes; a
// forward names the address alone.
func addrOf(cidr string) string {
	addr, _, ok := strings.Cut(cidr, "/")
	if !ok {
		return cidr
	}
	return addr
}

// conflictGuest is the guest address out of the gateway's conflict message,
// which ends with the forward already installed. An address this cannot find
// leaves the error naming the port alone, which is still the fact the user
// has to act on.
func conflictGuest(detail string) string {
	fields := strings.Fields(detail)
	if len(fields) == 0 {
		return ""
	}
	host, _, err := net.SplitHostPort(fields[len(fields)-1])
	if err != nil {
		return ""
	}
	return host
}
