package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/injector"
)

// podKey is the API key a pod holds itself and sends on a passthrough route.
const podKey = "sk-ant-api03-the-pod-holds-this-key"

func passthroughRoute() injector.Route {
	r := v1alpha1.Route{Name: "anthropic", Preset: v1alpha1.PresetAnthropicAPIKey, Passthrough: true,
		Request: &v1alpha1.RequestPolicy{Allow: &v1alpha1.RequestAllow{Paths: []string{"/v1/messages"}}}}
	return injector.Route{Policy: "anthropic-passthrough", Route: r}
}

// sendKey sends one POST to https://brig.internal/<sandbox>/r/<route><path>
// with key in X-Api-Key, and returns the status.
func sendKey(t *testing.T, c *http.Client, sandbox, route, key, path string) int {
	t.Helper()
	url := "https://" + injector.BrigInternal + "/" + sandbox + "/r/" + route + path
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", key)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// A node broker holds no secret for a passthrough route: the pod's own key
// goes upstream, the request rules still apply, and only the pod at the
// sandbox's address is served for it.
func TestNodeBrokerServesAPassthroughRoute(t *testing.T) {
	u := newUpstream(t)
	r := startBrokerWith(t, nodeConfig(t, io.Discard), u)
	added, err := r.client.Add(nodeSandbox("brig-a", "127.0.0.1", passthroughRoute()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.client.Add(nodeSandbox("brig-b", "10.255.0.1", passthroughRoute())); err != nil {
		t.Fatal(err)
	}
	pod := nodeClient(listenAddr(r), caPool(t, added), nil)

	if code := sendKey(t, pod, "brig-a", "anthropic", podKey, "/v1/messages"); code != http.StatusOK {
		t.Fatalf("the pod's own request got %d", code)
	}
	seen := u.requests()
	if len(seen) != 1 || seen[0].Get("X-Api-Key") != podKey {
		t.Fatalf("upstream saw %v, want the pod's X-Api-Key", seen)
	}

	if code := sendKey(t, pod, "brig-b", "anthropic", podKey, "/v1/messages"); code != http.StatusNotFound {
		t.Errorf("a request for another pod's sandbox got %d, want 404", code)
	}
	if code := sendKey(t, pod, "brig-a", "anthropic", podKey, "/v1/models"); code != http.StatusForbidden {
		t.Errorf("a path outside request.allow got %d, want 403", code)
	}
	if n := len(u.requests()); n != 1 {
		t.Errorf("upstream saw %d requests, want the one allowed request", n)
	}
	// The refusal's line is the last one written, so the log is whole.
	if lines := auditWith(t, r.dir, "check=request.allow.paths"); len(lines) == 0 {
		t.Fatal("the audit log has no check=request.allow.paths line")
	}
	blob, err := os.ReadFile(filepath.Join(r.dir, injector.AuditName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), podKey) {
		t.Errorf("the audit log carries the pod's key:\n%s", blob)
	}
}

// A sandbox with neither an address nor a brig.internal address reaches the
// broker only on the loopback port, which names no sandbox. Its passthrough
// route is refused at registration, and nothing is registered.
func TestAPassthroughRouteNeedsABinding(t *testing.T) {
	r := startBrokerWith(t, serverConfig{dir: shortDir(t), logTo: io.Discard}, nil)
	_, err := r.client.Add(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{passthroughRoute()}})
	ref := refusal(t, err)
	if ref.Status != http.StatusBadRequest || !strings.Contains(ref.Message, "passthrough") {
		t.Fatalf("an unbound passthrough route = %d %q, want 400 naming passthrough", ref.Status, ref.Message)
	}
	if st, _ := r.client.Status(); len(st.Sandboxes) != 0 {
		t.Errorf("sandboxes = %v after the refusal", st.Sandboxes)
	}
	// The same sandbox with a route that carries a session token is bound by
	// the token, and registers.
	if _, err := r.client.Add(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute()}}); err != nil {
		t.Errorf("a token route on the loopback port: %v", err)
	}
}
