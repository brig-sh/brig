package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
)

// The variables that turn the test binary into a broker. See
// TestBrokerHelperProcess.
const (
	brokerHelperEnv = "BRIG_TEST_BROKER_HELPER"
	upstreamLogEnv  = "BRIG_TEST_UPSTREAM_LOG"
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
	os.Exit(run(args, os.Stderr, serverConfig{
		openStore: func() (secret.Store, error) { return store, nil },
		transport: transportTo(up),
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
