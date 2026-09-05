package policy

import "sigs.k8s.io/yaml"

// Parse decodes and validates a policy document, in YAML or JSON.
//
// One path serves both because JSON is a subset of YAML. The decoder is
// strict: an unknown field is an error rather than something silently
// ignored, so a document that adds a field this package does not know about
// fails to parse rather than quietly losing it.
func Parse(blob []byte) (Policy, error) {
	var p Policy
	if err := yaml.UnmarshalStrict(blob, &p); err != nil {
		return Policy{}, err
	}
	return p, p.Validate()
}
