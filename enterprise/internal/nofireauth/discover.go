// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package nofireauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
)

// ErrNotSupported means the deployment has no Brig login yet: it has no
// authorization server metadata.
var ErrNotSupported = errors.New("this deployment does not support brig login yet")

// Metadata holds the RFC 8414 fields Brig uses, after validation.
type Metadata struct {
	Issuer                      string
	AuthorizationEndpoint       string
	TokenEndpoint               string
	DeviceAuthorizationEndpoint string
	RevocationEndpoint          string
}

// MetadataURL returns where RFC 8414 puts the metadata of d: the well-known
// segment goes between the host and the path prefix, not after it.
func MetadataURL(d Deployment) string {
	if d.u == nil {
		return ""
	}
	return d.u.Scheme + "://" + d.u.Host + "/.well-known/oauth-authorization-server" + d.u.EscapedPath()
}

type rawMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	DeviceEndpoint        string   `json:"device_authorization_endpoint"`
	RevocationEndpoint    string   `json:"revocation_endpoint"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
	IssParameterSupported bool     `json:"authorization_response_iss_parameter_supported"`
}

// Discover fetches and validates the metadata of the deployment (AUTH001
// section 5.1). It refuses metadata that names another issuer or another
// origin, or that lacks S256 or RFC 9207 support, before any further request.
// A missing device endpoint is not refused here: only the device flow needs
// it, and that flow says so.
func (c *Client) Discover(ctx context.Context) (Metadata, error) {
	req, err := http.NewRequest(http.MethodGet, MetadataURL(c.d), nil)
	if err != nil {
		return Metadata{}, err
	}
	req.Header.Set("Accept", "application/json")
	status, body, err := c.do(ctx, req)
	if err != nil {
		return Metadata{}, fmt.Errorf("cannot reach %s: %w. Check the URL and your network", c.d, err)
	}
	switch {
	case status == http.StatusNotFound:
		return Metadata{}, fmt.Errorf("%w: %s has no sign-in settings. Check the URL, or ask its administrator", ErrNotSupported, c.d)
	case status >= 300 && status < 400:
		return Metadata{}, fmt.Errorf("%s redirects its sign-in settings elsewhere, and Brig does not follow redirects. Pass the URL it redirects to as the argument", c.d)
	case status != http.StatusOK:
		return Metadata{}, fmt.Errorf("%s answered brig login with status %d. Check the URL, or try again later", c.d, status)
	}
	var m rawMetadata
	if err := json.Unmarshal(body, &m); err != nil || m.Issuer == "" {
		return Metadata{}, fmt.Errorf("%w: %s did not return sign-in settings. Check that the URL is a NOFire AI deployment", ErrNotSupported, c.d)
	}

	if m.Issuer != c.d.String() {
		return Metadata{}, fmt.Errorf("%s identifies itself as %q, and Brig refuses it because the two differ. Check the URL", c.d, CleanLine(m.Issuer))
	}
	endpoints := []struct{ name, url string }{
		{"authorization_endpoint", m.AuthorizationEndpoint},
		{"token_endpoint", m.TokenEndpoint},
		{"device_authorization_endpoint", m.DeviceEndpoint},
		{"revocation_endpoint", m.RevocationEndpoint},
	}
	for _, e := range endpoints {
		if e.url == "" {
			if e.name == "authorization_endpoint" || e.name == "token_endpoint" {
				return Metadata{}, fmt.Errorf("%s does not publish its %s, so brig login cannot sign in. Check that the URL is a NOFire AI deployment, or ask its administrator", c.d, e.name)
			}
			continue
		}
		if !c.d.SameOrigin(e.url) {
			return Metadata{}, fmt.Errorf("%s sends its %s to %q, which is not on the deployment. Brig refuses it. Check the URL, or ask its administrator", c.d, e.name, CleanLine(e.url))
		}
	}
	if !slices.Contains(m.CodeChallengeMethods, "S256") {
		return Metadata{}, fmt.Errorf("%s does not support PKCE with S256, which brig login requires. Ask its administrator to upgrade it", c.d)
	}
	if !m.IssParameterSupported {
		return Metadata{}, fmt.Errorf("%s does not support the iss authorization response parameter (RFC 9207), which brig login requires. Ask its administrator to upgrade it", c.d)
	}
	return Metadata{
		Issuer:                      m.Issuer,
		AuthorizationEndpoint:       m.AuthorizationEndpoint,
		TokenEndpoint:               m.TokenEndpoint,
		DeviceAuthorizationEndpoint: m.DeviceEndpoint,
		RevocationEndpoint:          m.RevocationEndpoint,
	}, nil
}
