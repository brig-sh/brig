// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package nofireauth

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
)

// UseRoots makes c trust only roots. With roots set, crypto/tls verifies with
// Go's own verifier on every OS, which is the verifier Linux always uses, so
// a test on macOS reaches the same certificate errors.
func UseRoots(c *Client, roots *x509.CertPool) {
	c.http.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
}
