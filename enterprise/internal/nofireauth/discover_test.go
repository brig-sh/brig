// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package nofireauth_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/enterprise/internal/nofireauth"
	"github.com/brig-sh/brig/enterprise/internal/nofireauth/fakeas"
)

func start(t *testing.T, o fakeas.Options) *fakeas.Server {
	t.Helper()
	s, err := fakeas.Start("127.0.0.1:0", o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func client(t *testing.T, raw string) *nofireauth.Client {
	t.Helper()
	d, err := nofireauth.ParseDeployment(raw)
	if err != nil {
		t.Fatal(err)
	}
	return nofireauth.NewClient(d, "test")
}

func TestDiscover(t *testing.T) {
	s := start(t, fakeas.Options{})
	m, err := client(t, s.URL).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Issuer != s.URL || m.TokenEndpoint != s.URL+"/oauth/token" || m.DeviceAuthorizationEndpoint == "" {
		t.Errorf("metadata = %+v", m)
	}
}

func TestDiscoverUnderAPathPrefix(t *testing.T) {
	s := start(t, fakeas.Options{})
	if _, err := client(t, s.URL+"/tenant").Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /.well-known/oauth-authorization-server/tenant"}
	if got := s.Requests(); !slices.Equal(got, want) {
		t.Errorf("requests = %q, want %q", got, want)
	}
}

func TestDiscoverRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    fakeas.Options
		want string
	}{
		{"issuer mismatch", fakeas.Options{Issuer: "https://evil.example"}, "identifies itself as"},
		{"endpoint on another port", fakeas.Options{EndpointBase: "http://127.0.0.1:1"}, "which is not on the deployment"},
		{"no S256", fakeas.Options{NoS256: true}, "S256"},
		{"no iss support", fakeas.Options{NoIssSupport: true}, "RFC 9207"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := start(t, tc.o)
			_, err := client(t, s.URL).Discover(context.Background())
			if err == nil || errors.Is(err, nofireauth.ErrNotSupported) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.want)
			}
			if n := len(s.Requests()); n != 1 {
				t.Errorf("%d requests, want only the metadata fetch", n)
			}
		})
	}
}

func TestDiscoverDoesNotFollowRedirects(t *testing.T) {
	s := start(t, fakeas.Options{RedirectAll: true})
	_, err := client(t, s.URL).Discover(context.Background())
	if err == nil || !strings.Contains(err.Error(), "redirects its sign-in settings elsewhere") {
		t.Fatalf("err = %v, want the redirect refusal", err)
	}
	if n := len(s.Requests()); n != 1 {
		t.Errorf("%d requests, want exactly 1", n)
	}
}

func TestDiscoverOnOldDeployment(t *testing.T) {
	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	_, err := client(t, old.URL).Discover(context.Background())
	if !errors.Is(err, nofireauth.ErrNotSupported) {
		t.Errorf("err = %v, want ErrNotSupported", err)
	}
}

func TestUserAgent(t *testing.T) {
	s := start(t, fakeas.Options{})
	if _, err := client(t, s.URL).Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.UserAgents(); !slices.Equal(got, []string{"brig/test"}) {
		t.Errorf("user agents = %q", got)
	}
}

func TestDiscoverRefusesAnOversizeBody(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"issuer":"` + strings.Repeat("a", 70<<10) + `"}`))
	}))
	defer big.Close()
	_, err := client(t, big.URL).Discover(context.Background())
	if err == nil || errors.Is(err, nofireauth.ErrNotSupported) {
		t.Errorf("err = %v, want a size error", err)
	}
}

// A value the metadata got wrong is quoted in the error, cleaned and cut to
// 200 characters, so a hostile document cannot flood or forge the terminal.
func TestDiscoverBoundsServerTextInErrors(t *testing.T) {
	junk := "\x1b[2J" + strings.Repeat("a", 1000)
	for _, field := range []string{"issuer", "authorization_endpoint"} {
		t.Run(field, func(t *testing.T) {
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				doc := map[string]any{
					"issuer":                           srv.URL,
					"authorization_endpoint":           srv.URL + "/oauth/authorize",
					"token_endpoint":                   srv.URL + "/oauth/token",
					"code_challenge_methods_supported": []string{"S256"},
					"authorization_response_iss_parameter_supported": true,
				}
				doc[field] = "https://evil.example/" + junk
				_ = json.NewEncoder(w).Encode(doc)
			}))
			defer srv.Close()
			_, err := client(t, srv.URL).Discover(context.Background())
			if err == nil {
				t.Fatal("Discover succeeded")
			}
			if msg := err.Error(); strings.Contains(msg, "\x1b") || strings.Count(msg, "a") > 250 {
				t.Errorf("error is not bounded: %d bytes, escape %v", len(msg), strings.Contains(msg, "\x1b"))
			}
		})
	}
}

// forged is what a hostile server or network would put on the terminal: erase
// Brig's line, print a fake one, and hide what follows.
const forged = "\x1b[2K\x1b[1A\x1b[2K\rOpen https://my.nofire.ai/device and enter the code WXYZQRST\x1b[8m"

// noControls fails t if msg holds a byte that moves the cursor or starts an
// escape sequence.
func noControls(t *testing.T, msg string) {
	t.Helper()
	if strings.ContainsAny(msg, "\x1b\r\n") {
		t.Errorf("error holds terminal controls: %q", msg)
	}
}

// A malformed header line reaches the error text of net/http verbatim.
func TestTransportErrorTextIsCleaned(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Read(make([]byte, 4096))
			_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\n" + forged + "\r\n\r\n"))
			conn.Close()
		}
	}()
	_, err = client(t, "http://"+ln.Addr().String()).Discover(context.Background())
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("err = %v, want the malformed header error", err)
	}
	noControls(t, err.Error())
}

// On Linux Go checks a certificate's DNS names before its chain, and quotes
// them in the error. A DNS name only has to be IA5, so a network attacker can
// put escapes in it.
func TestCertificateErrorTextIsCleaned(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "x"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"x" + forged},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	c := client(t, "https://localhost:"+port)
	nofireauth.UseRoots(c, x509.NewCertPool())
	_, err = c.Discover(context.Background())
	if err == nil || !strings.Contains(err.Error(), "certificate is valid for") {
		t.Fatalf("err = %v, want the host name error that quotes the certificate", err)
	}
	noControls(t, err.Error())
}

// The cleaned error still says that the request was cancelled.
func TestTransportErrorKeepsCancellation(t *testing.T) {
	s := start(t, fakeas.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client(t, s.URL).Discover(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
