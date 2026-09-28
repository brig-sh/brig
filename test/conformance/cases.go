package main

import "strings"

// expect is what a case is for, and so which rule judges it.
type expect int

const (
	// expectReach is an allowed control. Every boot has one, and it runs
	// first and last, because a denial proves nothing on a guest that
	// reaches nothing.
	expectReach expect = iota
	// expectDeny passes only on mode, the failure the policy causes. A
	// different failure is somebody else's doing. The run with no policy
	// has to get past that step too, or the case is unproven: a target
	// that is shut without a policy says nothing about the policy.
	expectDeny
	// expectGap is a hole default: allow leaves open and docs/policies.md
	// says so. It is recorded as a known gap when it opens.
	expectGap
	// expectNoRoute is the IPv6 literal. hull's gateway drops every IPv6
	// frame, with or without a policy, so the run with no policy says
	// whether the policy stopped it.
	expectNoRoute
)

func (e expect) String() string {
	switch e {
	case expectReach:
		return "reached"
	case expectGap:
		return "known gap"
	case expectNoRoute:
		return "no route"
	}
	return "denied"
}

// The two policies the suite boots under, each on a guest of its own, and a
// third boot with none.
const (
	denyPolicy  = "conformance-deny"
	allowPolicy = "conformance-allow"
	noPolicy    = ""
)

// policies are written as brig reads them, so the suite goes through brig
// policy attach like a user does. Every name in them resolves in public DNS,
// and each denied target sits outside every allow rule: 1.1.1.2 is inside
// the allowed range, so DoH and DoT go to 8.8.8.8.
var policies = map[string]string{
	denyPolicy: `apiVersion: brig.sh/v1alpha1
name: conformance-deny
desc: network conformance suite, deny default
egress:
  default: deny
  allow:
    - host: example.com
    - host: "*.debian.org"
    - cidr: 1.1.1.0/24
  deny:
    - host: www.debian.org
    - cidr: 1.1.1.1/32
`,
	allowPolicy: `apiVersion: brig.sh/v1alpha1
name: conformance-allow
desc: network conformance suite, allow default
egress:
  default: allow
  deny:
    - host: example.net
    - host: "*.kernel.org"
    - cidr: 9.9.9.9/32
`,
}

// Placeholders the runner fills in on the host before a case runs. hostSvc
// is the listener the suite opens on the host's outbound address. addrOf
// takes a name and gives the first IPv4 address the host resolves it to, so
// the guest can dial a literal it never looked up.
const (
	hostSvc = "{hostsvc}"
	addrOf  = "{addr:"
)

// udpTimeout is how long an alternate resolver case waits. On hvi the first
// questions to an outside resolver after a boot went unanswered for about
// five seconds at times, policy or not, and a later one got a reply. The
// probe's default of 5s then read a slow start as the policy's timeout.
const udpTimeout = "15s"

type testCase struct {
	id     string
	policy string
	title  string
	env    []string
	args   []string
	want   expect
	// mode is the failure the policy causes, for expectDeny and for
	// expectNoRoute once the run with no policy got through.
	mode outcome
	// unprovenWhy says why a guest may not get to this denied case's target
	// even with no policy. Only a case that says why may end unproven. Any
	// other unproven case fails the run, so a run whose denied targets were
	// all shut for some other reason cannot pass.
	unprovenWhy string
}

// cases are the cases from #264, in the order each boot runs them. Order
// matters in the allow boot: the gateway learns a glob's addresses only from
// the names a guest asks it for, so the literal of a glob-denied host goes
// before any lookup of that name.
var cases = []testCase{
	{id: "allowed-name", policy: denyPolicy, title: "a name the policy allows",
		args: []string{"tcp", "example.com:443"}, want: expectReach},
	{id: "allowed-glob", policy: denyPolicy, title: "a name under an allowed glob, beside a denied one",
		args: []string{"tcp", "deb.debian.org:443"}, want: expectReach},
	{id: "allowed-cidr", policy: denyPolicy, title: "an address in an allowed range, beside a denied one",
		args: []string{"tcp", "1.1.1.2:443"}, want: expectReach},
	// The UDP control. The alternate resolver case passes on the silence a
	// dropped datagram gets, and this shows the boot's UDP path works.
	{id: "allowed-resolver", policy: denyPolicy, title: "a resolver in an allowed range, over UDP",
		args: []string{"dns", "-timeout", udpTimeout, "1.1.1.2", "example.com"}, want: expectReach},
	// hull's gateway answers REFUSED for a name the policy denies. An
	// NXDOMAIN is also what it answers when its own lookup failed, so only
	// the REFUSED passes.
	{id: "denied-name", policy: denyPolicy, title: "a name no rule allows",
		args: []string{"tcp", "example.net:443"}, want: expectDeny, mode: resolveRefused},
	{id: "ipv4-literal", policy: denyPolicy, title: "an IPv4 literal no cidr rule matches",
		args: []string{"tcp", "9.9.9.9:443"}, want: expectDeny, mode: connectRefused},
	{id: "ipv6-literal", policy: denyPolicy, title: "an IPv6 literal no cidr rule matches",
		args: []string{"tcp", "[2606:4700:4700::1111]:443"}, want: expectNoRoute, mode: connectRefused},
	{id: "alternate-resolver", policy: denyPolicy, title: "a resolver other than the gateway, over UDP",
		args: []string{"dns", "-timeout", udpTimeout, "8.8.8.8", "example.net"}, want: expectDeny, mode: timedOut},
	{id: "dns-over-https", policy: denyPolicy, title: "DNS over HTTPS",
		args: []string{"doh", "https://8.8.8.8/dns-query", "example.net"}, want: expectDeny, mode: connectRefused},
	{id: "dns-over-tls", policy: denyPolicy, title: "DNS over TLS",
		args: []string{"dot", "8.8.8.8", "example.net"}, want: expectDeny, mode: connectRefused},
	{id: "deny-over-allow-host", policy: denyPolicy, title: "a deny host rule inside an allowed glob",
		args: []string{"tcp", "www.debian.org:443"}, want: expectDeny, mode: resolveRefused},
	{id: "deny-over-allow-cidr", policy: denyPolicy, title: "a deny cidr rule inside an allowed range",
		args: []string{"tcp", "1.1.1.1:443"}, want: expectDeny, mode: connectRefused},
	{id: "proxy-env", policy: denyPolicy, title: "curl with https_proxy set in the guest, pointed at the host",
		env:  []string{"https_proxy=http://" + hostSvc, "HTTPS_PROXY=http://" + hostSvc},
		args: []string{"doh", "-client", "curl", "https://dns.google/dns-query", "example.com"},
		want: expectDeny, mode: connectRefused},
	{id: "host-service", policy: denyPolicy, title: "a service bound on the host's outbound address",
		args: []string{"tcp", hostSvc}, want: expectDeny, mode: connectRefused},
	{id: "metadata", policy: denyPolicy, title: "169.254.169.254",
		args: []string{"tcp", "169.254.169.254:80"}, want: expectDeny, mode: connectRefused,
		unprovenWhy: "hull's gateway resets every connection to 169.254.0.0/16, with a policy or without one"},

	{id: "allow/allowed-name", policy: allowPolicy, title: "a name no rule denies",
		args: []string{"tcp", "example.com:443"}, want: expectReach},
	{id: "allow/glob-literal", policy: allowPolicy, title: "the literal of a host a deny glob covers, never looked up",
		args: []string{"tcp", addrOf + "git.kernel.org}:443"}, want: expectGap},
	{id: "allow/denied-glob", policy: allowPolicy, title: "a name a deny glob covers",
		args: []string{"tcp", "git.kernel.org:443"}, want: expectDeny, mode: connectRefused},
	{id: "allow/denied-name", policy: allowPolicy, title: "a name a deny host rule names",
		args: []string{"tcp", "example.net:443"}, want: expectDeny, mode: connectRefused},
	{id: "allow/denied-name-literal", policy: allowPolicy, title: "the literal of a host a deny host rule names",
		args: []string{"tcp", addrOf + "example.net}:443"}, want: expectDeny, mode: connectRefused},
	{id: "allow/denied-cidr", policy: allowPolicy, title: "an address a deny cidr rule covers",
		args: []string{"tcp", "9.9.9.9:443"}, want: expectDeny, mode: connectRefused},
	{id: "allow/alternate-resolver", policy: allowPolicy, title: "a glob-denied name from a resolver other than the gateway",
		args: []string{"dns", "-timeout", udpTimeout, "8.8.8.8", "git.kernel.org"}, want: expectGap},
	{id: "allow/dns-over-https", policy: allowPolicy, title: "a glob-denied name over DNS over HTTPS",
		args: []string{"doh", "https://8.8.8.8/dns-query", "git.kernel.org"}, want: expectGap},
	{id: "allow/dns-over-tls", policy: allowPolicy, title: "a glob-denied name over DNS over TLS",
		args: []string{"dot", "8.8.8.8", "git.kernel.org"}, want: expectGap},
}

// command is the probe invocation as the record prints it, placeholders and
// all, so two runs of one case read the same whatever the host's address.
func (c testCase) command() string {
	return strings.Join(append(append([]string{}, c.env...), append([]string{"netprobe"}, c.args...)...), " ")
}
