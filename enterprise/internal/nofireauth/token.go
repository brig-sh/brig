// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package nofireauth

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxToken bounds the access token Brig accepts. A token is sent on every
// request to the deployment, so a huge one is refused, not stored.
const maxToken = 8 << 10

// Grant is an access token and the lifetime the deployment granted it.
type Grant struct {
	AccessToken string
	ExpiresIn   time.Duration
}

// oauthError is an RFC 6749 section 5.2 error from the token endpoint. The
// device flow reads code to tell authorization_pending from a refusal.
type oauthError struct {
	server      Deployment
	code        string
	description string
}

func (e *oauthError) Error() string {
	return "the token request was refused: " + Clean(e.server, oauthMessage(e.code, e.description))
}

// oauthMessage joins an RFC 6749 error code and its description as Brig
// prints them. The caller cleans the result: both come from the server.
func oauthMessage(code, description string) string {
	if description == "" {
		return code
	}
	return code + ": " + description
}

type tokenResponse struct {
	AccessToken      string          `json:"access_token"`
	TokenType        string          `json:"token_type"`
	ExpiresIn        json.RawMessage `json:"expires_in"`
	Error            string          `json:"error"`
	ErrorDescription string          `json:"error_description"`
}

// requestToken posts form to the token endpoint and returns the grant. A
// token it refuses is revoked first, so a token Brig will not keep does not
// stay valid on the server.
func (c *Client) requestToken(ctx context.Context, m Metadata, form url.Values) (Grant, error) {
	status, body, err := c.postForm(ctx, m.TokenEndpoint, "token address", "request a token", form)
	if err != nil {
		return Grant{}, err
	}
	var r tokenResponse
	jsonErr := json.Unmarshal(body, &r)
	switch {
	case status != http.StatusOK && jsonErr == nil && r.Error != "":
		return Grant{}, &oauthError{server: c.d, code: r.Error, description: r.ErrorDescription}
	case status != http.StatusOK:
		return Grant{}, fmt.Errorf("%s answered the token request with status %d. Run brig login again later", c.d, status)
	case jsonErr != nil:
		// The body may hold a token, but one that cannot be parsed cannot
		// be revoked either; the error says nothing of the body.
		return Grant{}, fmt.Errorf("%s sent a token response that is not JSON. Run brig login again later, or ask its administrator", c.d)
	}

	g, problem := r.grant()
	if problem == "" {
		return g, nil
	}
	err = fmt.Errorf("%s issued a token Brig cannot use: %s", c.d, problem)
	if r.AccessToken == "" {
		return Grant{}, err
	}
	// The user may have cancelled; the token is revoked all the same.
	if rerr := c.Revoke(context.WithoutCancel(ctx), m, r.AccessToken); rerr != nil {
		return Grant{}, fmt.Errorf("%w. Brig could not revoke it, so it stays valid until it expires: %v. Revoke it from the dashboard", err, rerr)
	}
	return Grant{}, fmt.Errorf("%w. Brig revoked it", err)
}

// grant validates r. problem says what is wrong and never quotes the token.
func (r tokenResponse) grant() (Grant, string) {
	switch {
	case r.AccessToken == "":
		return Grant{}, "the response has no access token"
	case len(r.AccessToken) > maxToken:
		return Grant{}, fmt.Sprintf("the access token is larger than %d KiB", maxToken>>10)
	case !strings.EqualFold(r.TokenType, "Bearer"):
		return Grant{}, fmt.Sprintf("its type is %q, and brig accepts only Bearer", CleanLine(r.TokenType))
	}
	// expires_in must be a JSON integer: a missing, fractional or quoted
	// value is refused, as is one too large to be a time.Duration.
	n, err := strconv.ParseInt(string(r.ExpiresIn), 10, 64)
	if err != nil || n > math.MaxInt64/int64(time.Second) {
		return Grant{}, "its lifetime is missing or is not a whole number of seconds"
	}
	if n <= 0 {
		return Grant{}, fmt.Sprintf("its lifetime is %d seconds, and brig needs a positive lifetime", n)
	}
	return Grant{AccessToken: r.AccessToken, ExpiresIn: time.Duration(n) * time.Second}, ""
}
