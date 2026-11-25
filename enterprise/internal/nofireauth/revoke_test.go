// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package nofireauth_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/brig-sh/brig/enterprise/internal/nofireauth/fakeas"
)

func TestRevokeSendsToken(t *testing.T) {
	s := start(t, fakeas.Options{})
	c := client(t, s.URL)
	m, err := c.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Revoke(context.Background(), m, "tok-123"); err != nil {
		t.Fatal(err)
	}
	if got := s.Revoked(); !slices.Equal(got, []string{"tok-123"}) {
		t.Errorf("revoked = %q", got)
	}
	// The fake answers invalid_client unless client_id=brig was in the form.
	if !slices.Contains(s.Requests(), "POST /oauth/revoke") {
		t.Errorf("requests = %q", s.Requests())
	}
}

func TestRevokeFailureIsAnError(t *testing.T) {
	s := start(t, fakeas.Options{RevokeStatus: 500})
	c := client(t, s.URL)
	m, err := c.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = c.Revoke(context.Background(), m, "secret-token-value")
	if err == nil {
		t.Fatal("Revoke succeeded on a 500")
	}
	if strings.Contains(err.Error(), "secret-token-value") {
		t.Errorf("error contains the token: %q", err)
	}
}
