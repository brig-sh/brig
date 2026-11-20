package cilium

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"sigs.k8s.io/yaml"
)

// PodIdentityCIDR is the IPv4 address of the EKS Pod Identity agent on
// every node.
const PodIdentityCIDR = "169.254.170.23/32"

// PodIdentityCIDR6 is the IPv6 address of the EKS Pod Identity agent, which
// a pod on an IPv6 cluster reaches.
const PodIdentityCIDR6 = "fd00:ec2::23/128"

// ClusterDomain is the search domain under which a pod's resolver looks up
// a name first.
const ClusterDomain = "cluster.local"

// DNSPort is the port of the kube-dns rule.
const DNSPort = "53"

// BrokerPort is the port of the node broker, which a pod reaches at
// brig.internal.
const BrokerPort = "443"

// BrokerCIDR is the link-local address the node broker listens on, as the
// urunc installer sets it. Options.BrokerCIDR replaces it.
const BrokerCIDR = "169.254.171.1/32"

// PodIdentityPort is the port of the EKS Pod Identity agent.
const PodIdentityPort = "80"

// Options holds what a policy file does not say about the cluster.
type Options struct {
	// Name is the object name. Empty means the policy's metadata.name.
	Name string
	// Namespace is the object namespace. Empty leaves it out.
	Namespace string
	// PodSelector is the matchLabels of the endpoint selector. Empty
	// selects every pod in the namespace.
	PodSelector map[string]string
	// PodIdentity adds a rule to the EKS Pod Identity agent.
	PodIdentity bool
	// BrokerCIDR is the address of the node broker, as an address or a
	// prefix of its full length. Empty means BrokerCIDR.
	BrokerCIDR string
	// DNSSearch lists search domains of the pod's resolver beyond
	// ClusterDomain, such as the node's own. Under a deny default the DNS
	// rule answers the lookups the resolver makes under each one.
	DNSSearch []string
	// ClusterCIDRs lists the address ranges of the cluster's pods and
	// nodes. Cilium matches a pod or a node by its identity and applies no
	// CIDR rule to it, so Render needs these ranges to render a CIDR rule
	// that covers them.
	ClusterCIDRs []string
	// Cluster adds a rule to the cluster entity: every pod and node of the
	// cluster, on every port. A brig policy's egress names the hosts a
	// sandbox reaches outside; a pod also talks to the services beside it,
	// which the policy does not name.
	Cluster bool
}

// Render returns the CiliumNetworkPolicy for the egress section of p. It
// does no I/O.
//
// A policy with no spec.egress is refused. The object name is o.Name, or
// metadata.name when o.Name is empty, and it must be a DNS-1123 subdomain:
// brig allows '_' in a policy name and Kubernetes does not.
//
// The egress list always starts with the same rules, in this order:
//
//   - a rule to kube-dns on port 53, with a DNS rule that sends the lookups
//     through Cilium's DNS proxy. toFQDNs only works for names the proxy has
//     seen resolved. Under an allow default the rule lets every name
//     through. Under a deny default it lets through the allow hosts,
//     brig.internal when p has routes, and every name under ClusterDomain
//     and o.DNSSearch. The proxy refuses any other lookup, so a guest
//     cannot carry data out in the names it looks up.
//   - when p has routes, a rule to the broker's address on port 443, for the
//     node broker at brig.internal. The address is a CIDR rule, not the host
//     entity: it sits on the node with link scope, so Cilium does not count
//     it as the host identity.
//   - when o.PodIdentity is set, a rule to PodIdentityCIDR and
//     PodIdentityCIDR6 on port 80.
//   - when o.Cluster is set, a rule to the cluster entity on every port.
//
// Under a deny default, enableDefaultDeny.egress is true and each allow rule
// follows as its own egress entry, in document order, on every port. A host
// with no '*' becomes toFQDNs matchName, a host with '*' becomes toFQDNs
// matchPattern, and a CIDR becomes toCIDR in its canonical form.
//
// Under an allow default, enableDefaultDeny.egress is false and the allow
// rules are left out, because Cilium already lets that traffic through.
//
// Each deny CIDR becomes its own egressDeny toCIDR entry, in document order.
// A CIDR is masked, and an IPv4-mapped one is written as the IPv4 prefix it
// names.
// Cilium has no toFQDNs for egressDeny, so a deny host cannot be rendered.
// Under a deny default, a deny host that no allow host covers is already
// refused and is dropped; for a deny host with '*', no allow host may match
// any name it matches. The renderer cannot resolve names, so a deny host is
// also an error when the policy has any allow CIDR, which may contain the
// host's addresses, and when o.Cluster is set, since the cluster rule lets
// through any pod or node the host may name. Any other deny host is an
// error. A dropped deny host is listed by DroppedDenyHosts.
//
// Cilium applies no CIDR rule to a pod or a node, so a CIDR is also read
// against o.ClusterCIDRs. A deny CIDR that covers all of them also denies
// every pod and node (see clusterDeny), and an allow CIDR that covers all of
// them also opens the cluster entity. A deny of part of them is an error.
// Without o.ClusterCIDRs, a deny of a private range is an error under an
// allow default or o.Cluster. An allow of part of the cluster, or of a
// private range, leaves those pods and nodes closed and is listed by
// Warnings.
//
// A host is lowercased and loses one trailing dot. It must then be one or
// more labels joined by '.', each a non-empty run of [a-z0-9-*]; a bare "*"
// is accepted. Only '*' is a wildcard. Brig's enforcer lets '*' cross a dot,
// and Cilium's '*' never does, so a leading "*." becomes several
// matchPatterns: "**." for a Cilium release that spans labels with it, and
// "*.", "*.*." and so on up to globDepth labels for one that reads "**." as
// one label, as Cilium 1.17 does. None matches the apex, as "*." does not in
// brig. A '*' elsewhere in a host is copied as written and matches within one
// label only, which is narrower than brig.
//
// Errors name the list and index of the rule, as in allow[2] or deny[0].
func Render(p *v1alpha1.Policy, o Options) (*NetworkPolicy, error) {
	if p == nil {
		return nil, errors.New("no policy to render")
	}
	eg := p.Spec.Egress
	if eg == nil {
		return nil, fmt.Errorf("policy %q has no spec.egress to render", p.Metadata.Name)
	}
	var deny bool
	switch eg.Default {
	case v1alpha1.EgressDeny:
		deny = true
	case v1alpha1.EgressAllow:
	default:
		return nil, fmt.Errorf("egress default %q is neither %q nor %q", eg.Default, v1alpha1.EgressAllow, v1alpha1.EgressDeny)
	}

	name := o.Name
	if name == "" {
		name = p.Metadata.Name
	}
	if err := checkObjectName(name); err != nil {
		return nil, err
	}
	res, err := o.resolve()
	if err != nil {
		return nil, err
	}
	broker, search := res.broker, res.search

	np := &NetworkPolicy{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata:   ObjectMeta{Name: name, Namespace: o.Namespace},
		Spec: Spec{
			EndpointSelector:  EndpointSelector{MatchLabels: copyLabels(o.PodSelector)},
			EnableDefaultDeny: DefaultDeny{Egress: &deny},
		},
	}

	// Cilium learns the addresses behind a toFQDNs name only from lookups
	// that pass through its DNS proxy. Without this rule every toFQDNs rule
	// below matches nothing, so it is written under either default. Its
	// names are set once the allow rules are read.
	np.Spec.Egress = append(np.Spec.Egress, EgressRule{
		ToEndpoints: []EndpointSelector{{MatchLabels: map[string]string{
			"k8s:io.kubernetes.pod.namespace": "kube-system",
			"k8s:k8s-app":                     "kube-dns",
		}}},
		ToPorts: []PortRule{{
			Ports: []PortProtocol{{Port: DNSPort, Protocol: "ANY"}},
			Rules: &L7Rules{},
		}},
	})
	// A route is served by the node broker, which the pod reaches at
	// brig.internal on a link-local address of the node.
	if len(p.Spec.Routes) > 0 {
		np.Spec.Egress = append(np.Spec.Egress, EgressRule{
			ToCIDR:  []string{broker},
			ToPorts: []PortRule{{Ports: []PortProtocol{{Port: BrokerPort, Protocol: "TCP"}}}},
		})
	}
	if o.PodIdentity {
		np.Spec.Egress = append(np.Spec.Egress, EgressRule{
			ToCIDR:  []string{PodIdentityCIDR, PodIdentityCIDR6},
			ToPorts: []PortRule{{Ports: []PortProtocol{{Port: PodIdentityPort, Protocol: "TCP"}}}},
		})
	}
	if o.Cluster {
		np.Spec.Egress = append(np.Spec.Egress, EgressRule{ToEntities: []string{"cluster"}})
	}

	// allowHosts holds the normalized allow hosts and their index, for the
	// deny check below. It stays empty under an allow default.
	var allowHosts []allowHost
	// names holds the lookups the DNS rule lets through under a deny
	// default, in order and without repeats.
	var names []DNSRule
	addName := func(r DNSRule) {
		if !slices.Contains(names, r) {
			names = append(names, r)
		}
	}
	// allowCIDR is the index of the first allow CIDR, or -1.
	allowCIDR := -1
	// allowCluster and denyCluster are set by an allow or a deny CIDR that
	// covers every range in o.ClusterCIDRs.
	var allowCluster, denyCluster bool
	// Under an allow default Cilium already lets the traffic of every allow
	// rule through, so the rules would add nothing.
	if deny {
		for i, r := range eg.Allow {
			switch {
			case r.Host != "":
				h := normalizeHost(r.Host)
				if err := checkHost(h); err != nil {
					return nil, fmt.Errorf("allow[%d] host %q: %w", i, r.Host, err)
				}
				sels := []FQDNSelector{{MatchName: h}}
				if strings.Contains(h, "*") {
					sels = nil
					for _, pat := range ciliumPatterns(h) {
						sels = append(sels, FQDNSelector{MatchPattern: pat})
					}
				}
				np.Spec.Egress = append(np.Spec.Egress, EgressRule{ToFQDNs: sels})
				allowHosts = append(allowHosts, allowHost{i, h})
				for _, sel := range sels {
					addName(DNSRule(sel))
				}
			case r.CIDR != "":
				if allowCIDR < 0 {
					allowCIDR = i
				}
				c, err := canonicalCIDR(r.CIDR)
				if err != nil {
					return nil, fmt.Errorf("allow[%d] cidr: %w", i, err)
				}
				np.Spec.Egress = append(np.Spec.Egress, EgressRule{ToCIDR: []string{c}})
				switch reach := res.clusterReach(c); {
				case reach == reachAll:
					allowCluster = true
				case reach == reachPart:
					np.warnings = append(np.warnings, fmt.Sprintf("allow[%d] cidr %q covers part of the cluster's ranges. Cilium opens a pod or a node by its identity, not by its address, so that part stays closed", i, r.CIDR))
				case reach == reachMaybe:
					np.warnings = append(np.warnings, fmt.Sprintf("allow[%d] cidr %q is a private range. Cilium does not open a pod or a node of the cluster by its address, so any of them in this range stay closed; pass the cluster's ranges with --cluster-cidr to open them", i, r.CIDR))
				}
			default:
				return nil, fmt.Errorf("allow[%d] has neither host nor cidr", i)
			}
		}
	}

	for i, r := range eg.Deny {
		switch {
		case r.CIDR != "":
			c, err := canonicalCIDR(r.CIDR)
			if err != nil {
				return nil, fmt.Errorf("deny[%d] cidr: %w", i, err)
			}
			// Cilium matches a pod or a node by its identity and applies no
			// CIDR rule to it. A deny that covers all of the cluster's
			// ranges is rendered as a deny of every pod and node as well.
			// A deny of only part of them cannot be rendered.
			switch reach := res.clusterReach(c); {
			case reach == reachAll:
				denyCluster = true
			case reach == reachPart:
				return nil, fmt.Errorf("deny[%d] cidr %q covers part of the cluster's ranges, and Cilium matches a pod or a node by its identity, not by its address, so only a deny of all of them can be rendered", i, r.CIDR)
			case reach == reachMaybe && (!deny || o.Cluster):
				return nil, fmt.Errorf("deny[%d] cidr %q is a private range and may cover pods or nodes of the cluster, which Cilium does not match by address; pass the cluster's ranges with --cluster-cidr", i, r.CIDR)
			}
			np.Spec.EgressDeny = append(np.Spec.EgressDeny, EgressDenyRule{ToCIDR: []string{c}})
		case r.Host != "":
			h := normalizeHost(r.Host)
			if err := checkHost(h); err != nil {
				return nil, fmt.Errorf("deny[%d] host %q: %w", i, r.Host, err)
			}
			// Cilium egressDeny has no toFQDNs, so a deny host can only
			// render as the absence of an allow rule for it. Under an
			// allow default nothing is absent.
			if !deny {
				return nil, fmt.Errorf("deny[%d] host %q: Cilium egressDeny has no toFQDNs, and the allow default lets the host through", i, r.Host)
			}
			// Under a deny default the host is refused already, unless an
			// allow host reaches it. The check is wider than Cilium's
			// matching: it may refuse a policy that would have rendered
			// right, and it never drops a deny host that an allow host or
			// an allow CIDR lets through. A deny glob needs the overlap
			// test, since it can deny one name under an allow glob without
			// covering it. The drop holds only while no other policy that
			// selects the same pods allows the host, because Cilium joins
			// the allow rules of every policy. So it is reported.
			if o.Cluster {
				return nil, fmt.Errorf("deny[%d] host %q: --cluster lets through every pod and node of the cluster, which the host may name, and Cilium egressDeny has no toFQDNs, so the deny cannot be rendered", i, r.Host)
			}
			if allowCIDR >= 0 {
				return nil, fmt.Errorf("deny[%d] host %q: allow[%d] cidr %q may contain its addresses, and Cilium egressDeny has no toFQDNs, so the deny cannot be rendered", i, r.Host, allowCIDR, eg.Allow[allowCIDR].CIDR)
			}
			for _, a := range allowHosts {
				hit := globCovers(a.host, h)
				if strings.Contains(h, "*") {
					hit = globsOverlap(a.host, h)
				}
				if hit {
					return nil, fmt.Errorf("deny[%d] host %q overlaps allow[%d] host %q: Cilium egressDeny has no toFQDNs, so the deny cannot be rendered", i, r.Host, a.i, eg.Allow[a.i].Host)
				}
			}
			np.droppedDenyHosts = append(np.droppedDenyHosts, r.Host)
		default:
			return nil, fmt.Errorf("deny[%d] has neither host nor cidr", i)
		}
	}

	if allowCluster && !o.Cluster {
		np.Spec.Egress = append(np.Spec.Egress, EgressRule{ToEntities: []string{"cluster"}})
	}
	if denyCluster {
		np.Spec.EgressDeny = append(np.Spec.EgressDeny, clusterDeny()...)
	}

	dns := []DNSRule{{MatchPattern: "*"}}
	if deny {
		if len(p.Spec.Routes) > 0 {
			addName(DNSRule{MatchName: "brig.internal"})
		}
		depth := searchDepth(allowHosts)
		for _, d := range search {
			for _, pat := range spanPatterns(d, depth) {
				addName(DNSRule{MatchPattern: pat})
			}
		}
		dns = names
	}
	np.Spec.Egress[0].ToPorts[0].Rules.DNS = dns
	return np, nil
}

// searchDepth returns how many labels the patterns of a search domain cover
// on a Cilium release that reads "**." as one label. A resolver with the
// usual ndots:5 looks a name up under each search domain before it looks
// it up as written, and the proxy refuses a lookup it has no pattern for.
// glibc moves on past such a refusal, and musl waits until it times out,
// so the name never resolves in a musl image. The deepest such lookup is an allow host followed
// by the namespace and "svc". A leading "*." counts as globDepth labels.
// The result is never below globDepth, which leaves room for the names of
// services in the cluster.
func searchDepth(hosts []allowHost) int {
	depth := globDepth
	for _, a := range hosts {
		n := strings.Count(a.host, ".") + 1
		if strings.HasPrefix(a.host, "*.") {
			n += globDepth - 1
		}
		depth = max(depth, n+2)
	}
	return depth
}

// allowHost is a normalized allow host and its index in the allow list.
type allowHost struct {
	i    int
	host string
}

// Check returns an error for an option Kubernetes or Cilium would not take:
// a namespace that is not a DNS-1123 label, a broker address that is not one
// address, a search domain that is not a name, or a cluster range that is not
// a CIDR. Render makes the same checks.
func (o Options) Check() error {
	_, err := o.resolve()
	return err
}

// resolved holds the options Render reads after Options.resolve checked them.
type resolved struct {
	// broker is the CIDR rule of the node broker.
	broker string
	// search lists the search domains the DNS rule answers under,
	// ClusterDomain first.
	search []string
	// cluster lists the cluster's ranges, masked. It is empty when
	// o.ClusterCIDRs is.
	cluster []netip.Prefix
}

// resolve checks o and returns what Render reads from it.
func (o Options) resolve() (resolved, error) {
	var res resolved
	if o.Namespace != "" {
		if err := checkNamespace(o.Namespace); err != nil {
			return res, err
		}
	}
	res.broker = BrokerCIDR
	if o.BrokerCIDR != "" {
		b, err := brokerPrefix(o.BrokerCIDR)
		if err != nil {
			return res, err
		}
		res.broker = b
	}
	res.search = []string{ClusterDomain}
	for _, d := range o.DNSSearch {
		h := normalizeHost(d)
		if err := checkHost(h); err != nil {
			return res, fmt.Errorf("search domain %q: %w", d, err)
		}
		if strings.Contains(h, "*") {
			return res, fmt.Errorf("search domain %q: a search domain is a name, not a glob", d)
		}
		res.search = append(res.search, h)
	}
	for _, s := range o.ClusterCIDRs {
		c, err := canonicalCIDR(s)
		if err != nil {
			return res, fmt.Errorf("cluster cidr: %w", err)
		}
		res.cluster = append(res.cluster, netip.MustParsePrefix(c))
	}
	return res, nil
}

// reach says how much of the cluster's ranges a CIDR covers.
type reach int

const (
	// reachNone is a CIDR outside every range of the cluster.
	reachNone reach = iota
	// reachAll is a CIDR that contains every range of the cluster.
	reachAll
	// reachPart is a CIDR that overlaps a range of the cluster and does not
	// contain them all.
	reachPart
	// reachMaybe is a private CIDR when the cluster's ranges are not
	// given, so it may hold pods or nodes.
	reachMaybe
)

// privateRanges are the ranges a cluster's pods and nodes usually take
// their addresses from, for a CIDR when the cluster's ranges are not given.
var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fc00::/7"),
}

// clusterReach returns how much of the cluster's ranges the canonical CIDR
// c covers.
func (res resolved) clusterReach(c string) reach {
	p := netip.MustParsePrefix(c)
	if len(res.cluster) == 0 {
		for _, r := range privateRanges {
			if p.Overlaps(r) {
				return reachMaybe
			}
		}
		return reachNone
	}
	all, any := true, false
	for _, r := range res.cluster {
		if !p.Overlaps(r) {
			all = false
			continue
		}
		any = true
		if p.Bits() > r.Bits() {
			all = false
		}
	}
	switch {
	case all:
		return reachAll
	case any:
		return reachPart
	}
	return reachNone
}

// clusterDeny returns the deny rules for every pod and node of the cluster,
// which Cilium matches by identity and not by a CIDR rule. kube-dns stays
// reachable on port 53, where the DNS rule sends the lookups through the
// proxy. The selectors that pick pods require the namespace label, so they
// never match an address outside the cluster.
func clusterDeny() []EgressDenyRule {
	const nsKey = "k8s:io.kubernetes.pod.namespace"
	return []EgressDenyRule{
		{ToEndpoints: []EndpointSelector{
			{MatchExpressions: []LabelSelectorRequirement{
				{Key: nsKey, Operator: "Exists"},
				{Key: nsKey, Operator: "NotIn", Values: []string{"kube-system"}},
			}},
			{MatchExpressions: []LabelSelectorRequirement{
				{Key: nsKey, Operator: "In", Values: []string{"kube-system"}},
				{Key: "k8s:k8s-app", Operator: "NotIn", Values: []string{"kube-dns"}},
			}},
		}},
		{ToEntities: []string{"host", "remote-node", "kube-apiserver", "health", "init", "ingress", "unmanaged"}},
		{
			ToEndpoints: []EndpointSelector{{MatchLabels: map[string]string{
				nsKey:         "kube-system",
				"k8s:k8s-app": "kube-dns",
			}}},
			ToPorts: []PortRule{{Ports: []PortProtocol{
				{Port: "1", EndPort: 52, Protocol: "ANY"},
				{Port: "54", EndPort: 65535, Protocol: "ANY"},
			}}},
		},
	}
}

// globDepth is the most labels that the "*." patterns of a leading glob
// cover on a Cilium release that reads "**." as one label. Five covers the
// deepest AWS service names, such as
// bucket.s3.dualstack.us-east-1.amazonaws.com.
const globDepth = 5

// ciliumPatterns returns the matchPatterns for a checked host. A host without
// a leading "*." is one pattern, as written. A leading "*." spans labels in
// brig and Cilium's '*' does not, so it becomes "**." and then "*.", "*.*."
// and so on, up to globDepth labels before the rest of the host.
func ciliumPatterns(h string) []string {
	rest, ok := strings.CutPrefix(h, "*.")
	if !ok {
		return []string{h}
	}
	return spanPatterns(rest, globDepth)
}

// spanPatterns returns the patterns for the names under rest: "**." for a
// Cilium release that spans labels with it, and then "*.", "*.*." and so on
// up to depth labels before rest.
func spanPatterns(rest string, depth int) []string {
	out := []string{"**." + rest}
	prefix := ""
	for range depth {
		prefix += "*."
		out = append(out, prefix+rest)
	}
	return out
}

// canonicalCIDR returns s masked, with an IPv4-mapped prefix written as the
// IPv4 prefix it names. Cilium matches a mapped prefix as IPv6, so it would
// match no IPv4 address, and a deny rule would deny nothing. Brig's
// enforcer unmaps it the same way.
func canonicalCIDR(s string) (string, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return "", fmt.Errorf("%q is not a CIDR", s)
	}
	if p.Addr().Is4In6() && p.Bits() >= 96 {
		p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
	}
	return p.Masked().String(), nil
}

// brokerPrefix returns the CIDR rule for the broker address s: an address,
// or a prefix of its full length. A wider prefix is refused, because the
// rule would open port 443 to that whole range for every selected pod.
func brokerPrefix(s string) (string, error) {
	a, err := netip.ParseAddr(s)
	if err != nil {
		p, perr := netip.ParsePrefix(s)
		if perr != nil {
			return "", fmt.Errorf("broker address %q is not an address", s)
		}
		if p.Bits() != p.Addr().BitLen() {
			return "", fmt.Errorf("broker address %q is a range; give the one address the node broker listens on", s)
		}
		a = p.Addr()
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()).String(), nil
}

// checkNamespace refuses a namespace that Kubernetes does not take as a
// DNS-1123 label, which kubectl would refuse only at apply time.
func checkNamespace(ns string) error {
	bad := func(why string) error {
		return fmt.Errorf("namespace %q is not a Kubernetes namespace: %s", ns, why)
	}
	if len(ns) > 63 {
		return bad("it is longer than 63 characters")
	}
	for _, c := range ns {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return bad("only a-z, 0-9 and '-' are allowed")
		}
	}
	if ns[0] == '-' || ns[len(ns)-1] == '-' {
		return bad("it starts or ends with '-'")
	}
	return nil
}

// checkObjectName refuses a name that Kubernetes does not take as a DNS-1123
// subdomain. A brig policy name may hold '_', and without this check kubectl
// would refuse the object only at apply time.
func checkObjectName(name string) error {
	bad := func(why string) error {
		return fmt.Errorf("name %q is not a Kubernetes object name: %s; set a name that is", name, why)
	}
	if name == "" {
		return bad("it is empty")
	}
	if len(name) > 253 {
		return bad("it is longer than 253 characters")
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" {
			return bad("it has an empty label")
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return bad("only a-z, 0-9, '-' and '.' are allowed")
			}
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return bad("a label starts or ends with '-'")
		}
	}
	return nil
}

// copyLabels returns a copy of m, or nil when m is empty, so the policy does
// not change when the caller later changes m.
func copyLabels(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// RenderYAML is Render, marshalled as YAML. Mapping keys come out sorted.
func RenderYAML(p *v1alpha1.Policy, o Options) ([]byte, error) {
	np, err := Render(p, o)
	if err != nil {
		return nil, err
	}
	return yaml.Marshal(np)
}
