package main

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// auditFileConfig is the broker of one sandbox with its audit lines going to
// a file the test opened, as a platform opens one and hands it over with
// --audit-fd, capped at max bytes.
func auditFileConfig(t *testing.T, max int64) (serverConfig, string) {
	t.Helper()
	cfg := sandboxConfig(t)
	path := filepath.Join(shortDir(t), "audit.log")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cfg.auditFile = f
	cfg.auditMax = max
	return cfg, path
}

// The audit lines go to the handed file and nowhere in the broker
// directory, so a platform that owns the file owns where they land.
func TestAHandedAuditFileTakesTheAuditLines(t *testing.T) {
	u := newUpstream(t)
	cfg, path := auditFileConfig(t, 1<<20)
	r := startBrokerWith(t, cfg, u)
	added, err := r.client.Add(nodeSandbox("pod-a", "127.0.0.1", inventoryRoute()))
	if err != nil {
		t.Fatal(err)
	}
	pod := nodeClient(listenAddr(r), caPool(t, added), nil)
	if code, _ := sendTLS(t, pod, http.MethodGet, "pod-a", "inventory", "not-a-token", "/v2/items/7"); code != http.StatusUnauthorized {
		t.Fatalf("a wrong token got %d, want 401", code)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("arrived=pod-a")) {
		t.Errorf("the handed audit file has no line for the request: %q", data)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "audit.log")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the broker wrote audit.log in its directory too: %v", err)
	}
}

// Once the handed file is full the broker serves no route: a request it
// cannot audit does not reach the upstream.
func TestAFullAuditFileRefusesEveryRoute(t *testing.T) {
	u := newUpstream(t)
	cfg, _ := auditFileConfig(t, 1)
	r := startBrokerWith(t, cfg, u)
	added, err := r.client.Add(nodeSandbox("pod-a", "127.0.0.1", inventoryRoute()))
	if err != nil {
		t.Fatal(err)
	}
	token := r.mint(t, "pod-a", "inventory")
	pod := nodeClient(listenAddr(r), caPool(t, added), nil)
	// The first request is audited, and its line fills the file.
	if code, _ := sendTLS(t, pod, http.MethodGet, "pod-a", "inventory", "not-a-token", "/v2/items/7"); code != http.StatusUnauthorized {
		t.Fatalf("the first request got %d, want 401", code)
	}
	code, body := sendTLS(t, pod, http.MethodGet, "pod-a", "inventory", token, "/v2/items/7")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "audit log is full") {
		t.Fatalf("a request after the file filled got %d %q, want 503", code, body)
	}
	if n := len(u.requests()); n != 0 {
		t.Errorf("the upstream saw %d requests with the audit file full", n)
	}
}

// A handed descriptor that is not a regular file is refused at start.
func TestAHandedAuditFileMustBeARegularFile(t *testing.T) {
	cfg := sandboxConfig(t)
	f, err := os.Open(shortDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg.auditFile = f
	if _, err := newServer(cfg); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a directory as the audit file = %v, want refused", err)
	}
}

func TestRunRefusesAuditMaxWithoutAuditFD(t *testing.T) {
	var stderr bytes.Buffer
	dir := filepath.Join(shortDir(t), "broker")
	if got := run([]string{"--dir", dir, "--audit-max", "10"}, &stderr, serverConfig{}); got != 2 {
		t.Errorf("exit %d, want 2 (%s)", got, stderr.String())
	}
}
