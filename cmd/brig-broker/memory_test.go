package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/memory"
	"github.com/brig-sh/brig/internal/sessiontoken"
)

// startMemoryBroker serves a broker whose memory root is a fresh directory,
// and returns that root.
func startMemoryBroker(t *testing.T) (*running, string) {
	t.Helper()
	root := filepath.Join(shortDir(t), "memory")
	r := startBrokerWith(t, serverConfig{dir: shortDir(t), logTo: io.Discard, memoryDir: root}, nil)
	return r, root
}

// memoryReg is the memory registration of sandbox id, as the broker holds
// it.
func (r *running) memoryReg(t *testing.T, id string) memoryReg {
	t.Helper()
	r.srv.mu.Lock()
	defer r.srv.mu.Unlock()
	reg, ok := r.srv.memories[id]
	if !ok {
		t.Fatalf("sandbox %s has no memory registration", id)
	}
	return reg
}

func refusedStatus(err error) int {
	var ref *injector.RefusedError
	if errors.As(err, &ref) {
		return ref.Status
	}
	return 0
}

// A registered sandbox is minted a memory token that its own key verifies
// as a memory token, and the store it names is created.
func TestAddMemoryThenMint(t *testing.T) {
	r, root := startMemoryBroker(t)
	resp, err := r.client.AddMemory(injector.MemorySandbox{ID: "brig-a", Key: "notes"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.DivertSocket != "" || resp.CA != "" {
		t.Errorf("add memory = %+v, want no divert socket or CA yet", resp)
	}
	if info, err := os.Lstat(filepath.Join(root, "notes")); err != nil || !info.IsDir() {
		t.Errorf("the store notes was not created: %v", err)
	}
	tok, err := r.client.MintMemory(injector.MemoryMintRequest{Sandbox: "brig-a", Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	reg := r.memoryReg(t, "brig-a")
	c, err := sessiontoken.Verify(reg.key, tok, "brig-a", "", memoryTokenType, time.Now())
	if err != nil {
		t.Fatalf("the memory token does not verify: %v", err)
	}
	if c.Session != "s1" || c.Typ != "brig-memory" || c.Route != "" {
		t.Errorf("claims = %+v", c)
	}
	if c.Exp > time.Now().Add(injector.TokenTTL+time.Minute).Unix() {
		t.Errorf("the token outlives TokenTTL: exp %d", c.Exp)
	}
	if _, err := r.client.MintMemory(injector.MemoryMintRequest{Sandbox: "brig-b"}); refusedStatus(err) != http.StatusNotFound {
		t.Errorf("a memory token for an unregistered sandbox = %v, want 404", err)
	}
}

// A memory token is not a route token: a route refuses it, and a route
// token fails the memory check.
func TestMemoryTokenNotARouteToken(t *testing.T) {
	r, _ := startMemoryBroker(t)
	if err := r.client.AddSandbox(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute()}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.client.AddMemory(injector.MemorySandbox{ID: "brig-a", Key: "notes"}); err != nil {
		t.Fatal(err)
	}
	memTok, err := r.client.MintMemory(injector.MemoryMintRequest{Sandbox: "brig-a"})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.send(t, "brig-a", "anthropic", memTok, "/v1/messages"); got != http.StatusUnauthorized {
		t.Errorf("a memory token on a route got %d, want 401", got)
	}
	routeTok := r.mint(t, "brig-a", "anthropic")
	reg := r.memoryReg(t, "brig-a")
	if _, err := sessiontoken.Verify(reg.key, routeTok, "brig-a", "", memoryTokenType, time.Now()); err == nil {
		t.Error("a route token passed the memory check")
	}
	if _, err := sessiontoken.Verify(reg.key, memTok, "brig-a", "", "brig-route", time.Now()); err == nil {
		t.Error("a memory token passed as typ brig-route")
	}
}

// The memory key is its own file, 0600, not the host key, and a sandbox's
// memory key differs from its route key for the same id and nonce.
func TestMemoryKeyIsNotHostKey(t *testing.T) {
	r, _ := startMemoryBroker(t)
	if _, err := r.client.AddMemory(injector.MemorySandbox{ID: "brig-a", Key: "notes"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.dir, injector.MemoryKeyName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() != hostKeySize {
		t.Errorf("memory key mode %v size %d", info.Mode().Perm(), info.Size())
	}
	memKey, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := os.ReadFile(filepath.Join(r.dir, injector.HostKeyName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(memKey, hostKey) {
		t.Fatal("the memory key is the host key")
	}
	reg := r.memoryReg(t, "brig-a")
	routeKey, err := sessiontoken.SandboxKey(hostKey, "brig-sandbox", "brig-a", reg.nonce)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(reg.key, routeKey) {
		t.Error("the memory key of brig-a is its route key")
	}
	want, err := sessiontoken.SandboxKey(memKey, "brig-memory", "brig-a", reg.nonce)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reg.key, want) {
		t.Error("the memory key of brig-a is not derived from memorykey with label brig-memory")
	}
	// A restart reads the same memory key.
	r.cancel()
	<-r.done
	r.done <- nil
	again, err := loadOrCreateMemoryKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(memKey, again) {
		t.Error("the memory key changed across a restart")
	}
}

// A store key that is neither a store name nor a project key is refused
// before any path is built from it, and nothing is created.
func TestAddMemoryRefusesBadKey(t *testing.T) {
	r, root := startMemoryBroker(t)
	for _, key := range []string{"../x", "project-zz", "project-0123456789abcde", "project-0123456789ABCDEF", "a/b", "", ".x"} {
		_, err := r.client.AddMemory(injector.MemorySandbox{ID: "brig-a", Key: key})
		if refusedStatus(err) != http.StatusBadRequest {
			t.Errorf("key %q = %v, want 400", key, err)
		}
	}
	if _, err := r.client.AddMemory(injector.MemorySandbox{ID: "brig a", Key: "notes"}); refusedStatus(err) != http.StatusBadRequest {
		t.Errorf("id with a space = %v, want 400", err)
	}
	if _, err := r.client.AddMemory(injector.MemorySandbox{ID: "brig-a", Key: "notes", BrigInternal: "127.0.0.1"}); refusedStatus(err) != http.StatusBadRequest {
		t.Errorf("a loopback brig.internal = %v, want 400", err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(root), "x")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused key created %s: %v", filepath.Join(filepath.Dir(root), "x"), err)
	}
	if st, err := r.client.Status(); err != nil || len(st.Memory) != 0 {
		t.Errorf("after refusals status = %+v, %v", st, err)
	}
	if _, err := r.client.AddMemory(injector.MemorySandbox{ID: "brig-a", Key: memory.ProjectKey("/work/app"),
		Project: "/work/app"}); err != nil {
		t.Errorf("a project key was refused: %v", err)
	}
}

// A project store is registered with the folder its key was made from, and
// the broker records it, so `brig memory ls` names the folder. A named store
// with a folder, a project key without one and a folder of another key are
// refused.
func TestAddMemoryRecordsTheProject(t *testing.T) {
	r, root := startMemoryBroker(t)
	key := memory.ProjectKey("/work/app")
	for _, bad := range []injector.MemorySandbox{
		{ID: "brig-a", Key: "notes", Project: "/work/app"},
		{ID: "brig-a", Key: key},
		{ID: "brig-a", Key: key, Project: "/work/other"},
		{ID: "brig-a", Key: key, Project: "work/app"},
	} {
		if _, err := r.client.AddMemory(bad); refusedStatus(err) != http.StatusBadRequest {
			t.Errorf("%+v = %v, want 400", bad, err)
		}
	}
	if _, err := r.client.AddMemory(injector.MemorySandbox{ID: "brig-a", Key: key, Project: "/work/app"}); err != nil {
		t.Fatal(err)
	}
	got, err := memory.List(root)
	if err != nil || len(got) != 1 || got[0].Key != key || got[0].Project != "/work/app" {
		t.Errorf("memory.List = %+v, %v, want %s for /work/app", got, err, key)
	}
}

// The same registration again keeps the nonce, so a running agent's token
// stays valid. Another store under the same id is refused.
func TestAddMemoryOtherKey409(t *testing.T) {
	r, _ := startMemoryBroker(t)
	if _, err := r.client.AddMemory(injector.MemorySandbox{ID: "brig-a", Key: "notes"}); err != nil {
		t.Fatal(err)
	}
	tok, err := r.client.MintMemory(injector.MemoryMintRequest{Sandbox: "brig-a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.client.AddMemory(injector.MemorySandbox{ID: "brig-a", Key: "notes"}); err != nil {
		t.Fatalf("the same registration again: %v", err)
	}
	if _, err := sessiontoken.Verify(r.memoryReg(t, "brig-a").key, tok, "brig-a", "", memoryTokenType, time.Now()); err != nil {
		t.Errorf("the same registration again revoked the token: %v", err)
	}
	if _, err := r.client.AddMemory(injector.MemorySandbox{ID: "brig-a", Key: "other"}); refusedStatus(err) != http.StatusConflict {
		t.Errorf("another store under brig-a = %v, want 409", err)
	}
	if _, err := sessiontoken.Verify(r.memoryReg(t, "brig-a").key, tok, "brig-a", "", memoryTokenType, time.Now()); err != nil {
		t.Errorf("a refused registration revoked the token: %v", err)
	}
}

// A sandbox with memory alone keeps the broker: removing its routes leaves
// it counted, stop-if-idle leaves the broker running, and removing its
// memory leaves nothing.
func TestRemainingCountsMemory(t *testing.T) {
	r, _ := startMemoryBroker(t)
	if _, err := r.client.AddMemory(injector.MemorySandbox{ID: "brig-a", Key: "notes"}); err != nil {
		t.Fatal(err)
	}
	st, err := r.client.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(st.Memory, []string{"brig-a"}) || len(st.Sandboxes) != 0 {
		t.Errorf("status = %+v, want brig-a under memory alone", st)
	}
	rm, err := r.client.RemoveSandbox("brig-a")
	if err != nil || rm.Removed || rm.Remaining != 1 {
		t.Errorf("remove the routes = %+v, %v, want removed false, remaining 1", rm, err)
	}
	if stopping, err := r.client.Stop(true); err != nil || stopping {
		t.Errorf("stop ifIdle with memory registered = %v, %v", stopping, err)
	}
	rm, err = r.client.RemoveMemory("brig-a")
	if err != nil || !rm.Removed || rm.Remaining != 0 {
		t.Errorf("remove the memory = %+v, %v, want removed true, remaining 0", rm, err)
	}
	if _, err := r.client.MintMemory(injector.MemoryMintRequest{Sandbox: "brig-a"}); refusedStatus(err) != http.StatusNotFound {
		t.Errorf("a memory token after the removal = %v, want 404", err)
	}
	if stopping, err := r.client.Stop(true); err != nil || !stopping {
		t.Errorf("stop ifIdle with nothing registered = %v, %v", stopping, err)
	}
}

// memoryRig is a broker in this process with a memory root, for sandboxes
// that reach their memory at brig.internal.
type memoryRig struct {
	*running
	root string
}

func newMemoryRig(t *testing.T) *memoryRig {
	t.Helper()
	root := filepath.Join(shortDir(t), "memory")
	cfg := serverConfig{dir: shortDir(t), logTo: io.Discard, memoryDir: root}
	r := startBrokerWith(t, cfg, newUpstream(t))
	if err := r.store.Update("claude-credentials", fakeDoc("real-access-token", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	return &memoryRig{running: r, root: root}
}

// addMemory registers the memory of id, store key, at the brig.internal
// address internal, and returns the broker's answer.
func (r *memoryRig) addMemory(t *testing.T, id, key, internal string) injector.AddResponse {
	t.Helper()
	added, err := r.client.AddMemory(injector.MemorySandbox{ID: id, Key: key, BrigInternal: internal})
	if err != nil {
		t.Fatal(err)
	}
	return added
}

func (r *memoryRig) memoryToken(t *testing.T, id string) string {
	t.Helper()
	tok, err := r.client.MintMemory(injector.MemoryMintRequest{Sandbox: id})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// rpc sends one JSON-RPC body to https://brig.internal<path> and returns the
// status and the body.
func rpc(c *http.Client, path, token, body string) (int, string, error) {
	req, err := http.NewRequest(http.MethodPost, "https://"+injector.BrigInternal+path, strings.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(b), err
}

const (
	rpcInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	rpcToolsList  = `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
)

// A sandbox registered for memory alone is handed its divert socket and the
// CA, and serves its memory there. A token of another sandbox is refused on
// its socket.
func TestMemoryPathOtherSandboxToken401(t *testing.T) {
	rig := newMemoryRig(t)
	a := rig.addMemory(t, "brig-a", "notes-a", internalA)
	b := rig.addMemory(t, "brig-b", "notes-b", internalB)
	if a.DivertSocket == "" || a.CA == "" {
		t.Fatalf("add memory with brig.internal = %+v, want a divert socket and the CA", a)
	}
	if want, _ := injector.DivertSocket(rig.dir, "brig-a"); a.DivertSocket != want {
		t.Errorf("the divert socket is %s, want %s", a.DivertSocket, want)
	}
	tokA, tokB := rig.memoryToken(t, "brig-a"), rig.memoryToken(t, "brig-b")
	cb := guestClient(b.DivertSocket, internalB+":443", caPool(t, b))
	if got, body, err := rpc(cb, "/brig-b/memory", tokB, rpcInitialize); err != nil || got != http.StatusOK ||
		!strings.Contains(body, "brig-memory") {
		t.Fatalf("brig-b's own token: %d %q %v", got, body, err)
	}
	resp, err := func() (*http.Response, error) {
		req, err := http.NewRequest(http.MethodPost, "https://"+injector.BrigInternal+"/brig-b/memory",
			strings.NewReader(rpcInitialize))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tokA)
		return cb.Do(req)
	}()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("brig-a's token on /brig-b/memory: %d %q, want 401 with WWW-Authenticate: Bearer",
			resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	if strings.Count(strings.TrimSpace(string(body)), "\n") != 0 || strings.Contains(string(body), rig.root) {
		t.Errorf("the refusal is not one line without a host path: %q", body)
	}
	if got, _, err := rpc(cb, "/brig-b/memory", "", rpcInitialize); err != nil || got != http.StatusUnauthorized {
		t.Errorf("no token: %d %v, want 401", got, err)
	}
}

// A token is served only on the divert socket of its own sandbox: brig-a's
// token for /brig-a/memory arriving on brig-b's socket is refused.
func TestMemoryPathWrongArrival(t *testing.T) {
	rig := newMemoryRig(t)
	rig.addMemory(t, "brig-a", "notes-a", internalA)
	b := rig.addMemory(t, "brig-b", "notes-b", internalB)
	tokA := rig.memoryToken(t, "brig-a")
	cb := guestClient(b.DivertSocket, internalB+":443", caPool(t, b))
	if got, _, err := rpc(cb, "/brig-a/memory", tokA, rpcInitialize); err != nil || got != http.StatusNotFound {
		t.Errorf("brig-a's memory on brig-b's socket: %d %v, want 404", got, err)
	}
	// A sandbox with no memory registration is not found either.
	if got, _, err := rpc(cb, "/brig-c/memory", tokA, rpcInitialize); err != nil || got != http.StatusNotFound {
		t.Errorf("an unregistered sandbox: %d %v, want 404", got, err)
	}
}

// The audit line of a memory request names the sandbox whose socket it
// arrived on, and the sandbox its path names only as path, clipped. A guest
// of brig-b that asks for /brig-a/memory writes no line that blames brig-a,
// and a long path id cannot grow the line.
func TestMemoryAuditNamesTheArrival(t *testing.T) {
	rig := newMemoryRig(t)
	rig.addMemory(t, "brig-a", "notes-a", internalA)
	b := rig.addMemory(t, "brig-b", "notes-b", internalB)
	cb := guestClient(b.DivertSocket, internalB+":443", caPool(t, b))
	if got, _, err := rpc(cb, "/brig-a/memory", "", rpcInitialize); err != nil || got != http.StatusNotFound {
		t.Fatalf("brig-a's memory on brig-b's socket: %d %v, want 404", got, err)
	}
	long := strings.Repeat("x", 300)
	if got, _, err := rpc(cb, "/"+long+"/memory", "", rpcInitialize); err != nil || got != http.StatusNotFound {
		t.Fatalf("a long id on brig-b's socket: %d %v, want 404", got, err)
	}
	if lines := auditWith(t, rig.dir, "kind=memory", "sandbox=brig-b", "path=brig-a", "check=arrival"); len(lines) != 1 {
		t.Errorf("the audit lines of brig-b asking for brig-a: %q", lines)
	}
	if lines := auditWith(t, rig.dir, "kind=memory", "sandbox=brig-b", "path="+long[:64]+"...",
		"check=arrival"); len(lines) != 1 {
		t.Errorf("the audit lines of a long path id: %q", lines)
	}
	if lines := auditWith(t, rig.dir, "sandbox=brig-a"); len(lines) != 0 {
		t.Errorf("a request of brig-b wrote a line naming brig-a as its sandbox: %q", lines)
	}
	if lines := auditWith(t, rig.dir, strings.Repeat("x", 65)); len(lines) != 0 {
		t.Errorf("the path id was not clipped: %q", lines)
	}
}

// Memory is served only on a diverted connection: the loopback port answers
// /<id>/memory 404 with a valid token.
func TestMemoryNotOnLoopback(t *testing.T) {
	rig := newMemoryRig(t)
	rig.addMemory(t, "brig-a", "notes-a", internalA)
	tok := rig.memoryToken(t, "brig-a")
	url := fmt.Sprintf("http://127.0.0.1:%d/brig-a/memory", rig.srv.port)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(rpcInitialize))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || strings.Contains(string(body), "brig-memory") {
		t.Errorf("memory on the loopback port: %d %q, want 404", resp.StatusCode, body)
	}
}

// A route token is not a memory token on the memory path.
func TestRouteTokenOnMemoryPath401(t *testing.T) {
	rig := newMemoryRig(t)
	added, err := rig.client.Add(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute()}, BrigInternal: internalA})
	if err != nil {
		t.Fatal(err)
	}
	rig.addMemory(t, "brig-a", "notes-a", internalA)
	routeTok := rig.mint(t, "brig-a", "anthropic")
	c := guestClient(added.DivertSocket, internalA+":443", caPool(t, added))
	if got, _, err := rpc(c, "/brig-a/memory", routeTok, rpcInitialize); err != nil || got != http.StatusUnauthorized {
		t.Errorf("a route token on the memory path: %d %v, want 401", got, err)
	}
	if got, _, err := rpc(c, "/brig-a/memory", rig.memoryToken(t, "brig-a"), rpcInitialize); err != nil || got != http.StatusOK {
		t.Errorf("the memory token on the memory path: %d %v, want 200", got, err)
	}
}

// The audit line of a memory request names the tool and the memory, and
// never the body, the description, the query or the token.
func TestMemoryAuditHasNoBody(t *testing.T) {
	rig := newMemoryRig(t)
	a := rig.addMemory(t, "brig-a", "notes-a", internalA)
	tok := rig.memoryToken(t, "brig-a")
	c := guestClient(a.DivertSocket, internalA+":443", caPool(t, a))
	save := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"memory_save",` +
		`"arguments":{"name":"deploy-notes","description":"SECRETDESC","body":"SECRETBODY"}}}`
	if got, body, err := rpc(c, "/brig-a/memory", tok, save); err != nil || got != http.StatusOK ||
		!strings.Contains(body, "saved deploy-notes") {
		t.Fatalf("memory_save: %d %q %v", got, body, err)
	}
	search := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"memory_search",` +
		`"arguments":{"query":"SECRETQUERY"}}}`
	if got, _, err := rpc(c, "/brig-a/memory", tok, search); err != nil || got != http.StatusOK {
		t.Fatalf("memory_search: %d %v", got, err)
	}
	lines := auditWith(t, rig.dir, "kind=memory", "sandbox=brig-a", "method=tools/call", "tool=memory_save",
		"name=deploy-notes", "status=200")
	if len(lines) != 1 {
		t.Errorf("the audit lines of memory_save: %q", lines)
	}
	if got := auditWith(t, rig.dir, "kind=memory", "tool=memory_search"); len(got) != 1 {
		t.Errorf("the audit lines of memory_search: %q", got)
	}
	blob, err := os.ReadFile(filepath.Join(rig.dir, injector.AuditName))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SECRETBODY", "SECRETDESC", "SECRETQUERY", tok} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("the audit log holds %q", secret)
		}
	}
}

// Memory requests past the bound of one sandbox are answered 503 at once,
// and the routes on the same broker keep serving.
func TestMemoryBusy503RoutesStillServe(t *testing.T) {
	rig := newMemoryRig(t)
	added, err := rig.client.Add(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute()}, BrigInternal: internalA})
	if err != nil {
		t.Fatal(err)
	}
	rig.addMemory(t, "brig-a", "notes-a", internalA)
	b := rig.addMemory(t, "brig-b", "notes-b", internalB)
	tok := rig.memoryToken(t, "brig-a")
	c := guestClient(added.DivertSocket, internalA+":443", caPool(t, added))
	// The slots are taken as requests in flight would take them.
	for range maxMemoryPerSandbox {
		if !rig.srv.memGate.acquire("brig-a") {
			t.Fatal("a slot under the bound was refused")
		}
	}
	req, err := http.NewRequest(http.MethodPost, "https://"+injector.BrigInternal+"/brig-a/memory",
		strings.NewReader(rpcInitialize))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "1" ||
		strings.Count(strings.TrimSpace(string(body)), "\n") != 0 {
		t.Errorf("memory past the bound: %d Retry-After %q %q, want 503, 1 and one line",
			resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	if lines := auditWith(t, rig.dir, "kind=memory", "sandbox=brig-a", "status=503", "check=busy"); len(lines) != 1 {
		t.Errorf("the audit lines of the busy refusal: %q", lines)
	}
	routeTok := rig.mint(t, "brig-a", "anthropic")
	if got, err := post(c, "/brig-a/r/anthropic/v1/messages", routeTok); err != nil || got != http.StatusOK {
		t.Errorf("a route of the busy sandbox: %d %v, want 200", got, err)
	}
	// Another sandbox has slots of its own.
	cb := guestClient(b.DivertSocket, internalB+":443", caPool(t, b))
	if got, _, err := rpc(cb, "/brig-b/memory", rig.memoryToken(t, "brig-b"), rpcInitialize); err != nil || got != http.StatusOK {
		t.Errorf("brig-b while brig-a is busy: %d %v, want 200", got, err)
	}
	for range maxMemoryPerSandbox {
		rig.srv.memGate.release("brig-a")
	}
	if got, _, err := rpc(c, "/brig-a/memory", tok, rpcInitialize); err != nil || got != http.StatusOK {
		t.Errorf("memory once the slots are free: %d %v, want 200", got, err)
	}
}

// The bound over all sandboxes holds when each is under its own.
func TestMemoryGateGlobalBound(t *testing.T) {
	var g memoryGate
	n := 0
	for i := 0; n < maxMemoryInFlight; i++ {
		for range maxMemoryPerSandbox - 1 {
			if n == maxMemoryInFlight {
				break
			}
			if !g.acquire(fmt.Sprintf("brig-%d", i)) {
				t.Fatalf("slot %d was refused under the bound", n)
			}
			n++
		}
	}
	if g.acquire("brig-new") {
		t.Error("a slot past the global bound was granted")
	}
	g.release("brig-0")
	if !g.acquire("brig-new") {
		t.Error("a freed slot was not granted")
	}
}

// stalledMemoryRequest opens a guest connection to sock, sends the headers
// of a memory request and half its body, and stalls. It returns the
// connection, from which the broker's answer can be read.
func stalledMemoryRequest(t *testing.T, sock, internal string, pool *x509.CertPool, id, token string) *tls.Conn {
	t.Helper()
	raw, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(raw, "BRIG-DIVERT/1 %s 443 10.100.0.2 40000\n", internal); err != nil {
		t.Fatal(err)
	}
	c := tls.Client(raw, &tls.Config{RootCAs: pool, ServerName: injector.BrigInternal})
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Handshake(); err != nil {
		t.Fatal(err)
	}
	body := rpcInitialize
	if _, err := fmt.Fprintf(c, "POST /%s/memory HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\n"+
		"Authorization: Bearer %s\r\nContent-Length: %d\r\n\r\n%s", id, injector.BrigInternal, token,
		len(body), body[:len(body)/2]); err != nil {
		t.Fatal(err)
	}
	return c
}

// A guest that trickles its body holds no memory slot while it does: the
// body is read, under a deadline, before a slot is taken. Requests of the
// same sandbox stalled past its bound leave the next one served, and a
// stalled body is cut off at the deadline.
func TestStalledMemoryBodyHoldsNoSlot(t *testing.T) {
	// Put back after the broker's own cleanup, which runs first, so no
	// handler still reads it.
	saved := memoryReadTimeout
	t.Cleanup(func() { memoryReadTimeout = saved })
	memoryReadTimeout = 500 * time.Millisecond
	rig := newMemoryRig(t)
	a := rig.addMemory(t, "brig-a", "notes-a", internalA)
	tok := rig.memoryToken(t, "brig-a")
	pool := caPool(t, a)
	var stalled []*tls.Conn
	for range maxMemoryPerSandbox + 1 {
		stalled = append(stalled, stalledMemoryRequest(t, a.DivertSocket, internalA, pool, "brig-a", tok))
	}
	// The stalled requests are past the token check by the time this one
	// arrives.
	time.Sleep(100 * time.Millisecond)
	c := guestClient(a.DivertSocket, internalA+":443", pool)
	if got, _, err := rpc(c, "/brig-a/memory", tok, rpcInitialize); err != nil || got != http.StatusOK {
		t.Errorf("a request beside %d stalled ones: %d %v, want 200", len(stalled), got, err)
	}
	_ = stalled[0].SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(stalled[0]), nil)
	if err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("a stalled body was answered %d, want it cut off", resp.StatusCode)
		}
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Error("the broker did not cut off a stalled body within 5 s")
	}
}

// A store removed under a registration is not recreated empty by the next
// request: the agent gets the store's failure, with no host path, and the
// directory stays gone.
func TestMemoryStoreGoneIsNotRecreated(t *testing.T) {
	rig := newMemoryRig(t)
	a := rig.addMemory(t, "brig-a", "notes-a", internalA)
	tok := rig.memoryToken(t, "brig-a")
	if err := os.RemoveAll(filepath.Join(rig.root, "notes-a")); err != nil {
		t.Fatal(err)
	}
	c := guestClient(a.DivertSocket, internalA+":443", caPool(t, a))
	got, body, err := rpc(c, "/brig-a/memory", tok, rpcInitialize)
	if err != nil || got == http.StatusOK || strings.Contains(body, rig.root) {
		t.Errorf("a request on a removed store: %d %q %v, want a failure without the host path", got, body, err)
	}
	if _, err := os.Stat(filepath.Join(rig.root, "notes-a")); !os.IsNotExist(err) {
		t.Errorf("the removed store came back: %v", err)
	}
}

// A host route is served at <route>.brig.internal with the upstream's path,
// so a path shaped like memory's there goes to the route, not to memory.
func TestAHostRouteKeepsAMemoryShapedPath(t *testing.T) {
	for _, tc := range []struct {
		host string
		want bool
	}{
		{injector.BrigInternal, true},
		{injector.BrigInternal + ":443", true},
		{"github-api." + injector.BrigInternal, false},
		{"GitHub-API." + injector.BrigInternal + ".:443", false},
	} {
		r := httptest.NewRequest(http.MethodPost, "https://"+injector.BrigInternal+"/users/memory", nil)
		r.Host = tc.host
		id, ok := memoryTarget(r)
		if ok != tc.want || (ok && id != "users") {
			t.Errorf("Host %s, /users/memory: memory %v (sandbox %q), want %v", tc.host, ok, id, tc.want)
		}
	}
}
