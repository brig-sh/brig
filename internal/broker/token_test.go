package broker

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var testNonce = bytes.Repeat([]byte{1}, nonceSize)

func TestTokenIsClaimsDotHMAC(t *testing.T) {
	key, err := sandboxKey(testHostKey, "brig-a", testNonce)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	tok, err := mint(key, claims{Sandbox: "brig-a", Session: "refactor", Route: "anthropic",
		Exp: now.Add(time.Hour).Unix(), Typ: tokenType})
	if err != nil {
		t.Fatal(err)
	}
	body, mac, ok := strings.Cut(tok, ".")
	if !ok {
		t.Fatalf("token %q has no dot", tok)
	}
	blob, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"sandbox", "session", "route", "exp", "typ"} {
		if _, ok := got[k]; !ok {
			t.Errorf("claims lack %q: %s", k, blob)
		}
	}
	if len(got) != 5 {
		t.Errorf("claims hold %d fields, want 5: %s", len(got), blob)
	}
	sig, err := base64.RawURLEncoding.DecodeString(mac)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sig, sign(key, blob)) {
		t.Error("the signature is not HMAC-SHA256 of the claims with the sandbox key")
	}
	c, err := verify(key, tok, "brig-a", "anthropic", now)
	if err != nil || c.Session != "refactor" {
		t.Fatalf("verify = %+v, %v", c, err)
	}
}

func TestSandboxKeysDiffer(t *testing.T) {
	a, _ := sandboxKey(testHostKey, "brig-a", testNonce)
	b, _ := sandboxKey(testHostKey, "brig-b", testNonce)
	a2, _ := sandboxKey(bytes.Repeat([]byte{9}, 32), "brig-a", testNonce)
	a3, _ := sandboxKey(testHostKey, "brig-a", bytes.Repeat([]byte{2}, nonceSize))
	if bytes.Equal(a, b) || bytes.Equal(a, a2) || bytes.Equal(a, a3) {
		t.Fatal("two sandboxes, two hosts, or two registrations share a sandbox key")
	}
}

func TestVerifyNamesTheFailedCheck(t *testing.T) {
	key, _ := sandboxKey(testHostKey, "brig-a", testNonce)
	other, _ := sandboxKey(testHostKey, "brig-b", testNonce)
	now := time.Unix(1_800_000_000, 0)
	good := claims{Sandbox: "brig-a", Route: "anthropic", Exp: now.Add(time.Hour).Unix(), Typ: tokenType}
	tok := func(k []byte, c claims) string {
		s, err := mint(k, c)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	wrongTyp := good
	wrongTyp.Typ = "other"
	// A token signed with the right key that names another sandbox can only
	// come from a key derivation bug, and verify still refuses it.
	wrongSandbox := good
	wrongSandbox.Sandbox = "brig-b"
	expired := good
	expired.Exp = now.Unix()
	body, mac, _ := strings.Cut(tok(key, good), ".")
	for _, tc := range []struct {
		name, token, route string
		want               error
	}{
		{"empty", "", "anthropic", errTokenMissing},
		{"no dot", "abc", "anthropic", errTokenMissing},
		{"three parts", body + "." + mac + ".x", "anthropic", errTokenMissing},
		{"bad base64", body + ".!!", "anthropic", errTokenMissing},
		{"other key", tok(other, good), "anthropic", errTokenSignature},
		{"swapped claims", base64.RawURLEncoding.EncodeToString([]byte(`{"sandbox":"brig-a","route":"github","exp":9999999999,"typ":"brig-route","session":""}`)) + "." + mac, "github", errTokenSignature},
		{"wrong typ", tok(key, wrongTyp), "anthropic", errTokenSignature},
		{"wrong sandbox", tok(key, wrongSandbox), "anthropic", errTokenSandbox},
		{"wrong route", tok(key, good), "github", errTokenRoute},
		{"expired", tok(key, expired), "anthropic", errTokenExpired},
	} {
		if _, err := verify(key, tc.token, "brig-a", tc.route, now); err != tc.want {
			t.Errorf("%s: verify = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestTokenHasOneSpelling(t *testing.T) {
	key, _ := sandboxKey(testHostKey, "brig-a", testNonce)
	now := time.Unix(1_800_000_000, 0)
	good := claims{Sandbox: "brig-a", Route: "anthropic", Exp: now.Add(time.Hour).Unix(), Typ: tokenType}
	tok, err := mint(key, good)
	if err != nil {
		t.Fatal(err)
	}
	// The last character of a 32-byte MAC carries two unused bits. Setting
	// one gives another spelling of the same bytes.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, tok[len(tok)-1])
	other := tok[:len(tok)-1] + string(alphabet[last^1])
	if _, err := verify(key, other, "brig-a", "anthropic", now); err == nil {
		t.Fatal("a token with another last character verified")
	}
	// Signed claims with data after the JSON object.
	blob, _ := json.Marshal(good)
	blob = append(blob, []byte(` {"route":"github"}`)...)
	trailing := base64.RawURLEncoding.EncodeToString(blob) + "." + base64.RawURLEncoding.EncodeToString(sign(key, blob))
	if _, err := verify(key, trailing, "brig-a", "anthropic", now); err != errTokenSignature {
		t.Fatalf("a token with data after its claims: %v", err)
	}
}

func TestNewRefusesAShortHostKey(t *testing.T) {
	if _, err := New(Config{HostKey: make([]byte, 16), Credentials: &fakeCreds{}}); err == nil {
		t.Fatal("New took a 16-byte host key")
	}
}

func TestMintNeedsARegisteredRoute(t *testing.T) {
	h, err := New(Config{HostKey: testHostKey, Credentials: &fakeCreds{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Mint("brig-a", "", "anthropic", time.Hour); err == nil {
		t.Fatal("Mint minted for a sandbox that is not registered")
	}
	if err := h.AddSandbox(Sandbox{ID: "brig-a", Routes: []Route{customRoute(t, "inventory", "https://example.com", nil)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Mint("brig-a", "", "anthropic", time.Hour); err == nil {
		t.Fatal("Mint minted for a route the sandbox does not have")
	}
	if _, err := h.Mint("brig-a", "", "inventory", 0); err == nil {
		t.Fatal("Mint minted a token with no lifetime")
	}
	if _, err := h.Mint("brig-a", "", "inventory", time.Hour); err != nil {
		t.Fatal(err)
	}
}
