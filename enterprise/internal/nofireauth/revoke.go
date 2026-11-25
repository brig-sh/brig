// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package nofireauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Revoke asks the deployment to revoke token (RFC 7009). An error never
// contains the token: it is a credential, and errors are printed.
func (c *Client) Revoke(ctx context.Context, m Metadata, token string) error {
	if m.RevocationEndpoint == "" {
		return errors.New("the deployment does not offer token revocation")
	}
	form := url.Values{"token": {token}, "client_id": {ClientID}}
	req, err := http.NewRequest(http.MethodPost, m.RevocationEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("cannot revoke the token at %s: its revocation address is not valid", c.d)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	status, _, err := c.do(ctx, req)
	if err != nil {
		return fmt.Errorf("revoke the token at %s: %w", c.d, err)
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("the revocation request to %s failed with status %d", c.d, status)
	}
	return nil
}
