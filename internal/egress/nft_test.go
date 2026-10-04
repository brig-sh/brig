package egress

import (
	"net/netip"
	"os/exec"
	"strings"
	"testing"
	"time"
)

var testNet = Network{Bridge: "br-ebd625d78bb2", Gateway: netip.MustParseAddr("10.4.1.1"),
	Subnet:   netip.MustParsePrefix("10.4.1.0/24"),
	Redirect: []netip.Addr{netip.MustParseAddr("10.0.2.3"), netip.MustParseAddr("127.0.0.53"), netip.MustParseAddr("10.4.1.1")}}

const testMark = 0xbeef

// chain returns the body of one chain of a ruleset.
func chain(t *testing.T, script, name string) string {
	t.Helper()
	_, body, ok := strings.Cut(script, "\tchain "+name+" {\n")
	if !ok {
		t.Fatalf("ruleset has no chain %s:\n%s", name, script)
	}
	body, _, _ = strings.Cut(body, "\t}\n")
	return body
}

func TestRulesetDenyDefault(t *testing.T) {
	p := mustParse(t, "deny",
		[]string{"host=api.anthropic.com", "cidr=1.1.1.0/24", "cidr=2001:db8::/32"},
		[]string{"cidr=1.1.1.2/32", "cidr=169.254.169.254/32"})
	script, err := Ruleset("brig_egress_ubuntu-egpoc", testNet, p, testMark)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"add table inet brig_egress_ubuntu-egpoc\ndelete table inet brig_egress_ubuntu-egpoc\n",
		// Only the resolvers the runtime configured, never loopback or the
		// gateway itself. DNS to any other server meets the filter.
		`iifname "br-ebd625d78bb2" ip daddr { 10.0.2.3 } meta l4proto { tcp, udp } th dport 53 dnat ip to 10.4.1.1:53`,
		"set allow_net { type ipv4_addr; flags interval; elements = { 1.1.1.0/24 }; }",
		// The metadata address sits inside 169.254.0.0/16, and nft refuses
		// overlapping intervals in one set.
		"set deny_net { type ipv4_addr; flags interval; elements = { 169.254.0.0/16, 1.1.1.2/32 }; }",
		"set allow_host { type ipv4_addr; flags timeout; }",
		"set alive { type ifname; flags timeout; }",
		"\tchain admit {\n\t\tct mark set ct mark and 0x0000ffff or 0xbeef0000 accept\n\t}\n",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("ruleset lacks %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "2001:db8") {
		t.Error("an IPv6 prefix reached an ipv4_addr set")
	}

	// The order is the policy: spoofed sources and IPv6 first, then the
	// connections this boot admitted, then a dead resolver, then deny before
	// allow before the default.
	want := `iifname != "br-ebd625d78bb2" return
meta nfproto ipv6 goto refuse
ip saddr != 10.4.1.0/24 drop
ct state established,related ct mark and 0xffff0000 == 0xbeef0000 accept
iifname != @alive goto refuse
meta l4proto != { tcp, udp, icmp } goto refuse
ip daddr @deny_net goto refuse
ip daddr @deny_ip goto refuse
ip daddr @deny_host goto refuse
ip daddr @allow_net goto admit
ip daddr @allow_ip goto admit
ip daddr @allow_host goto admit
goto refuse
`
	body := chain(t, script, "egress")
	if got := strings.ReplaceAll(body[strings.Index(body, "iifname"):], "\t", ""); got != want {
		t.Errorf("egress chain =\n%s\nwant\n%s", got, want)
	}
}

func TestRulesetAllowDefault(t *testing.T) {
	script, err := Ruleset("t", testNet, mustParse(t, "allow", nil, []string{"host=example.com"}), testMark)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(chain(t, script, "egress"), "ip daddr @allow_host goto admit\n\t\tgoto admit\n") {
		t.Errorf("allow default does not end in admit:\n%s", script)
	}
	// The link-local range is refused under either default.
	if !strings.Contains(script, "set deny_net { type ipv4_addr; flags interval; elements = { 169.254.0.0/16 }; }") {
		t.Errorf("allow default lets the link-local range through:\n%s", script)
	}
	if !strings.Contains(script, "set allow_net { type ipv4_addr; flags interval; }") {
		t.Errorf("empty set carries elements:\n%s", script)
	}
	noRedirect := testNet
	noRedirect.Redirect = nil
	if script, _ := Ruleset("t", noRedirect, mustParse(t, "allow", nil, nil), testMark); strings.Contains(script, "dnat") {
		t.Errorf("a network with nothing to redirect has a dnat rule:\n%s", script)
	}
}

// The guest's packets to the namespace's own addresses reach the resolver and
// ping, and nothing else. A conntrack rule here would also admit a forged
// reply to one of the namespace's own flows, such as the resolver's upstream
// queries.
func TestRulesetGuestReachesOnlyItsResolver(t *testing.T) {
	for _, def := range []string{"allow", "deny"} {
		script, err := Ruleset("t", testNet, mustParse(t, def, nil, nil), testMark)
		if err != nil {
			t.Fatal(err)
		}
		want := `iifname != "br-ebd625d78bb2" ip daddr 10.4.1.1 meta l4proto { tcp, udp } th dport 53 drop
iifname != "br-ebd625d78bb2" return
ip saddr != 10.4.1.0/24 drop
ip daddr 10.4.1.1 meta l4proto { tcp, udp } th dport 53 accept
ip daddr 10.4.1.1 icmp type echo-request accept
goto refuse
`
		body := chain(t, script, "guest_in")
		if got := strings.ReplaceAll(body[strings.Index(body, "iifname"):], "\t", ""); got != want {
			t.Errorf("%s: guest_in chain =\n%s\nwant\n%s", def, got, want)
		}
	}
}

func TestRulesetRefusesBadNetwork(t *testing.T) {
	p := mustParse(t, "deny", nil, nil)
	sub := testNet.Subnet
	for _, n := range []Network{
		{Bridge: `br" ; flush ruleset ; "`, Gateway: testNet.Gateway, Subnet: sub},
		{Bridge: "", Gateway: testNet.Gateway, Subnet: sub},
		{Bridge: "br-0123456789abcdef", Gateway: testNet.Gateway, Subnet: sub},
		{Bridge: testNet.Bridge, Gateway: netip.MustParseAddr("fd00::1"), Subnet: sub},
		{Bridge: testNet.Bridge, Gateway: testNet.Gateway},
		{Bridge: testNet.Bridge, Gateway: testNet.Gateway, Subnet: netip.MustParsePrefix("10.4.2.0/24")},
	} {
		if _, err := Ruleset("t", n, p, testMark); err == nil {
			t.Errorf("Ruleset accepted %+v", n)
		}
	}
	// Every connection the kernel tracks without a mark carries zero.
	if _, err := Ruleset("t", testNet, p, 0); err == nil {
		t.Error("Ruleset accepted a zero mark")
	}
}

// The ruleset loads in a real nft, where an unprivileged network namespace
// can be had. CI hosts often refuse one, and the test skips there.
func TestRulesetLoadsInNFT(t *testing.T) {
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
	for _, def := range []string{"allow", "deny"} {
		p := mustParse(t, def, []string{"host=example.com", "cidr=1.1.1.0/24"}, []string{"cidr=169.254.169.254/32"})
		script, err := Ruleset("brig_egress_test", testNet, p, testMark)
		if err != nil {
			t.Fatal(err)
		}
		script += HeartbeatScript("brig_egress_test", testNet.Bridge, HeartbeatLapse)
		script += PinScript("brig_egress_test", SetAllowIP, []netip.Addr{netip.MustParseAddr("1.2.3.4")}, time.Minute)
		cmd := exec.Command(unshare, "-rn", nft, "-f", "-")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: nft refused the ruleset: %v\n%s", def, err, out)
		}
	}
}

func TestTableName(t *testing.T) {
	if got, err := TableName("brig-ubuntu-egpoc"); err != nil || got != "brig_egress_ubuntu-egpoc" {
		t.Errorf("TableName = %q, %v", got, err)
	}
	for _, bad := range []string{"brig-", "brig-UPPER", "brig-a b", "brig-a;b"} {
		if _, err := TableName(bad); err == nil {
			t.Errorf("TableName(%q) succeeded", bad)
		}
	}
}

func TestPinScript(t *testing.T) {
	got := PinScript("t", SetAllowIP, []netip.Addr{
		netip.MustParseAddr("1.2.3.4"), netip.MustParseAddr("::1"), netip.MustParseAddr("5.6.7.8"),
		// nft fails a delete that names one element twice, and the whole
		// transaction with it.
		netip.MustParseAddr("1.2.3.4"),
	}, 2*time.Minute)
	want := "add element inet t allow_ip { 1.2.3.4 timeout 120s, 5.6.7.8 timeout 120s }\n" +
		"delete element inet t allow_ip { 1.2.3.4, 5.6.7.8 }\n" +
		"add element inet t allow_ip { 1.2.3.4 timeout 120s, 5.6.7.8 timeout 120s }\n"
	if got != want {
		t.Errorf("PinScript =\n%s\nwant\n%s", got, want)
	}
	if PinScript("t", SetAllowIP, []netip.Addr{netip.MustParseAddr("::1")}, time.Minute) != "" {
		t.Error("an IPv6-only pin produced a script")
	}
}

func TestHeartbeatScript(t *testing.T) {
	got := HeartbeatScript("t", "br-x", 15*time.Second)
	want := "add element inet t alive { \"br-x\" timeout 15s }\n" +
		"delete element inet t alive { \"br-x\" }\n" +
		"add element inet t alive { \"br-x\" timeout 15s }\n"
	if got != want {
		t.Errorf("HeartbeatScript =\n%s\nwant\n%s", got, want)
	}
	if HeartbeatEvery*2 >= HeartbeatLapse {
		t.Errorf("one missed beat of %s lapses the %s heartbeat", HeartbeatEvery, HeartbeatLapse)
	}
}
