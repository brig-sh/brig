package broker

import (
	"fmt"
	"time"

	"github.com/brig-sh/brig/internal/sessiontoken"
)

// tokenType is the typ claim of a session token. It keeps a token from being
// read as another HMAC-signed document that a later key derivation shares.
const tokenType = "brig-route"

// sandboxLabel is the key-derivation label of a route token.
const sandboxLabel = "brig-sandbox"

// minHostKey is the shortest host key the broker takes.
const minHostKey = sessiontoken.MinHostKey

// DefaultTokenTTL is the lifetime of a session token minted per exec.
const DefaultTokenTTL = 12 * time.Hour

// claims is the signed part of a session token.
type claims = sessiontoken.Claims

// nonceSize is the length of the registration nonce of a sandbox.
const nonceSize = 16

// sandboxKey derives the HMAC key of one registration of a sandbox. Signing
// lives in internal/sessiontoken, so a second process can sign and check
// tokens without linking the broker.
func sandboxKey(hostKey []byte, sandbox string, nonce []byte) ([]byte, error) {
	return sessiontoken.SandboxKey(hostKey, sandboxLabel, sandbox, nonce)
}

func mint(key []byte, c claims) (string, error) { return sessiontoken.Mint(key, c) }

// The failed checks of a session token. Each is the check= field of the audit
// line of a 401.
var (
	errTokenMissing   = sessiontoken.ErrMissing
	errTokenSignature = sessiontoken.ErrSignature
	errTokenSandbox   = sessiontoken.ErrSandbox
	errTokenRoute     = sessiontoken.ErrRoute
	errTokenExpired   = sessiontoken.ErrExpired
)

// verify checks a route token: its signature, then that it names sandbox and
// route and has not expired at now.
func verify(key []byte, token, sandbox, route string, now time.Time) (claims, error) {
	return sessiontoken.Verify(key, token, sandbox, route, tokenType, now)
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
