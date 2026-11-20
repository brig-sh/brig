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

// The document Claude Code keeps holds expiresAt and accessToken for the
// login and again for each MCP server signed in with OAuth. A bare name that
// finds two values must find nothing rather than whichever map order reaches
// first: an expiry taken from an MCP entry warned on every run that a valid
// login had expired. The loop keeps a lucky order from passing this.
func TestAFieldFoundTwiceWithDifferentValuesIsNotAMatch(t *testing.T) {
	blob := []byte(`{
		"claudeAiOauth": {"accessToken": "login", "expiresAt": 1900000000000},
		"mcpOAuth": {
			"one": {"accessToken": "mcp-one", "expiresAt": 1800000000000},
			"two": {"accessToken": "mcp-two", "expiresAt": 1800000000001}
		}
	}`)
	for i := 0; i < 500; i++ {
		if got, ok := Number(blob, "expiresAt"); ok {
			t.Fatalf("Number(expiresAt) = %d on try %d; want no match", got, i)
		}
		if got, ok := String(blob, "accessToken"); ok {
			t.Fatalf("String(accessToken) = %q on try %d; want no match", got, i)
		}
	}
}

// A dotted name is a path from the root first, which is how a profile names
// the one expiresAt of the login in a document that holds several.
func TestADottedNameIsAPathFromTheRoot(t *testing.T) {
	blob := []byte(`{
		"claudeAiOauth": {"accessToken": "login", "expiresAt": 1900000000000},
		"mcpOAuth": {
			"one": {"accessToken": "mcp-one", "expiresAt": 1800000000000},
			"two": {"accessToken": "mcp-two", "expiresAt": 1800000000001}
		}
	}`)
	for i := 0; i < 500; i++ {
		if got, ok := Number(blob, "claudeAiOauth.expiresAt"); !ok || got != 1900000000000 {
			t.Fatalf("Number(claudeAiOauth.expiresAt) = %d, %v on try %d; want 1900000000000, true", got, ok, i)
		}
		if got, ok := String(blob, "claudeAiOauth.accessToken"); !ok || got != "login" {
			t.Fatalf("String(claudeAiOauth.accessToken) = %q, %v on try %d; want \"login\", true", got, ok, i)
		}
	}
}

// A dotted name that is not a path from the root is still searched at depth,
// so an envelope around the whole document needs no change to the profile. A
// key that holds the whole dotted name is a key, not a path.
func TestADottedNameIsAlsoFoundAtDepthAndAsALiteralKey(t *testing.T) {
	wrapped := []byte(`{"envelope":{"claudeAiOauth":{"expiresAt":1900000000000}}}`)
	if got, ok := Number(wrapped, "claudeAiOauth.expiresAt"); !ok || got != 1900000000000 {
		t.Errorf("Number at depth = %d, %v; want 1900000000000, true", got, ok)
	}
	literal := []byte(`{"claudeAiOauth.expiresAt":1900000000000}`)
	if got, ok := Number(literal, "claudeAiOauth.expiresAt"); !ok || got != 1900000000000 {
		t.Errorf("Number of a literal key = %d, %v; want 1900000000000, true", got, ok)
	}
}

// The same value found twice is one answer, not an ambiguity.
func TestTheSameValueFoundTwiceIsAMatch(t *testing.T) {
	blob := []byte(`{"a":{"expiresAt":1900000000000},"b":{"expiresAt":1900000000000}}`)
	if got, ok := Number(blob, "expiresAt"); !ok || got != 1900000000000 {
		t.Errorf("Number(expiresAt) = %d, %v; want 1900000000000, true", got, ok)
	}
}
