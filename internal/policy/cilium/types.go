// Package cilium renders the egress section of a brig policy as a
// CiliumNetworkPolicy.
//
// A cluster that runs brig's broker on its nodes can enforce egress at the
// CNI instead of in brig. This package gives that cluster the Cilium form of
// a policy. It holds its own copy of the few CiliumNetworkPolicy fields it
// writes, so brig does not link the Cilium module, and it does no I/O.
package cilium

// APIVersion is the apiVersion of every object Render returns.
const APIVersion = "cilium.io/v2"

// Kind is the kind of every object Render returns.
const Kind = "CiliumNetworkPolicy"

// NetworkPolicy is a CiliumNetworkPolicy, reduced to the fields Render
// writes. The json tags match the Cilium CRD, and sigs.k8s.io/yaml marshals
// through them.
type NetworkPolicy struct {
	// APIVersion is always APIVersion.
	APIVersion string `json:"apiVersion"`
	// Kind is always Kind.
	Kind string `json:"kind"`
	// Metadata names the object.
	Metadata ObjectMeta `json:"metadata"`
	// Spec holds the rules.
	Spec Spec `json:"spec"`
}

// ObjectMeta is the Kubernetes object metadata Render writes.
type ObjectMeta struct {
	// Name is a DNS-1123 subdomain.
	Name string `json:"name"`
	// Namespace is left out when empty, so kubectl applies the object to
	// the namespace of its context.
	Namespace string `json:"namespace,omitempty"`
}

// Spec is the rule set of a CiliumNetworkPolicy.
type Spec struct {
	// EndpointSelector picks the pods the policy applies to. An empty
	// selector picks every pod in the namespace.
	EndpointSelector EndpointSelector `json:"endpointSelector"`
	// EnableDefaultDeny says whether traffic no rule names is dropped.
	EnableDefaultDeny DefaultDeny `json:"enableDefaultDeny"`
	// Egress lists the allowed destinations.
	Egress []EgressRule `json:"egress,omitempty"`
	// EgressDeny lists the refused destinations. Cilium applies it before
	// Egress.
	EgressDeny []EgressDenyRule `json:"egressDeny,omitempty"`
}

// EndpointSelector selects endpoints by label.
type EndpointSelector struct {
	// MatchLabels is left out when empty, so an empty selector marshals as
	// {}.
	MatchLabels map[string]string `json:"matchLabels,omitempty"`
}

// DefaultDeny holds the default verdict for egress.
type DefaultDeny struct {
	// Egress is a pointer so that false is written out rather than left
	// out. Cilium reads a missing value as true for a policy that has
	// egress rules.
	Egress *bool `json:"egress,omitempty"`
}

// EgressRule is one allowed set of destinations. Exactly one of
// ToEndpoints, ToEntities, ToCIDR and ToFQDNs is set by Render.
type EgressRule struct {
	// ToEndpoints selects pods in the cluster.
	ToEndpoints []EndpointSelector `json:"toEndpoints,omitempty"`
	// ToEntities names Cilium entities such as "host".
	ToEntities []string `json:"toEntities,omitempty"`
	// ToCIDR lists network ranges.
	ToCIDR []string `json:"toCIDR,omitempty"`
	// ToFQDNs lists DNS names, which Cilium learns through its DNS proxy.
	ToFQDNs []FQDNSelector `json:"toFQDNs,omitempty"`
	// ToPorts limits the rule to some ports. Left out, the rule covers
	// every port.
	ToPorts []PortRule `json:"toPorts,omitempty"`
}

// EgressDenyRule is one refused set of destinations. Cilium has no toFQDNs
// for egressDeny, so a deny rule names network ranges only.
type EgressDenyRule struct {
	// ToCIDR lists network ranges.
	ToCIDR []string `json:"toCIDR,omitempty"`
}

// FQDNSelector names a DNS name. Exactly one field is set.
type FQDNSelector struct {
	// MatchName is an exact name.
	MatchName string `json:"matchName,omitempty"`
	// MatchPattern is a name in which '*' is a wildcard.
	MatchPattern string `json:"matchPattern,omitempty"`
}

// PortRule limits a rule to some ports, and can add layer 7 rules.
type PortRule struct {
	// Ports lists the ports.
	Ports []PortProtocol `json:"ports"`
	// Rules holds layer 7 rules. It is left out when nil.
	Rules *L7Rules `json:"rules,omitempty"`
}

// PortProtocol is one port and its protocol.
type PortProtocol struct {
	// Port is a decimal port number, as a string.
	Port string `json:"port"`
	// Protocol is TCP, UDP or ANY.
	Protocol string `json:"protocol"`
}

// L7Rules holds the layer 7 rules of a port rule. Render writes DNS rules
// only.
type L7Rules struct {
	// DNS sends the lookups through Cilium's DNS proxy.
	DNS []DNSRule `json:"dns"`
}

// DNSRule names the lookups the DNS proxy lets through.
type DNSRule struct {
	// MatchPattern is a name in which '*' is a wildcard.
	MatchPattern string `json:"matchPattern"`
}
