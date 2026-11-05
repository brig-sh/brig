package cilium

import (
	"strings"
	"testing"
)

func TestCheckHost(t *testing.T) {
	for _, c := range []struct {
		in string
		// ok says whether the normalized host is accepted.
		ok bool
		// wildcard says whether the refusal must name '*' as the only
		// wildcard.
		wildcard bool
	}{
		{in: "api-?.example.com", wildcard: true},
		{in: "api[0].example.com", wildcard: true},
		{in: "api]0.example.com", wildcard: true},
		{in: `api\.example.com`, wildcard: true},
		{in: "api example.com", wildcard: true},
		{in: ""},
		{in: "a..b"},
		{in: "API.Example.COM", ok: true},
		{in: "api.example.com.", ok: true},
		{in: "*", ok: true},
		{in: "*.githubusercontent.com", ok: true},
		{in: "api.example.com", ok: true},
	} {
		err := checkHost(normalizeHost(c.in))
		switch {
		case c.ok && err != nil:
			t.Errorf("%q: %v, want it accepted", c.in, err)
		case !c.ok && err == nil:
			t.Errorf("%q: accepted, want a refusal", c.in)
		case c.wildcard && !strings.Contains(err.Error(), "only '*' is a wildcard"):
			t.Errorf("%q: %v, want it to say only '*' is a wildcard", c.in, err)
		}
	}
	if got := normalizeHost("API.Example.com."); got != "api.example.com" {
		t.Errorf("normalizeHost = %q, want api.example.com", got)
	}
	// Only one trailing dot goes; the second leaves an empty label.
	if err := checkHost(normalizeHost("api.example.com..")); err == nil {
		t.Error("two trailing dots accepted")
	}
}

func TestGlobCovers(t *testing.T) {
	for _, c := range []struct {
		glob, host string
		want       bool
	}{
		{"*.example.com", "api.example.com", true},
		{"*.example.com", "a.b.example.com", true},
		{"*.example.com", "example.com", false},
		{"*.example.com", "api.example.org", false},
		// '*' matches zero characters too, so this covers the label "a".
		{"a*.example.com", "a.example.com", true},
		{"a*.example.com", "abc.example.com", true},
		{"a*.example.com", "b.example.com", false},
		{"a*b*c", "abc", true},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "acb", false},
		{"api.example.com", "api.example.com", true},
		{"api.example.com", "www.api.example.com", false},
		{"api.example.com", "api.example.co", false},
		{"*", "anything.at.all", true},
		{"*", "", true},
	} {
		if got := globCovers(c.glob, c.host); got != c.want {
			t.Errorf("globCovers(%q, %q) = %v, want %v", c.glob, c.host, got, c.want)
		}
	}
}

func TestGlobsOverlap(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"api.example.com", "*.example.com", true},
		{"*.example.com", "*.example.com", true},
		{"*.github.com", "*.evil.com", false},
		{"a*", "*b", true},
		{"a*", "b*", false},
		{"*.example.com", "api.example.com", true},
		{"api.example.com", "www.example.com", false},
		{"*", "x", true},
	} {
		if got := globsOverlap(c.a, c.b); got != c.want {
			t.Errorf("globsOverlap(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
		if got := globsOverlap(c.b, c.a); got != c.want {
			t.Errorf("globsOverlap(%q, %q) = %v, want %v", c.b, c.a, got, c.want)
		}
	}
}
