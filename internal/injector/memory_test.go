package injector

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCall is one request a fake broker took.
type fakeCall struct {
	Method, Path, Body string
}

// fakeControl serves answer on the control socket of a fresh broker
// directory, records every request, and returns the directory and the
// record.
func fakeControl(t *testing.T, answer func(method, path string) (int, string)) (string, func() []fakeCall) {
	t.Helper()
	dir, err := os.MkdirTemp("", "brk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, SocketName))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls []fakeCall
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, fakeCall{r.Method, r.URL.Path, string(body)})
		mu.Unlock()
		code, out := answer(r.Method, r.URL.Path)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, out)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return dir, func() []fakeCall {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(calls)
	}
}

func paths(calls []fakeCall) []string {
	var out []string
	for _, c := range calls {
		out = append(out, c.Method+" "+c.Path)
	}
	return out
}

// The three memory calls go to their own endpoints, apart from the routes'.
func TestMemoryClientCalls(t *testing.T) {
	dir, calls := fakeControl(t, func(method, path string) (int, string) {
		switch method + " " + path {
		case "POST /memory/sandboxes":
			return 200, `{}`
		case "DELETE /memory/sandboxes/brig-a":
			return 200, `{"removed":true,"remaining":2}`
		case "POST /memory/tokens":
			return 200, `{"token":"fake-memory-token"}`
		}
		return 404, "no such endpoint"
	})
	c := Client{Socket: filepath.Join(dir, SocketName)}
	if _, err := c.AddMemory(MemorySandbox{ID: "brig-a", Session: "s1", Key: "notes", BrigInternal: "192.168.64.1"}); err != nil {
		t.Fatal(err)
	}
	rm, err := c.RemoveMemory("brig-a")
	if err != nil || !rm.Removed || rm.Remaining != 2 {
		t.Fatalf("remove memory = %+v, %v", rm, err)
	}
	tok, err := c.MintMemory(MemoryMintRequest{Sandbox: "brig-a", Session: "s1", TTLSeconds: 60})
	if err != nil || tok != "fake-memory-token" {
		t.Fatalf("mint memory = %q, %v", tok, err)
	}
	got := calls()
	if want := []string{"POST /memory/sandboxes", "DELETE /memory/sandboxes/brig-a", "POST /memory/tokens"}; !slices.Equal(paths(got), want) {
		t.Fatalf("calls = %v, want %v", paths(got), want)
	}
	var add map[string]any
	if err := json.Unmarshal([]byte(got[0].Body), &add); err != nil {
		t.Fatal(err)
	}
	if add["id"] != "brig-a" || add["session"] != "s1" || add["key"] != "notes" || add["brigInternal"] != "192.168.64.1" {
		t.Errorf("add memory body = %s", got[0].Body)
	}
	var mint map[string]any
	if err := json.Unmarshal([]byte(got[2].Body), &mint); err != nil {
		t.Fatal(err)
	}
	if mint["sandbox"] != "brig-a" || mint["session"] != "s1" || mint["ttlSeconds"] != float64(60) {
		t.Errorf("mint memory body = %s", got[2].Body)
	}
}

// An empty memory token is an error, not a token.
func TestMintMemoryRefusesAnEmptyToken(t *testing.T) {
	dir, _ := fakeControl(t, func(string, string) (int, string) { return 200, `{"token":""}` })
	if _, err := (Client{Socket: filepath.Join(dir, SocketName)}).MintMemory(MemoryMintRequest{Sandbox: "brig-a"}); err == nil {
		t.Error("an empty memory token was taken")
	}
}

// Forget removes the routes and the memory of a sandbox, and stops the
// broker only when the last removal leaves nothing.
func TestForgetRemovesRoutesAndMemory(t *testing.T) {
	for _, tc := range []struct {
		remaining string
		want      []string
	}{
		{"0", []string{"DELETE /sandboxes/brig-a", "DELETE /memory/sandboxes/brig-a", "DELETE /mcp/sandboxes/brig-a", "POST /stop"}},
		{"1", []string{"DELETE /sandboxes/brig-a", "DELETE /memory/sandboxes/brig-a", "DELETE /mcp/sandboxes/brig-a"}},
	} {
		dir, calls := fakeControl(t, func(method, path string) (int, string) {
			if path == "/stop" {
				return 200, `{"stopping":true}`
			}
			return 200, `{"removed":true,"remaining":` + tc.remaining + `}`
		})
		if err := Forget(dir, "brig-a"); err != nil {
			t.Fatal(err)
		}
		if got := paths(calls()); !slices.Equal(got, tc.want) {
			t.Errorf("remaining %s: calls = %v, want %v", tc.remaining, got, tc.want)
		}
	}
}

// A broker older than memory answers 404 to the memory endpoints. Forget
// still revokes the routes and stops it.
func TestForgetToleratesABrokerWithoutMemory(t *testing.T) {
	dir, calls := fakeControl(t, func(method, path string) (int, string) {
		switch path {
		case "/sandboxes/brig-a":
			return 200, `{"removed":true,"remaining":0}`
		case "/stop":
			return 200, `{"stopping":true}`
		}
		return 404, "404 page not found"
	})
	if err := Forget(dir, "brig-a"); err != nil {
		t.Fatal(err)
	}
	if got, want := paths(calls()), []string{"DELETE /sandboxes/brig-a", "DELETE /memory/sandboxes/brig-a", "DELETE /mcp/sandboxes/brig-a", "POST /stop"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

// A refusal other than a missing endpoint is still returned.
func TestForgetReturnsAMemoryRefusal(t *testing.T) {
	dir, _ := fakeControl(t, func(method, path string) (int, string) {
		if strings.HasPrefix(path, "/memory/") {
			return 500, "the memory store is broken"
		}
		return 200, `{"removed":true,"remaining":0}`
	})
	var ref *RefusedError
	if err := Forget(dir, "brig-a"); !errors.As(err, &ref) || ref.Status != 500 {
		t.Errorf("Forget = %v, want the 500 refusal", err)
	}
}

// ForgetAll removes every route and every memory registration, and names
// each sandbox it removed anything of once.
func TestForgetAllRemovesMemory(t *testing.T) {
	dir, calls := fakeControl(t, func(method, path string) (int, string) {
		switch {
		case path == "/status":
			return 200, `{"pid":1,"port":1,"sandboxes":["brig-a"],"memory":["brig-a","brig-b"]}`
		case path == "/stop":
			return 200, `{"stopping":true}`
		}
		return 200, `{"removed":true,"remaining":0}`
	})
	got, err := ForgetAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"brig-a", "brig-b"}) {
		t.Errorf("ForgetAll removed %v, want brig-a brig-b", got)
	}
	want := []string{"GET /status", "DELETE /sandboxes/brig-a", "DELETE /memory/sandboxes/brig-a",
		"DELETE /memory/sandboxes/brig-b", "POST /stop"}
	if p := paths(calls()); !slices.Equal(p, want) {
		t.Errorf("calls = %v, want %v", p, want)
	}
}

// RegisterMemory starts the broker as Register does, the store lands under
// BRIG_STATE_DIR and not under the home directory, and Forget of the
// sandbox stops the broker.
func TestRegisterMemoryStartsTheBroker(t *testing.T) {
	tmp := setBroker(t)
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopBroker(dir) })
	mem, err := MemoryDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(tmp, "state", "memory"); mem != want {
		t.Fatalf("MemoryDir = %s, want %s", mem, want)
	}
	key := "brig-test-store-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	b, resp, err := RegisterMemory(dir, MemorySandbox{ID: "brig-a", Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if resp.DivertSocket != "" {
		t.Errorf("RegisterMemory answer = %+v", resp)
	}
	if info, err := os.Lstat(filepath.Join(mem, key)); err != nil || !info.IsDir() {
		t.Errorf("the store is not under BRIG_STATE_DIR: %v", err)
	}
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := os.Lstat(filepath.Join(home, ".brig", "memory", key)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the broker wrote the store under the home directory: %v", err)
		}
	}
	if _, err := b.Client.MintMemory(MemoryMintRequest{Sandbox: "brig-a"}); err != nil {
		t.Fatal(err)
	}
	st, err := b.Client.Status()
	if err != nil || !slices.Equal(st.Memory, []string{"brig-a"}) {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if err := Forget(dir, "brig-a"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := livePID(dir); !ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, ok := livePID(dir); ok {
		t.Error("the broker did not stop when its one memory sandbox was forgotten")
	}
}

// ForgetRoutes removes the routes of a sandbox and never its memory, so the
// memory token another exec's agent holds stays valid. It stops the broker
// only when nothing is left.
func TestForgetRoutesKeepsMemory(t *testing.T) {
	for _, tc := range []struct {
		remaining string
		want      []string
	}{
		{"1", []string{"DELETE /sandboxes/brig-a"}},
		{"0", []string{"DELETE /sandboxes/brig-a", "POST /stop"}},
	} {
		dir, calls := fakeControl(t, func(method, path string) (int, string) {
			if path == "/stop" {
				return 200, `{"stopping":true}`
			}
			return 200, `{"removed":true,"remaining":` + tc.remaining + `}`
		})
		if err := ForgetRoutes(dir, "brig-a"); err != nil {
			t.Fatal(err)
		}
		if got := paths(calls()); !slices.Equal(got, tc.want) {
			t.Errorf("remaining %s: calls = %v, want %v", tc.remaining, got, tc.want)
		}
	}
}

// Revoking memory does not depend on removing the routes: when the route
// removal is refused, Forget still removes the memory, stops a broker left
// with nothing, and returns the refusal.
func TestForgetRemovesMemoryWhenRoutesFail(t *testing.T) {
	dir, calls := fakeControl(t, func(method, path string) (int, string) {
		switch path {
		case "/sandboxes/brig-a":
			return 500, "the routes are broken"
		case "/stop":
			return 200, `{"stopping":true}`
		}
		return 200, `{"removed":true,"remaining":0}`
	})
	var ref *RefusedError
	if err := Forget(dir, "brig-a"); !errors.As(err, &ref) || ref.Status != 500 {
		t.Errorf("Forget = %v, want the 500 refusal", err)
	}
	want := []string{"DELETE /sandboxes/brig-a", "DELETE /memory/sandboxes/brig-a", "DELETE /mcp/sandboxes/brig-a", "POST /stop"}
	if got := paths(calls()); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

// ForgetAll goes on to the memory registrations when a route removal is
// refused, and returns the refusal.
func TestForgetAllRemovesMemoryWhenRoutesFail(t *testing.T) {
	dir, calls := fakeControl(t, func(method, path string) (int, string) {
		switch {
		case path == "/status":
			return 200, `{"pid":1,"port":1,"sandboxes":["brig-a"],"memory":["brig-a"]}`
		case path == "/stop":
			return 200, `{"stopping":true}`
		case path == "/sandboxes/brig-a":
			return 500, "the routes are broken"
		}
		return 200, `{"removed":true,"remaining":0}`
	})
	got, err := ForgetAll(dir)
	var ref *RefusedError
	if !errors.As(err, &ref) || ref.Status != 500 {
		t.Errorf("ForgetAll = %v, want the 500 refusal", err)
	}
	if !slices.Equal(got, []string{"brig-a"}) {
		t.Errorf("ForgetAll removed %v, want brig-a", got)
	}
	want := []string{"GET /status", "DELETE /sandboxes/brig-a", "DELETE /memory/sandboxes/brig-a", "POST /stop"}
	if p := paths(calls()); !slices.Equal(p, want) {
		t.Errorf("calls = %v, want %v", p, want)
	}
}
