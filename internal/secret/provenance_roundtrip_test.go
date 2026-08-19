package secret

import "strings"

import "testing"

// The write side and the read side have to agree, or brig loses track of its
// own imports: DecodeProvenance drops a From it will not print, and an empty
// From reads downstream as "brig did not write this" -- which makes `brig
// secret ls` show a dash for an imported secret and makes the next import
// refuse it as hand-created, for good. These are locators a real host
// produces, not adversarial input.
func TestOrdinaryLocatorsSurviveTheRoundTrip(t *testing.T) {
	for _, from := range []string{
		"keychain:Claude Code-credentials",
		"file:~/.claude/.credentials.json",
		"file:~/Library/Application Support/My,Tool/creds.json",
		"keychain:Café-credentials",
		"file:~/a/b#c.json",
		"command",
		"file:~/" + strings.Repeat("d/", 200) + "creds.json",
	} {
		p := Provenance{V: ProvenanceVersion, From: SafeFrom(from), ExpiresAt: 1755436980000}
		encoded, err := p.Encode()
		if err != nil {
			t.Fatalf("%q: Encode: %v", from, err)
		}
		got, ok := DecodeProvenance(encoded)
		if !ok {
			t.Errorf("%q: decoded as not-brig's", from)
			continue
		}
		if got.From == "" {
			t.Errorf("%q: From came back empty, so this import reads as hand-created", from)
		}
		if got != p {
			t.Errorf("%q: round trip changed the provenance: %+v -> %+v", from, p, got)
		}
	}
}

// SafeFrom is a rendering, not a sanitiser for untrusted input: it must leave
// an already-valid locator exactly as it was, or the stored provenance stops
// matching what the profile says.
func TestSafeFromLeavesAValidLocatorAlone(t *testing.T) {
	for _, from := range []string{
		"keychain:Claude Code-credentials",
		"file:~/.claude/.credentials.json",
		"command",
	} {
		if got := SafeFrom(from); got != from {
			t.Errorf("SafeFrom(%q) = %q, want it unchanged", from, got)
		}
	}
}
