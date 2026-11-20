package sessiontoken

import (
	"bytes"
	"testing"
	"time"
)

func testKey(t *testing.T, label, sandbox string) []byte {
	t.Helper()
	hk := make([]byte, MinHostKey)
	for i := range hk {
		hk[i] = byte(i)
	}
	key, err := SandboxKey(hk, label, sandbox, bytes.Repeat([]byte{1}, 16))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// The token below is what the broker minted before signing moved here. A
// route token that changed by a byte would be refused by a broker that did
// not move with it.
func TestRouteTokenUnchanged(t *testing.T) {
	const want = "eyJzYW5kYm94IjoiYnJpZy1hIiwic2Vzc2lvbiI6InJlZmFjdG9yIiwicm91dGUiOiJhbnRocm9waWMiLCJleHAiOjE4MDAwMDM2MDAsInR5cCI6ImJyaWctcm91dGUifQ.L4nfSP2fW3TLnqEw2x-bXR1Gujjy0wdMnF90uLOXgC0"
	key := testKey(t, "brig-sandbox", "brig-a")
	got, err := Mint(key, Claims{Sandbox: "brig-a", Session: "refactor", Route: "anthropic", Exp: 1800003600, Typ: "brig-route"})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("token = %s, want %s", got, want)
	}
}

func TestVerifyRefusesOtherTyp(t *testing.T) {
	key := testKey(t, "brig-sandbox", "brig-a")
	now := time.Unix(1_800_000_000, 0)
	exp := now.Add(time.Hour).Unix()
	for _, tc := range []struct{ minted, asked string }{
		{"brig-memory", "brig-route"},
		{"brig-route", "brig-memory"},
	} {
		tok, err := Mint(key, Claims{Sandbox: "brig-a", Route: "r", Exp: exp, Typ: tc.minted})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Verify(key, tok, "brig-a", "r", tc.asked, now); err != ErrSignature {
			t.Errorf("a %s token checked as %s: %v, want %v", tc.minted, tc.asked, err, ErrSignature)
		}
		if _, err := Verify(key, tok, "brig-a", "r", tc.minted, now); err != nil {
			t.Errorf("a %s token checked as itself: %v", tc.minted, err)
		}
	}
}

func TestVerifyRefusesOtherSandbox(t *testing.T) {
	key := testKey(t, "brig-sandbox", "brig-a")
	now := time.Unix(1_800_000_000, 0)
	tok, err := Mint(key, Claims{Sandbox: "brig-b", Route: "r", Exp: now.Add(time.Hour).Unix(), Typ: "brig-route"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(key, tok, "brig-a", "r", "brig-route", now); err != ErrSandbox {
		t.Errorf("a token naming another sandbox: %v, want %v", err, ErrSandbox)
	}
}

func TestVerifyRefusesExpired(t *testing.T) {
	key := testKey(t, "brig-sandbox", "brig-a")
	now := time.Unix(1_800_000_000, 0)
	tok, err := Mint(key, Claims{Sandbox: "brig-a", Route: "r", Exp: now.Unix(), Typ: "brig-route"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(key, tok, "brig-a", "r", "brig-route", now); err != ErrExpired {
		t.Errorf("a token expiring at now: %v, want %v", err, ErrExpired)
	}
}
