package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/policy"
)

// storedPolicy is a policy stored on the control socket. A node's policy
// sync stores each Policy object of the cluster here, and a sandbox
// registers with a reference to one instead of with its routes.
type storedPolicy struct {
	ref     injector.PolicyRef
	version string
	digest  string
	// routes are the routes of the policy as policy.Routes expanded them,
	// in the order the policy lists them.
	routes []v1alpha1.Route
}

// key is how the broker keys the policy: "<namespace>/<name>". A namespace
// is a DNS label and a name has no slash, so two refs never share a key.
func policyKey(ref injector.PolicyRef) string { return ref.Namespace + "/" + ref.Name }

// maxNamespace is the longest Kubernetes namespace, a DNS label.
const maxNamespace = 63

// maxVersion bounds the version of a stored policy. It is the caller's own
// name, such as a Kubernetes generation, and status and the log carry it.
const maxVersion = 128

// checkPolicyRef refuses a ref whose namespace is not a DNS label, the form
// of a Kubernetes namespace, or whose name is not one a policy file may
// declare.
func checkPolicyRef(ref injector.PolicyRef) error {
	ns := ref.Namespace
	if ns == "" || len(ns) > maxNamespace || ns[0] == '-' || ns[len(ns)-1] == '-' ||
		strings.ContainsFunc(ns, func(c rune) bool { return c != '-' && (c < '0' || c > '9') && (c < 'a' || c > 'z') }) {
		return fmt.Errorf("the namespace %q is not lowercase letters, digits and dashes, at most %d of them, "+
			"starting and ending with a letter or digit", ns, maxNamespace)
	}
	if err := policy.CheckName(ref.Name); err != nil {
		return fmt.Errorf("policy %s: %w", ns, err)
	}
	return nil
}

// checkVersion refuses a version that is empty, longer than maxVersion, or
// holds anything but printable ASCII without spaces, since the log and
// status carry it as it is.
func checkVersion(v string) error {
	if v == "" || len(v) > maxVersion || strings.ContainsFunc(v, func(c rune) bool { return c <= ' ' || c > '~' }) {
		return fmt.Errorf("the version %q is not 1 to %d printable characters without spaces", v, maxVersion)
	}
	return nil
}

// parsePolicy checks a policy document the way a policy file is checked,
// with policy.Parse, and expands its routes. It refuses what a stored policy
// cannot hold. Nothing is stored here.
func (s *server) parsePolicy(ref injector.PolicyRef, req injector.PutPolicyRequest) (*storedPolicy, error) {
	if err := checkVersion(req.Version); err != nil {
		return nil, err
	}
	if len(req.Policy) == 0 {
		return nil, errors.New("the request has no policy")
	}
	p, err := policy.Parse(req.Policy)
	if err != nil {
		return nil, err
	}
	// The name in the document is the one a refusal of its routes names,
	// so it is the one the path stores it under.
	if p.Metadata.Name != ref.Name {
		return nil, fmt.Errorf("the policy is named %q, and the request stores it as %q", p.Metadata.Name, ref.Name)
	}
	routes, err := policy.Routes([]policy.Policy{p})
	if err != nil {
		return nil, err
	}
	for _, r := range routes {
		// A route that vends is checked with the host's aws CLI at every
		// registration, and the answer carries what the check found. An
		// update of a running sandbox cannot run that check, so a stored
		// policy does not take such a route.
		if r.Vends() {
			return nil, fmt.Errorf("route %s vends AWS keys through the host's aws CLI, and a stored policy "+
				"cannot hold a route that vends", r.Name)
		}
		// The lifecycle of a route's credential is refused here and not at
		// the first registration, so a policy that stored cleanly registers.
		if c := r.Credential; c != nil && c.Lifecycle != "" {
			if _, err := s.creds.renewerFor(c.Lifecycle); err != nil {
				return nil, fmt.Errorf("route %s: %w", r.Name, err)
			}
		}
	}
	canonical, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonical)
	return &storedPolicy{ref: ref, version: req.Version, digest: hex.EncodeToString(sum[:]), routes: routes}, nil
}

// policyRef reads the ref of a policy request from its path, and answers 400
// for one that is not valid.
func policyRef(w http.ResponseWriter, r *http.Request) (injector.PolicyRef, bool) {
	ref := injector.PolicyRef{Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}
	if err := checkPolicyRef(ref); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return ref, false
	}
	return ref, true
}

// putPolicy stores a policy under namespace/name. A policy that does not
// load is refused and the policy stored before stays. The same content again
// changes nothing.
func (s *server) putPolicy(w http.ResponseWriter, r *http.Request) {
	ref, ok := policyRef(w, r)
	if !ok {
		return
	}
	var req injector.PutPolicyRequest
	if !decode(w, r, &req) {
		return
	}
	sp, err := s.parsePolicy(ref, req)
	if err != nil {
		http.Error(w, fmt.Sprintf("policy %s: %v", policyKey(ref), err), http.StatusBadRequest)
		return
	}
	key := policyKey(ref)
	s.polMu.Lock()
	defer s.polMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refuseStopping(w) {
		return
	}
	old := s.policies[key]
	if old == nil || old.digest != sp.digest {
		s.logger.Printf("policy %s: stored version %s, digest %s", key, sp.version, sp.digest)
	}
	s.policies[key] = sp
	reply(w, injector.PutPolicyResponse{Version: sp.version, Digest: sp.digest, PendingRoutes: []string{}})
}

// deletePolicy removes the policy stored under namespace/name.
func (s *server) deletePolicy(w http.ResponseWriter, r *http.Request) {
	ref, ok := policyRef(w, r)
	if !ok {
		return
	}
	key := policyKey(ref)
	s.polMu.Lock()
	defer s.polMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	_, removed := s.policies[key]
	delete(s.policies, key)
	if removed {
		s.logger.Printf("policy %s: removed", key)
	}
	reply(w, injector.DeletePolicyResponse{Removed: removed})
}

// listPolicies answers with every stored policy, sorted by key.
func (s *server) listPolicies(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := injector.PoliciesResponse{Policies: []injector.PolicyInfo{}}
	for _, key := range slices.Sorted(maps.Keys(s.policies)) {
		sp := s.policies[key]
		out.Policies = append(out.Policies, injector.PolicyInfo{Namespace: sp.ref.Namespace, Name: sp.ref.Name,
			Version: sp.version, Digest: sp.digest})
	}
	reply(w, out)
}

// policyVersions maps each stored policy to its version, for status. s.mu is
// held.
func (s *server) policyVersions() map[string]string {
	if len(s.policies) == 0 {
		return nil
	}
	out := make(map[string]string, len(s.policies))
	for key, sp := range s.policies {
		out[key] = sp.version
	}
	return out
}
