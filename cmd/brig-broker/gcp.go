package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/brig-sh/brig/internal/broker"
	"github.com/brig-sh/brig/internal/gcpcred"
	"github.com/brig-sh/brig/internal/secret"
)

// gcpMinter is the broker.Minter of the gcp routes. It mints through the
// host's gcloud and holds the tokens in memory, per sandbox and route.
type gcpMinter struct {
	bin   string
	cache *gcpcred.Cache
	// log is the broker log. A failure the user cannot act on is written
	// there, and the guest is told to look.
	log *log.Logger

	mu sync.Mutex
	// failing holds "<sandbox>/<route>" whose last mint failed, so status
	// names a route whose guest gets 502s, as it does for a route that
	// vends.
	failing map[string]bool
}

var _ broker.Minter = (*gcpMinter)(nil)

// newGCPMinter returns the minter. The gcloud environment is built once, from
// the broker's own, with the variables that would choose an identity over
// the pointer taken out. cli.Mint goes to the cache as it is, so its own
// timeout bounds a gcloud that hangs.
func newGCPMinter(bin, tokenInfoURL string, now func() time.Time, logger *log.Logger) *gcpMinter {
	if now == nil {
		now = time.Now
	}
	cli := gcpcred.CLI{Bin: bin, Env: gcpcred.Environ(os.Environ()), TokenInfoURL: tokenInfoURL}
	return &gcpMinter{bin: bin, cache: gcpcred.NewCache(cli.Mint, now), log: logger, failing: map[string]bool{}}
}

// Mint answers a request on route with a token that has at least 2 minutes
// left. A pointer that does not parse is not one the user fixes by logging
// in, so it goes to the broker log and the guest is told to look there.
func (m *gcpMinter) Mint(ctx context.Context, sandbox, route string, doc []byte) (string, error) {
	p, err := gcpcred.ParsePointer(doc)
	if err != nil {
		return "", m.failed(sandbox, route, err, false)
	}
	tok, err := m.cache.Get(ctx, sandbox, route, p)
	if err != nil {
		return "", m.failed(sandbox, route, err, true)
	}
	m.succeeded(sandbox, route)
	return tok.Value, nil
}

// check is the registration's look at a gcp route whose secret is a
// pointer: it parses, and the cache holds a token. A lapsed login refuses
// the run that would otherwise start and fail at the guest's first Google
// call, and the request after registration is served from the cache.
func (m *gcpMinter) check(ctx context.Context, sandbox, route string, doc []byte) error {
	p, err := gcpcred.ParsePointer(doc)
	if err != nil {
		return m.failed(sandbox, route, err, false)
	}
	if err := m.cache.Ensure(ctx, sandbox, route, p); err != nil {
		return m.failed(sandbox, route, err, true)
	}
	m.succeeded(sandbox, route)
	return nil
}

// failed records the failure and returns the error to answer with. ranGCloud is whether err came
// from minting. One the user fixes on the host is a *broker.VendError, shown
// to the guest. Any other goes to the broker log and returns as it is, and
// the guest is told only to look there: its text can name paths of the host
// or quote gcloud's stderr. gcpcred keeps stdout, where the token is, out of
// every error.
func (m *gcpMinter) failed(sandbox, route string, err error, ranGCloud bool) error {
	m.mu.Lock()
	m.failing[sandbox+"/"+route] = true
	m.mu.Unlock()
	var lapsed *gcpcred.ErrLoginExpired
	var denied *gcpcred.ErrCannotImpersonate
	if errors.As(err, &lapsed) || errors.As(err, &denied) {
		return &broker.VendError{Message: err.Error()}
	}
	// gcpcred reports a failed start in words, not as exec.ErrNotFound, so
	// the binary is looked up again here, and only after a mint failed: a
	// malformed pointer is not fixed by installing gcloud.
	if ranGCloud {
		if _, lerr := exec.LookPath(m.gcloud()); lerr != nil {
			return &broker.VendError{Message: gcpcred.InstallHint}
		}
	}
	m.log.Printf("sandbox %s: route %s: minting the Google token: %v", sandbox, route, err)
	return err
}

func (m *gcpMinter) gcloud() string {
	if m.bin == "" {
		return "gcloud"
	}
	return m.bin
}

func (m *gcpMinter) succeeded(sandbox, route string) {
	m.mu.Lock()
	delete(m.failing, sandbox+"/"+route)
	m.mu.Unlock()
}

// forget drops the tokens and the failures of a sandbox that is gone, or
// whose registration was refused.
func (m *gcpMinter) forget(sandbox string) {
	m.cache.Forget(sandbox)
	m.mu.Lock()
	for k := range m.failing {
		if strings.HasPrefix(k, sandbox+"/") {
			delete(m.failing, k)
		}
	}
	m.mu.Unlock()
}

// report returns the routes whose last mint failed, sorted.
func (m *gcpMinter) report() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Sorted(maps.Keys(m.failing))
}

// checkMints mints the token of each gcp route of sb whose secret is a
// pointer, so a lapsed login refuses the registration as checkVends does for
// an aws route. Its error is the refusal's text and names the route. Nothing
// is registered here, and s.mu is not held.
func (s *server) checkMints(ctx context.Context, sb broker.Sandbox) error {
	wl := secret.Workload{Sandbox: sb.ID, Profile: sb.Profile}
	for _, rt := range sb.Routes {
		if len(rt.Route.Services) == 0 || rt.Route.Credential == nil {
			continue
		}
		name := rt.Route.Name
		doc, err := s.creds.Resolve(ctx, rt.Route.Credential.SecretRef.Name, wl)
		if err != nil {
			// A gcp route may inject a raw token, and a secret that does not
			// resolve is reported on the request for one of those. A
			// pointer is not treated otherwise: the store is the same.
			continue
		}
		if !gcpcred.IsPointer(doc) {
			continue
		}
		if err := s.gcp.check(ctx, sb.ID, name, doc); err != nil {
			return fmt.Errorf("route %s: %w", name, err)
		}
	}
	return nil
}
