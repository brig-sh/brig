package egress

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/divert"
)

var divertNet = func() Network {
	n := testNet
	n.Divert = Divert{Routes: []divert.Route{{Host: "api.anthropic.com", Port: 443}, {Host: "203.0.113.9", Port: 8443}},
		Port: DivertPort}
	return n
}()

func TestRulesetDiverts(t *testing.T) {
	script, err := Ruleset("t", divertNet, mustParse(t, "deny", []string{"host=api.anthropic.com"}, nil), testMark)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"set divert_0 { type ipv4_addr; size 4096; }",
		"set divert_1 { type ipv4_addr; size 4096; }",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("ruleset lacks %q:\n%s", want, script)
		}
	}
	// All of the guest's DNS goes to the resolver, which learns the bound
	// hosts' addresses from it.
	if got, want := chain(t, script, "dns"), "\t\ttype nat hook prerouting priority dstnat - 10; policy accept;\n"+
		"\t\tiifname \"br-ebd625d78bb2\" ip daddr != 10.4.1.1 meta l4proto { tcp, udp } th dport 53 dnat ip to 10.4.1.1:53\n"; got != want {
		t.Errorf("dns chain =\n%s\nwant\n%s", got, want)
	}
	if got, want := chain(t, script, "divert"), "\t\ttype nat hook prerouting priority dstnat - 10; policy accept;\n"+
		"\t\tiifname \"br-ebd625d78bb2\" ip daddr @divert_0 tcp dport 443 dnat ip to 10.4.1.1:15443\n"+
		"\t\tiifname \"br-ebd625d78bb2\" ip daddr @divert_1 tcp dport 8443 dnat ip to 10.4.1.1:15443\n"; got != want {
		t.Errorf("divert chain =\n%s\nwant\n%s", got, want)
	}
	// What the divert chain missed never leaves by the forward path: a flow
	// tracked before its address joined the set, or UDP such as QUIC.
	if got, want := chain(t, script, "divert_fwd"), "\t\ttype filter hook forward priority filter - 10; policy accept;\n"+
		"\t\tiifname != \"br-ebd625d78bb2\" return\n"+
		"\t\tip daddr @divert_0 meta l4proto { tcp, udp } th dport 443 goto refuse\n"+
		"\t\tip daddr @divert_1 meta l4proto { tcp, udp } th dport 8443 goto refuse\n"; got != want {
		t.Errorf("divert_fwd chain =\n%s\nwant\n%s", got, want)
	}
	// The relay takes redirected connections from this bridge alone. A guest
	// that dials it directly, or another bridge, is refused.
	want := `iifname != "br-ebd625d78bb2" ip daddr 10.4.1.1 meta l4proto { tcp, udp } th dport 53 drop
iifname != "br-ebd625d78bb2" ip daddr 10.4.1.1 tcp dport 15443 drop
iifname != "br-ebd625d78bb2" return
ip saddr != 10.4.1.0/24 drop
ip daddr 10.4.1.1 meta l4proto { tcp, udp } th dport 53 accept
ip daddr 10.4.1.1 tcp dport 15443 ct status dnat accept
ip daddr 10.4.1.1 icmp type echo-request accept
goto refuse
`
	body := chain(t, script, "guest_in")
	if got := strings.ReplaceAll(body[strings.Index(body, "iifname"):], "\t", ""); got != want {
		t.Errorf("guest_in chain =\n%s\nwant\n%s", got, want)
	}
	if !strings.Contains(script, "\tchain egress {") {
		t.Error("a divert under a policy lost the egress chain")
	}
}

// Without a policy, the table diverts and refuses nothing else.
func TestRulesetDivertsWithoutAPolicy(t *testing.T) {
	script, err := Ruleset("t", divertNet, Unfiltered(), testMark)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "\tchain egress {") {
		t.Errorf("a table with no policy has an egress chain:\n%s", script)
	}
	for _, c := range []string{"dns", "divert", "divert_fwd", "guest_in", "refuse"} {
		chain(t, script, c)
	}
	if _, err := Ruleset("t", testNet, Unfiltered(), testMark); err == nil {
		t.Error("Ruleset accepted a table with no policy and no divert")
	}
}

// A sandbox with no divert gets the table it got before diverts existed.
func TestRulesetWithoutADivertHasNoDivertRule(t *testing.T) {
	for _, def := range []string{"allow", "deny"} {
		script, err := Ruleset("t", testNet, mustParse(t, def, []string{"host=example.com"}, nil), testMark)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(script, "divert") || strings.Contains(script, "15443") {
			t.Errorf("%s: a ruleset with no divert mentions one:\n%s", def, script)
		}
	}
}

func TestRulesetRefusesABadDivert(t *testing.T) {
	p := mustParse(t, "deny", nil, nil)
	for name, edit := range map[string]func(*Network){
		"relay on port 0":  func(n *Network) { n.Divert.Port = 0 },
		"relay on port 53": func(n *Network) { n.Divert.Port = 53 },
		"route on port 53": func(n *Network) { n.Divert.Routes = []divert.Route{{Host: "a.test", Port: 53}} },
		"route on port 0":  func(n *Network) { n.Divert.Routes = []divert.Route{{Host: "a.test", Port: 0}} },
	} {
		n := divertNet
		edit(&n)
		if _, err := Ruleset("t", n, p, testMark); err == nil {
			t.Errorf("%s: Ruleset accepted it", name)
		}
	}
}

func TestDivertScript(t *testing.T) {
	got := DivertScript("t", 1, []netip.Addr{
		netip.MustParseAddr("1.2.3.4"), netip.MustParseAddr("::1"), netip.MustParseAddr("1.2.3.4"), netip.MustParseAddr("5.6.7.8"),
	})
	if want := "add element inet t divert_1 { 1.2.3.4, 5.6.7.8 }\n"; got != want {
		t.Errorf("DivertScript = %q, want %q", got, want)
	}
	if DivertScript("t", 0, []netip.Addr{netip.MustParseAddr("::1")}) != "" {
		t.Error("an IPv6-only divert produced a script")
	}
}

// The divert ruleset loads in a real nft, where an unprivileged network
// namespace can be had.
func TestDivertRulesetLoadsInNFT(t *testing.T) {
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		t.Skip("no unshare")
	}
	nft, err := exec.LookPath("nft")
	if err != nil {
		t.Skip("no nft")
	}
	if err := exec.Command(unshare, "-rn", "true").Run(); err != nil {
		t.Skipf("no unprivileged network namespace: %v", err)
	}
	for _, p := range []*Policy{Unfiltered(), mustParse(t, "deny", []string{"host=api.anthropic.com"}, nil)} {
		script, err := Ruleset("brig_egress_test", divertNet, p, testMark)
		if err != nil {
			t.Fatal(err)
		}
		script += DivertScript("brig_egress_test", 0, []netip.Addr{netip.MustParseAddr("160.79.104.10")})
		script += DivertScript("brig_egress_test", 0, []netip.Addr{netip.MustParseAddr("160.79.104.10")})
		cmd := exec.Command(unshare, "-rn", nft, "-f", "-")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("default %q: nft refused the ruleset: %v\n%s", p.Default, err, out)
		}
	}
}

// named is a resource record with an owner name of its own.
type named struct {
	owner string
	rtype uint16
	data  []byte
}

// buildChain answers query with records that name their owners in full.
func buildChain(query []byte, answers []named) []byte {
	q, err := parseQuery(query)
	if err != nil {
		panic(err)
	}
	msg := append([]byte(nil), query[:q.end]...)
	msg[2] |= 0x80
	msg[3] = 0x80
	binary.BigEndian.PutUint16(msg[6:], uint16(len(answers)))
	for _, r := range answers {
		msg = appendName(msg, r.owner)
		msg = binary.BigEndian.AppendUint16(msg, r.rtype)
		msg = binary.BigEndian.AppendUint16(msg, classIN)
		msg = binary.BigEndian.AppendUint32(msg, 300)
		msg = binary.BigEndian.AppendUint16(msg, uint16(len(r.data)))
		msg = append(msg, r.data...)
	}
	return msg
}

func cname(owner, target string) named { return named{owner, typeCNAME, appendName(nil, target)} }

func a(owner, addr string) named {
	b := netip.MustParseAddr(addr).As4()
	return named{owner, typeA, b[:]}
}

func TestChainAddrsFollowsCNAMEs(t *testing.T) {
	// The diverted host is an alias of an edge name.
	resp := buildChain(buildQuery(1, "api.anthropic.com", typeA), []named{
		cname("api.anthropic.com", "edge.example.net"),
		a("edge.example.net", "160.79.104.10"),
		a("unrelated.test", "192.0.2.1"),
	})
	got, err := chainAddrs(resp, "API.Anthropic.com.")
	if err != nil || len(got) != 1 || got[0].String() != "160.79.104.10" {
		t.Errorf("diverted host: %v, %v", got, err)
	}
	// The guest asks for an alias of the diverted host.
	resp = buildChain(buildQuery(2, "alias.test", typeA), []named{
		cname("alias.test", "api.anthropic.com"),
		a("api.anthropic.com", "160.79.104.11"),
	})
	if got, err := chainAddrs(resp, "api.anthropic.com"); err != nil || len(got) != 1 || got[0].String() != "160.79.104.11" {
		t.Errorf("alias: %v, %v", got, err)
	}
	if got, _ := chainAddrs(resp, "other.test"); len(got) != 0 {
		t.Errorf("a host the answer does not name got %v", got)
	}
	// A loop ends.
	resp = buildChain(buildQuery(3, "a.test", typeA), []named{cname("a.test", "b.test"), cname("b.test", "a.test")})
	if got, err := chainAddrs(resp, "a.test"); err != nil || len(got) != 0 {
		t.Errorf("loop: %v, %v", got, err)
	}
	// The usual answer, owners compressed to the question.
	resp = buildResponse(buildQuery(4, "api.anthropic.com", typeA), []rr{aRecord("160.79.104.12", 60)}, nil)
	if got, err := chainAddrs(resp, "api.anthropic.com"); err != nil || len(got) != 1 {
		t.Errorf("compressed: %v, %v", got, err)
	}
}

func divertResolver(t *testing.T, nft []string, answer string) *Resolver {
	t.Helper()
	asked := 0
	return &Resolver{
		Policy:   Unfiltered(),
		Upstream: []string{"up:53"},
		Pins:     &fakePins{},
		Diverts: &Diverts{Set: divert.NewSet([]divert.Route{{Host: "api.anthropic.com", Port: 443}}),
			Table: "t", NFT: nft},
		exchange: upstreamAnswering(answer, &asked),
	}
}

// The guest gets the address of a diverted host only once the relay and the
// table divert it.
func TestAnswerDivertsBeforeTheGuestLearnsTheAddress(t *testing.T) {
	bin, scripts, _, _ := fakeNFT(t)
	r := divertResolver(t, []string{bin}, "160.79.104.10")
	resp := r.Answer(context.Background(), buildQuery(1, "api.anthropic.com", typeA), "udp", guest)
	if rcode(resp) != 0 || ancount(resp) != 1 {
		t.Fatalf("rcode %d, %d answers", rcode(resp), ancount(resp))
	}
	if !r.Diverts.Set.Match(netip.MustParseAddrPort("160.79.104.10:443")) {
		t.Error("the relay does not divert the answered address")
	}
	blob, _ := os.ReadFile(scripts)
	if !strings.Contains(string(blob), "add element inet t divert_0 { 160.79.104.10 }") {
		t.Errorf("the table was not given the address:\n%s", blob)
	}
	// Under no policy, any other name is answered and diverts nothing.
	resp = r.Answer(context.Background(), buildQuery(2, "example.com", typeA), "udp", guest)
	if rcode(resp) != 0 || len(r.Diverts.Set.Addrs(0)) != 1 {
		t.Errorf("another name: rcode %d, divert set %v", rcode(resp), r.Diverts.Set.Addrs(0))
	}
}

// A guest must never learn an address of a diverted host that the table would
// route directly. A divert that fails fails the query.
func TestAnswerFailsClosedWhenTheDivertFails(t *testing.T) {
	r := divertResolver(t, []string{"/nonexistent/nft"}, "160.79.104.10")
	resp := r.Answer(context.Background(), buildQuery(1, "api.anthropic.com", typeA), "udp", guest)
	if rcode(resp) != rcodeServFail || ancount(resp) != 0 {
		t.Errorf("rcode %d, %d answers; want SERVFAIL and no answer", rcode(resp), ancount(resp))
	}
}

func TestUnfilteredAnswersEveryName(t *testing.T) {
	p := Unfiltered()
	if p.Filters() || !p.AllowsQuery("anything.test") || p.HasHostRules() {
		t.Errorf("Unfiltered = %+v", p)
	}
	if !mustParse(t, "deny", nil, nil).Filters() {
		t.Error("a deny policy does not filter")
	}
}

func TestMainRefusesABadDivert(t *testing.T) {
	base := []string{"--table", "t", "--bridge", "br-x", "--listen", "10.4.1.1", "--subnet", "10.4.1.0/24",
		"--upstream", "10.0.2.3:53", "--mark", "7"}
	for name, extra := range map[string][]string{
		"no socket":           {"--divert", "api.anthropic.com:443"},
		"a relative socket":   {"--divert", "api.anthropic.com:443", "--divert-socket", "s.sock"},
		"a socket, no divert": {"--egress-default", "deny", "--divert-socket", "/run/s.sock"},
		"IPv6":                {"--divert", "[2001:db8::1]:443", "--divert-socket", "/run/s.sock"},
		"port 53":             {"--divert", "dns.test:53", "--divert-socket", "/run/s.sock"},
		"relay on port 0":     {"--divert", "a.test:443", "--divert-socket", "/run/s.sock", "--divert-port", "0"},
		"rules, no default":   {"--divert", "a.test:443", "--divert-socket", "/run/s.sock", "--egress-allow", "host=a.test"},
		"nothing to enforce":  nil,
	} {
		var stderr bytes.Buffer
		if code := Main(append(append([]string(nil), base...), extra...), &stderr); code != 1 {
			t.Errorf("%s: exit %d, want 1:\n%s", name, code, stderr.String())
		}
	}
}

// A table that went away took its divert sets with it. Its reinstall puts
// the addresses back in the same nft transaction as the table.
func TestTableKeeperPutsTheDivertsBack(t *testing.T) {
	bin, scripts, gone, _ := fakeNFT(t)
	d := &Diverts{Set: divert.NewSet([]divert.Route{{Host: "api.anthropic.com", Port: 443}}), Table: "t", NFT: []string{bin}}
	if _, err := d.Set.Add(0, []netip.Addr{netip.MustParseAddr("160.79.104.10")}); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	k := &tableKeeper{nft: []string{bin}, table: "t", script: "add table inet t\n", bridge: "br-x",
		log: log.New(&logged, "", 0), diverts: d}
	if err := k.install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gone, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { k.run(ctx, 10*time.Millisecond); close(done) }()
	deadline := time.Now().Add(4 * time.Second)
	for {
		if _, err := os.Stat(gone); errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the table was never installed again:\n%s", logged.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	blob, _ := os.ReadFile(scripts)
	want := "add table inet t\nadd element inet t divert_0 { 160.79.104.10 }\n"
	if n := strings.Count(string(blob), want); n != 2 {
		t.Errorf("%d installs carried the diverts in the same script, want 2:\n%s", n, blob)
	}
}

// brig boots the guest once the resolver logs that it is up. By then the
// diverted host's own addresses are diverted and the relay listens.
func TestServeIsUpOnlyOnceTheDivertIsReady(t *testing.T) {
	defer func(prev uint16) { dnsPort = prev }(dnsPort)
	dnsPort = 0
	bin, scripts, _, _ := fakeNFT(t)
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(probe.Addr().(*net.TCPAddr).Port)
	_ = probe.Close()

	n := Network{Gateway: netip.MustParseAddr("127.0.0.1"),
		Divert: Divert{Routes: []divert.Route{{Host: "api.anthropic.com", Port: 443}}, Port: port}}
	d := &Diverts{Set: divert.NewSet(n.Divert.Routes), Table: "t", NFT: []string{bin}}
	r := &Resolver{Policy: Unfiltered(), Upstream: []string{fakeUpstreamAfter(t, "160.79.104.10", 100*time.Millisecond)},
		Pins: &fakePins{}, Diverts: d}
	logged := &syncBuffer{}
	logger := log.New(logged, "", 0)
	k := &tableKeeper{nft: []string{bin}, table: "t", script: "add table inet t\n", bridge: "br-x", log: logger, diverts: d}
	rl := &divert.Relay{Socket: "/nonexistent/s.sock", Set: d.Set}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, k, r, rl, n, RefreshInterval, 0, 0, logger) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logged.String(), "egress resolver on ") {
		if time.Now().After(deadline) {
			t.Fatalf("the resolver never came up:\n%s", logged.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !d.Set.Match(netip.MustParseAddrPort("160.79.104.10:443")) {
		t.Error("the resolver was up before the diverted host was diverted")
	}
	blob, _ := os.ReadFile(scripts)
	if !strings.HasPrefix(string(blob), "add table inet t\nadd element inet t divert_0 { 160.79.104.10 }\n") {
		t.Errorf("the first install lacks the diverted host:\n%s", blob)
	}
	// The relay resets a connection it cannot hand to the injector. A reset
	// that wins the race with the dial still means the relay listened.
	c, err := net.Dial("tcp4", netip.AddrPortFrom(n.Gateway, port).String())
	if err != nil && !errors.Is(err, syscall.ECONNRESET) {
		t.Errorf("the relay does not listen: %v", err)
	} else if err == nil {
		_ = c.Close()
	}
	cancel()
	if err := <-served; err != nil {
		t.Errorf("serve returned %v", err)
	}
}

// A diverted host that does not resolve at the start fails the start, and brig
// refuses the boot.
func TestServeRefusesABoundHostThatDoesNotResolve(t *testing.T) {
	bin, _, _, _ := fakeNFT(t)
	n := Network{Gateway: netip.MustParseAddr("127.0.0.1"),
		Divert: Divert{Routes: []divert.Route{{Host: "api.anthropic.com", Port: 443}}, Port: DivertPort}}
	d := &Diverts{Set: divert.NewSet(n.Divert.Routes), Table: "t", NFT: []string{bin}}
	// Nothing listens on the upstream.
	probe, _ := net.Listen("tcp4", "127.0.0.1:0")
	up := probe.Addr().String()
	_ = probe.Close()
	r := &Resolver{Policy: Unfiltered(), Upstream: []string{up}, Pins: &fakePins{}, Diverts: d}
	k := &tableKeeper{nft: []string{bin}, table: "t", script: "add table inet t\n", bridge: "br-x",
		log: log.New(&bytes.Buffer{}, "", 0), diverts: d}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := serve(ctx, k, r, &divert.Relay{Set: d.Set}, n, RefreshInterval, 0, 0, log.New(&bytes.Buffer{}, "", 0))
	if err == nil || !strings.Contains(err.Error(), "divert api.anthropic.com:443") {
		t.Errorf("serve = %v; want a refusal naming the route", err)
	}
}

// A divert by address learns nothing from DNS, so the guest's DNS is left as
// a table with no divert leaves it: what it sends to the bridge address or to
// a resolver of its resolv.conf goes to brig's resolver, which answers
// brig.internal, and DNS to any other server is ordinary traffic under the
// policy. Only a divert by host name takes all of the guest's DNS.
func TestAnAddressDivertLeavesTheGuestsDNSAlone(t *testing.T) {
	n := testNet
	n.Divert = Divert{Routes: []divert.Route{{Host: n.Gateway.String(), Port: 443}}, Port: DivertPort}
	plain, err := Ruleset("t", testNet, mustParse(t, "deny", []string{"cidr=1.1.1.1/32"}, nil), testMark)
	if err != nil {
		t.Fatal(err)
	}
	want := chain(t, plain, "dns")
	for _, p := range []*Policy{Unfiltered(), mustParse(t, "deny", []string{"cidr=1.1.1.1/32"}, nil)} {
		script, err := Ruleset("t", n, p, testMark)
		if err != nil {
			t.Fatal(err)
		}
		if got := chain(t, script, "dns"); got != want {
			t.Errorf("default %q: dns chain =\n%s\nwant the one of a table with no divert\n%s", p.Default, got, want)
		}
		if strings.Contains(script, "ip daddr != ") {
			t.Errorf("default %q: the table takes DNS to every server:\n%s", p.Default, script)
		}
	}
	n.Divert.Routes = append(n.Divert.Routes, divert.Route{Host: "api.example.com", Port: 443})
	script, err := Ruleset("t", n, Unfiltered(), testMark)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(chain(t, script, "dns"), "ip daddr != 10.4.1.1 meta l4proto { tcp, udp } th dport 53 dnat ip to 10.4.1.1:53") {
		t.Errorf("a divert by host name does not take all of the guest's DNS:\n%s", script)
	}
}

// brig.internal on nerdctl is the bridge's own address, diverted on port 443
// alone. The divert chain redirects that port to the relay, and the guest's
// input chain refuses every other port of the bridge address but DNS and the
// relay's redirected connections. The relay matches that port alone, and no
// other address.
func TestRulesetDivertsBrigInternalOnPort443Alone(t *testing.T) {
	n := testNet
	gw := n.Gateway
	n.Divert = Divert{Routes: []divert.Route{{Host: gw.String(), Port: 443}}, Port: DivertPort}
	for _, p := range []*Policy{Unfiltered(), mustParse(t, "deny", nil, nil)} {
		script, err := Ruleset("t", n, p, testMark)
		if err != nil {
			t.Fatal(err)
		}
		if got := chain(t, script, "divert"); !strings.Contains(got,
			`iifname "br-ebd625d78bb2" ip daddr @divert_0 tcp dport 443 dnat ip to 10.4.1.1:15443`) ||
			strings.Count(got, "dnat") != 1 {
			t.Errorf("default %q: divert chain:\n%s", p.Default, got)
		}
		in := chain(t, script, "guest_in")
		for _, want := range []string{"ip daddr 10.4.1.1 meta l4proto { tcp, udp } th dport 53 accept",
			"ip daddr 10.4.1.1 tcp dport 15443 ct status dnat accept", "goto refuse"} {
			if !strings.Contains(in, want) {
				t.Errorf("default %q: guest_in lacks %q:\n%s", p.Default, want, in)
			}
		}
		if strings.Contains(in, "dport 443 accept") || strings.Contains(in, "dport 80") {
			t.Errorf("default %q: guest_in admits another port of the bridge address:\n%s", p.Default, in)
		}
	}
	set := divert.NewSet(n.Divert.Routes)
	for dst, want := range map[string]bool{"10.4.1.1:443": true, "10.4.1.1:80": false, "10.4.1.1:8443": false,
		"10.4.1.2:443": false, "160.79.104.10:443": false} {
		if got := set.Match(netip.MustParseAddrPort(dst)); got != want {
			t.Errorf("Match(%s) = %v, want %v", dst, got, want)
		}
	}
	if s := DivertScript("t", 0, set.Addrs(0)); s != "add element inet t divert_0 { 10.4.1.1 }\n" {
		t.Errorf("DivertScript = %q", s)
	}
}
