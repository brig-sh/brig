package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/secret"
	"github.com/brig-sh/brig/internal/testpki"
)

// The variables that turn the test binary into a broker. See
// TestBrokerHelperProcess.
const (
	brokerHelperEnv = "BRIG_TEST_BROKER_HELPER"
	// mtlsHostEnv names a host that the helper serves with an upstream that
	// demands a client certificate. Unset, the helper has no such host.
	mtlsHostEnv    = "BRIG_TEST_MTLS_HOST"
	upstreamLogEnv = "BRIG_TEST_UPSTREAM_LOG"
	// brokerParentEnv names the test process. The helper exits when it is
	// gone, so a test that is killed leaves no broker behind. smoke.sh sets
	// none: there the broker outlives the brig that started it, as the real
	// one does.
	brokerParentEnv = "BRIG_TEST_BROKER_PARENT"
)

// TestBrokerHelperProcess is not a test. Run with BRIG_TEST_BROKER_HELPER=1,
// the test binary is brig-broker: it reads the broker's command line and
// serves through run, as the real binary does, with two parts swapped. A
// memory store holding the fake credentials of these tests stands in for the
// keychain, and a TLS upstream in this process stands in for every route's
// upstream. The upstream appends what each request carried to the file
// BRIG_TEST_UPSTREAM_LOG names, one JSON line each, and answers with the
// Authorization it saw. A guest that got that answer unredacted would hold
// the credential.
//
// brig starts the broker with an environment of its own (brokerEnv), so the
// switch reaches it through a wrapper script that BRIG_BROKER_BIN names.
// script/smoke.sh writes that wrapper around a test binary of this package,
// so its routed run reads no keychain and reaches no real upstream.
func TestBrokerHelperProcess(t *testing.T) {
	if os.Getenv(brokerHelperEnv) != "1" {
		return
	}
	args := os.Args[1:]
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if parent, err := strconv.Atoi(os.Getenv(brokerParentEnv)); err == nil {
		go func() {
			for syscall.Kill(parent, 0) == nil {
				time.Sleep(200 * time.Millisecond)
			}
			os.Exit(1)
		}()
	}
	up, err := loggingUpstream(os.Getenv(upstreamLogEnv))
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "brig-broker helper:", err)
		os.Exit(1)
	}
	store := newMemStore()
	var tr http.RoundTripper = transportTo(up)
	if host := os.Getenv(mtlsHostEnv); host != "" {
		var err error
		if tr, err = mtlsHelperTransport(store, up, host); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "brig-broker helper:", err)
			os.Exit(1)
		}
	}
	os.Exit(run(args, os.Stderr, serverConfig{
		openStore: func() (secret.Store, error) { return store, nil },
		transport: tr,
		renewerFor: func(v1alpha1.Lifecycle) (secret.Renewer, error) {
			return renewerFunc(func(context.Context, []byte) ([]byte, error) {
				return nil, errors.New("the helper broker never refreshes: its credential is not due")
			}), nil
		},
	}))
}

// upstreamRequest is one line of the upstream log: what one request carried
// when it left the broker.
type upstreamRequest struct {
	Method string      `json:"method"`
	Path   string      `json:"path"`
	Header http.Header `json:"header"`
	Body   string      `json:"body"`
	// Peer is the common name of the client certificate the request
	// presented, when the upstream asked for one.
	Peer string `json:"peer,omitempty"`
}

// mtlsHelperTransport starts an upstream for host that requires a client
// certificate, seeds store with a client certificate and the CA that signed
// the upstream, and returns a transport that dials host:443 to that upstream
// and every other address to the plain one.
func mtlsHelperTransport(store *memStore, plain *httptest.Server, host string) (http.RoundTripper, error) {
	ca, err := testpki.NewCA("helper mTLS CA")
	if err != nil {
		return nil, err
	}
	srvCert, err := ca.Server(host, "example.com")
	if err != nil {
		return nil, err
	}
	now := time.Now()
	certPEM, keyPEM, err := ca.Client("inventory-client", now.Add(-time.Hour), now.Add(365*24*time.Hour))
	if err != nil {
		return nil, err
	}
	store.mu.Lock()
	store.values["inventory-client"] = append(append([]byte{}, certPEM...), keyPEM...)
	store.values["example-root-ca"] = append([]byte{}, ca.PEM...)
	store.mu.Unlock()
	mtls, err := mtlsUpstream(os.Getenv(upstreamLogEnv), ca, srvCert)
	if err != nil {
		return nil, err
	}
	plainAddr := plain.Listener.Addr().String()
	mtlsAddr := mtls.Listener.Addr().String()
	pool := x509.NewCertPool()
	pool.AddCert(plain.Certificate())
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			to := plainAddr
			if h, _, err := net.SplitHostPort(addr); err == nil && h == host {
				to = mtlsAddr
			}
			return (&net.Dialer{}).DialContext(ctx, network, to)
		},
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "example.com"},
	}, nil
}

// mtlsUpstream is loggingUpstream behind RequireAndVerifyClientCert: a
// request that gets through is logged with the subject of its certificate.
func mtlsUpstream(path string, ca *testpki.CA, serverCert tls.Certificate) (*httptest.Server, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		peer := ""
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			peer = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		line, _ := json.Marshal(upstreamRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header, Body: string(body), Peer: peer})
		mu.Lock()
		_, _ = f.Write(append(line, '\n'))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	srv.TLS = &tls.Config{
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.Pool(),
		Certificates: []tls.Certificate{serverCert},
	}
	srv.StartTLS()
	return srv, nil
}

// loggingUpstream is a TLS server that appends each request to the file at
// path and answers with the Authorization it saw.
func loggingUpstream(path string) (*httptest.Server, error) {
	if path == "" {
		return nil, errors.New(upstreamLogEnv + " names no file")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		line, _ := json.Marshal(upstreamRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header, Body: string(body)})
		mu.Lock()
		_, _ = f.Write(append(line, '\n'))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		saw, _ := json.Marshal(map[string]string{"saw": r.Header.Get("Authorization")})
		_, _ = w.Write(saw)
	})), nil
}
