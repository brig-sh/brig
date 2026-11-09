package cilium

import (
	"errors"
	"fmt"
	"strings"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"sigs.k8s.io/yaml"
)

// PodIdentityCIDR is the address of the EKS Pod Identity agent on every
// node.
const PodIdentityCIDR = "169.254.170.23/32"

// DNSPort is the port of the kube-dns rule.
const DNSPort = "53"

// BrokerPort is the port of the node broker, which a pod reaches at
// brig.internal.
const BrokerPort = "443"

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
//   - a rule to kube-dns on port 53, with a DNS rule that sends every lookup
//     through Cilium's DNS proxy. toFQDNs only works for names the proxy has
//     seen resolved.
//   - when p has routes, a rule to the host entity on port 443, for the node
//     broker at brig.internal.
//   - when o.PodIdentity is set, a rule to PodIdentityCIDR on port 80.
//   - when o.Cluster is set, a rule to the cluster entity on every port.
//
// Under a deny default, enableDefaultDeny.egress is true and each allow rule
// follows as its own egress entry, in document order, on every port. A host
// with no '*' becomes toFQDNs matchName, a host with '*' becomes toFQDNs
// matchPattern, and a CIDR becomes toCIDR as written.
//
// Under an allow default, enableDefaultDeny.egress is false and the allow
// rules are left out, because Cilium already lets that traffic through.
//
// Each deny CIDR becomes its own egressDeny toCIDR entry, in document order.
// Cilium has no toFQDNs for egressDeny, so a deny host cannot be rendered.
// Under a deny default, a deny host that no allow host covers is already
// refused and is dropped; for a deny host with '*', no allow host may match
// any name it matches. The renderer cannot resolve names, so a deny host is
// also an error when the policy has any allow CIDR, which may contain the
// host's addresses. Any other deny host is an error.
//
// A host is lowercased and loses one trailing dot. It must then be one or
// more labels joined by '.', each a non-empty run of [a-z0-9-*]; a bare "*"
// is accepted. Only '*' is a wildcard. Brig's enforcer lets '*' cross a dot,
// and Cilium's '*' never does, so a leading "*." is written as "**." in the
// matchPattern. In Cilium, "**." matches one or more labels and never the
// apex, as "*." does in brig. It needs a Cilium release that supports "**.".
// A '*' elsewhere in a host is copied as written and matches within one
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
	// below matches nothing, so it is written under either default.
	np.Spec.Egress = append(np.Spec.Egress, EgressRule{
		ToEndpoints: []EndpointSelector{{MatchLabels: map[string]string{
			"k8s:io.kubernetes.pod.namespace": "kube-system",
			"k8s:k8s-app":                     "kube-dns",
		}}},
		ToPorts: []PortRule{{
			Ports: []PortProtocol{{Port: DNSPort, Protocol: "ANY"}},
			Rules: &L7Rules{DNS: []DNSRule{{MatchPattern: "*"}}},
		}},
	})
	// A route is served by the node broker, which the pod reaches at
	// brig.internal on the node's own address.
	if len(p.Spec.Routes) > 0 {
		np.Spec.Egress = append(np.Spec.Egress, EgressRule{
			ToEntities: []string{"host"},
			ToPorts:    []PortRule{{Ports: []PortProtocol{{Port: BrokerPort, Protocol: "TCP"}}}},
		})
	}
	if o.PodIdentity {
		np.Spec.Egress = append(np.Spec.Egress, EgressRule{
			ToCIDR:  []string{PodIdentityCIDR},
			ToPorts: []PortRule{{Ports: []PortProtocol{{Port: PodIdentityPort, Protocol: "TCP"}}}},
		})
	}
	if o.Cluster {
		np.Spec.Egress = append(np.Spec.Egress, EgressRule{ToEntities: []string{"cluster"}})
	}

	// allowHosts holds the normalized allow hosts and their index, for the
	// deny check below. It stays empty under an allow default.
	type allowHost struct {
		i    int
		host string
	}
	var allowHosts []allowHost
	// allowCIDR is the index of the first allow CIDR, or -1.
	allowCIDR := -1
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
				sel := FQDNSelector{MatchName: h}
				if strings.Contains(h, "*") {
					sel = FQDNSelector{MatchPattern: ciliumPattern(h)}
				}
				np.Spec.Egress = append(np.Spec.Egress, EgressRule{ToFQDNs: []FQDNSelector{sel}})
				allowHosts = append(allowHosts, allowHost{i, h})
			case r.CIDR != "":
				if allowCIDR < 0 {
					allowCIDR = i
				}
				np.Spec.Egress = append(np.Spec.Egress, EgressRule{ToCIDR: []string{r.CIDR}})
			default:
				return nil, fmt.Errorf("allow[%d] has neither host nor cidr", i)
			}
		}
	}

	for i, r := range eg.Deny {
		switch {
		case r.CIDR != "":
			np.Spec.EgressDeny = append(np.Spec.EgressDeny, EgressDenyRule{ToCIDR: []string{r.CIDR}})
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
			// matching on purpose: it may refuse a policy that would have
			// rendered right, and it never drops a deny host that an allow
			// host or an allow CIDR lets through. A deny glob needs the overlap test: it
			// can deny one name under an allow glob without covering it.
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
		default:
			return nil, fmt.Errorf("deny[%d] has neither host nor cidr", i)
		}
	}
	return np, nil
}

// ciliumPattern returns the matchPattern for a checked host. A leading "*."
// becomes "**." so that it spans labels as brig's does.
func ciliumPattern(h string) string {
	if strings.HasPrefix(h, "*.") {
		return "*" + h
	}
	return h
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
