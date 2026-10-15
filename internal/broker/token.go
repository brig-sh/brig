package broker

import (
	"bytes"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// tokenType is the typ claim of a session token. It keeps a token from being
// read as another HMAC-signed document that a later key derivation shares.
const tokenType = "brig-route"

// minHostKey is the shortest host key the broker takes, the output size of
// SHA-256.
const minHostKey = 32

// DefaultTokenTTL is the lifetime of a session token minted per exec.
const DefaultTokenTTL = 12 * time.Hour

// claims is the signed part of a session token.
type claims struct {
	Sandbox string `json:"sandbox"`
	Session string `json:"session"`
	Route   string `json:"route"`
	Exp     int64  `json:"exp"`
	Typ     string `json:"typ"`
}

// nonceSize is the length of the registration nonce of a sandbox.
const nonceSize = 16

// sandboxKey derives the HMAC key of one registration of a sandbox from the
// host key. Each sandbox has its own key, so the tokens of one sandbox can be
// refused without touching another's. The nonce is drawn per registration,
// so a sandbox added again under the same id refuses the tokens of the
// previous one.
func sandboxKey(hostKey []byte, sandbox string, nonce []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, hostKey, nil, "brig-sandbox:"+sandbox+":"+string(nonce), sha256.Size)
}

// mint returns base64url(claims) "." base64url(HMAC-SHA256(key, claims)).
func mint(key []byte, c claims) (string, error) {
	blob, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString(blob) + "." + enc.EncodeToString(sign(key, blob)), nil
}

func sign(key, blob []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(blob)
	return m.Sum(nil)
}

// The failed checks of a session token. Each is the check= field of the audit
// line of a 401.
var (
	errTokenMissing   = errors.New("token")
	errTokenSignature = errors.New("signature")
	errTokenSandbox   = errors.New("token.sandbox")
	errTokenRoute     = errors.New("token.route")
	errTokenExpired   = errors.New("expired")
)

// verify checks the signature of token with key, then that it names sandbox
// and route and has not expired at now. The claims are parsed only after the
// signature matches, so a forged token never reaches the JSON decoder.
func verify(key []byte, token, sandbox, route string, now time.Time) (claims, error) {
	body, mac, ok := strings.Cut(token, ".")
	if !ok || body == "" || mac == "" || strings.Contains(mac, ".") {
		return claims{}, errTokenMissing
	}
	// Strict refuses the unused low bits of the last character, so one token
	// has one spelling.
	enc := base64.RawURLEncoding.Strict()
	blob, err := enc.DecodeString(body)
	if err != nil {
		return claims{}, errTokenMissing
	}
	got, err := enc.DecodeString(mac)
	if err != nil {
		return claims{}, errTokenMissing
	}
	if !hmac.Equal(got, sign(key, blob)) {
		return claims{}, errTokenSignature
	}
	var c claims
	dec := json.NewDecoder(bytes.NewReader(blob))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || c.Typ != tokenType {
		return claims{}, errTokenSignature
	}
	if _, err := dec.Token(); err != io.EOF {
		return claims{}, errTokenSignature
	}
	switch {
	case c.Sandbox != sandbox:
		return claims{}, errTokenSandbox
	case c.Route != route:
		return claims{}, errTokenRoute
	case !now.Before(time.Unix(c.Exp, 0)):
		return claims{}, errTokenExpired
	}
	return c, nil
}

// Mint returns a session token for one route of a registered sandbox, valid
// for ttl from now.
//
// The caller delivers it to the guest in place of the credential. The broker
// only verifies tokens, so a guest cannot mint one: it holds neither the host
// key nor any sandbox key. The sandbox keeps the token among its live ones,
// for the placeholder scan.
func (h *Handler) Mint(sandbox, session, route string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		return "", fmt.Errorf("a session token needs a positive lifetime, not %s", ttl)
	}
	sb := h.sandbox(sandbox)
	if sb == nil {
		return "", fmt.Errorf("sandbox %q is not registered with the broker", sandbox)
	}
	if _, ok := sb.routes[route]; !ok {
		return "", fmt.Errorf("sandbox %s has no route %q", sandbox, route)
	}
	exp := h.now().Add(ttl).Unix()
	tok, err := mint(sb.key, claims{
		Sandbox: sandbox,
		Session: session,
		Route:   route,
		Exp:     exp,
		Typ:     tokenType,
	})
	if err != nil {
		return "", err
	}
	sb.remember(tok, route, time.Unix(exp, 0))
	return tok, nil
}
