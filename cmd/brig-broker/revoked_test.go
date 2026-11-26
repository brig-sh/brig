package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/secret"
)

// restart stops r and serves a new broker on the same directory.
func (r *running) restart(t *testing.T, u *upstream) *running {
	t.Helper()
	r.cancel()
	<-r.done
	r.done <- nil
	return startBroker(t, r.dir, u, nil)
}

// A revoked sandbox loses its tokens at once, and the registration that the
// next exec of it sends is refused, so the agent gets no new token and the
// upstream never sees another request for it.
func TestARevokedSandboxCannotRegisterAgain(t *testing.T) {
	u := newUpstream(t)
	r := startBroker(t, shortDir(t), u, nil)
	sb := injector.Sandbox{ID: "brig-a", Profile: "claude-code", Routes: []injector.Route{claudeRoute()}}
	if err := r.client.AddSandbox(sb); err != nil {
		t.Fatal(err)
	}
	token := r.mint(t, "brig-a", "anthropic")
	got, err := r.client.Revoke("brig-a", injector.RevokeRequest{By: "ops@example.com", Reason: "unexpected egress"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Removed || got.Remaining != 0 || got.Revocation.ID != "brig-a" || got.Revocation.By != "ops@example.com" ||
		got.Revocation.At.IsZero() {
		t.Fatalf("revoke = %+v", got)
	}
	if code := r.send(t, "brig-a", "anthropic", token, "/v1/messages"); code != http.StatusNotFound {
		t.Errorf("the token of a revoked sandbox got %d, want 404", code)
	}
	err = r.client.AddSandbox(sb)
	var refused *injector.RefusedError
	if !errors.As(err, &refused) || refused.Status != http.StatusForbidden || refused.Kind != injector.RefusalRevoked {
		t.Fatalf("registering a revoked sandbox = %v, want a 403 revoked refusal", err)
	}
	for _, want := range []string{"brig-a", "ops@example.com", "unexpected egress", "brig broker reinstate brig-a"} {
		if !strings.Contains(refused.Message, want) {
			t.Errorf("the refusal %q does not name %q", refused.Message, want)
		}
	}
	if _, err := r.client.Mint(injector.MintRequest{Sandbox: "brig-a", Route: "anthropic"}); err == nil {
		t.Error("a revoked sandbox was minted a token")
	}
	if n := len(u.requests()); n != 0 {
		t.Errorf("upstream saw %d requests, want none", n)
	}
	st, err := r.client.Status()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Sandboxes) != 0 || len(st.Revoked) != 1 || st.Revoked[0].ID != "brig-a" {
		t.Errorf("status = %+v, want brig-a revoked and nothing registered", st)
	}
	audit, err := os.ReadFile(filepath.Join(r.dir, injector.AuditName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"msg=revoke sandbox=brig-a", "msg=register sandbox=brig-a status=403 check=revoked"} {
		if !strings.Contains(string(audit), want) {
			t.Errorf("the audit log has no %q:\n%s", want, audit)
		}
	}
}

// The revocation is kept on disk, so a broker that stops when it is idle,
// and the one the next run starts, still refuse the sandbox. Only a
// reinstatement lets it register again.
func TestARevocationOutlivesTheBrokerUntilReinstated(t *testing.T) {
	u := newUpstream(t)
	r := startBroker(t, shortDir(t), u, nil)
	sb := injector.Sandbox{ID: "brig-a", Profile: "claude-code", Routes: []injector.Route{claudeRoute()}}
	if _, err := r.client.Revoke("brig-a", injector.RevokeRequest{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(r.dir, injector.RevokedName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("%s has mode %v, want 0600", injector.RevokedName, info.Mode().Perm())
	}
	r = r.restart(t, u)
	var refused *injector.RefusedError
	if err := r.client.AddSandbox(sb); !errors.As(err, &refused) || refused.Kind != injector.RefusalRevoked {
		t.Fatalf("registering after a restart = %v, want a revoked refusal", err)
	}
	// A second revoke keeps the first record.
	first, err := r.client.Status()
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.client.Revoke("brig-a", injector.RevokeRequest{By: "someone else"})
	if err != nil || again.Revocation.By != "" || !again.Revocation.At.Equal(first.Revoked[0].At) {
		t.Errorf("a second revoke = %+v, %v, want the first record kept", again, err)
	}
	re, err := r.client.Reinstate("brig-a")
	if err != nil || !re.Reinstated {
		t.Fatalf("reinstate = %+v, %v", re, err)
	}
	if err := r.client.AddSandbox(sb); err != nil {
		t.Fatalf("registering after the reinstatement: %v", err)
	}
	if code := r.send(t, "brig-a", "anthropic", r.mint(t, "brig-a", "anthropic"), "/v1/messages"); code != http.StatusOK {
		t.Errorf("a token after the reinstatement got %d", code)
	}
	r = r.restart(t, u)
	if err := r.client.AddSandbox(sb); err != nil {
		t.Errorf("the reinstatement did not outlive a restart: %v", err)
	}
	if re, err := r.client.Reinstate("brig-b"); err != nil || re.Reinstated {
		t.Errorf("reinstating a sandbox never revoked = %+v, %v", re, err)
	}
}

// A revocation file the broker cannot read is not read as none: the broker
// refuses to start rather than serve the sandboxes it names.
func TestABrokerWithAnUnreadableRevocationFileDoesNotStart(t *testing.T) {
	for name, body := range map[string]string{
		"not json":   "not json",
		"invalid id": `[{"id":"a b","at":"2026-01-02T03:04:05Z"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := shortDir(t)
			if err := os.WriteFile(filepath.Join(dir, injector.RevokedName), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := serverConfig{dir: dir, renewerFor: noRenewer(t), logTo: io.Discard,
				openStore: func() (secret.Store, error) { return newMemStore(), nil }}
			if srv, err := newServer(cfg); err == nil {
				srv.close()
				t.Fatal("a broker started on a revocation file it could not read")
			}
		})
	}
	dir := shortDir(t)
	if err := os.WriteFile(filepath.Join(dir, injector.RevokedName), []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := serverConfig{dir: dir, renewerFor: noRenewer(t), logTo: io.Discard,
		openStore: func() (secret.Store, error) { return newMemStore(), nil }}
	if srv, err := newServer(cfg); err == nil {
		srv.close()
		t.Fatal("a broker started on a revocation file others can read")
	}
}

// A revoke names a valid sandbox and carries one bounded line of text.
func TestARevokeIsStrict(t *testing.T) {
	r := startBroker(t, shortDir(t), nil, nil)
	for _, c := range []struct {
		id  string
		req injector.RevokeRequest
	}{
		{"a b", injector.RevokeRequest{}},
		{"brig-a", injector.RevokeRequest{By: "a\nb"}},
		{"brig-a", injector.RevokeRequest{Reason: strings.Repeat("x", maxRevokeText+1)}},
	} {
		var refused *injector.RefusedError
		if _, err := r.client.Revoke(c.id, c.req); !errors.As(err, &refused) || refused.Status != http.StatusBadRequest {
			t.Errorf("revoke %q %+v = %v, want 400", c.id, c.req, err)
		}
	}
	st, err := r.client.Status()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Revoked) != 0 {
		t.Errorf("refused revokes recorded %+v", st.Revoked)
	}
}
