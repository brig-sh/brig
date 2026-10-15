package v1alpha1

// Group is the API group of every document in this package.
const Group = "brig.sh"

// Version is the API version of this package.
const Version = "v1alpha1"

// APIVersion is the only apiVersion a document of this package declares.
const APIVersion = Group + "/" + Version

// Kind is the only kind a policy document declares.
const Kind = "Policy"

// TypeMeta names the shape of a document.
type TypeMeta struct {
	// APIVersion is required and must be APIVersion.
	APIVersion string `json:"apiVersion"`
	// Kind is required and must be Kind.
	Kind string `json:"kind"`
}

// ObjectMeta is the metadata of a document. Name, labels and annotations
// match the Kubernetes ObjectMeta fields of the same JSON name. ObjectMeta
// has no description, so a conversion to a CRD object carries it in an
// annotation.
type ObjectMeta struct {
	// Name is required. It uses lowercase letters, digits, dot, dash and
	// underscore, starts with a letter or digit, and reads back as itself
	// when written unquoted in YAML.
	Name string `json:"name"`
	// Description is optional, and is one line.
	Description string `json:"description,omitempty"`
	// Labels is optional. Brig does not read it.
	Labels map[string]string `json:"labels,omitempty"`
	// Annotations is optional. Brig does not read it.
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Policy is one named policy document.
type Policy struct {
	TypeMeta `json:",inline"`
	// Metadata is required, and names the policy.
	Metadata ObjectMeta `json:"metadata"`
	// Spec is required, and holds the rules of the policy.
	Spec PolicySpec `json:"spec"`
}

// PolicySpec is what a policy asks for.
type PolicySpec struct {
	// Egress is required in this version. It is the outbound rule set.
	Egress *Egress `json:"egress,omitempty"`
}

// EgressDefault is the verdict for traffic that no rule names.
type EgressDefault string

// The two egress defaults.
const (
	EgressAllow EgressDefault = "allow"
	EgressDeny  EgressDefault = "deny"
)

// Egress is what the agent may reach outbound.
type Egress struct {
	// Default is required, and is "allow" or "deny". It applies to traffic
	// that neither list names.
	Default EgressDefault `json:"default"`
	// Allow is optional. It lists the exceptions to a "deny" default.
	Allow []Rule `json:"allow,omitempty"`
	// Deny is optional. A destination named here is refused even when Allow
	// or Default would admit it.
	Deny []Rule `json:"deny,omitempty"`
}

// Rule is one destination. Exactly one of Host and CIDR is set.
type Rule struct {
	// Host is a domain or a glob such as "*.githubusercontent.com". It must
	// not hold whitespace or a control character.
	Host string `json:"host,omitempty"`
	// CIDR is a network range such as "10.0.0.0/8". It must parse as a CIDR.
	CIDR string `json:"cidr,omitempty"`
}
