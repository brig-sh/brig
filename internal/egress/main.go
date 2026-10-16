package egress

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/brig-sh/brig/internal/divert"
)

// Verb is the hidden argument that makes a brig binary run the resolver. brig
// starts it inside the namespace it filters, and nothing else is meant to.
const Verb = "__egress-resolver"

// dnsPort is the port the resolver answers on. A variable so a test can bind
// one it is allowed to.
var dnsPort uint16 = 53

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// Main runs the resolver with the command line args and returns the exit
// status. It installs the sandbox's table, keeps it in place, and serves
// until SIGTERM or SIGINT.
//
// With --divert, the same process also runs the relay. The relay checks each
// connection against the addresses the resolver learns, so the two share one
// copy of the divert sets in memory. One process also means one record, one
// stop and one heartbeat for both.
func Main(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet(Verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	table := fs.String("table", "", "the sandbox's nftables table")
	listen := fs.String("listen", "", "the bridge address to answer DNS on")
	bridge := fs.String("bridge", "", "the sandbox's bridge")
	subnet := fs.String("subnet", "", "the sandbox network")
	mark := fs.Uint("mark", 0, "the conntrack mark of this boot, 1 to 65535")
	nftBin := fs.String("nft", "nft", "the nft binary")
	def := fs.String("egress-default", "", "allow or deny")
	refresh := fs.Duration("egress-refresh", RefreshInterval, "how often to re-resolve the rules that name one host; 0 disables")
	divertSocket := fs.String("divert-socket", "", "the injector's unix socket")
	divertPort := fs.Uint("divert-port", uint(DivertPort), "the relay's port on the bridge address")
	var upstream, redirect, allow, deny, diverts, hosts listFlag
	fs.Var(&diverts, "divert", "a host:port whose connections go to the injector (repeatable)")
	fs.Var(&hosts, "host", "a name=ip the resolver answers itself, whatever the policy (repeatable)")
	fs.Var(&upstream, "upstream", "a resolver to forward to, as host:port (repeatable)")
	fs.Var(&redirect, "redirect", "a resolver whose DNS traffic is sent to this one (repeatable)")
	fs.Var(&allow, "egress-allow", "host=<glob> or cidr=<cidr> (repeatable)")
	fs.Var(&deny, "egress-deny", "host=<glob> or cidr=<cidr> (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	logger := log.New(stderr, "", log.LstdFlags)
	err := func() error {
		if *table == "" || len(upstream) == 0 {
			return errors.New("--table and --upstream are required")
		}
		if *mark > 0xffff {
			return fmt.Errorf("--mark %d is above 65535", *mark)
		}
		n, err := network(*bridge, *listen, *subnet, redirect)
		if err != nil {
			return err
		}
		if n.Divert, err = divertOf(diverts, *divertSocket, *divertPort); err != nil {
			return err
		}
		// A sandbox that diverts and has no policy gets a resolver that
		// answers every name.
		p := Unfiltered()
		if *def != "" || len(allow)+len(deny) > 0 || len(diverts) == 0 {
			if p, err = Parse(*def, allow, deny); err != nil {
				return err
			}
		}
		script, err := Ruleset(*table, n, p, uint16(*mark))
		if err != nil {
			return err
		}
		records, err := hostsOf(hosts)
		if err != nil {
			return err
		}
		r := &Resolver{Policy: p, Upstream: upstream, Pins: NFT{Bin: *nftBin, Table: *table}, Log: logger,
			Hosts: records}
		k := &tableKeeper{nft: []string{*nftBin}, table: *table, script: script, bridge: n.Bridge, log: logger,
			pin: func(ctx context.Context) { r.RefreshHosts(ctx, *refresh) }}
		var rl *divert.Relay
		if len(n.Divert.Routes) > 0 {
			r.Diverts = &Diverts{Set: divert.NewSet(n.Divert.Routes), Table: *table, NFT: []string{*nftBin}}
			k.diverts = r.Diverts
			rl = &divert.Relay{Socket: *divertSocket, Set: r.Diverts.Set, Log: r.logGuest}
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		return serve(ctx, k, r, rl, n, *refresh, len(allow), len(deny), logger)
	}()
	if err != nil {
		logger.Printf("egress resolver: %v", err)
		return 1
	}
	return 0
}

func network(bridge, listen, subnet string, redirect []string) (Network, error) {
	n := Network{Bridge: bridge}
	var err error
	if n.Gateway, err = netip.ParseAddr(listen); err != nil {
		return Network{}, fmt.Errorf("--listen %q: %w", listen, err)
	}
	if n.Subnet, err = netip.ParsePrefix(subnet); err != nil {
		return Network{}, fmt.Errorf("--subnet %q: %w", subnet, err)
	}
	for _, r := range redirect {
		a, err := netip.ParseAddr(r)
		if err != nil {
			return Network{}, fmt.Errorf("--redirect %q: %w", r, err)
		}
		n.Redirect = append(n.Redirect, a)
	}
	return n, nil
}

// hostsOf reads the --host flags: name=ip, a plain host name and one IPv4
// address, each name once.
func hostsOf(values []string) (map[string]netip.Addr, error) {
	out := map[string]netip.Addr{}
	for _, v := range values {
		name, ip, ok := strings.Cut(v, "=")
		name = strings.TrimSuffix(strings.ToLower(name), ".")
		if !ok || !divert.Hostname(name) {
			return nil, fmt.Errorf("--host %q is not name=ip", v)
		}
		a, err := netip.ParseAddr(ip)
		if err != nil || !a.Is4() {
			return nil, fmt.Errorf("--host %q: %q is not an IPv4 address", v, ip)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("--host %q: %s is named twice", v, name)
		}
		out[name] = a
	}
	return out, nil
}

// divertOf reads the divert flags. A divert with no socket, or a socket with
// no divert, is refused.
func divertOf(routes []string, socket string, port uint) (Divert, error) {
	if len(routes) == 0 {
		if socket != "" {
			return Divert{}, errors.New("--divert-socket needs at least one --divert")
		}
		return Divert{}, nil
	}
	if !filepath.IsAbs(socket) {
		return Divert{}, fmt.Errorf("--divert-socket %q is not an absolute path", socket)
	}
	if port == 0 || port > 0xffff {
		return Divert{}, fmt.Errorf("--divert-port %d is not a port", port)
	}
	d := Divert{Port: uint16(port)}
	for _, s := range routes {
		r, err := divert.ParseRoute(s)
		if err != nil {
			return Divert{}, err
		}
		d.Routes = append(d.Routes, r)
	}
	return d, nil
}

// serve installs the table, binds the resolver's sockets and the relay's,
// and then logs the line brig waits for. A guest booted after that line meets
// the whole policy, and every divert. It answers until ctx is done.
//
// The hosts of the divert routes are resolved before the table goes in, and
// a host that does not resolve fails the start.
func serve(ctx context.Context, k *tableKeeper, r *Resolver, rl *divert.Relay, n Network, refresh time.Duration, nAllow, nDeny int, logger *log.Logger) error {
	if r.Diverts != nil {
		if err := r.Diverts.Resolve(ctx, r.netResolver(), false); err != nil {
			return err
		}
	}
	if err := k.install(ctx); err != nil {
		return err
	}
	pc, ln, err := listen(ctx, n.Gateway)
	if err != nil {
		return err
	}
	var rln net.Listener
	if rl != nil {
		lc := net.ListenConfig{Control: freebind}
		rln, err = lc.Listen(ctx, "tcp4", netip.AddrPortFrom(n.Gateway, n.Divert.Port).String())
		if err != nil {
			_ = pc.Close()
			_ = ln.Close()
			return fmt.Errorf("the divert relay: %w", err)
		}
	}
	def := r.Policy.Default
	if def == "" {
		def = "none"
	}
	logger.Printf("egress resolver on %s for table %s, default %s (%d allow, %d deny), upstream %s",
		netip.AddrPortFrom(n.Gateway, dnsPort), k.table, def, nAllow, nDeny, strings.Join(r.Upstream, " "))

	errc := make(chan error, 3)
	go func() { errc <- r.ServeUDP(ctx, pc) }()
	go func() { errc <- r.ServeTCP(ctx, ln) }()
	if rl != nil {
		logger.Printf("egress: diverting %d routes through %s to %s", len(n.Divert.Routes), rln.Addr(), rl.Socket)
		go func() { errc <- rl.Serve(ctx, rln) }()
		go r.Diverts.watch(ctx, r.netResolver(), refresh, r.logf)
	}
	go r.WatchHosts(ctx, refresh)
	go k.run(ctx, HeartbeatEvery)
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

// listen binds the resolver's sockets on addr alone. The bridge address may
// not exist yet, since the bridge comes up with the sandbox's first
// container, so the sockets bind with IP_FREEBIND.
func listen(ctx context.Context, addr netip.Addr) (net.PacketConn, net.Listener, error) {
	lc := net.ListenConfig{Control: freebind}
	hostport := netip.AddrPortFrom(addr, dnsPort).String()
	pc, err := lc.ListenPacket(ctx, "udp4", hostport)
	if err != nil {
		return nil, nil, err
	}
	ln, err := lc.Listen(ctx, "tcp4", hostport)
	if err != nil {
		_ = pc.Close()
		return nil, nil, err
	}
	return pc, ln, nil
}

// tableKeeper keeps a sandbox's table in place for as long as the resolver
// runs. It renews the heartbeat, and installs the table again when something
// removed it, such as a `flush ruleset` on the host.
type tableKeeper struct {
	nft    []string
	table  string
	script string
	bridge string
	log    *log.Logger
	// pin pins the addresses of the rules that name one host. Nil pins
	// nothing.
	pin func(context.Context)
	// diverts puts its addresses in the table with each install. Nil
	// diverts nothing.
	diverts *Diverts
}

// install installs the table, pins the hosts and starts the heartbeat, in
// that order. Until the heartbeat starts, the table refuses every new
// connection. A connection admitted while a denied host had no address in
// the table would keep its mark, and skip the deny sets for its whole life.
func (k *tableKeeper) install(ctx context.Context) error {
	run := func() error { return RunNFT(ctx, k.nft, k.script) }
	if k.diverts != nil {
		// A table that went away took its divert sets with it. The addresses
		// go back in the same transaction as the table, so no connection to
		// one is routed directly in between.
		run = func() error { return k.diverts.install(ctx, k.nft, k.script) }
	}
	if err := run(); err != nil {
		return fmt.Errorf("could not install the table %s: %w", k.table, err)
	}
	if k.pin != nil {
		k.pin(ctx)
	}
	return k.beat(ctx)
}

func (k *tableKeeper) beat(ctx context.Context) error {
	return RunNFT(ctx, k.nft, HeartbeatScript(k.table, k.bridge, HeartbeatLapse))
}

// run renews the heartbeat every interval until ctx is done. A table installed
// again starts with no pins. install puts the hosts' back, and the answers'
// come back as the guest asks again.
func (k *tableKeeper) run(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		err := k.beat(ctx)
		if err == nil || ctx.Err() != nil {
			continue
		}
		// A heartbeat that failed on a table still in place lets the bridge
		// lapse, which refuses new connections. Installing the table again
		// would also drop every pin, so that is kept for a table that is gone.
		if RunNFT(ctx, k.nft, "list table inet "+k.table+"\n") == nil {
			k.log.Printf("egress: could not renew the heartbeat: %v", err)
			continue
		}
		k.log.Printf("egress: the table %s is gone, installing it again", k.table)
		if err := k.install(ctx); err != nil {
			k.log.Printf("egress: %v", err)
		}
	}
}
