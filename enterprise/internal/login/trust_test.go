// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package login

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/enterprise/internal/nofireauth/fakeas"
	"github.com/brig-sh/brig/internal/secret/login"
)

// AccessToken returns the token only while every trust store the environment
// sets is the one accepted at login, and otherwise the name of the variable and
// no token. Nobody who has no unexpired login is told about a variable.
func TestAccessTokenWithATrustStoreFromTheEnvironment(t *testing.T) {
	a := parseDeployment(t, "https://a.example")
	live := func(trust map[string]string) login.Token {
		return login.Token{URL: a.String(), AccessToken: "tok-a", ExpiresAt: time.Now().Add(time.Hour), Trust: trust}
	}
	for _, tc := range []struct {
		name      string
		item      *login.Token
		env       map[string]string
		wantToken string
		wantVar   string
	}{
		{"none set", ptr(live(nil)), nil, "tok-a", ""},
		{"set and accepted", ptr(live(map[string]string{"SSL_CERT_FILE": "/etc/ca.pem"})), map[string]string{"SSL_CERT_FILE": "/etc/ca.pem"}, "tok-a", ""},
		{"set and never accepted", ptr(live(nil)), map[string]string{"SSL_CERT_FILE": "/etc/ca.pem"}, "", "SSL_CERT_FILE"},
		{"accepted but the value changed", ptr(live(map[string]string{"SSL_CERT_FILE": "/etc/ca.pem"})), map[string]string{"SSL_CERT_FILE": "/etc/other.pem"}, "", "SSL_CERT_FILE"},
		{"the other variable was accepted", ptr(live(map[string]string{"SSL_CERT_DIR": "/etc/certs"})), map[string]string{"SSL_CERT_FILE": "/etc/certs"}, "", "SSL_CERT_FILE"},
		{"both set, one accepted", ptr(live(map[string]string{"SSL_CERT_FILE": "/f"})), map[string]string{"SSL_CERT_FILE": "/f", "SSL_CERT_DIR": "/d"}, "", "SSL_CERT_DIR"},
		{"accepted, then unset", ptr(live(map[string]string{"SSL_CERT_FILE": "/f"})), nil, "tok-a", ""},
		{"not signed in", nil, map[string]string{"SSL_CERT_FILE": "/f"}, "", ""},
		{"login expired", ptr(login.Token{URL: a.String(), AccessToken: "tok-a", ExpiresAt: time.Now().Add(-time.Hour)}), map[string]string{"SSL_CERT_FILE": "/f"}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := loginHost(t)
			if tc.item != nil {
				m.items[a.String()] = *tc.item
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			tok, v := access(a)
			if tok != tc.wantToken || v != tc.wantVar {
				t.Errorf("AccessToken = %q, %q; want %q, %q", tok, v, tc.wantToken, tc.wantVar)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// A record written before the trust field existed accepts nothing.
func TestAStoredLoginWithNoTrustFieldAcceptsNothing(t *testing.T) {
	m := loginHost(t)
	a := parseDeployment(t, "https://a.example")
	m.items[a.String()] = login.Token{URL: a.String(), AccessToken: "tok-a", ExpiresAt: time.Now().Add(time.Hour)}
	t.Setenv("SSL_CERT_DIR", "/etc/certs")
	if tok, v := access(a); tok != "" || v != "SSL_CERT_DIR" {
		t.Errorf("AccessToken = %q, %q", tok, v)
	}
}

// brig login stores the trust store the person answered y to, with its exact
// value, beside the token. A login with none set stores none, and the next
// login replaces what an earlier one stored.
func TestLoginStoresTheTrustStoreItAccepted(t *testing.T) {
	m := loginHost(t)
	s := fakeDeployment(t, fakeas.Options{})
	t.Setenv("SSL_CERT_FILE", "/etc/ca.pem")
	t.Setenv("SSL_CERT_DIR", "/etc/certs")
	master := terminalStdin(t)
	if _, err := master.Write([]byte("y\n")); err != nil {
		t.Fatal(err)
	}
	var err error
	captureStderr(t, func() { err = loginCmd(io.Discard, []string{s.URL, "--device"}) })
	if err != nil {
		t.Fatal(err)
	}
	got := m.items[s.URL].Trust
	if len(got) != 2 || got["SSL_CERT_FILE"] != "/etc/ca.pem" || got["SSL_CERT_DIR"] != "/etc/certs" {
		t.Errorf("stored trust %v", got)
	}

	// The next login, with no trust store set, replaces it.
	t.Setenv("SSL_CERT_FILE", "")
	t.Setenv("SSL_CERT_DIR", "")
	if err := loginCmd(io.Discard, []string{s.URL, "--device"}); err != nil {
		t.Fatal(err)
	}
	if got := m.items[s.URL].Trust; len(got) != 0 {
		t.Errorf("the next login kept %v", got)
	}
}

// brig logout removes the stored item, and the accepted trust store with it.
func TestLogoutRemovesTheAcceptedTrustStore(t *testing.T) {
	m := loginHost(t)
	s := fakeDeployment(t, fakeas.Options{})
	t.Setenv("SSL_CERT_FILE", "/etc/ca.pem")
	master := terminalStdin(t)
	if _, err := master.Write([]byte("y\n")); err != nil {
		t.Fatal(err)
	}
	var err error
	captureStderr(t, func() { err = loginCmd(io.Discard, []string{s.URL, "--device"}) })
	if err != nil || len(m.items[s.URL].Trust) != 1 {
		t.Fatalf("login: %v, trust %v", err, m.items[s.URL].Trust)
	}
	t.Setenv("SSL_CERT_FILE", "")
	if err := logoutCmd(io.Discard, []string{s.URL}); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.items[s.URL]; ok {
		t.Error("the item, and its trust, stayed after logout")
	}
	t.Setenv("SSL_CERT_FILE", "/etc/ca.pem")
	d := parseDeployment(t, s.URL)
	if tok, v := access(d); tok != "" || v != "" {
		t.Errorf("AccessToken after logout = %q, %q", tok, v)
	}
}

// A relative value names a different file in every working directory, so it is
// stored as an absolute path and compared as one. The same relative value from
// the directory it was accepted in is accepted, with or without a ./ or a
// parent step in it. The same text from another directory, such as a cloned
// repository that ships its own certs/ca.pem, is not.
func TestARelativeTrustStoreIsAcceptedOnlyFromTheDirectoryItWasAcceptedIn(t *testing.T) {
	m := loginHost(t)
	s := fakeDeployment(t, fakeas.Options{})
	home := t.TempDir()
	repo := t.TempDir()
	t.Chdir(home)
	t.Setenv("SSL_CERT_FILE", "certs/ca.pem")
	master := terminalStdin(t)
	if _, err := master.Write([]byte("y\n")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	var err error
	captureStderr(t, func() { err = loginCmd(&out, []string{s.URL, "--device"}) })
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "certs", "ca.pem")
	if got := m.items[s.URL].Trust["SSL_CERT_FILE"]; got != want {
		t.Fatalf("stored %q, want the absolute path %q", got, want)
	}
	if !strings.Contains(out.String(), "Trusting certificates from "+want+" (from SSL_CERT_FILE)") {
		t.Errorf("the question names %q", out.String())
	}
	d := parseDeployment(t, s.URL)

	for _, val := range []string{"certs/ca.pem", "./certs/ca.pem", want, home + "/certs/./ca.pem", home + "//certs/ca.pem"} {
		t.Setenv("SSL_CERT_FILE", val)
		if tok, v := access(d); tok == "" || v != "" {
			t.Errorf("%q from the same directory: %q, %q; want it accepted", val, tok, v)
		}
	}
	t.Chdir(repo)
	t.Setenv("SSL_CERT_FILE", "certs/ca.pem")
	if tok, v := access(d); tok != "" || v != "SSL_CERT_FILE" {
		t.Errorf("the same text from another directory: %q, %q; want it refused", tok, v)
	}
}

// A value with a .. element is never accepted, because the kernel resolves .. through
// symlinks and cleaning it away by text names another file. The reviewer's
// case: a parent step after a symlink cleans to the accepted path.
func TestAValueWithAParentElementIsNeverAccepted(t *testing.T) {
	m := loginHost(t)
	a := parseDeployment(t, "https://a.example")
	dir := t.TempDir()
	accepted := filepath.Join(dir, "etc", "ssl", "corp", "ca.pem")
	m.items[a.String()] = login.Token{URL: a.String(), AccessToken: "tok-a", ExpiresAt: time.Now().Add(time.Hour),
		Trust: map[string]string{"SSL_CERT_FILE": accepted}}
	for _, val := range []string{
		dir + "/etc/ssl/corp/link/../ca.pem", // cleans to the accepted path
		"../x/etc/ssl/corp/ca.pem",
		accepted + "/..",
		"..",
	} {
		t.Setenv("SSL_CERT_FILE", val)
		if tok, v := access(a); tok != "" || v != "SSL_CERT_FILE" {
			t.Errorf("%q: %q, %q; want it refused", val, tok, v)
		}
	}
	// ".." inside an element is not a parent step.
	m.items[a.String()] = login.Token{URL: a.String(), AccessToken: "tok-a", ExpiresAt: time.Now().Add(time.Hour),
		Trust: map[string]string{"SSL_CERT_FILE": filepath.Join(dir, "a..b", "ca.pem")}}
	t.Setenv("SSL_CERT_FILE", dir+"/a..b/ca.pem")
	if tok, v := access(a); tok == "" || v != "" {
		t.Errorf("a..b was refused: %q, %q", tok, v)
	}
	// One .. entry of an SSL_CERT_DIR list refuses the list.
	t.Setenv("SSL_CERT_FILE", "")
	m.items[a.String()] = login.Token{URL: a.String(), AccessToken: "tok-a", ExpiresAt: time.Now().Add(time.Hour),
		Trust: map[string]string{"SSL_CERT_DIR": dir + "/d1:" + dir + "/d2"}}
	t.Setenv("SSL_CERT_DIR", dir+"/d1:"+dir+"/d2")
	if tok, _ := access(a); tok == "" {
		t.Error("an accepted list was refused")
	}
	t.Setenv("SSL_CERT_DIR", dir+"/d1:"+dir+"/x/../d2")
	if tok, v := access(a); tok != "" || v != "SSL_CERT_DIR" {
		t.Errorf("a list with a .. entry: %q, %q", tok, v)
	}
}

// brig login does not ask about a value with a .. element. It says so in one
// line, sends nothing, and stores nothing.
func TestLoginRefusesAValueWithAParentElement(t *testing.T) {
	m := loginHost(t)
	s := fakeDeployment(t, fakeas.Options{})
	t.Setenv("SSL_CERT_FILE", "certs/../ca.pem")
	var out bytes.Buffer
	var err error
	stderr := captureStderr(t, func() { err = loginCmd(&out, []string{s.URL, "--device"}) })
	want := "Brig does not trust a certificate path with .. in it, and SSL_CERT_FILE has one. Set an absolute path"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
	if strings.Contains(stderr, "Continue logging in") || len(s.Requests()) != 0 || len(m.items) != 0 {
		t.Errorf("asked %q, requests %v, stored %v", stderr, s.Requests(), m.items)
	}
}

// Go reads SSL_CERT_FILE as one path and splits only SSL_CERT_DIR at ":"
// (crypto/x509 root_unix.go). A colon in a relative SSL_CERT_FILE is part of
// the name, and an empty entry of SSL_CERT_DIR stays empty.
func TestOnlyTheDirectoryVariableIsAList(t *testing.T) {
	wd := t.TempDir()
	t.Chdir(wd)
	got, err := canonicalTrust("SSL_CERT_FILE", "certs:prod/ca.pem")
	if err != nil || got != filepath.Join(wd, "certs:prod", "ca.pem") {
		t.Errorf("SSL_CERT_FILE = %q, %v", got, err)
	}
	got, err = canonicalTrust("SSL_CERT_DIR", "d1::d2/./x")
	if err != nil || got != filepath.Join(wd, "d1")+"::"+filepath.Join(wd, "d2", "x") {
		t.Errorf("SSL_CERT_DIR = %q, %v", got, err)
	}
}

// A relative value is refused when the working directory cannot be read, and
// brig login says so instead of asking.
func TestARelativeValueIsRefusedWhenTheWorkingDirectoryCannotBeRead(t *testing.T) {
	old := getwd
	getwd = func() (string, error) { return "", errors.New("no working directory") }
	t.Cleanup(func() { getwd = old })
	if _, err := canonicalTrust("SSL_CERT_FILE", "ca.pem"); !errors.Is(err, errWorkingDir) {
		t.Errorf("err = %v", err)
	}
	if got, err := canonicalTrust("SSL_CERT_FILE", "/abs/ca.pem"); err != nil || got != "/abs/ca.pem" {
		t.Errorf("an absolute value: %q, %v", got, err)
	}
	loginHost(t)
	s := fakeDeployment(t, fakeas.Options{})
	t.Setenv("SSL_CERT_FILE", "ca.pem")
	err := loginCmd(io.Discard, []string{s.URL, "--device"})
	if err == nil || !strings.Contains(err.Error(), "cannot read the working directory") || len(s.Requests()) != 0 {
		t.Errorf("err = %v, requests %v", err, s.Requests())
	}
}
