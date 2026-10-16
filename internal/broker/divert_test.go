package broker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// relayRoute is customRoute in the route mode relay, which a host process
// registered for a test serves.
func relayRoute(t *testing.T, name, upstream string) Route {
	t.Helper()
	r := customRoute(t, name, upstream, nil)
	r.Route.Mode = "relay"
	return r
}

// A route besides endpoint is taken only from a registration that names its
// mode, so it is never registered with nothing to serve it.
func TestAddSandboxRefusesARouteInAnotherMode(t *testing.T) {
	h, err := New(Config{HostKey: testHostKey, Credentials: &fakeCreds{}})
	if err != nil {
		t.Fatal(err)
	}
	rt := relayRoute(t, "inventory", "https://example.com")
	err = h.AddSandbox(Sandbox{ID: "brig-a", Routes: []Route{rt}})
	if err == nil || !strings.Contains(err.Error(), `route inventory is mode "relay", and the sandbox was registered with nothing to serve it`) {
		t.Fatalf("AddSandbox = %v, want the route refused", err)
	}
	if err := h.AddSandbox(Sandbox{ID: "brig-a", Routes: []Route{rt}, Modes: []v1alpha1.RouteMode{"relay"}}); err != nil {
		t.Fatalf("the route with its mode registered was refused: %v", err)
	}
}

// The host process binds a diverted connection by the host and port it
// dialed. Two routes besides endpoint on one host and port would leave one
// of them unreachable, so the registration is refused.
func TestAddSandboxRefusesTwoDivertedRoutesOnOneHostAndPort(t *testing.T) {
	h, err := New(Config{HostKey: testHostKey, Credentials: &fakeCreds{}})
	if err != nil {
		t.Fatal(err)
	}
	relay := []v1alpha1.RouteMode{"relay"}
	err = h.AddSandbox(Sandbox{ID: "brig-a", Modes: relay, Routes: []Route{
		relayRoute(t, "a", "https://example.com"), relayRoute(t, "b", "https://example.com:443")}})
	if err == nil || !strings.Contains(err.Error(), "routes a and b are both diverted on example.com:443") {
		t.Errorf("AddSandbox = %v, want the second route refused", err)
	}
	if err := h.AddSandbox(Sandbox{ID: "brig-a", Modes: relay, Routes: []Route{
		relayRoute(t, "a", "https://example.com"), relayRoute(t, "b", "https://example.com:8443"),
		customRoute(t, "c", "https://example.com", nil)}}); err != nil {
		t.Errorf("routes on two ports, and an endpoint route on the same one: %v", err)
	}
}

// A route besides endpoint is not served over the endpoint path, whatever
// its token: its policy did not ask for an endpoint.
func TestServeHTTPRefusesARouteInAnotherMode(t *testing.T) {
	h, err := New(Config{HostKey: testHostKey, Credentials: newFakeCreds()})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.AddSandbox(Sandbox{ID: "brig-a", Profile: "claude-code", Modes: []v1alpha1.RouteMode{"relay"},
		Routes: []Route{relayRoute(t, "inventory", "https://example.com")}}); err != nil {
		t.Fatal(err)
	}
	tok, err := h.Mint("brig-a", "", "inventory", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/brig-a/r/inventory/items", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("a route in another mode over the endpoint path got %d, want 404", rec.Code)
	}
}
