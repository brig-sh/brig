package secret

import (
	"strings"
	"testing"
)

func TestProvenanceRoundTrips(t *testing.T) {
	want := Provenance{V: ProvenanceVersion, From: "keychain:Claude Code-credentials", ExpiresAt: 1755436980000}
	encoded, err := want.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, ok := DecodeProvenance(encoded)
	if !ok {
		t.Fatalf("DecodeProvenance(%q) failed", encoded)
	}
	if got != want {
		t.Errorf("round trip gave %+v, want %+v", got, want)
	}
}

// The encoded form has to survive a security(1) command line without
// quoting, so it may hold nothing that a shell-ish tokenizer would treat as
// a separator or a quote. That is the whole reason it is not raw JSON.
func TestEncodedFormNeedsNoQuoting(t *testing.T) {
	encoded, err := Provenance{V: ProvenanceVersion, From: "keychain:Claude Code-credentials"}.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for _, r := range encoded {
		switch r {
		case ' ', '\t', '\n', '"', '\\', '\'':
			t.Fatalf("encoded provenance %q holds %q, which would need quoting", encoded, r)
		}
	}
}

// A comment attribute brig did not write is not provenance, and must not be
// guessed at: another tool's item in the namespace would otherwise be
// reported with an expiry it never had.
func TestForeignCommentIsNotProvenance(t *testing.T) {
	for _, s := range []string{"", "written by hand", "brig1:!!!not-base64"} {
		if _, ok := DecodeProvenance(s); ok {
			t.Errorf("DecodeProvenance(%q) claimed to be brig's", s)
		}
	}
}

func TestZeroProvenanceIsAbsent(t *testing.T) {
	if !(Provenance{}).IsZero() {
		t.Error("the zero provenance does not report itself absent")
	}
}

// The comment attribute is attacker-controlled input, not brig's own: any
// process running as this user can write an item into brig's namespace (see
// keychain_darwin.go's comment on service), and From is about to be printed
// into `brig secret ls` and into a warning that tells the user to run a
// command. A From holding a control character or an escape sequence would
// reach a terminal unfiltered -- a lever aimed at whoever reads brig's
// output -- so DecodeProvenance drops it to the zero value rather than
// passing it through. That is the value every caller already renders as
// absent, not a new failure mode for them to handle.
func TestHostileFromIsSanitised(t *testing.T) {
	cases := []struct {
		name string
		from string
	}{
		{"ansi escape", "keychain:\x1b[31mnot a color\x1b[0m"},
		{"too long", "keychain:" + strings.Repeat("x", 4096)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			encoded, err := Provenance{V: ProvenanceVersion, From: c.from, ExpiresAt: 1}.Encode()
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			got, ok := DecodeProvenance(encoded)
			if !ok {
				t.Fatalf("DecodeProvenance(%q) failed outright, want a sanitised zero-value From", encoded)
			}
			if got.From != "" {
				t.Errorf("From = %q, want it rejected to the zero value", got.From)
			}
		})
	}
}

// The locators this feature actually produces -- Source.Locator's shapes --
// must still survive, or every real secret would show FROM as "-".
func TestLegitimateLocatorsSurviveSanitising(t *testing.T) {
	for _, from := range []string{
		"keychain:Claude Code-credentials",
		"file:~/.claude/.credentials.json",
		"env:GH_TOKEN",
	} {
		encoded, err := Provenance{V: ProvenanceVersion, From: from}.Encode()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		got, ok := DecodeProvenance(encoded)
		if !ok || got.From != from {
			t.Errorf("DecodeProvenance(%q) = %+v, %v; want From = %q", encoded, got, ok, from)
		}
	}
}
