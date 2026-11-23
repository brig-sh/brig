package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/brig-sh/brig/internal/injector"
)

// claudePolicy is a policy document, in JSON, with one claude-code route
// whose requests may name only models, and a github route when withGitHub.
func claudePolicy(name string, withGitHub bool, models ...string) json.RawMessage {
	routes := []any{map[string]any{
		"name":       "anthropic",
		"preset":     "claude-code",
		"credential": map[string]any{"secretRef": map[string]any{"name": "claude-credentials"}},
		"request":    map[string]any{"body": map[string]any{"models": models}},
	}}
	if withGitHub {
		routes = append(routes, map[string]any{
			"name":       "github",
			"preset":     "github-token",
			"credential": map[string]any{"secretRef": map[string]any{"name": "gh-token"}},
		})
	}
	doc, err := json.Marshal(map[string]any{
		"apiVersion": "brig.sh/v1alpha1",
		"kind":       "Policy",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"egress": map[string]any{"default": "deny"},
			"routes": routes,
		},
	})
	if err != nil {
		panic(err)
	}
	return doc
}

// control sends one raw request to the control socket and returns the status
// and the body.
func (r *running) control(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", r.client.Socket)
	}}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequest(method, "http://broker"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(got)
}

func (r *running) putPolicy(t *testing.T, ref injector.PolicyRef, version string, doc json.RawMessage) injector.PutPolicyResponse {
	t.Helper()
	out, err := r.client.PutPolicy(ref, injector.PutPolicyRequest{Version: version, Policy: doc})
	if err != nil {
		t.Fatalf("put %s/%s version %s: %v", ref.Namespace, ref.Name, version, err)
	}
	return out
}

func (r *running) policies(t *testing.T) []injector.PolicyInfo {
	t.Helper()
	out, err := r.client.Policies()
	if err != nil {
		t.Fatal(err)
	}
	return out.Policies
}

func wantRefused(t *testing.T, err error, status int, what string) {
	t.Helper()
	var refused *injector.RefusedError
	if !errors.As(err, &refused) || refused.Status != status {
		t.Errorf("%s = %v, want a %d refusal", what, err, status)
	}
}

// A policy that does not load as a policy file is refused with 400, and the
// policy stored before it stays, with its version and digest.
func TestAnInvalidPolicyIsRefusedAndTheOldOneStays(t *testing.T) {
	r := startBroker(t, shortDir(t), nil, nil)
	ref := injector.PolicyRef{Namespace: "sandboxes", Name: "claude"}
	first := r.putPolicy(t, ref, "1", claudePolicy("claude", false, "claude-sonnet-4-5"))

	vends := `{"apiVersion":"brig.sh/v1alpha1","kind":"Policy","metadata":{"name":"claude"},` +
		`"spec":{"routes":[{"name":"aws","preset":"aws","credential":{"secretRef":{"name":"aws-dev"}}}]}}`
	for _, tc := range []struct{ name, doc, want string }{
		{"not a policy", `{"apiVersion":"brig.sh/v1alpha1","kind":"Policy"}`, "metadata.name is required"},
		{"an unknown field", strings.Replace(string(claudePolicy("claude", false, "x")), `"spec":{`, `"spec":{"extra":1,`, 1),
			`unknown field "extra"`},
		{"an unknown preset", strings.Replace(string(claudePolicy("claude", false, "x")), "claude-code", "claude-kode", 1),
			`preset "claude-kode"`},
		{"another name", string(claudePolicy("other", false, "x")), `named "other"`},
		{"a route that vends", vends, "cannot hold a route that vends"},
		{"a duplicate route", strings.Replace(string(claudePolicy("claude", true, "x")), `"name":"github"`, `"name":"anthropic"`, 1),
			"declared twice"},
		{"no policy", ``, "no policy"},
		{"a policy that is []", `[]`, "cannot unmarshal array"},
	} {
		body := `{"version":"2","policy":` + tc.doc + `}`
		if tc.doc == "" {
			body = `{"version":"2"}`
		}
		code, msg := r.control(t, http.MethodPut, "/policies/sandboxes/claude", body)
		if code != http.StatusBadRequest || !strings.Contains(msg, tc.want) {
			t.Errorf("%s: got %d %s, want 400 that says %q", tc.name, code, msg, tc.want)
		}
	}
	got := r.policies(t)
	if len(got) != 1 || got[0].Version != "1" || got[0].Digest != first.Digest {
		t.Errorf("after the refusals the store holds %+v, want version 1 digest %s", got, first.Digest)
	}
}

// Every control request about a policy is strict: a field the protocol does
// not have, a namespace or name that is not one, and a version that is not
// one are refused with 400 before anything is stored.
func TestPolicyRequestsAreStrict(t *testing.T) {
	r := startBroker(t, shortDir(t), nil, nil)
	doc := string(claudePolicy("claude", false, "x"))
	for _, tc := range []struct{ name, path, body string }{
		{"unknown field", "/policies/sandboxes/claude", `{"version":"1","policy":` + doc + `,"extra":1}`},
		{"not json", "/policies/sandboxes/claude", `not json`},
		{"empty version", "/policies/sandboxes/claude", `{"version":"","policy":` + doc + `}`},
		{"version with a space", "/policies/sandboxes/claude", `{"version":"1 2","policy":` + doc + `}`},
		{"upper-case namespace", "/policies/Sandboxes/claude", `{"version":"1","policy":` + doc + `}`},
		{"namespace with a dot", "/policies/sand.boxes/claude", `{"version":"1","policy":` + doc + `}`},
		{"namespace ending in a dash", "/policies/sandboxes-/claude", `{"version":"1","policy":` + doc + `}`},
		{"long namespace", "/policies/" + strings.Repeat("a", 64) + "/claude", `{"version":"1","policy":` + doc + `}`},
		{"upper-case name", "/policies/sandboxes/Claude", `{"version":"1","policy":` + doc + `}`},
	} {
		if code, msg := r.control(t, http.MethodPut, tc.path, tc.body); code != http.StatusBadRequest {
			t.Errorf("%s: got %d %s, want 400", tc.name, code, msg)
		}
	}
	if code, _ := r.control(t, http.MethodDelete, "/policies/Sandboxes/claude", ""); code != http.StatusBadRequest {
		t.Errorf("delete of a bad namespace got %d, want 400", code)
	}
	if got := r.policies(t); len(got) != 0 {
		t.Errorf("the store holds %+v after refused requests", got)
	}
}

// The store lists every policy by namespace and name, status names each with
// its version, and a delete removes one.
func TestThePolicyStoreListsDeletesAndReportsStatus(t *testing.T) {
	r := startBroker(t, shortDir(t), nil, nil)
	a := injector.PolicyRef{Namespace: "a", Name: "claude"}
	b := injector.PolicyRef{Namespace: "b", Name: "claude"}
	ra := r.putPolicy(t, a, "3", claudePolicy("claude", false, "claude-sonnet-4-5"))
	rb := r.putPolicy(t, b, "7", claudePolicy("claude", true, "claude-*"))
	if ra.Version != "3" || ra.Sandboxes != 0 || ra.PendingRoutes == nil || len(ra.PendingRoutes) != 0 {
		t.Errorf("put a = %+v", ra)
	}
	if ra.Digest == rb.Digest || len(ra.Digest) != 64 {
		t.Errorf("digests %q and %q, want two different hex SHA-256 sums", ra.Digest, rb.Digest)
	}
	got := r.policies(t)
	want := []injector.PolicyInfo{
		{Namespace: "a", Name: "claude", Version: "3", Digest: ra.Digest},
		{Namespace: "b", Name: "claude", Version: "7", Digest: rb.Digest},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("policies = %+v, want %+v", got, want)
	}
	st, err := r.client.Status()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Policies) != 2 || st.Policies["a/claude"] != "3" || st.Policies["b/claude"] != "7" {
		t.Errorf("status policies = %v", st.Policies)
	}
	del, err := r.client.DeletePolicy(a)
	if err != nil || !del.Removed || del.Sandboxes != 0 {
		t.Errorf("delete a = %+v, %v", del, err)
	}
	if del, err = r.client.DeletePolicy(a); err != nil || del.Removed {
		t.Errorf("a second delete of a = %+v, %v, want removed false", del, err)
	}
	if got := r.policies(t); len(got) != 1 || got[0].Namespace != "b" {
		t.Errorf("after the delete the store holds %+v, want b/claude alone", got)
	}
	if code, body := r.control(t, http.MethodGet, "/policies", ""); code != http.StatusOK ||
		!strings.Contains(body, `"policies":[{`) {
		t.Errorf("list = %d %s", code, body)
	}
}

// The digest is of the policy and not of its spelling: the same document
// with other spacing and key order stores as the same content, and again
// under the same version changes nothing.
func TestTheSamePolicyAgainIsANoOp(t *testing.T) {
	r := startBroker(t, shortDir(t), nil, nil)
	ref := injector.PolicyRef{Namespace: "sandboxes", Name: "claude"}
	first := r.putPolicy(t, ref, "1", claudePolicy("claude", false, "claude-sonnet-4-5"))
	var spaced map[string]any
	if err := json.Unmarshal(claudePolicy("claude", false, "claude-sonnet-4-5"), &spaced); err != nil {
		t.Fatal(err)
	}
	respelled, err := json.MarshalIndent(spaced, "", "    ")
	if err != nil {
		t.Fatal(err)
	}
	again := r.putPolicy(t, ref, "1", respelled)
	if again.Digest != first.Digest || again.Version != "1" {
		t.Errorf("the same policy again = %+v, want %+v", again, first)
	}
	other := r.putPolicy(t, ref, "2", claudePolicy("claude", false, "claude-haiku-*"))
	if other.Digest == first.Digest {
		t.Error("another policy has the same digest")
	}
	wantRefused(t, func() error {
		_, err := r.client.PutPolicy(injector.PolicyRef{Namespace: "sandboxes", Name: "claude"},
			injector.PutPolicyRequest{Version: "3", Policy: json.RawMessage(`{"kind":"Policy"}`)})
		return err
	}(), http.StatusBadRequest, "a put of a broken policy")
}

// sendModel sends one Messages request for model to a route of the running
// broker, with token in the claude-code carrier.
func (r *running) sendModel(t *testing.T, sandbox, token, model string) int {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/%s/r/anthropic/v1/messages", r.srv.port, sandbox)
	body := `{"model":"` + model + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// wantAudit waits for the last line of the audit log to hold every one of
// want. The handler writes the line after the response is sent, so it can
// land after the client has the answer.
func (r *running) wantAudit(t *testing.T, want ...string) {
	t.Helper()
	var last string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		blob, err := os.ReadFile(filepath.Join(r.dir, injector.AuditName))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(blob)), "\n")
		last = lines[len(lines)-1]
		if !slices.ContainsFunc(want, func(w string) bool { return !strings.Contains(last, w) }) {
			return
		}
	}
	t.Errorf("the last audit line is %s, want %q", last, want)
}

// byRef is a registration of sandbox id by reference to ref.
func byRef(id string, ref injector.PolicyRef) injector.Sandbox {
	return injector.Sandbox{ID: id, Profile: "claude-code", PolicyRef: &ref}
}

var sandboxesClaude = injector.PolicyRef{Namespace: "sandboxes", Name: "claude"}

// A registration by reference to a policy the broker does not hold fails
// closed with 424, and registers nothing. A policy stored under another
// namespace is another policy: a pod cannot reach it by naming its name.
func TestARegistrationByAPolicyThatIsNotStoredFailsClosed(t *testing.T) {
	u := newUpstream(t)
	r := startBroker(t, shortDir(t), u, nil)
	_, err := r.client.Add(byRef("pod-a", sandboxesClaude))
	var refused *injector.RefusedError
	if !errors.As(err, &refused) || refused.Status != http.StatusFailedDependency ||
		refused.Kind != injector.RefusalPolicy || !strings.Contains(refused.Message, "policy sandboxes/claude is not loaded") {
		t.Fatalf("a registration by a policy nobody stored = %v, want a 424 policy refusal", err)
	}
	r.putPolicy(t, injector.PolicyRef{Namespace: "team-a", Name: "claude"}, "1", claudePolicy("claude", false, "claude-*"))
	_, err = r.client.Add(byRef("pod-b", injector.PolicyRef{Namespace: "team-b", Name: "claude"}))
	wantRefused(t, err, http.StatusFailedDependency, "a registration in team-b by the name of team-a's policy")
	st, err := r.client.Status()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Sandboxes) != 0 {
		t.Errorf("refused registrations left sandboxes %v", st.Sandboxes)
	}
	if got := r.send(t, "pod-b", "anthropic", "", "/v1/messages"); got != http.StatusNotFound {
		t.Errorf("a request for the refused sandbox got %d, want 404", got)
	}
	if n := len(u.requests()); n != 0 {
		t.Errorf("the upstream saw %d requests", n)
	}
}

// A registration names its routes or a policy. Both at once is refused, and
// so is a reference that is not a namespace and a name.
func TestAPolicyRefWithRoutesIsRefused(t *testing.T) {
	r := startBroker(t, shortDir(t), nil, nil)
	r.putPolicy(t, sandboxesClaude, "1", claudePolicy("claude", false, "claude-*"))
	both := byRef("pod-a", sandboxesClaude)
	both.Routes = []injector.Route{githubRoute()}
	_, err := r.client.Add(both)
	wantRefused(t, err, http.StatusBadRequest, "a registration with a policyRef and routes")
	for _, ref := range []injector.PolicyRef{{Namespace: "", Name: "claude"}, {Namespace: "sandboxes", Name: ""},
		{Namespace: "Sandboxes", Name: "claude"}, {Namespace: "sandboxes", Name: "../claude"}} {
		_, err := r.client.Add(byRef("pod-a", ref))
		wantRefused(t, err, http.StatusBadRequest, fmt.Sprintf("a registration by %+v", ref))
	}
	if code, msg := r.control(t, http.MethodPost, "/sandboxes",
		`{"id":"pod-a","profile":"x","policyRef":{"namespace":"sandboxes","name":"claude","extra":1}}`); code != http.StatusBadRequest {
		t.Errorf("a policyRef with an unknown field got %d %s, want 400", code, msg)
	}
	if st, err := r.client.Status(); err != nil || len(st.Sandboxes) != 0 {
		t.Errorf("status after refused registrations = %+v, %v", st, err)
	}
}

// A registration by reference gets every route of the stored policy, in its
// order, with the guest.env each one writes, and is served under its rules.
func TestARegistrationByReferenceGetsThePolicyRoutes(t *testing.T) {
	u := newUpstream(t)
	r := startBroker(t, shortDir(t), u, nil)
	r.putPolicy(t, sandboxesClaude, "1", claudePolicy("claude", true, "claude-sonnet-4-5"))
	code, body := r.control(t, http.MethodPost, "/sandboxes",
		`{"id":"pod-a","profile":"claude-code","policyRef":{"namespace":"sandboxes","name":"claude"}}`)
	if code != http.StatusOK || !strings.Contains(body, `"routes":[{"name":"anthropic","guestEnv":{`) {
		t.Fatalf("registration = %d %s, want the routes in the answer", code, body)
	}
	resp, err := r.client.Add(byRef("pod-a", sandboxesClaude))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Routes) != 2 || resp.Routes[0].Name != "anthropic" || resp.Routes[1].Name != "github" {
		t.Fatalf("routes = %+v, want anthropic then github", resp.Routes)
	}
	if got := resp.Routes[0].GuestEnv["ANTHROPIC_BASE_URL"]; got != "{route.url}" {
		t.Errorf("anthropic guestEnv = %v, want ANTHROPIC_BASE_URL {route.url} as the preset writes it", resp.Routes[0].GuestEnv)
	}
	tok := r.mint(t, "pod-a", "anthropic")
	if got := r.sendModel(t, "pod-a", tok, "claude-sonnet-4-5"); got != http.StatusOK {
		t.Errorf("an allowed model got %d", got)
	}
	if got := r.sendModel(t, "pod-a", tok, "claude-opus-4-1"); got != http.StatusForbidden {
		t.Errorf("a model the policy does not allow got %d, want 403", got)
	}
	if n := len(u.requests()); n != 1 {
		t.Errorf("the upstream saw %d requests, want the allowed one", n)
	}
	// The same registration again keeps the key, so the pod's token works.
	if _, err := r.client.Add(byRef("pod-a", sandboxesClaude)); err != nil {
		t.Fatal(err)
	}
	if got := r.sendModel(t, "pod-a", tok, "claude-sonnet-4-5"); got != http.StatusOK {
		t.Errorf("the token after the same registration again got %d", got)
	}
	if got := r.policies(t); len(got) != 1 || got[0].Sandboxes != 1 {
		t.Errorf("policies = %+v, want one sandbox on sandboxes/claude", got)
	}
}

// A rule a PUT tightens applies to the next request of a sandbox already
// registered with the policy, under the token it already holds.
func TestATightenedRuleAppliesToARunningSandbox(t *testing.T) {
	u := newUpstream(t)
	r := startBroker(t, shortDir(t), u, nil)
	r.putPolicy(t, sandboxesClaude, "1", claudePolicy("claude", false, "claude-*"))
	if _, err := r.client.Add(byRef("pod-a", sandboxesClaude)); err != nil {
		t.Fatal(err)
	}
	tok := r.mint(t, "pod-a", "anthropic")
	if got := r.sendModel(t, "pod-a", tok, "claude-opus-4-1"); got != http.StatusOK {
		t.Fatalf("a model version 1 allows got %d", got)
	}
	put := r.putPolicy(t, sandboxesClaude, "2", claudePolicy("claude", false, "claude-sonnet-4-5"))
	if put.Sandboxes != 1 || len(put.PendingRoutes) != 0 {
		t.Errorf("put = %+v, want one sandbox and nothing pending", put)
	}
	if got := r.sendModel(t, "pod-a", tok, "claude-opus-4-1"); got != http.StatusForbidden {
		t.Errorf("a model version 2 no longer allows got %d, want 403", got)
	}
	r.wantAudit(t, "status=403", "check=body", "request.body.models")
	if n := len(u.requests()); n != 1 {
		t.Errorf("the upstream saw %d requests, want the one before the update", n)
	}
	if got := r.sendModel(t, "pod-a", tok, "claude-sonnet-4-5"); got != http.StatusOK {
		t.Errorf("a model version 2 allows got %d under the token from before", got)
	}
	if st, err := r.client.Status(); err != nil || st.Policies["sandboxes/claude"] != "2" {
		t.Errorf("status = %+v, %v, want sandboxes/claude at version 2", st, err)
	}
}

// A route a PUT removes answers 404 on the next request of a running
// sandbox, check=route, and no token is minted for it again. The route that
// stays keeps its token.
func TestARouteRemovedByAPutStopsServing(t *testing.T) {
	u := newUpstream(t)
	r := startBroker(t, shortDir(t), u, nil)
	r.putPolicy(t, sandboxesClaude, "1", claudePolicy("claude", true, "claude-*"))
	if _, err := r.client.Add(byRef("pod-a", sandboxesClaude)); err != nil {
		t.Fatal(err)
	}
	gh := r.mint(t, "pod-a", "github")
	claude := r.mint(t, "pod-a", "anthropic")
	r.putPolicy(t, sandboxesClaude, "2", claudePolicy("claude", false, "claude-*"))
	if got := r.send(t, "pod-a", "github", gh, "/brig-sh/brig.git/info/refs"); got != http.StatusNotFound {
		t.Errorf("the removed route got %d, want 404", got)
	}
	r.wantAudit(t, "status=404", "check=route")
	if n := len(u.requests()); n != 0 {
		t.Errorf("the upstream saw %d requests for a removed route", n)
	}
	_, err := r.client.Mint(injector.MintRequest{Sandbox: "pod-a", Route: "github"})
	wantRefused(t, err, http.StatusNotFound, "a mint for the removed route")
	if got := r.sendModel(t, "pod-a", claude, "claude-sonnet-4-5"); got != http.StatusOK {
		t.Errorf("the route that stays got %d under its token", got)
	}
	// Stored again with the route, the policy does not give it back to the
	// running sandbox, whose pod was created without it.
	put := r.putPolicy(t, sandboxesClaude, "3", claudePolicy("claude", true, "claude-*"))
	if len(put.PendingRoutes) != 1 || put.PendingRoutes[0] != "github" {
		t.Errorf("pendingRoutes = %v, want [github]", put.PendingRoutes)
	}
	if got := r.send(t, "pod-a", "github", gh, "/brig-sh/brig.git/info/refs"); got != http.StatusNotFound {
		t.Errorf("the route stored again got %d for the running sandbox, want 404", got)
	}
}

// A route a PUT adds is reported pending and is not added to a running
// sandbox. A pod registered after the PUT gets it.
func TestANewRouteIsPendingForARunningSandbox(t *testing.T) {
	r := startBroker(t, shortDir(t), newUpstream(t), nil)
	r.putPolicy(t, sandboxesClaude, "1", claudePolicy("claude", false, "claude-*"))
	if _, err := r.client.Add(byRef("pod-a", sandboxesClaude)); err != nil {
		t.Fatal(err)
	}
	put := r.putPolicy(t, sandboxesClaude, "2", claudePolicy("claude", true, "claude-*"))
	if put.Sandboxes != 1 || len(put.PendingRoutes) != 1 || put.PendingRoutes[0] != "github" {
		t.Errorf("put = %+v, want one sandbox and github pending", put)
	}
	_, err := r.client.Mint(injector.MintRequest{Sandbox: "pod-a", Route: "github"})
	wantRefused(t, err, http.StatusNotFound, "a mint of the new route for the running sandbox")
	again, err := r.client.Add(byRef("pod-a", sandboxesClaude))
	if err != nil || len(again.Routes) != 1 || again.Routes[0].Name != "anthropic" {
		t.Errorf("the same registration again = %+v, %v, want anthropic alone", again.Routes, err)
	}
	fresh, err := r.client.Add(byRef("pod-b", sandboxesClaude))
	if err != nil || len(fresh.Routes) != 2 {
		t.Fatalf("a new pod = %+v, %v, want both routes", fresh.Routes, err)
	}
	r.mint(t, "pod-b", "github")
}

// A deleted policy leaves every sandbox registered with it with no route: it
// fails closed. The policy stored again gives it nothing back.
func TestADeletedPolicyLeavesTheSandboxWithNoRoutes(t *testing.T) {
	u := newUpstream(t)
	r := startBroker(t, shortDir(t), u, nil)
	r.putPolicy(t, sandboxesClaude, "1", claudePolicy("claude", true, "claude-*"))
	if _, err := r.client.Add(byRef("pod-a", sandboxesClaude)); err != nil {
		t.Fatal(err)
	}
	claude := r.mint(t, "pod-a", "anthropic")
	del, err := r.client.DeletePolicy(sandboxesClaude)
	if err != nil || !del.Removed || del.Sandboxes != 1 {
		t.Fatalf("delete = %+v, %v, want removed with one sandbox", del, err)
	}
	if got := r.sendModel(t, "pod-a", claude, "claude-sonnet-4-5"); got != http.StatusNotFound {
		t.Errorf("a request after the delete got %d, want 404", got)
	}
	for _, route := range []string{"anthropic", "github"} {
		_, err := r.client.Mint(injector.MintRequest{Sandbox: "pod-a", Route: route})
		wantRefused(t, err, http.StatusNotFound, "a mint of "+route+" after the delete")
	}
	_, err = r.client.Add(byRef("pod-b", sandboxesClaude))
	wantRefused(t, err, http.StatusFailedDependency, "a registration after the delete")
	put := r.putPolicy(t, sandboxesClaude, "2", claudePolicy("claude", true, "claude-*"))
	if put.Sandboxes != 1 || len(put.PendingRoutes) != 2 {
		t.Errorf("put after the delete = %+v, want pod-a with both routes pending", put)
	}
	if got := r.sendModel(t, "pod-a", claude, "claude-sonnet-4-5"); got != http.StatusNotFound {
		t.Errorf("a request after the policy came back got %d, want 404", got)
	}
	if n := len(u.requests()); n != 0 {
		t.Errorf("the upstream saw %d requests", n)
	}
}

// A PUT that does not load leaves the running sandbox under the policy
// stored before it.
func TestAnInvalidPutKeepsTheOldPolicyServing(t *testing.T) {
	r := startBroker(t, shortDir(t), newUpstream(t), nil)
	r.putPolicy(t, sandboxesClaude, "1", claudePolicy("claude", false, "claude-sonnet-4-5"))
	if _, err := r.client.Add(byRef("pod-a", sandboxesClaude)); err != nil {
		t.Fatal(err)
	}
	tok := r.mint(t, "pod-a", "anthropic")
	broken := strings.Replace(string(claudePolicy("claude", false, "claude-*")), `"models"`, `"model"`, 1)
	_, err := r.client.PutPolicy(sandboxesClaude, injector.PutPolicyRequest{Version: "2", Policy: json.RawMessage(broken)})
	wantRefused(t, err, http.StatusBadRequest, "a put with an unknown field in request.body")
	if got := r.sendModel(t, "pod-a", tok, "claude-sonnet-4-5"); got != http.StatusOK {
		t.Errorf("a model version 1 allows got %d after the refused put", got)
	}
	if got := r.sendModel(t, "pod-a", tok, "claude-opus-4-1"); got != http.StatusForbidden {
		t.Errorf("a model version 1 refuses got %d after the refused put, want 403", got)
	}
	if st, err := r.client.Status(); err != nil || st.Policies["sandboxes/claude"] != "1" {
		t.Errorf("status = %+v, %v, want version 1", st, err)
	}
}

// A PUT that a registered sandbox cannot take is refused with 409, and the
// policy and the sandbox stay as they were. A loopback sandbox has nothing
// to bind a passthrough request to.
func TestAPutARunningSandboxCannotTakeIsRefused(t *testing.T) {
	r := startBroker(t, shortDir(t), newUpstream(t), nil)
	first := r.putPolicy(t, sandboxesClaude, "1", claudePolicy("claude", false, "claude-*"))
	if _, err := r.client.Add(byRef("pod-a", sandboxesClaude)); err != nil {
		t.Fatal(err)
	}
	tok := r.mint(t, "pod-a", "anthropic")
	passthrough := strings.Replace(string(claudePolicy("claude", false, "claude-*")),
		`"credential":{"secretRef":{"name":"claude-credentials"}}`, `"passthrough":true`, 1)
	_, err := r.client.PutPolicy(sandboxesClaude, injector.PutPolicyRequest{Version: "2", Policy: json.RawMessage(passthrough)})
	wantRefused(t, err, http.StatusConflict, "a passthrough route for a loopback sandbox")
	if got := r.policies(t); len(got) != 1 || got[0].Version != "1" || got[0].Digest != first.Digest {
		t.Errorf("after the refusal the store holds %+v", got)
	}
	if got := r.sendModel(t, "pod-a", tok, "claude-opus-4-1"); got != http.StatusOK {
		t.Errorf("the sandbox got %d after the refused put, want its old rules", got)
	}
}

// The same version and digest again changes nothing for a running sandbox:
// its token works and the answer is the same.
func TestTheSamePolicyAgainKeepsARunningSandbox(t *testing.T) {
	r := startBroker(t, shortDir(t), newUpstream(t), nil)
	first := r.putPolicy(t, sandboxesClaude, "1", claudePolicy("claude", false, "claude-*"))
	if _, err := r.client.Add(byRef("pod-a", sandboxesClaude)); err != nil {
		t.Fatal(err)
	}
	tok := r.mint(t, "pod-a", "anthropic")
	again := r.putPolicy(t, sandboxesClaude, "1", claudePolicy("claude", false, "claude-*"))
	if again.Version != first.Version || again.Digest != first.Digest || again.Sandboxes != 1 || len(again.PendingRoutes) != 0 {
		t.Errorf("the same policy again = %+v, want %+v with one sandbox", again, first)
	}
	if got := r.sendModel(t, "pod-a", tok, "claude-sonnet-4-5"); got != http.StatusOK {
		t.Errorf("the token after the same policy again got %d", got)
	}
}

// Policy updates, registrations by reference and requests at once. Every
// request is refused by both versions for different reasons: version a
// allows its model and denies its tool, version b allows the tool and
// denies the model. A request that saw the model rule of one version and
// the tool rule of the other would reach the upstream. Run with -race.
func TestPolicyUpdatesRaceRequestsAndRegistrations(t *testing.T) {
	u := newUpstream(t)
	r := startBroker(t, shortDir(t), u, nil)
	version := func(model, deny string) json.RawMessage {
		doc := strings.Replace(string(claudePolicy("claude", false, model)), `"models":`,
			`"tools":{"deny":["`+deny+`"]},"models":`, 1)
		return json.RawMessage(doc)
	}
	a, b := version("claude-opus-4-1", "Bash"), version("claude-sonnet-4-5", "Read")
	r.putPolicy(t, sandboxesClaude, "a", a)
	if _, err := r.client.Add(byRef("pod-a", sandboxesClaude)); err != nil {
		t.Fatal(err)
	}
	tok := r.mint(t, "pod-a", "anthropic")
	url := fmt.Sprintf("http://127.0.0.1:%d/pod-a/r/anthropic/v1/messages", r.srv.port)
	body := `{"model":"claude-opus-4-1","max_tokens":16,"tools":[{"name":"Bash","input_schema":{}}],` +
		`"messages":[{"role":"user","content":"hi"}]}`

	var wg sync.WaitGroup
	codes := make(chan int, 160)
	for range 4 {
		wg.Go(func() {
			for range 40 {
				req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
				if err != nil {
					t.Error(err)
					return
				}
				req.Header.Set("Authorization", "Bearer "+tok)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				codes <- resp.StatusCode
			}
		})
	}
	wg.Go(func() {
		for i := range 40 {
			doc, v := a, "a"
			if i%2 == 0 {
				doc, v = b, "b"
			}
			if _, err := r.client.PutPolicy(sandboxesClaude, injector.PutPolicyRequest{Version: v, Policy: doc}); err != nil {
				t.Error(err)
				return
			}
		}
	})
	wg.Go(func() {
		for i := range 20 {
			id := fmt.Sprintf("pod-%d", i)
			if _, err := r.client.Add(byRef(id, sandboxesClaude)); err != nil {
				t.Error(err)
				return
			}
			if _, err := r.client.RemoveSandbox(id); err != nil {
				t.Error(err)
				return
			}
		}
	})
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusForbidden {
			t.Errorf("a request during the updates got %d, want 403 from either version", code)
		}
	}
	if n := len(u.requests()); n != 0 {
		t.Errorf("the upstream saw %d requests that both versions refuse", n)
	}
}
