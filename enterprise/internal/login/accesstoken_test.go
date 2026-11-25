// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package login

import (
	"testing"
	"time"

	"github.com/brig-sh/brig/enterprise/internal/nofireauth"
	"github.com/brig-sh/brig/internal/secret/login"
)

func parseDeployment(t *testing.T, raw string) nofireauth.Deployment {
	t.Helper()
	d, err := nofireauth.ParseDeployment(raw)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// AccessToken answers for exactly the deployment it is given. A token stored
// for another deployment, an expired one, an empty one and a host with no
// keychain all read as "".
func TestAccessTokenIsTheStoredUnexpiredTokenOfThatDeployment(t *testing.T) {
	m := loginHost(t)
	a := parseDeployment(t, "https://a.example")
	b := parseDeployment(t, "https://b.example")
	m.items[a.String()] = login.Token{URL: a.String(), AccessToken: "tok-a", ExpiresAt: time.Now().Add(time.Hour)}

	if got, _ := AccessToken(a); got != "tok-a" {
		t.Errorf("AccessToken(a) = %q, want tok-a", got)
	}
	if got, _ := AccessToken(b); got != "" {
		t.Errorf("AccessToken(b) = %q, want nothing: the token is a's", got)
	}
	if got, _ := AccessToken(nofireauth.Deployment{}); got != "" {
		t.Errorf("AccessToken(zero) = %q, want nothing", got)
	}

	m.items[a.String()] = login.Token{URL: a.String(), AccessToken: "tok-a", ExpiresAt: time.Now().Add(-time.Minute)}
	if got, _ := AccessToken(a); got != "" {
		t.Errorf("an expired token read as %q", got)
	}
	m.items[a.String()] = login.Token{URL: a.String(), ExpiresAt: time.Now().Add(time.Hour)}
	if got, _ := AccessToken(a); got != "" {
		t.Errorf("an empty token read as %q", got)
	}

	openLoginStore = func() (login.Store, error) { return nil, login.ErrUnsupported }
	if got, _ := AccessToken(a); got != "" {
		t.Errorf("a host with no keychain read as %q", got)
	}
}

// AccessToken reads and nothing more: it does not write, replace or delete
// what the store holds, also when the token is expired.
func TestAccessTokenChangesNothingInTheStore(t *testing.T) {
	m := loginHost(t)
	a := parseDeployment(t, "https://a.example")
	m.items[a.String()] = login.Token{URL: a.String(), AccessToken: "tok-a", ExpiresAt: time.Now().Add(-time.Hour)}
	_, _ = AccessToken(a)
	if len(m.items) != 1 || m.items[a.String()].AccessToken != "tok-a" {
		t.Errorf("the store changed: %v", m.items)
	}
}
