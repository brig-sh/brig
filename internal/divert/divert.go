// Package divert hands a guest's TCP connections to brig's injector.
//
// A diverted destination is a host and a port, such as brig.internal's
// address on port 443. A guest connection to it must reach the injector
// (cmd/brig-broker), which serves it. The connection must never be routed to
// the destination directly.
//
// On nerdctl, an nftables rule redirects such a connection to a relay on the
// sandbox's bridge address. The relay reads where the connection was going,
// checks that it is a diverted destination, and copies the bytes to the
// injector's unix socket after a one-line preamble. It never reads or logs
// the payload. internal/egress installs the rule and runs the relay in its
// resolver. A destination named by host is diverted at the addresses the
// resolver's answers to the guest give it.
//
// The preamble and the socket are the divert contract, shared with hull's
// gateway on macOS. See Preamble.
package divert

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// MaxAddrs is the most addresses one route diverts. The table's set has the
// same size, so an answer that would overflow it fails the query.
const MaxAddrs = 4096

// Route is one diverted destination: a host by name, or an IPv4 address, and
// a TCP port.
type Route struct {
	// Host is the host name in lower case with no trailing dot, or an IPv4
	// address.
	Host string
	// Port is the TCP port.
	Port uint16
}

// String returns the route as host:port.
func (r Route) String() string {
	return net.JoinHostPort(r.Host, strconv.Itoa(int(r.Port)))
}

// ParseRoute reads a route from host:port.
//
// The sandbox network carries no IPv6, so an IPv6 address is refused. Port 53
// is refused too, since the table sends the guest's DNS to brig's resolver.
func ParseRoute(s string) (Route, error) {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return Route{}, fmt.Errorf("divert %q is not host:port: %w", s, err)
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil || p == 0 {
		return Route{}, fmt.Errorf("divert %q: %q is not a TCP port", s, port)
	}
	if p == 53 {
		return Route{}, fmt.Errorf("divert %q: port 53 carries the guest's DNS, which brig answers", s)
	}
	if a, err := netip.ParseAddr(host); err == nil {
		a = a.Unmap()
		if !a.Is4() {
			return Route{}, fmt.Errorf("divert %q: the sandbox network carries no IPv6, so brig cannot divert it", s)
		}
		return Route{Host: a.String(), Port: uint16(p)}, nil
	}
	name := strings.TrimSuffix(strings.ToLower(host), ".")
	if !Hostname(name) {
		return Route{}, fmt.Errorf("divert %q: %q is not a host name", s, host)
	}
	return Route{Host: name, Port: uint16(p)}, nil
}

// Hostname returns whether name is a plain DNS host name.
func Hostname(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for _, c := range []byte(label) {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// Set holds the addresses each route diverts. The relay checks a connection
// against it, and the resolver adds to it before the guest gets an answer.
//
// Nothing is ever removed: an address that expired while the guest still
// used it would be routed directly. An address a diverted host no longer
// uses stays in the set, and its connections still go to the injector, which
// serves them or closes them.
type Set struct {
	routes []Route
	mu     sync.RWMutex
	addrs  []map[netip.Addr]struct{}
}

// NewSet returns a set for routes. A route to an IPv4 address starts with
// that address.
func NewSet(routes []Route) *Set {
	s := &Set{routes: slices.Clone(routes), addrs: make([]map[netip.Addr]struct{}, len(routes))}
	for i, r := range routes {
		s.addrs[i] = map[netip.Addr]struct{}{}
		if a, err := netip.ParseAddr(r.Host); err == nil && a.Is4() {
			s.addrs[i][a] = struct{}{}
		}
	}
	return s
}

// Routes returns the routes of the set, in order.
func (s *Set) Routes() []Route { return slices.Clone(s.routes) }

// Lookup returns the indexes of the routes whose host is name.
func (s *Set) Lookup(name string) []int {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	var out []int
	for i, r := range s.routes {
		if r.Host == name {
			out = append(out, i)
		}
	}
	return out
}

// Add adds the IPv4 addresses of addrs to route i and returns them, each
// once. It adds none and fails when the route would hold more than MaxAddrs.
func (s *Set) Add(i int, addrs []netip.Addr) ([]netip.Addr, error) {
	if i < 0 || i >= len(s.routes) {
		return nil, fmt.Errorf("no divert route %d", i)
	}
	var v4 []netip.Addr
	for _, a := range addrs {
		if a = a.Unmap(); a.Is4() && !slices.Contains(v4, a) {
			v4 = append(v4, a)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fresh := 0
	for _, a := range v4 {
		if _, ok := s.addrs[i][a]; !ok {
			fresh++
		}
	}
	if len(s.addrs[i])+fresh > MaxAddrs {
		return nil, fmt.Errorf("the divert set of %s is full at %d addresses", s.routes[i], MaxAddrs)
	}
	for _, a := range v4 {
		s.addrs[i][a] = struct{}{}
	}
	return v4, nil
}

// Addrs returns the addresses of route i, sorted.
func (s *Set) Addrs(i int) []netip.Addr {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]netip.Addr, 0, len(s.addrs[i]))
	for a := range s.addrs[i] {
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b netip.Addr) int { return a.Compare(b) })
	return out
}

// Match returns whether some route diverts dst.
func (s *Set) Match(dst netip.AddrPort) bool {
	a := dst.Addr().Unmap()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i, r := range s.routes {
		if r.Port != dst.Port() {
			continue
		}
		if _, ok := s.addrs[i][a]; ok {
			return true
		}
	}
	return false
}

// Magic starts every preamble. The number is the version of the contract.
const Magic = "BRIG-DIVERT/1"

// MaxPreamble is the longest preamble, newline included.
const MaxPreamble = 160

// Preamble returns the line the relay writes to the injector before the
// guest's bytes:
//
//	BRIG-DIVERT/1 <dst-ip> <dst-port> <src-ip> <src-port>\n
//
// dst is where the guest connection was going, and src is the guest's end.
// The addresses are in netip.Addr.String form, with no zone and IPv4 not
// mapped into IPv6.
func Preamble(dst, src netip.AddrPort) ([]byte, error) {
	if !dst.IsValid() || !src.IsValid() || dst.Port() == 0 || src.Port() == 0 {
		return nil, errors.New("a divert preamble needs both ends of the connection, each with a port")
	}
	da, sa := dst.Addr().Unmap().WithZone(""), src.Addr().Unmap().WithZone("")
	line := fmt.Sprintf("%s %s %d %s %d\n", Magic, da, dst.Port(), sa, src.Port())
	if len(line) > MaxPreamble {
		return nil, fmt.Errorf("the divert preamble is %d bytes, above %d", len(line), MaxPreamble)
	}
	return []byte(line), nil
}

// ParsePreamble reads a preamble line, newline included. It accepts only the
// exact form Preamble writes: each address as netip.Addr.String prints it,
// and each port in decimal with no leading zero and not 0. The injector
// reads the preamble with it too.
func ParsePreamble(line []byte) (dst, src netip.AddrPort, err error) {
	if len(line) > MaxPreamble {
		return dst, src, fmt.Errorf("the divert preamble is longer than %d bytes", MaxPreamble)
	}
	text, ok := strings.CutSuffix(string(line), "\n")
	if !ok {
		return dst, src, errors.New("the divert preamble does not end in a newline")
	}
	f := strings.Split(text, " ")
	if len(f) != 5 || f[0] != Magic {
		return dst, src, fmt.Errorf("%q is not a %s preamble", text, Magic)
	}
	if dst, err = addrPort(f[1], f[2]); err != nil {
		return dst, src, err
	}
	if src, err = addrPort(f[3], f[4]); err != nil {
		return dst, src, err
	}
	return dst, src, nil
}

func addrPort(addr, port string) (netip.AddrPort, error) {
	a, err := netip.ParseAddr(addr)
	if err != nil || a.Zone() != "" || a.String() != addr {
		return netip.AddrPort{}, fmt.Errorf("%q is not an address in its canonical form", addr)
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil || p == 0 || strconv.FormatUint(p, 10) != port {
		return netip.AddrPort{}, fmt.Errorf("%q is not a port in decimal", port)
	}
	return netip.AddrPortFrom(a, uint16(p)), nil
}
