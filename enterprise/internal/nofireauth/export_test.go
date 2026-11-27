// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package nofireauth

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
)

// UseRoots makes c trust only roots. With roots set, crypto/tls verifies with
// Go's own verifier on every OS, which is the verifier Linux always uses, so
// a test on macOS reaches the same certificate errors.
func UseRoots(c *Client, roots *x509.CertPool) {
	c.http.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
}

// RedirectHTML is redirectHTML, for the test of its escaping.
var RedirectHTML = redirectHTML

// WatchListener makes Run listen through wrap, until the returned function runs.
func WatchListener(wrap func(net.Listener) net.Listener) (restore func()) {
	old := listenLoopback
	listenLoopback = func() (net.Listener, error) {
		ln, err := old()
		if err != nil {
			return nil, err
		}
		return wrap(ln), nil
	}
	return func() { listenLoopback = old }
}
