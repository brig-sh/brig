package policy

import (
	"strings"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"sigs.k8s.io/yaml"
)

// Parse decodes and validates a policy document, in YAML or JSON.
//
// One path serves both because JSON is a subset of YAML. The decoder is
// strict: an unknown field is an error rather than something silently
// ignored, so a document that adds a field this package does not know about
// fails to parse rather than quietly losing it.
//
// A document with no kind: is the retired flat form. Parse converts it into
// the same Policy, so a caller never handles two shapes. LoadAll records
// which form a file used, for the commands that report it.
func Parse(blob []byte) (Policy, error) {
	p, _, err := parse(blob)
	return p, err
}

// parse is Parse, and also reports whether the document used the flat form.
func parse(blob []byte) (Policy, bool, error) {
	flat, err := isFlat(blob)
	if err != nil {
		return Policy{}, false, err
	}
	var p Policy
	if flat {
		p, err = decodeFlat(blob)
	} else {
		var d *Policy
		d, err = v1alpha1.Decode(blob)
		if d != nil {
			p = *d
		}
	}
	if err != nil {
		return Policy{}, flat, err
	}
	if err := p.Validate(); err != nil {
		return Policy{}, flat, err
	}
	// The routes are checked again after preset expansion, which only this
	// package can do, so a save that breaks a preset route is refused here.
	if _, err := Routes([]Policy{p}); err != nil {
		return Policy{}, flat, err
	}
	return p, flat, nil
}

// isFlat reports whether a document leaves out kind:, which is how the flat
// form is told apart from the envelope. The key is looked for, not its value,
// so an envelope with kind: "" is judged as an envelope and refused there.
func isFlat(blob []byte) (bool, error) {
	var top map[string]any
	if err := yaml.Unmarshal(blob, &top); err != nil {
		return false, err
	}
	_, hasKind := top["kind"]
	return !hasKind, nil
}

// flatPolicy is the retired flat form of a policy document.
type flatPolicy struct {
	APIVersion string          `json:"apiVersion"`
	Name       string          `json:"name"`
	Desc       string          `json:"desc,omitempty"`
	Egress     v1alpha1.Egress `json:"egress,omitempty"`
}

// decodeFlat decodes the flat form, and converts it into the envelope. The
// flat form always had an egress block, so the conversion always sets
// spec.egress, and Validate refuses a missing default the way the flat form
// always did.
//
// A key matches its field without regard to case, as it always did in the
// flat form, so Egress: and Host: still load; the envelope is matched byte
// for byte. The flat form took a desc: over several lines, and
// metadata.description is one line, so the lines are joined with single
// spaces. Every flat document that loaded before the envelope still loads,
// except one that names a field twice in two spellings, which loaded with
// one of them silently dropped, and one with a second document after ---,
// which was ignored.
func decodeFlat(blob []byte) (Policy, error) {
	var f flatPolicy
	if err := v1alpha1.UnmarshalFolded(blob, &f); err != nil {
		return Policy{}, err
	}
	egress := f.Egress
	return Policy{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: f.APIVersion, Kind: v1alpha1.Kind},
		Metadata: v1alpha1.ObjectMeta{Name: f.Name, Description: oneLine(f.Desc)},
		Spec:     v1alpha1.PolicySpec{Egress: &egress},
	}, nil
}

// oneLine joins the non-blank lines of s with single spaces.
func oneLine(s string) string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, " ")
}
