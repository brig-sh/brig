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
	"strings"
	"syscall"
	"time"
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
	var upstream, redirect, allow, deny listFlag
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
		p, err := Parse(*def, allow, deny)
		if err != nil {
			return err
		}
		script, err := Ruleset(*table, n, p, uint16(*mark))
		if err != nil {
			return err
		}
		r := &Resolver{Policy: p, Upstream: upstream, Pins: NFT{Bin: *nftBin, Table: *table}, Log: logger}
		k := &tableKeeper{nft: []string{*nftBin}, table: *table, script: script, bridge: n.Bridge, log: logger,
			pin: func(ctx context.Context) { r.RefreshHosts(ctx, *refresh) }}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		return serve(ctx, k, r, n.Gateway, *refresh, len(allow), len(deny), logger)
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

// serve installs the table, binds the resolver's sockets and then logs the
// line brig waits for, so a guest booted after that line meets the whole
// policy. It answers until ctx is done.
func serve(ctx context.Context, k *tableKeeper, r *Resolver, addr netip.Addr, refresh time.Duration, nAllow, nDeny int, logger *log.Logger) error {
	if err := k.install(ctx); err != nil {
		return err
	}
	pc, ln, err := listen(ctx, addr)
	if err != nil {
		return err
	}
	logger.Printf("egress resolver on %s for table %s, default %s (%d allow, %d deny), upstream %s",
		netip.AddrPortFrom(addr, dnsPort), k.table, r.Policy.Default, nAllow, nDeny, strings.Join(r.Upstream, " "))

	errc := make(chan error, 2)
	go func() { errc <- r.ServeUDP(ctx, pc) }()
	go func() { errc <- r.ServeTCP(ctx, ln) }()
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
}

// install installs the table, pins the hosts and starts the heartbeat, in
// that order. Until the heartbeat starts, the table refuses every new
// connection. A connection admitted while a denied host had no address in
// the table would keep its mark, and skip the deny sets for its whole life.
func (k *tableKeeper) install(ctx context.Context) error {
	if err := RunNFT(ctx, k.nft, k.script); err != nil {
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
