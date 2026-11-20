// Package sessiontoken signs and checks the session tokens that brig's
// processes hand a guest: an HMAC-SHA256 over a small claims document, keyed
// per sandbox from a host key. It is its own package so a process that signs
// or checks tokens need not link internal/broker, and so the run path's ban
// on signing (internal/hostsrc/arch_test.go) checks one import. The kinds of
// token differ in their typ claim and in the label of their key derivation,
// so a token of one kind is not read as another.
package sessiontoken

import (
	"bytes"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

// MinHostKey is the shortest host key a signer takes, the output size of
// SHA-256.
const MinHostKey = 32

// Claims is the signed part of a session token.
type Claims struct {
	Sandbox string `json:"sandbox"`
	Session string `json:"session"`
	Route   string `json:"route"`
	Exp     int64  `json:"exp"`
	Typ     string `json:"typ"`
}

// SandboxKey derives the HMAC key of one registration of a sandbox from the
// host key. Each sandbox has its own key, so the tokens of one sandbox can be
// refused without touching another's. The nonce is drawn per registration,
// so a sandbox added again under the same id refuses the tokens of the
// previous one. The label names the kind of token the key signs, so one kind
// cannot be forged from another's key.
func SandboxKey(hostKey []byte, label, sandbox string, nonce []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, hostKey, nil, label+":"+sandbox+":"+string(nonce), sha256.Size)
}

// Mint returns base64url(claims) "." base64url(HMAC-SHA256(key, claims)).
func Mint(key []byte, c Claims) (string, error) {
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
	ErrMissing   = errors.New("token")
	ErrSignature = errors.New("signature")
	ErrSandbox   = errors.New("token.sandbox")
	ErrRoute     = errors.New("token.route")
	ErrExpired   = errors.New("expired")
)

// Verify checks the signature of token with key, then that it is of kind typ,
// names sandbox and route and has not expired at now. The claims are parsed
// only after the signature matches, so a forged token never reaches the JSON
// decoder.
func Verify(key []byte, token, sandbox, route, typ string, now time.Time) (Claims, error) {
	body, mac, ok := strings.Cut(token, ".")
	if !ok || body == "" || mac == "" || strings.Contains(mac, ".") {
		return Claims{}, ErrMissing
	}
	// Strict refuses the unused low bits of the last character, so one token
	// has one spelling.
	enc := base64.RawURLEncoding.Strict()
	blob, err := enc.DecodeString(body)
	if err != nil {
		return Claims{}, ErrMissing
	}
	got, err := enc.DecodeString(mac)
	if err != nil {
		return Claims{}, ErrMissing
	}
	if !hmac.Equal(got, sign(key, blob)) {
		return Claims{}, ErrSignature
	}
	var c Claims
	dec := json.NewDecoder(bytes.NewReader(blob))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || c.Typ != typ {
		return Claims{}, ErrSignature
	}
	if _, err := dec.Token(); err != io.EOF {
		return Claims{}, ErrSignature
	}
	switch {
	case c.Sandbox != sandbox:
		return Claims{}, ErrSandbox
	case c.Route != route:
		return Claims{}, ErrRoute
	case !now.Before(time.Unix(c.Exp, 0)):
		return Claims{}, ErrExpired
	}
	return c, nil
}
