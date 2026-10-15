package renew

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/brig-sh/brig/internal/secret"
)

// The provider facts the anthropic-oauth lifecycle rests on. A change on the
// provider's side lands here and nowhere else.
//
//   - A refresh is a POST of a JSON body to the token endpoint, with the
//     client id Claude Code itself uses.
//   - The provider's edge refuses Go's default User-Agent, so the request
//     names itself the way the CLI does: claude-cli/<version> (external, cli).
//   - The provider rotates the refresh token single-use. A refresh makes the
//     document it started from invalid.
const (
	anthropicTokenURL   = "https://api.anthropic.com/v1/oauth/token"
	anthropicClientID   = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	anthropicCLIVersion = "2.1.292"
	anthropicUserAgent  = "claude-cli/" + anthropicCLIVersion + " (external, cli)"
)

// maxTokenResponse bounds the token endpoint's answer. A real one is a few
// hundred bytes.
const maxTokenResponse = 64 << 10

// tokenTimeout bounds one refresh. The renewal runs on a context its caller
// cannot cancel, so this is what stops a provider that never answers.
const tokenTimeout = 30 * time.Second

// The bounds of a believable expires_in. A value outside them is not used:
// the pair is kept with no expiry and renewed on a 401.
const (
	minExpiresIn = 60
	maxExpiresIn = 30 * 24 * 60 * 60
)

// AnthropicOAuth is the anthropic-oauth lifecycle, the Renewer for the
// claude-credentials document. The zero value talks to the provider.
//
// The document is Claude Code's own .credentials.json, as `brig secret import
// claude-code` stores it: {"claudeAiOauth": {"accessToken", "refreshToken",
// "expiresAt", ...}}, with expiresAt in epoch milliseconds. A refresh replaces
// those three fields and keeps every other field as it was.
//
// The refresh goes from the host to the token endpoint over HTTPS, with
// system roots and full verification. It goes through the host's proxy when
// HTTPS_PROXY names one, and never through a broker route.
type AnthropicOAuth struct {
	// tokenURL and client replace the endpoint and the client in tests.
	tokenURL string
	client   *http.Client
	now      func() time.Time
}

var (
	_ secret.Renewer = AnthropicOAuth{}
	_ secret.Expirer = AnthropicOAuth{}
)

// refreshRequest is the body of a refresh.
type refreshRequest struct {
	GrantType    string `json:"grant_type"`
	RefreshToken string `json:"refresh_token"`
	ClientID     string `json:"client_id"`
}

// tokenResponse is the part of the token endpoint's answer a refresh reads.
// The tokens are pointers so that an absent one is told apart from an empty
// one. expires_in stays raw, because a bad one does not void the pair.
type tokenResponse struct {
	AccessToken  *string         `json:"access_token"`
	RefreshToken *string         `json:"refresh_token"`
	ExpiresIn    json.RawMessage `json:"expires_in"`
}

// errTokenShape is every 200 from the token endpoint that holds no token
// pair. It carries no part of the body, which can hold a token.
var errTokenShape = errors.New("anthropic-oauth: the token endpoint answered with a body " +
	"that is not the expected token pair")

// Renew refreshes the OAuth pair in current and returns the document with the
// new access token, refresh token and expiry.
//
// An answer that is not a 200 with both tokens is refused. The error names
// the status, or a fixed shape error for a 200, and nothing from the body.
// Every failure after the request was written wraps secret.ErrSpent, because
// the provider may have rotated the refresh token before it.
func (a AnthropicOAuth) Renew(ctx context.Context, current []byte) ([]byte, error) {
	doc, oauth, err := splitClaudeCredentials(current)
	if err != nil {
		return nil, err
	}
	var refresh string
	if err := json.Unmarshal(oauth["refreshToken"], &refresh); err != nil || refresh == "" {
		return nil, errors.New("anthropic-oauth: the stored document has no refresh token")
	}
	endpoint, err := url.Parse(a.endpoint())
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" {
		return nil, errors.New("anthropic-oauth: the token endpoint is not an https URL")
	}

	body, err := json.Marshal(refreshRequest{
		GrantType:    "refresh_token",
		RefreshToken: refresh,
		ClientID:     anthropicClientID,
	})
	if err != nil {
		return nil, err
	}
	// sent is set once the whole request is written. From then on the
	// provider may have rotated the refresh token, whatever happens next.
	var sent atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				sent.Store(true)
			}
		},
	})
	ctx, cancel := context.WithTimeout(ctx, tokenTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", anthropicUserAgent)

	resp, err := a.httpClient().Do(req)
	if err != nil {
		err = fmt.Errorf("anthropic-oauth: the token endpoint did not answer: %w", err)
		if sent.Load() {
			return nil, spent(err)
		}
		return nil, err
	}
	defer resp.Body.Close()
	tr, err := readTokenResponse(resp)
	if err != nil {
		return nil, spent(err)
	}

	// expiresAt 0 is a document with no known expiry. Expiry reads it so.
	var expiresAt int64
	var in int64
	if json.Unmarshal(tr.ExpiresIn, &in) == nil && in >= minExpiresIn && in <= maxExpiresIn {
		expiresAt = a.clock().Add(time.Duration(in) * time.Second).UnixMilli()
	}
	for field, v := range map[string]any{
		"accessToken":  *tr.AccessToken,
		"refreshToken": *tr.RefreshToken,
		"expiresAt":    expiresAt,
	} {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		oauth[field] = raw
	}
	inner, err := marshalRaw(oauth)
	if err != nil {
		return nil, err
	}
	doc["claudeAiOauth"] = inner
	return marshalRaw(doc)
}

// readTokenResponse checks the status and decodes a token pair.
func readTokenResponse(resp *http.Response) (tokenResponse, error) {
	// The status code and not resp.Status: the reason phrase is the
	// provider's text.
	if resp.StatusCode != http.StatusOK {
		return tokenResponse{}, fmt.Errorf("anthropic-oauth: the token endpoint answered %d %s",
			resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	blob, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponse+1))
	if err != nil {
		return tokenResponse{}, fmt.Errorf("anthropic-oauth: reading the token endpoint's answer: %w", err)
	}
	if len(blob) > maxTokenResponse {
		return tokenResponse{}, errTokenShape
	}
	var tr tokenResponse
	// The decode error is dropped: a syntax error quotes the body.
	if err := json.Unmarshal(blob, &tr); err != nil {
		return tokenResponse{}, errTokenShape
	}
	if tr.AccessToken == nil || tr.RefreshToken == nil ||
		!tokenShaped(*tr.AccessToken) || !tokenShaped(*tr.RefreshToken) {
		return tokenResponse{}, errTokenShape
	}
	return tr, nil
}

// spent marks err as a failure after the refresh reached the provider.
func spent(err error) error { return fmt.Errorf("%w: %w", err, secret.ErrSpent) }

// Expiry returns claudeAiOauth.expiresAt, or the zero time when the document
// carries none or carries 0.
func (AnthropicOAuth) Expiry(doc []byte) (time.Time, error) {
	_, oauth, err := splitClaudeCredentials(doc)
	if err != nil {
		return time.Time{}, err
	}
	raw, ok := oauth["expiresAt"]
	if !ok || string(raw) == "null" {
		return time.Time{}, nil
	}
	var ms int64
	if err := json.Unmarshal(raw, &ms); err != nil {
		return time.Time{}, errors.New("anthropic-oauth: claudeAiOauth.expiresAt is not epoch milliseconds")
	}
	if ms == 0 {
		return time.Time{}, nil
	}
	return time.UnixMilli(ms), nil
}

func (a AnthropicOAuth) endpoint() string {
	if a.tokenURL != "" {
		return a.tokenURL
	}
	return anthropicTokenURL
}

// httpClient returns the client to send a refresh with, refusing redirects.
// A 307 or 308 sends the body again, refresh token included, to wherever the
// Location header points.
func (a AnthropicOAuth) httpClient() *http.Client {
	c := http.Client{Timeout: tokenTimeout}
	if a.client != nil {
		c = *a.client
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

func (a AnthropicOAuth) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// splitClaudeCredentials decodes the document and its claudeAiOauth object,
// each as raw fields, so a rewrite keeps the fields it does not name. The
// errors quote nothing from the document.
func splitClaudeCredentials(doc []byte) (map[string]json.RawMessage, map[string]json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(doc, &top); err != nil || top == nil {
		return nil, nil, errors.New("anthropic-oauth: the stored document is not a JSON object")
	}
	var oauth map[string]json.RawMessage
	if err := json.Unmarshal(top["claudeAiOauth"], &oauth); err != nil || oauth == nil {
		return nil, nil, errors.New("anthropic-oauth: the stored document has no claudeAiOauth object")
	}
	return top, oauth, nil
}

// marshalRaw encodes fields without HTML escaping. json.Marshal would
// rewrite a <, > or & in a field brig only carries through.
func marshalRaw(fields map[string]json.RawMessage) (json.RawMessage, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(fields); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// tokenShaped reports whether s can be a token: not empty, and printable
// ASCII with no space. The access token goes into an Authorization header.
func tokenShaped(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}
