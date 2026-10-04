package egress

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The nftables side. Each sandbox under a policy gets a table of its own in
// the network namespace that routes its bridge. One table per sandbox lets a
// boot replace the rules, and a stop remove them, in one transaction that
// touches no other sandbox.
//
// The chains hook in at a priority below iptables' filter and nat chains. A
// drop or a reject in any base chain is final, so the CNI plugins' own accept
// rules cannot undo a verdict made here.

// Set names. The resolver adds to the four address sets and the kernel
// expires what it adds. The two network sets hold the cidr rules and never
// change while the sandbox runs.
const (
	// SetAllowIP and SetDenyIP hold the addresses of the answers the guest
	// asked for.
	SetAllowIP = "allow_ip"
	SetDenyIP  = "deny_ip"
	// SetAllowHost and SetDenyHost hold the addresses the resolver looks up
	// for the rules that name one host. They are separate from the answer
	// sets so that a refresh never shortens the timeout of an answer, or the
	// other way round.
	SetAllowHost = "allow_host"
	SetDenyHost  = "deny_host"
	setAllowNet  = "allow_net"
	setDenyNet   = "deny_net"
	// setAlive holds the bridge name while the resolver runs. The resolver
	// adds it again every HeartbeatEvery, and it times out after
	// HeartbeatLapse.
	setAlive = "alive"
)

// The heartbeat. A table outlives a resolver that died, and without the
// resolver the deny pins lapse while an allow default still admits whatever
// they covered. So a new connection is refused once the bridge name times
// out of setAlive.
const (
	HeartbeatEvery = 5 * time.Second
	HeartbeatLapse = 15 * time.Second
)

// TablePrefix starts the name of every table brig installs, so a sweep can
// tell its own tables from anyone else's.
const TablePrefix = "brig_egress_"

// linkLocal is never reachable from a sandbox under a policy. hull's gateway
// refuses it with or without a policy. The cloud metadata address lives in it.
var linkLocal = netip.MustParsePrefix("169.254.0.0/16")

var tableNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// TableName returns the name of the table for one sandbox.
func TableName(sandbox string) (string, error) {
	rest := strings.TrimPrefix(sandbox, "brig-")
	if !tableNameRe.MatchString(rest) {
		return "", fmt.Errorf("sandbox name %q cannot name an nftables table", sandbox)
	}
	return TablePrefix + rest, nil
}

// Network is the sandbox network the rules apply to.
type Network struct {
	// Bridge is the interface the sandbox's traffic arrives on.
	Bridge string
	// Gateway is the bridge's own address. The resolver listens on port 53
	// there.
	Gateway netip.Addr
	// Subnet is the sandbox network. A packet from the bridge with a source
	// outside it is dropped.
	Subnet netip.Prefix
	// Redirect is the resolvers the runtime writes into the guest's
	// resolv.conf beside Gateway. DNS sent to them goes to the resolver.
	// DNS sent to any other server is ordinary traffic under the policy, as
	// it is on hull's gateway.
	Redirect []netip.Addr
}

var ifaceRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)

// Ruleset returns the nft script that installs the table for one sandbox,
// replacing any table of that name in the same transaction.
//
// mark tags the connections this table admitted, in the upper 16 bits of the
// conntrack mark. Only a connection with this boot's mark skips the rules. A
// connection the kernel still tracks from an earlier boot of the sandbox is
// judged again under the rules of this one.
func Ruleset(table string, n Network, p *Policy, mark uint16) (string, error) {
	if !ifaceRe.MatchString(n.Bridge) {
		return "", fmt.Errorf("%q is not an interface name", n.Bridge)
	}
	if !n.Gateway.Is4() {
		return "", fmt.Errorf("bridge address %s is not IPv4", n.Gateway)
	}
	if !n.Subnet.Addr().Is4() || !n.Subnet.Contains(n.Gateway) {
		return "", fmt.Errorf("subnet %s does not hold the bridge address %s", n.Subnet, n.Gateway)
	}
	if mark == 0 {
		return "", fmt.Errorf("the connection mark is zero, which every untagged connection carries")
	}
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	br := `"` + n.Bridge + `"`
	gw := n.Gateway.String()
	subnet := n.Subnet.Masked().String()

	// add-then-delete makes the delete succeed whether or not a table of this
	// name exists, so the script installs and reinstalls alike.
	w("add table inet %s", table)
	w("delete table inet %s", table)
	w("table inet %s {", table)
	for _, set := range []string{SetAllowIP, SetDenyIP, SetAllowHost, SetDenyHost} {
		w("\tset %s { type ipv4_addr; flags timeout; }", set)
	}
	w("\tset %s { type ipv4_addr; flags interval;%s }", setAllowNet, elements(p.allow.v4CIDRs()))
	w("\tset %s { type ipv4_addr; flags interval;%s }", setDenyNet, elements(append([]netip.Prefix{linkLocal}, p.deny.v4CIDRs()...)))
	w("\tset %s { type ifname; flags timeout; }", setAlive)

	var redirect []string
	for _, a := range n.Redirect {
		if a.Is4() && !a.IsLoopback() && a != n.Gateway && !slices.Contains(redirect, a.String()) {
			redirect = append(redirect, a.String())
		}
	}
	if len(redirect) > 0 {
		w("\tchain dns {")
		w("\t\ttype nat hook prerouting priority dstnat - 10; policy accept;")
		w("\t\tiifname %s ip daddr { %s } meta l4proto { tcp, udp } th dport 53 dnat ip to %s:53",
			br, strings.Join(redirect, ", "), gw)
		w("\t}")
	}

	// The guest reaches the host side of its network for DNS and ping to the
	// bridge address alone. Nothing here accepts by conntrack state: a
	// packet the guest forges to match one of the namespace's own flows, such
	// as the resolver's upstream queries, would pass that test. And only this
	// sandbox asks its resolver.
	w("\tchain guest_in {")
	w("\t\ttype filter hook input priority filter - 10; policy accept;")
	w("\t\tiifname != %s ip daddr %s meta l4proto { tcp, udp } th dport 53 drop", br, gw)
	w("\t\tiifname != %s return", br)
	w("\t\tip saddr != %s drop", subnet)
	w("\t\tip daddr %s meta l4proto { tcp, udp } th dport 53 accept", gw)
	w("\t\tip daddr %s icmp type echo-request accept", gw)
	w("\t\tgoto refuse")
	w("\t}")

	// A connection is judged on its first packet. Deny beats allow, and allow
	// beats the default. hull's gateway carries TCP and UDP, and answers
	// ping. Here ping reaches an address the rules admit, and every other
	// protocol is refused.
	w("\tchain egress {")
	w("\t\ttype filter hook forward priority filter - 10; policy accept;")
	w("\t\tiifname != %s return", br)
	w("\t\tmeta nfproto ipv6 goto refuse")
	w("\t\tip saddr != %s drop", subnet)
	w("\t\tct state established,related ct mark and 0xffff0000 == 0x%04x0000 accept", mark)
	w("\t\tiifname != @%s goto refuse", setAlive)
	w("\t\tmeta l4proto != { tcp, udp, icmp } goto refuse")
	for _, set := range []string{setDenyNet, SetDenyIP, SetDenyHost} {
		w("\t\tip daddr @%s goto refuse", set)
	}
	for _, set := range []string{setAllowNet, SetAllowIP, SetAllowHost} {
		w("\t\tip daddr @%s goto admit", set)
	}
	if p.Default == "allow" {
		w("\t\tgoto admit")
	} else {
		w("\t\tgoto refuse")
	}
	w("\t}")

	// The mark keeps the lower 16 bits, which anything else in the namespace
	// may use.
	w("\tchain admit {")
	w("\t\tct mark set ct mark and 0x0000ffff or 0x%04x0000 accept", mark)
	w("\t}")

	// TCP gets a reset, as on hull's gateway, so a refused connection fails at
	// once. Everything else is dropped.
	w("\tchain refuse {")
	w("\t\tmeta l4proto tcp reject with tcp reset")
	w("\t\tdrop")
	w("\t}")
	w("}")
	return b.String(), nil
}

// DeleteScript returns the nft script that removes one sandbox's table, and
// succeeds when there is none.
func DeleteScript(table string) string {
	return fmt.Sprintf("add table inet %s\ndelete table inet %s\n", table, table)
}

// PinScript returns the nft script that puts addrs in a set for lifetime.
//
// An add does not refresh the timeout of an element that is already in the
// set. So the script adds, deletes and adds again, in one transaction: the
// first add makes the delete valid, and the last add starts a fresh timeout.
// An address listed twice would fail the delete, and the whole transaction
// with it, so each address appears once.
func PinScript(table, set string, addrs []netip.Addr, lifetime time.Duration) string {
	var timed, plain []string
	secs := int(lifetime / time.Second)
	for _, a := range addrs {
		if !a.Is4() || slices.Contains(plain, a.String()) {
			continue
		}
		timed = append(timed, fmt.Sprintf("%s timeout %ds", a, secs))
		plain = append(plain, a.String())
	}
	if len(timed) == 0 {
		return ""
	}
	return refreshScript(table, set, "{ "+strings.Join(timed, ", ")+" }", "{ "+strings.Join(plain, ", ")+" }")
}

// HeartbeatScript returns the nft script that keeps bridge in the table's
// alive set for lapse.
func HeartbeatScript(table, bridge string, lapse time.Duration) string {
	name := `"` + bridge + `"`
	return refreshScript(table, setAlive,
		fmt.Sprintf("{ %s timeout %ds }", name, int(lapse/time.Second)), "{ "+name+" }")
}

func refreshScript(table, set, timed, plain string) string {
	return fmt.Sprintf("add element inet %[1]s %[2]s %[3]s\ndelete element inet %[1]s %[2]s %[4]s\nadd element inet %[1]s %[2]s %[3]s\n",
		table, set, timed, plain)
}

// elements returns the elements clause of an interval set. nft refuses two
// overlapping intervals in one set, so a prefix inside another is dropped.
// Two prefixes either nest or are disjoint, so this leaves no overlap.
func elements(prefixes []netip.Prefix) string {
	sorted := slices.Clone(prefixes)
	slices.SortFunc(sorted, func(a, b netip.Prefix) int { return a.Bits() - b.Bits() })
	var kept []string
	var wide []netip.Prefix
	for _, p := range sorted {
		if slices.ContainsFunc(wide, func(w netip.Prefix) bool { return w.Contains(p.Addr()) }) {
			continue
		}
		wide = append(wide, p)
		kept = append(kept, p.String())
	}
	if len(kept) == 0 {
		return ""
	}
	return " elements = { " + strings.Join(kept, ", ") + " };"
}

// NFT runs nft in the network namespace the process runs in. The resolver
// runs inside the namespace it filters, so a plain nft reaches the right
// tables.
type NFT struct {
	// Bin is the nft binary.
	Bin string
	// Table is the sandbox's table.
	Table string
}

// Pin adds addrs to set for lifetime.
func (n NFT) Pin(set string, addrs []netip.Addr, lifetime time.Duration) error {
	script := PinScript(n.Table, set, addrs, lifetime)
	if script == "" {
		return nil
	}
	return RunNFT(context.Background(), []string{n.Bin}, script)
}

// RunNFT runs an nft script through argv, which is nft itself or a command
// that enters a namespace and then runs nft.
func RunNFT(ctx context.Context, argv []string, script string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], append(argv[1:], "-f", "-")...)
	cmd.Stdin = strings.NewReader(script)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return fmt.Errorf("nft: %w: %s", err, msg)
		}
		return fmt.Errorf("nft: %w", err)
	}
	return nil
}
