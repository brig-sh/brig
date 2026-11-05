package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/secret"
)

// filesDir is a directory laid out like a mounted Secret, with each of files
// written as a 0400 regular file. A caller chmods a file to make it
// world-readable, or adds symlinks for the kubelet layout.
func filesDir(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for name, value := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, value, 0o400); err != nil {
			t.Fatal(err)
		}
		// WriteFile leaves the mode to the umask; set it, so a test sees the
		// mode it asked for.
		if err := os.Chmod(path, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// failingStore stands for brig's own store on a broker that was given
// --secret-dir: nothing may open it.
func failingStore(t *testing.T) func() (secret.Store, error) {
	return func() (secret.Store, error) {
		t.Error("the platform store was opened although --secret-dir names the store")
		return nil, errors.New("the platform store must not open")
	}
}

// With --secret-dir the credentials come from the files, and the platform
// store is never opened.
func TestSecretDirServesRoutesFromFiles(t *testing.T) {
	u := newUpstream(t)
	dir := filesDir(t, map[string][]byte{
		"claude-credentials": fakeDoc(fakeAccess, time.Now().Add(time.Hour)),
		"gh-token":           []byte(fakeGitHub + "\n"),
	})
	r := startBrokerWith(t, serverConfig{dir: shortDir(t), logTo: io.Discard, secretDir: dir,
		openStore: failingStore(t)}, u)
	if err := r.client.AddSandbox(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute(), githubRoute()}}); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"anthropic", "github"} {
		token := r.mint(t, "brig-a", route)
		if got := r.send(t, "brig-a", route, token, "/v1/messages"); got != http.StatusOK {
			t.Fatalf("route %s answered %d", route, got)
		}
	}
	seen := u.requests()
	if len(seen) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(seen))
	}
	if got := seen[0].Get("Authorization"); got != "Bearer "+fakeAccess {
		t.Errorf("anthropic: upstream saw %q, want the access token from the file", got)
	}
	// The GitHub route sends the token as Basic auth. The decoded pair
	// ends with the token and no newline: the route trims what it injects.
	basic, ok := strings.CutPrefix(seen[1].Get("Authorization"), "Basic ")
	pair, err := base64.StdEncoding.DecodeString(basic)
	if !ok || err != nil || !strings.HasSuffix(string(pair), ":"+fakeGitHub) {
		t.Errorf("github: upstream saw %q, want the token from the file", seen[1].Get("Authorization"))
	}
	if r.store.reads != 0 {
		t.Errorf("the memory store was read %d times", r.store.reads)
	}
}

// A world-readable secret file stops the broker at start and the error names
// the file. The mode is set with chmod, so the umask cannot decide it.
func TestSecretDirRefusesAWorldReadableFile(t *testing.T) {
	dir := filesDir(t, map[string][]byte{"gh-token": []byte(fakeGitHub)})
	path := filepath.Join(dir, "gh-token")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := newServer(serverConfig{dir: shortDir(t), logTo: io.Discard, secretDir: dir,
		openStore: failingStore(t)})
	if err == nil {
		t.Fatal("a broker started on a world-readable secret")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("the error does not name the file: %v", err)
	}
}

// A secret directory that is not there stops the broker too, instead of
// leaving every route to answer 502.
func TestSecretDirThatDoesNotOpenStopsTheBroker(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	if _, err := newServer(serverConfig{dir: shortDir(t), logTo: io.Discard, secretDir: missing,
		openStore: failingStore(t)}); err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("newServer = %v, want an error naming %s", err, missing)
	}
}

// Keys that do not fit the secret-name grammar, such as ca.crt, are named
// once in the log and not served.
func TestSecretDirLogsSkippedNamesOnce(t *testing.T) {
	logs := &lockedBuffer{}
	dir := filesDir(t, map[string][]byte{"gh-token": []byte(fakeGitHub), "ca.crt": []byte("pem")})
	startBrokerWith(t, serverConfig{dir: shortDir(t), logTo: logs, secretDir: dir,
		openStore: failingStore(t)}, nil)
	if n := strings.Count(logs.String(), "ca.crt"); n != 1 {
		t.Fatalf("the log names ca.crt %d times, want once:\n%s", n, logs)
	}
	if strings.Contains(logs.String(), "gh-token") {
		t.Errorf("the log names a secret that is served:\n%s", logs)
	}
}

// A credential with a lifecycle is renewed in memory on a read-only
// directory. The renewed value is served, the file is not touched, the broker
// logs once that it cannot write back, and nothing is pending or failing.
func TestSecretDirRenewsInMemory(t *testing.T) {
	u := newUpstream(t)
	logs := &lockedBuffer{}
	due := fakeDoc(fakeAccess, time.Now().Add(2*time.Minute))
	dir := filesDir(t, map[string][]byte{"claude-credentials": due})
	var renewals atomic.Int32
	renewers := func(v1alpha1.Lifecycle) (secret.Renewer, error) {
		return renewerFunc(func(context.Context, []byte) ([]byte, error) {
			renewals.Add(1)
			return fakeDoc("renewed-access-token", time.Now().Add(8*time.Hour)), nil
		}), nil
	}
	r := startBrokerWith(t, serverConfig{dir: shortDir(t), logTo: logs, secretDir: dir,
		openStore: failingStore(t), renewerFor: renewers}, u)
	if err := r.client.AddSandbox(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute()}}); err != nil {
		t.Fatal(err)
	}
	token := r.mint(t, "brig-a", "anthropic")
	for range 2 {
		if got := r.send(t, "brig-a", "anthropic", token, "/v1/messages"); got != http.StatusOK {
			t.Fatalf("a request got %d", got)
		}
	}
	for i, h := range u.requests() {
		if got := h.Get("Authorization"); got != "Bearer renewed-access-token" {
			t.Errorf("request %d: upstream saw %q, want the renewed token", i, got)
		}
	}
	if n := renewals.Load(); n != 1 {
		t.Errorf("%d renewals, want 1", n)
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "claude-credentials"))
	if err != nil || !bytes.Equal(onDisk, due) {
		t.Errorf("the file was changed: %q, %v", onDisk, err)
	}
	if n := strings.Count(logs.String(), "read-only"); n != 1 {
		t.Errorf("the log says read-only %d times, want once:\n%s", n, logs)
	}
	st, err := r.client.Status()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Pending) != 0 || len(st.Failing) != 0 {
		t.Errorf("status = pending %v, failing %v, want none", st.Pending, st.Failing)
	}
}
