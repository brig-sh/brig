// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package login

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/brig-sh/brig/enterprise/internal/nofireauth"
	"github.com/brig-sh/brig/internal/secret/login"
)

// access is AccessToken with the run's bound, for the tests that do not
// concern the bound.
func access(d nofireauth.Deployment) (string, string) {
	tok, v, _, _ := AccessToken(d, KeyringWait)
	return tok, v
}

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

	if got, _ := access(a); got != "tok-a" {
		t.Errorf("AccessToken(a) = %q, want tok-a", got)
	}
	if got, _ := access(b); got != "" {
		t.Errorf("AccessToken(b) = %q, want nothing: the token is a's", got)
	}
	if got, _ := access(nofireauth.Deployment{}); got != "" {
		t.Errorf("AccessToken(zero) = %q, want nothing", got)
	}

	m.items[a.String()] = login.Token{URL: a.String(), AccessToken: "tok-a", ExpiresAt: time.Now().Add(-time.Minute)}
	if got, _ := access(a); got != "" {
		t.Errorf("an expired token read as %q", got)
	}
	m.items[a.String()] = login.Token{URL: a.String(), ExpiresAt: time.Now().Add(time.Hour)}
	if got, _ := access(a); got != "" {
		t.Errorf("an empty token read as %q", got)
	}

	openLoginStore = func() (login.Store, error) { return nil, login.ErrUnsupported }
	if got, _ := access(a); got != "" {
		t.Errorf("a host with no keychain read as %q", got)
	}
}

// AccessToken reads and nothing more: it does not write, replace or delete
// what the store holds, also when the token is expired.
func TestAccessTokenChangesNothingInTheStore(t *testing.T) {
	m := loginHost(t)
	a := parseDeployment(t, "https://a.example")
	m.items[a.String()] = login.Token{URL: a.String(), AccessToken: "tok-a", ExpiresAt: time.Now().Add(-time.Hour)}
	_, _ = access(a)
	if len(m.items) != 1 || m.items[a.String()].AccessToken != "tok-a" {
		t.Errorf("the store changed: %v", m.items)
	}
}

// blockingStore is a keychain that waits on an unlock prompt nobody sees: Load
// does not return until the test lets it, and it counts every write.
type blockingStore struct {
	release chan struct{}
	writes  atomic.Int32
	loads   atomic.Int32
}

func (b *blockingStore) Save(login.Token) error { b.writes.Add(1); return nil }
func (b *blockingStore) Delete(string) error    { b.writes.Add(1); return nil }
func (b *blockingStore) Load(url string) (login.Token, error) {
	b.loads.Add(1)
	<-b.release
	return login.Token{URL: url, AccessToken: "late", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// A keychain that does not answer holds the run for the bound and no longer. The
// answer that comes after is dropped, and the goroutine that waited for it only
// reads.
func TestAccessTokenStopsWaitingForAKeychainThatDoesNotAnswer(t *testing.T) {
	loginHost(t)
	b := &blockingStore{release: make(chan struct{})}
	openLoginStore = func() (login.Store, error) { return b, nil }
	a := parseDeployment(t, "https://a.example")
	type res struct {
		tok, v   string
		timedOut bool
	}
	done := make(chan res, 1)
	go func() {
		tok, v, to, _ := AccessToken(a, 100*time.Millisecond)
		done <- res{tok, v, to}
	}()
	var r res
	select {
	case r = <-done:
	case <-time.After(2 * time.Second):
		close(b.release)
		t.Fatal("AccessToken waited on the keychain past its bound")
	}
	tok, v, timedOut := r.tok, r.v, r.timedOut
	if tok != "" || v != "" || !timedOut {
		t.Errorf("AccessToken = %q, %q, %v; want nothing and a timeout", tok, v, timedOut)
	}
	close(b.release) // the prompt is answered late
	time.Sleep(50 * time.Millisecond)
	if b.writes.Load() != 0 || b.loads.Load() != 1 {
		t.Errorf("writes %d, loads %d; a late answer must write nothing", b.writes.Load(), b.loads.Load())
	}
}

// A keychain that fails, or has no item, is not a timeout: the run goes on
// unrecorded without a word, as with no login.
func TestAccessTokenIsNotATimeoutWhenTheKeychainFails(t *testing.T) {
	m := loginHost(t)
	a := parseDeployment(t, "https://a.example")
	if _, _, timedOut, _ := AccessToken(a, time.Second); timedOut {
		t.Error("a missing item was a timeout")
	}
	openLoginStore = func() (login.Store, error) { return nil, login.ErrUnsupported }
	if _, _, timedOut, _ := AccessToken(a, time.Second); timedOut {
		t.Error("a host with no keychain was a timeout")
	}
	_ = m
}
