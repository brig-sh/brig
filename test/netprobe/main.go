// Command netprobe tries one network path from inside a guest and names how
// it ended: reached, resolve-refused, resolve-nxdomain, resolve-timeout,
// resolve-failed, connect-refused, timeout, no-route, client-missing, error,
// or error-after-connect.
//
// The network conformance suite (#264) copies it into a sandbox and runs one
// case per call. A denied case passes only on the failure mode the policy
// causes. So the probe keeps a name the resolver refused apart from one it
// did not find or never answered for, a failed lookup apart from a connection
// that failed, and a refused port apart from a dropped one. It is built
// static and with the standard library alone, so it runs on any guest image
// whatever libc or tools it carries:
//
//	make netprobe
//
// Usage:
//
//	netprobe tcp [-timeout d] HOST:PORT        a name, or an IPv4 or [IPv6] literal
//	netprobe dns [-timeout d] ADDR[:53] NAME   ask an alternate resolver over UDP
//	netprobe dot [-timeout d] HOST[:853] NAME  DNS over TLS
//	netprobe doh [-timeout d] [-client curl] URL NAME
//	                                          DNS over HTTPS, by the probe's own
//	                                          client or by the guest's curl
//
// It prints one line, the outcome first, then the mode, the target and what
// happened. The exit status is 0 for reached, 1 for a path that was measured
// and not reached, 2 for a usage error, and 3 when nothing was measured:
// client-missing, or an error such as a certificate the guest does not trust.
// Only 0 means the target was reached. error-after-connect is an error once a
// connection was made, so the path was open that far, and error is any other.
//
// DoT and DoH verify the server's certificate against the guest's CA bundle.
// An image with none, such as ubuntu:latest, gets error-after-connect on
// both. Point the probe at a bundle with SSL_CERT_FILE.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage:
  netprobe tcp [-timeout d] HOST:PORT
  netprobe dns [-timeout d] ADDR[:53] NAME
  netprobe dot [-timeout d] HOST[:853] NAME
  netprobe doh [-timeout d] [-client curl] URL NAME
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	mode := args[0]
	fs := flag.NewFlagSet("netprobe "+mode, flag.ContinueOnError)
	fs.SetOutput(stderr)
	timeout := fs.Duration("timeout", 5*time.Second, "how long the whole case gets")
	client := fs.String("client", "", "doh only: \"curl\" to use the guest's curl")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	bad := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "netprobe: "+format+"\n", a...)
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	want := map[string]int{"tcp": 1, "dns": 2, "dot": 2, "doh": 2}
	n, ok := want[mode]
	if !ok {
		return bad("unknown mode %q", mode)
	}
	if len(rest) != n {
		return bad("%s takes %d arguments, got %d", mode, n, len(rest))
	}
	if *client != "" && (mode != "doh" || *client != "curl") {
		return bad("-client takes only \"curl\", and only with doh")
	}
	// A deadline already past fails every dial before a packet leaves, and
	// the line reads as a measured timeout.
	if *timeout <= 0 {
		return bad("-timeout must be above zero, got %v", *timeout)
	}

	p := newProber(*timeout)
	ctx := context.Background()
	var r result
	switch mode {
	case "tcp":
		if _, _, err := net.SplitHostPort(rest[0]); err != nil {
			return bad("%v", err)
		}
		r = p.tcp(ctx, rest[0])
	case "dns":
		server, err := withPort(rest[0], "53")
		if err != nil {
			return bad("%v", err)
		}
		// The alternate resolver is an address. A name for it goes through
		// the very resolver this case goes around.
		host, _, _ := net.SplitHostPort(server)
		if _, err := netip.ParseAddr(host); err != nil {
			return bad("alternate resolver %q is not an IP address", rest[0])
		}
		r = p.askResolver(ctx, server, rest[1])
	case "dot":
		target, err := withPort(rest[0], "853")
		if err != nil {
			return bad("%v", err)
		}
		r = p.dot(ctx, target, rest[1])
	case "doh":
		if _, _, err := dohURL(rest[0]); err != nil {
			return bad("%v", err)
		}
		if *client == "curl" {
			r = p.dohCurl(ctx, rest[0], rest[1])
		} else {
			r = p.doh(ctx, rest[0], rest[1])
		}
	}
	fmt.Fprintf(stdout, "%s %s %s %s\n", r.outcome, mode, rest[0], r.detail)
	return exitCode(r.outcome)
}

// withPort adds the default port when target has none. A bare IPv6 literal
// has colons of its own, so it is checked for before SplitHostPort, bracketed
// or not. JoinHostPort brackets it again otherwise.
func withPort(target, port string) (string, error) {
	lit := strings.TrimSuffix(strings.TrimPrefix(target, "["), "]")
	if a, err := netip.ParseAddr(lit); err == nil {
		return net.JoinHostPort(a.String(), port), nil
	}
	if _, _, err := net.SplitHostPort(target); err == nil {
		return target, nil
	}
	if target == "" {
		return "", fmt.Errorf("empty target")
	}
	return net.JoinHostPort(target, port), nil
}
