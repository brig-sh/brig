package secret

import "testing"

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
