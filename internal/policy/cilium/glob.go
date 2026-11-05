package cilium

import (
	"fmt"
	"strings"
)

// normalizeHost lowercases h and strips one trailing dot, so "API.Example.com."
// and "api.example.com" name the same host.
func normalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// checkHost reports whether a normalized host is a pattern Cilium reads the
// way brig writes it. A host is one or more labels joined by '.', and each
// label is a non-empty run of [a-z0-9-*]. A bare "*" is accepted. Anything
// else, '?', '[', ']' and '\' included, is refused, because only '*' is a
// wildcard in a Cilium FQDN pattern.
func checkHost(h string) error {
	if h == "" {
		return fmt.Errorf("the host is empty")
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" {
			return fmt.Errorf("host %q has an empty label", h)
		}
		for _, c := range label {
			switch {
			case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '*':
			default:
				return fmt.Errorf("host %q contains %q; a host is letters, digits, '-' and '.', and only '*' is a wildcard", h, c)
			}
		}
	}
	return nil
}

// globCovers reports whether the allow host glob covers the deny host host.
// Both are normalized. A glob with no '*' covers only itself. Otherwise each
// '*' matches zero or more characters, dots included, so "a*.example.com"
// covers "a.example.com" and "*.example.com" covers "a.b.example.com".
//
// This is wider than Cilium on purpose: there a leading "*." matches one or
// more labels and never the apex, and a '*' inside a label does not cross a
// dot. A wider match can only make Render refuse a deny host it could have
// dropped. It never drops a deny host that Cilium would let through.
func globCovers(glob, host string) bool {
	if !strings.Contains(glob, "*") {
		return glob == host
	}
	parts := strings.Split(glob, "*")
	first, last := parts[0], parts[len(parts)-1]
	if !strings.HasPrefix(host, first) {
		return false
	}
	rest := host[len(first):]
	// Taking the leftmost match of each middle segment leaves the most of
	// host for the segments after it, so a miss here is a miss for every
	// placement.
	for _, seg := range parts[1 : len(parts)-1] {
		i := strings.Index(rest, seg)
		if i < 0 {
			return false
		}
		rest = rest[i+len(seg):]
	}
	return strings.HasSuffix(rest, last)
}

// globsOverlap reports whether some host matches both a and b, with '*'
// read as in globCovers. Render needs it for a deny host that is itself a
// glob: "*.example.com" under an allow of "api.example.com" names no host
// the allow glob covers as a whole, yet it denies one that Cilium would let
// through.
func globsOverlap(a, b string) bool {
	// seen[i][j] marks a state already explored from a[i:] and b[j:]. A
	// state that was reached once and did not succeed cannot succeed later.
	seen := make([][]bool, len(a)+1)
	for i := range seen {
		seen[i] = make([]bool, len(b)+1)
	}
	var walk func(i, j int) bool
	walk = func(i, j int) bool {
		if seen[i][j] {
			return false
		}
		seen[i][j] = true
		if i == len(a) && j == len(b) {
			return true
		}
		if i < len(a) && a[i] == '*' {
			// The star in a matches nothing more, or also takes b[j].
			if walk(i+1, j) || (j < len(b) && walk(i, j+1)) {
				return true
			}
		}
		if j < len(b) && b[j] == '*' {
			if walk(i, j+1) || (i < len(a) && walk(i+1, j)) {
				return true
			}
		}
		if i < len(a) && j < len(b) && a[i] != '*' && b[j] != '*' && a[i] == b[j] {
			return walk(i+1, j+1)
		}
		return false
	}
	return walk(0, 0)
}
