package injector

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// The three MCP calls go to their own endpoints, apart from the routes' and
// memory's, so a route or memory endpoint never takes an MCP registration.
func TestClientMCPPaths(t *testing.T) {
	dir, calls := fakeControl(t, func(method, path string) (int, string) {
		switch method + " " + path {
		case "POST /mcp/sandboxes":
			return 200, `{"divertSocket":"/x/sock","ca":"PEM"}`
		case "DELETE /mcp/sandboxes/brig-a":
			return 200, `{"removed":true,"remaining":2}`
		case "POST /mcp/tokens":
			return 200, `{"token":"fake-mcp-token"}`
		}
		return 404, "no such endpoint"
	})
	c := Client{Socket: filepath.Join(dir, SocketName)}
	srv := v1alpha1.MCPServer{Name: "nofire", Preset: v1alpha1.MCPPresetNofire,
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: "nofire-token"}},
		Tools:      &v1alpha1.MCPTools{Allow: []string{"*"}}}
	added, err := c.AddMCP(MCPSandbox{ID: "brig-a", Profile: "claude-code", Session: "s1",
		Servers: []MCPServer{{Policy: "nofire", Server: srv}}, BrigInternal: "192.168.64.1"})
	if err != nil {
		t.Fatal(err)
	}
	if added.DivertSocket != "/x/sock" || added.CA != "PEM" {
		t.Errorf("add mcp = %+v", added)
	}
	rm, err := c.RemoveMCP("brig-a")
	if err != nil || !rm.Removed || rm.Remaining != 2 {
		t.Fatalf("remove mcp = %+v, %v", rm, err)
	}
	tok, err := c.MintMCP(MCPMintRequest{Sandbox: "brig-a", Session: "s1", TTLSeconds: 60})
	if err != nil || tok != "fake-mcp-token" {
		t.Fatalf("mint mcp = %q, %v", tok, err)
	}
	got := calls()
	if want := []string{"POST /mcp/sandboxes", "DELETE /mcp/sandboxes/brig-a", "POST /mcp/tokens"}; !slices.Equal(paths(got), want) {
		t.Fatalf("calls = %v, want %v", paths(got), want)
	}
	var add struct {
		ID, Profile, Session, BrigInternal string
		Servers                            []struct {
			Policy string
			Server v1alpha1.MCPServer
		}
	}
	if err := json.Unmarshal([]byte(got[0].Body), &add); err != nil {
		t.Fatal(err)
	}
	if add.ID != "brig-a" || add.Profile != "claude-code" || add.Session != "s1" || add.BrigInternal != "192.168.64.1" ||
		len(add.Servers) != 1 || add.Servers[0].Policy != "nofire" || add.Servers[0].Server.Name != "nofire" {
		t.Errorf("add mcp body = %s", got[0].Body)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(got[0].Body), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "profile", "session", "servers", "brigInternal"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("add mcp body has no %q: %s", k, got[0].Body)
		}
	}
	var mint map[string]any
	if err := json.Unmarshal([]byte(got[2].Body), &mint); err != nil {
		t.Fatal(err)
	}
	if mint["sandbox"] != "brig-a" || mint["session"] != "s1" || mint["ttlSeconds"] != float64(60) {
		t.Errorf("mint mcp body = %s", got[2].Body)
	}
}

// An empty MCP token is an error, not a token.
func TestMintMCPRefusesAnEmptyToken(t *testing.T) {
	dir, _ := fakeControl(t, func(string, string) (int, string) { return 200, `{"token":""}` })
	if _, err := (Client{Socket: filepath.Join(dir, SocketName)}).MintMCP(MCPMintRequest{Sandbox: "brig-a"}); err == nil {
		t.Error("an empty mcp token was taken")
	}
}

// RegisterMCP starts the broker as Register does, and the broker hands back
// the divert socket and the CA a sandbox reaches brig.internal by.
func TestRegisterMCPStartsTheBroker(t *testing.T) {
	setBroker(t)
	// The divert socket is a unix socket under the state directory, and the
	// temporary directory of macOS is too long for its path.
	short, err := os.MkdirTemp("/tmp", "bm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	t.Setenv("BRIG_STATE_DIR", short)
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopBroker(dir) })
	srv := v1alpha1.MCPServer{Name: "nofire", Preset: v1alpha1.MCPPresetNofire,
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: "nofire-token"}},
		Tools:      &v1alpha1.MCPTools{Allow: []string{"*"}}}
	b, resp, err := RegisterMCP(dir, MCPSandbox{ID: "brig-a", Profile: "claude-code",
		Servers: []MCPServer{{Policy: "nofire", Server: srv}}, BrigInternal: "198.18.1.1"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.DivertSocket == "" || resp.CA == "" {
		t.Errorf("RegisterMCP answer = %+v, want a divert socket and the CA", resp)
	}
	if _, err := b.Client.MintMCP(MCPMintRequest{Sandbox: "brig-a"}); err != nil {
		t.Fatal(err)
	}
	st, err := b.Client.Status()
	if err != nil || !slices.Equal(st.MCP, []string{"brig-a"}) {
		t.Fatalf("status = %+v, %v", st, err)
	}
	rm, err := b.Client.RemoveMCP("brig-a")
	if err != nil || !rm.Removed || rm.Remaining != 0 {
		t.Errorf("remove mcp = %+v, %v, want removed, none remaining", rm, err)
	}
}

// Forget removes the MCP servers of a sandbox beside its routes and memory,
// and stops the broker only when the last removal leaves nothing: the
// answer to the MCP removal counts all three.
func TestForgetRemovesMCP(t *testing.T) {
	for _, tc := range []struct {
		remaining string
		want      []string
	}{
		{"0", []string{"DELETE /sandboxes/brig-a", "DELETE /memory/sandboxes/brig-a", "DELETE /mcp/sandboxes/brig-a", "POST /stop"}},
		{"1", []string{"DELETE /sandboxes/brig-a", "DELETE /memory/sandboxes/brig-a", "DELETE /mcp/sandboxes/brig-a"}},
	} {
		dir, calls := fakeControl(t, func(method, path string) (int, string) {
			switch path {
			case "/stop":
				return 200, `{"stopping":true}`
			case "/mcp/sandboxes/brig-a":
				return 200, `{"removed":true,"remaining":` + tc.remaining + `}`
			}
			return 200, `{"removed":true,"remaining":5}`
		})
		if err := Forget(dir, "brig-a"); err != nil {
			t.Fatal(err)
		}
		if got := paths(calls()); !slices.Equal(got, tc.want) {
			t.Errorf("remaining %s: calls = %v, want %v", tc.remaining, got, tc.want)
		}
	}
}

// A broker older than MCP answers 404 to the MCP endpoints. Forget still
// revokes the routes and memory and stops it; any other MCP refusal is
// returned, and the broker is left running.
func TestForgetToleratesABrokerWithoutMCP(t *testing.T) {
	dir, calls := fakeControl(t, func(method, path string) (int, string) {
		switch path {
		case "/sandboxes/brig-a", "/memory/sandboxes/brig-a":
			return 200, `{"removed":true,"remaining":0}`
		case "/stop":
			return 200, `{"stopping":true}`
		}
		return 404, "404 page not found"
	})
	if err := Forget(dir, "brig-a"); err != nil {
		t.Fatal(err)
	}
	want := []string{"DELETE /sandboxes/brig-a", "DELETE /memory/sandboxes/brig-a", "DELETE /mcp/sandboxes/brig-a", "POST /stop"}
	if got := paths(calls()); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
	dir, calls = fakeControl(t, func(method, path string) (int, string) {
		if strings.HasPrefix(path, "/mcp/") {
			return 500, "the mcp registry is broken"
		}
		return 200, `{"removed":true,"remaining":0}`
	})
	var ref *RefusedError
	if err := Forget(dir, "brig-a"); !errors.As(err, &ref) || ref.Status != 500 {
		t.Errorf("Forget = %v, want the 500 refusal", err)
	}
	if got := paths(calls()); slices.Contains(got, "POST /stop") {
		t.Errorf("a refused mcp removal stopped the broker: %v", got)
	}
}

// ForgetAll removes every MCP registration, and names each sandbox it
// removed anything of once.
func TestForgetAllRemovesMCP(t *testing.T) {
	dir, calls := fakeControl(t, func(method, path string) (int, string) {
		switch path {
		case "/status":
			return 200, `{"pid":1,"port":1,"sandboxes":["brig-a"],"memory":["brig-a"],"mcp":["brig-a","brig-c"]}`
		case "/stop":
			return 200, `{"stopping":true}`
		}
		return 200, `{"removed":true,"remaining":0}`
	})
	got, err := ForgetAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"brig-a", "brig-c"}) {
		t.Errorf("ForgetAll removed %v, want brig-a brig-c", got)
	}
	want := []string{"GET /status", "DELETE /sandboxes/brig-a", "DELETE /memory/sandboxes/brig-a",
		"DELETE /mcp/sandboxes/brig-a", "DELETE /mcp/sandboxes/brig-c", "POST /stop"}
	if p := paths(calls()); !slices.Equal(p, want) {
		t.Errorf("calls = %v, want %v", p, want)
	}
}
