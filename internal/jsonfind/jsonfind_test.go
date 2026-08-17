package jsonfind

import "testing"

// The search is recursive because a credential arrives wrapped in an
// envelope more often than not -- claudeAiOauth.accessToken is the case this
// exists for -- and a configured path per profile would be a second thing to
// get wrong.
func TestFindsAFieldAtAnyDepth(t *testing.T) {
	blob := []byte(`{"claudeAiOauth":{"accessToken":"tok","expiresAt":1755436980000}}`)
	if got, ok := String(blob, "accessToken"); !ok || got != "tok" {
		t.Errorf("String(accessToken) = %q, %v; want \"tok\", true", got, ok)
	}
	if got, ok := Number(blob, "expiresAt"); !ok || got != 1755436980000 {
		t.Errorf("Number(expiresAt) = %d, %v; want 1755436980000, true", got, ok)
	}
}

// A field that is there but of the wrong type is not a match: a caller asking
// for a token wants a string, and handing it the number it found instead
// would store something that resolves and never authenticates.
func TestWrongTypeIsNotAMatch(t *testing.T) {
	if _, ok := String([]byte(`{"accessToken":7}`), "accessToken"); ok {
		t.Error("a number satisfied a string field")
	}
}

func TestNotJSONIsNotAMatch(t *testing.T) {
	if _, ok := String([]byte("plain token text"), "accessToken"); ok {
		t.Error("a non-JSON blob matched")
	}
}
