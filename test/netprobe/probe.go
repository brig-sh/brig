package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type outcome string

const (
	reached outcome = "reached"
	// The resolve outcomes name the rcode the guest's resolver answered a
	// name with. A policy that stops a name at the resolver answers REFUSED,
	// and a resolver that failed to look the name up answers NXDOMAIN or
	// SERVFAIL, so the three are kept apart.
	resolveRefused  outcome = "resolve-refused"
	resolveNXDomain outcome = "resolve-nxdomain"
	resolveTimeout  outcome = "resolve-timeout"
	// resolveFailed is any other failed lookup: SERVFAIL, an answer with no
	// address, or no resolver to ask.
	resolveFailed  outcome = "resolve-failed"
	connectRefused outcome = "connect-refused"
	timedOut       outcome = "timeout"
	noRoute        outcome = "no-route"
	// clientMissing, failed and failedAfterConnect mean the probe measured
	// nothing. None of them is ever a pass, for an allowed case or a denied
	// one. failedAfterConnect is an error once a connection was made, or once
	// a reply came back, so the path was open that far. failed is any other
	// error.
	clientMissing      outcome = "client-missing"
	failed             outcome = "error"
	failedAfterConnect outcome = "error-after-connect"
)

const (
	exitReached       = 0
	exitNotReached    = 1
	exitUsage         = 2
	exitCannotMeasure = 3
)

// exitCode sorts the outcomes into three codes a shell can branch on. An
// outcome this table does not know is a failure to measure, so a new one
// never falls through to zero.
func exitCode(o outcome) int {
	switch o {
	case reached:
		return exitReached
	case resolveRefused, resolveNXDomain, resolveTimeout, resolveFailed,
		connectRefused, timedOut, noRoute:
		return exitNotReached
	default:
		return exitCannotMeasure
	}
}

// resend is how long askResolver waits for a reply before it sends the
// question again. glibc's stub resolver waits 5 seconds, which is the whole of
// a default case.
const resend = time.Second

type result struct {
	outcome outcome
	detail  string
}

// classify names how a network operation failed. A DNS error is looked at
// first: since Go 1.23 it unwraps to its cause, so a resolver timeout also
// matches the timeout checks below, and a name that never resolved is not a
// connection that timed out.
func classify(err error) outcome {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return resolveFailed
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return connectRefused
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		// ENETUNREACH is what a guest with no IPv6 route gets for a v6
		// literal. A guest that has a v6 route behind a gateway dropping
		// the frames gets a timeout instead, and is reported as one.
		return noRoute
	case errors.Is(err, syscall.ETIMEDOUT), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, os.ErrDeadlineExceeded):
		return timedOut
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return timedOut
	}
	return failed
}

type prober struct {
	timeout  time.Duration
	resolver *net.Resolver
	// nameserver is the address of the resolver that resolver asks, for
	// asking it again when a lookup fails. See unresolved.
	nameserver func() (string, error)
	dialer     func(ctx context.Context, network, addr string) (net.Conn, error)
	// roots is nil in the guest, for the system pool. Tests set it to trust
	// the certificate of a local listener.
	roots    *x509.CertPool
	lookPath func(string) (string, error)
}

func newProber(timeout time.Duration) *prober {
	var d net.Dialer
	return &prober{
		timeout:    timeout,
		resolver:   net.DefaultResolver,
		nameserver: func() (string, error) { return firstNameserver(resolvConf) },
		dialer:     d.DialContext,
		lookPath:   exec.LookPath,
	}
}

// resolvConf names the resolver the guest was given.
const resolvConf = "/etc/resolv.conf"

// firstNameserver returns the first nameserver a resolv.conf names, with port
// 53. The stub resolver asks that one first.
func firstNameserver(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		a, err := netip.ParseAddr(f[1])
		if err != nil {
			return "", fmt.Errorf("%s: nameserver %q is not an address", path, f[1])
		}
		return net.JoinHostPort(a.String(), "53"), nil
	}
	return "", fmt.Errorf("%s names no nameserver", path)
}

// connect resolves host when it is a name, then dials each address in turn.
// The two steps are kept apart so a name the resolver refused is never
// reported as a connection that failed. A literal skips the resolver.
//
// Every address shares the caller's one timeout, and only the first
// address's failure is reported. That is enough for a literal and a name with
// one address. For a name with several, a first address that stalls uses up
// the time before the next is tried, and the detail names only the first.
func (p *prober) connect(ctx context.Context, network, hostport string) (net.Conn, result) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, result{failed, err.Error()}
	}
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		addrs, err = p.resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, p.unresolved(ctx, host, err.Error())
		}
		if len(addrs) == 0 {
			return nil, result{resolveFailed, "lookup " + host + ": no addresses"}
		}
	}
	var first result
	for i, a := range addrs {
		c, err := p.dialer(ctx, network, net.JoinHostPort(a.Unmap().String(), port))
		if err == nil {
			return c, result{reached, "connected to " + c.RemoteAddr().String()}
		}
		if i == 0 {
			first = result{classify(err), err.Error()}
		}
	}
	return nil, first
}

// unresolved names how the lookup of host failed. Go's resolver reports
// REFUSED and SERVFAIL alike as "server misbehaving", and a policy's REFUSED
// has to be told apart from the NXDOMAIN of a failed upstream lookup. So the
// question goes again, straight to the guest's resolver, and the rcode of
// its reply names the outcome.
func (p *prober) unresolved(ctx context.Context, host, detail string) result {
	server, err := p.nameserver()
	if err != nil {
		return result{resolveFailed, detail + "; no resolver to ask for the rcode: " + err.Error()}
	}
	rep, r := p.ask(ctx, dialResolver, server, host)
	switch {
	case r.outcome == timedOut:
		return result{resolveTimeout, detail + "; asked again, " + server + " sent no reply: " + r.detail}
	case r.outcome != reached:
		return result{resolveFailed, detail + "; asked again: " + r.detail}
	}
	detail += "; asked again, " + r.detail
	switch rep.rcode {
	case rcodeRefused:
		return result{resolveRefused, detail}
	case rcodeNXDomain:
		return result{resolveNXDomain, detail}
	}
	return result{resolveFailed, detail}
}

// after reports a failure once a connection is up, and says so. It is always
// an error, a deadline included: a peer that accepts and then goes quiet has
// let a packet through, and a timeout here reads the same as a dropped SYN.
func after(c net.Conn, what string, err error) result {
	return result{failedAfterConnect, fmt.Sprintf("connected to %s, then %s: %v", c.RemoteAddr(), what, err)}
}

func (p *prober) tcp(ctx context.Context, target string) result {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	c, r := p.connect(ctx, "tcp", target)
	if c != nil {
		c.Close()
	}
	return r
}

// askResolver sends one question straight to server over UDP, around the
// resolver the guest was given. Any reply to the question is a reach,
// NXDOMAIN included: the resolver answered, so the guest can talk to it.
func (p *prober) askResolver(ctx context.Context, server, name string) result {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	_, r := p.ask(ctx, p.connect, server, name)
	return r
}

// dialResolver dials the guest's own resolver. It is part of resolving a
// name, so it goes around the dialer that reaches a case's target.
func dialResolver(ctx context.Context, network, addr string) (net.Conn, result) {
	var d net.Dialer
	c, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, result{classify(err), err.Error()}
	}
	return c, result{}
}

// ask sends a question for name to server over UDP and returns the reply.
// The result is reached for any reply to the question, whatever its rcode.
func (p *prober) ask(ctx context.Context, dial func(context.Context, string, string) (net.Conn, result),
	server, name string) (reply, result) {
	id := newID()
	q, err := buildQuery(id, name, typeA)
	if err != nil {
		return reply{}, result{failed, err.Error()}
	}
	c, r := dial(ctx, "udp", server)
	if c == nil {
		return reply{}, r
	}
	defer c.Close()
	// One datagram is not one measurement. On hvi the first question to an
	// outside resolver after a boot went unanswered at times, with a policy
	// and without one, and a question a few seconds later got a reply. So
	// the question goes again every resend, as a stub resolver sends it, and
	// only silence for the whole timeout is a timeout.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(p.timeout)
	}
	buf := make([]byte, 1500)
	var n int
	for {
		if _, err := c.Write(q); err != nil {
			return reply{}, result{classify(err), err.Error()}
		}
		next := time.Now().Add(resend)
		if next.After(deadline) {
			next = deadline
		}
		c.SetReadDeadline(next)
		n, err = c.Read(buf)
		if err == nil {
			break
		}
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() || !time.Now().Before(deadline) {
			// A connected UDP socket on Linux reads back ECONNREFUSED when
			// the server's port answered with ICMP port unreachable.
			return reply{}, result{classify(err), err.Error()}
		}
	}
	rep, err := parseReply(buf[:n], id)
	if err != nil {
		return reply{}, result{failedAfterConnect, fmt.Sprintf("%s answered, then: %v", server, err)}
	}
	return rep, result{reached, fmt.Sprintf("%s answered %s", server, rep)}
}

func setDeadline(ctx context.Context, c net.Conn) {
	if d, ok := ctx.Deadline(); ok {
		c.SetDeadline(d)
	}
}

func (p *prober) tlsClient(c net.Conn, host string, protos ...string) *tls.Conn {
	return tls.Client(c, &tls.Config{ServerName: host, RootCAs: p.roots, NextProtos: protos})
}

// dot asks one question over DNS over TLS (RFC 7858). The certificate is
// verified, so a gateway that answers in the resolver's place is an error and
// not a reach.
func (p *prober) dot(ctx context.Context, target, name string) result {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return result{failed, err.Error()}
	}
	id := newID()
	q, err := buildQuery(id, name, typeA)
	if err != nil {
		return result{failed, err.Error()}
	}
	c, r := p.connect(ctx, "tcp", target)
	if c == nil {
		return r
	}
	defer c.Close()
	setDeadline(ctx, c)
	tc := p.tlsClient(c, host)
	if err := tc.HandshakeContext(ctx); err != nil {
		return after(c, "TLS", err)
	}
	msg := binary.BigEndian.AppendUint16(nil, uint16(len(q)))
	if _, err := tc.Write(append(msg, q...)); err != nil {
		return after(c, "write", err)
	}
	var n uint16
	if err := binary.Read(tc, binary.BigEndian, &n); err != nil {
		return after(c, "read", err)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(tc, b); err != nil {
		return after(c, "read", err)
	}
	rep, err := parseReply(b, id)
	if err != nil {
		return result{failedAfterConnect, fmt.Sprintf("connected to %s, then: %v", c.RemoteAddr(), err)}
	}
	return result{reached, fmt.Sprintf("%s answered %s over TLS", c.RemoteAddr(), rep)}
}

// dohURL checks a DoH endpoint and returns the address to dial. Only https
// is accepted: DoH over plain HTTP is a different case from the one a policy
// is being tested against.
func dohURL(raw string) (*url.URL, string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", err
	}
	if u.Scheme != "https" || u.Hostname() == "" {
		return nil, "", fmt.Errorf("DoH endpoint %q is not an https URL", raw)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return u, net.JoinHostPort(u.Hostname(), port), nil
}

// doh asks one question over DNS over HTTPS (RFC 8484), as a POST of the wire
// message. Reached means HTTP 200 and a reply to this question. Any other
// status is a page from something in the way and resolves nothing.
func (p *prober) doh(ctx context.Context, raw, name string) result {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	u, target, err := dohURL(raw)
	if err != nil {
		return result{failed, err.Error()}
	}
	id := newID()
	q, err := buildQuery(id, name, typeA)
	if err != nil {
		return result{failed, err.Error()}
	}
	c, r := p.connect(ctx, "tcp", target)
	if c == nil {
		return r
	}
	defer c.Close()
	setDeadline(ctx, c)
	tc := p.tlsClient(c, u.Hostname(), "http/1.1")
	if err := tc.HandshakeContext(ctx); err != nil {
		return after(c, "TLS", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(q))
	if err != nil {
		return result{failed, err.Error()}
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	if err := req.Write(tc); err != nil {
		return after(c, "write", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), req)
	if err != nil {
		return after(c, "read", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return result{failedAfterConnect, fmt.Sprintf("connected to %s, then HTTP %s", c.RemoteAddr(), resp.Status)}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return after(c, "read", err)
	}
	rep, err := parseReply(b, id)
	if err != nil {
		return result{failedAfterConnect, fmt.Sprintf("connected to %s, then: %v", c.RemoteAddr(), err)}
	}
	return result{reached, fmt.Sprintf("%s answered %s over HTTPS", c.RemoteAddr(), rep)}
}
