package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

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
