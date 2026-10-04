package egress

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"sync"
	"time"
)

// The lifetimes hull's gateway uses, kept equal so a host rule holds for as
// long on Linux as on macOS.
const (
	// PinTTL is the TTL the resolver writes into every answer when the policy
	// has host rules.
	PinTTL = 60 * time.Second
	// PinGrace is how long a pinned address stays reachable after PinTTL. It
	// covers a guest that connects on an answer that expired between the
	// lookup and the first packet.
	PinGrace = 60 * time.Second
	// RefreshInterval is how often the resolver re-resolves the rules that
	// name exactly one host.
	RefreshInterval = 30 * time.Second
	// refreshRetention is how many intervals a refreshed address outlives the
	// last refresh that returned it, so one failed lookup does not cut the
	// sandbox off.
	refreshRetention = 3
)

const (
	upstreamTimeout = 4 * time.Second
	tcpIdleTimeout  = 10 * time.Second
	// maxInFlight bounds the queries answered at once.
	maxInFlight = 64
	// maxUDPQuery is the largest UDP query read. A query is one question,
	// and EDNS0 options rarely take it past a few hundred bytes.
	maxUDPQuery = 4096
)

// The guest decides how many queries it sends, and the log is on the host's
// disk. So the lines a query causes are limited three ways: one name is
// logged once every guestLogEvery, all names together guestLogBurst times in
// one guestLogEvery, and guestLogBytes over the resolver's life.
const (
	guestLogEvery = 30 * time.Second
	guestLogBurst = 20
	guestLogBytes = 4 << 20
	guestLogKeys  = 4096
)

// Pinner puts addresses in one of the sets a sandbox's table filters on.
type Pinner interface {
	Pin(set string, addrs []netip.Addr, lifetime time.Duration) error
}

// Resolver answers a sandbox's DNS under a policy.
type Resolver struct {
	// Policy decides which names are answered and where their addresses are
	// pinned.
	Policy *Policy
	// Upstream is the servers queries are forwarded to, as host:port, tried
	// in order.
	Upstream []string
	// Pins puts answered addresses in the sandbox's table.
	Pins Pinner
	// Log receives what the resolver refused and what failed. Nil logs
	// nothing.
	Log *log.Logger

	// exchange sends one message upstream over network ("udp" or "tcp").
	// Nil means exchangeNet. Tests replace it.
	exchange func(ctx context.Context, network, server string, msg []byte) ([]byte, error)

	mu         sync.Mutex
	logged     map[string]time.Time
	window     time.Time
	inWindow   int
	suppressed int
	loggedSize int
}

func (r *Resolver) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log.Printf(format, args...)
	}
}

// logGuest logs a line that a guest's query caused, within the limits above.
// key is what makes two lines the same.
func (r *Resolver) logGuest(key, format string, args ...any) {
	if r.Log == nil {
		return
	}
	line := fmt.Sprintf(format, args...)
	now := time.Now()
	r.mu.Lock()
	if r.logged == nil || len(r.logged) >= guestLogKeys {
		r.logged = map[string]time.Time{}
	}
	var dropped int
	if now.Sub(r.window) >= guestLogEvery {
		dropped, r.suppressed = r.suppressed, 0
		r.window, r.inWindow = now, 0
	}
	last, seen := r.logged[key]
	write := !seen || now.Sub(last) >= guestLogEvery
	if write && r.inWindow >= guestLogBurst {
		r.suppressed++
		write = false
	}
	wasFull := r.loggedSize >= guestLogBytes
	if write && !wasFull {
		r.logged[key] = now
		r.inWindow++
		r.loggedSize += len(line)
	}
	nowFull := r.loggedSize >= guestLogBytes
	r.mu.Unlock()
	if wasFull {
		return
	}
	if dropped > 0 {
		r.logf("egress: %d more refused or failed queries were not logged", dropped)
	}
	if write {
		r.logf("%s", line)
	}
	if nowFull {
		r.logf("egress: the log of refused and failed queries is full, and no more are logged")
	}
}

// Answer returns the response to one query, or nil when the message is not
// worth an answer. network is the transport the query arrived on, and the
// query goes upstream on the same one.
func (r *Resolver) Answer(ctx context.Context, query []byte, network string, guest netip.Addr) []byte {
	q, err := parseQuery(query)
	if err != nil {
		if len(query) < headerLen || query[2]&0x80 != 0 {
			return nil
		}
		if errors.Is(err, errOpcode) {
			return headerReply(query, rcodeNotImp)
		}
		return headerReply(query, rcodeFormErr)
	}
	if !r.Policy.AllowsQuery(q.name) {
		r.logGuest(q.name, "egress: refused the query %s from %s", q.name, guest)
		return reply(query, q, rcodeRefused)
	}
	if !q.forwarded() {
		return reply(query, q, 0)
	}
	resp, err := r.forward(ctx, network, query)
	if err != nil {
		r.logGuest(q.name, "egress: %s: %v", q.name, err)
		return reply(query, q, rcodeServFail)
	}
	var ttl uint32
	if r.Policy.HasHostRules() {
		ttl = uint32(PinTTL / time.Second)
	}
	resp, addrs, err := inspectAnswer(resp, query, q, ttl)
	if err != nil {
		r.logGuest(q.name, "egress: %s: bad upstream answer: %v", q.name, err)
		return reply(query, q, rcodeServFail)
	}
	if set, ok := r.pinSet(q); ok && len(addrs) > 0 {
		// The guest must not get an address before the set that governs it
		// does. A failed pin fails the query.
		if err := r.Pins.Pin(set, addrs, PinTTL+PinGrace); err != nil {
			r.logGuest(q.name, "egress: %s: could not pin %v: %v", q.name, addrs, err)
			return reply(query, q, rcodeServFail)
		}
	}
	return resp
}

// pinSet returns the set the addresses of an answer go in, and false when
// they need none. Under an allow default, an address no deny glob covers is
// reachable without a pin.
func (r *Resolver) pinSet(q question) (string, bool) {
	if q.qtype != typeA || !r.Policy.HasHostRules() {
		return "", false
	}
	if r.Policy.DeniesQuery(q.name) {
		return SetDenyIP, true
	}
	if r.Policy.Default == "allow" {
		return "", false
	}
	return SetAllowIP, true
}

// forward sends query upstream and returns the response under the query's
// own ID.
//
// The upstream query carries a random ID. The guest chose the ID of its own
// query, and an ID it knows is half of a forged answer.
func (r *Resolver) forward(ctx context.Context, network string, query []byte) ([]byte, error) {
	exchange := r.exchange
	if exchange == nil {
		exchange = exchangeNet
	}
	if len(r.Upstream) == 0 {
		return nil, errors.New("no upstream resolver")
	}
	out := append([]byte(nil), query...)
	_, _ = rand.Read(out[:2])
	var errs []error
	for _, server := range r.Upstream {
		resp, err := exchange(ctx, network, server, out)
		if err == nil && (len(resp) < headerLen || resp[0] != out[0] || resp[1] != out[1]) {
			err = errors.New("response ID does not match the query")
		}
		if err == nil {
			copy(resp, query[:2])
			return resp, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", server, err))
	}
	return nil, errors.Join(errs...)
}

// exchangeNet sends msg to server and returns the first response whose ID
// matches.
func exchangeNet(ctx context.Context, network, server string, msg []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, upstreamTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, server)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if network == "tcp" {
		if err := writeTCP(conn, msg); err != nil {
			return nil, err
		}
		return readTCP(conn)
	}
	if _, err := conn.Write(msg); err != nil {
		return nil, err
	}
	buf := make([]byte, 65535)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return nil, err
		}
		if n >= 2 && buf[0] == msg[0] && buf[1] == msg[1] {
			return append([]byte(nil), buf[:n]...), nil
		}
	}
}

func writeTCP(w io.Writer, msg []byte) error {
	out := make([]byte, 2+len(msg))
	binary.BigEndian.PutUint16(out, uint16(len(msg)))
	copy(out[2:], msg)
	_, err := w.Write(out)
	return err
}

func readTCP(rd io.Reader) ([]byte, error) {
	var n [2]byte
	if _, err := io.ReadFull(rd, n[:]); err != nil {
		return nil, err
	}
	msg := make([]byte, binary.BigEndian.Uint16(n[:]))
	if _, err := io.ReadFull(rd, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// ServeUDP answers queries on pc until ctx is done.
func (r *Resolver) ServeUDP(ctx context.Context, pc net.PacketConn) error {
	sem := make(chan struct{}, maxInFlight)
	go func() {
		<-ctx.Done()
		_ = pc.Close()
	}()
	for {
		buf := make([]byte, maxUDPQuery)
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			if resp := r.Answer(ctx, buf[:n], "udp", addrOf(from)); resp != nil {
				_, _ = pc.WriteTo(resp, from)
			}
		}()
	}
}

// ServeTCP answers queries on connections accepted from ln until ctx is done.
func (r *Resolver) ServeTCP(ctx context.Context, ln net.Listener) error {
	sem := make(chan struct{}, maxInFlight)
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			defer func() { _ = conn.Close() }()
			guest := addrOf(conn.RemoteAddr())
			for {
				_ = conn.SetDeadline(time.Now().Add(tcpIdleTimeout))
				query, err := readTCP(conn)
				if err != nil {
					return
				}
				resp := r.Answer(ctx, query, "tcp", guest)
				if resp == nil || writeTCP(conn, resp) != nil {
					return
				}
			}
		}()
	}
}

func addrOf(a net.Addr) netip.Addr {
	if ap, err := netip.ParseAddrPort(a.String()); err == nil {
		return ap.Addr().Unmap()
	}
	return netip.Addr{}
}

// RefreshHosts resolves the rules that name exactly one host, once, and pins
// what they resolve to for every guest, for refreshRetention intervals. An
// interval of zero turns the refresh off.
//
// A host rule otherwise holds only through the answers the guest asked for.
// A guest that caches an address past its TTL, or that connects after the
// name moved to a new address, would then be refused a host the policy
// allows. A guest that dials a denied host's address without asking would
// be let through. Globs cannot be resolved ahead of a query, so they stay
// query-driven.
func (r *Resolver) RefreshHosts(ctx context.Context, interval time.Duration) {
	allow, deny := r.Policy.allow.literalHosts(), r.Policy.deny.literalHosts()
	if interval <= 0 || len(allow)+len(deny) == 0 {
		return
	}
	res := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			var errs []error
			for _, server := range r.Upstream {
				conn, err := d.DialContext(ctx, network, server)
				if err == nil {
					return conn, nil
				}
				errs = append(errs, err)
			}
			return nil, errors.Join(errs...)
		},
	}
	lifetime := refreshRetention * interval
	var wg sync.WaitGroup
	for _, h := range allow {
		wg.Add(1)
		go func() { defer wg.Done(); r.refreshHost(ctx, res, h, SetAllowHost, lifetime) }()
	}
	for _, h := range deny {
		wg.Add(1)
		go func() { defer wg.Done(); r.refreshHost(ctx, res, h, SetDenyHost, lifetime) }()
	}
	wg.Wait()
}

// WatchHosts calls RefreshHosts every interval until ctx is done. The first
// call is the caller's, before the guest can connect.
func (r *Resolver) WatchHosts(ctx context.Context, interval time.Duration) {
	if interval <= 0 || len(r.Policy.allow.literalHosts())+len(r.Policy.deny.literalHosts()) == 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.RefreshHosts(ctx, interval)
		}
	}
}

func (r *Resolver) refreshHost(ctx context.Context, res *net.Resolver, host, set string, lifetime time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, upstreamTimeout)
	defer cancel()
	addrs, err := res.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		r.logf("egress: cannot resolve %s, keeping its current addresses: %v", host, err)
		return
	}
	if err := r.Pins.Pin(set, addrs, lifetime); err != nil {
		r.logf("egress: could not pin %s %v: %v", host, addrs, err)
	}
}
