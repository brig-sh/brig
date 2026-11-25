package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/broker"
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

// binding is a sandbox registered by reference to a stored policy.
type binding struct {
	// key is the policy's policyKey.
	key string
	// routes are the names of the routes the sandbox has. An update of the
	// policy keeps a name or drops it, and never adds one: the token and
	// the environment of a route are written into a pod when it is created.
	routes []string
}

// brokerRoutes returns every route of the policy to register, in its order.
func (sp *storedPolicy) brokerRoutes() []broker.Route {
	out := make([]broker.Route, 0, len(sp.routes))
	for _, rt := range sp.routes {
		out = append(out, broker.Route{Policy: sp.ref.Name, Route: rt})
	}
	return out
}

// keptRoutes returns the routes of the policy that names lists, in its order.
func (sp *storedPolicy) keptRoutes(names []string) []broker.Route {
	var out []broker.Route
	for _, rt := range sp.routes {
		if slices.Contains(names, rt.Name) {
			out = append(out, broker.Route{Policy: sp.ref.Name, Route: rt})
		}
	}
	return out
}

// routeNames returns the names of the routes of the policy, in its order.
func (sp *storedPolicy) routeNames() []string {
	names := make([]string, 0, len(sp.routes))
	for _, rt := range sp.routes {
		names = append(names, rt.Name)
	}
	return names
}

// boundRoutes returns the routes sandbox id has of the policy it registered
// with, in the policy's order, with their guest.env. It is nil for a
// sandbox registered with its routes. s.mu is held.
func (s *server) boundRoutes(id string) []injector.RouteEnv {
	b := s.bound[id]
	if b == nil {
		return nil
	}
	out := []injector.RouteEnv{}
	sp := s.policies[b.key]
	if sp == nil {
		return out
	}
	for _, rt := range sp.routes {
		if !slices.Contains(b.routes, rt.Name) {
			continue
		}
		env := map[string]string{}
		if rt.Guest != nil {
			maps.Copy(env, rt.Guest.Env)
		}
		out = append(out, injector.RouteEnv{Name: rt.Name, GuestEnv: env})
	}
	return out
}

// boundTo returns the ids of the sandboxes registered with the policy key,
// sorted. s.mu is held.
func (s *server) boundTo(key string) []string {
	var ids []string
	for id, b := range s.bound {
		if b.key == key {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// pendingRoutes returns the routes of sp that a sandbox registered with it
// does not have, in the policy's order. s.mu is held.
func (s *server) pendingRoutes(ids []string, sp *storedPolicy) []string {
	pending := []string{}
	for _, rt := range sp.routes {
		for _, id := range ids {
			if !slices.Contains(s.bound[id].routes, rt.Name) {
				pending = append(pending, rt.Name)
				break
			}
		}
	}
	return pending
}

// rebind moves the sandboxes ids, registered with a reference to the policy
// that sp replaces, to sp. Each keeps the routes of sp it already has, with
// the rules sp gives them, and loses the others. Every sandbox is checked
// before any moves, so a refusal leaves all of them as they were. s.mu is
// held, and so is polMu.
//
// The checks are those of a registration that need no host CLI. A route
// that vends is not in a stored policy. A gcp route whose secret turns out to
// be a gcloud pointer on a node is refused at its request, as it is when a
// mounted Secret changes under a registered sandbox.
func (s *server) rebind(ids []string, sp *storedPolicy) error {
	moves := make([][]broker.Route, len(ids))
	for i, id := range ids {
		routes := sp.keptRoutes(s.bound[id].routes)
		var internal netip.Addr
		if l := s.diverts[id]; l != nil {
			internal = l.internal
		}
		if err := refuseUnboundPassthrough(broker.Sandbox{ID: id, Routes: routes}, s.addrs[id], internal); err != nil {
			return fmt.Errorf("sandbox %s: %w", id, err)
		}
		if err := s.handler.CheckRoutes(id, routes); err != nil {
			return err
		}
		moves[i] = routes
	}
	for i, id := range ids {
		for _, rt := range moves[i] {
			if c := rt.Route.Credential; c != nil {
				// parsePolicy checked the lifecycle, so track does not fail.
				if created, err := s.creds.track(c.SecretRef.Name, c.Lifecycle); err == nil && created {
					s.creds.recover(c.SecretRef.Name)
				}
			}
		}
		if err := s.handler.SetRoutes(id, moves[i]); err != nil {
			// CheckRoutes took the same routes under the same lock, so this
			// does not happen. A sandbox that cannot take the policy's rules
			// does not keep the ones the policy dropped.
			s.logger.Printf("sandbox %s: cannot move to policy %s, and it is removed: %v", id, policyKey(sp.ref), err)
			s.forget(id)
			continue
		}
		names := make([]string, 0, len(moves[i]))
		for _, rt := range moves[i] {
			names = append(names, rt.Route.Name)
		}
		s.bound[id].routes = names
	}
	return nil
}

// putPolicy stores a policy under namespace/name, and moves every sandbox
// registered with it to it at once. A policy that does not load is refused
// with 400, and one that a registered sandbox cannot take with 409. Either
// way the policy stored before stays, and so do the sandboxes. The same
// content again changes nothing.
func (s *server) putPolicy(w http.ResponseWriter, r *http.Request) {
	ref, ok := policyRef(w, r)
	if !ok {
		return
	}
	var req injector.PutPolicyRequest
	if !decode(w, r, &req) {
		return
	}
	key := policyKey(ref)
	sp, err := s.parsePolicy(ref, req)
	if err != nil {
		http.Error(w, fmt.Sprintf("policy %s: %v", key, err), http.StatusBadRequest)
		return
	}
	s.polMu.Lock()
	defer s.polMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refuseStopping(w) {
		return
	}
	if other := s.otherPolicy(key); other != "" {
		http.Error(w, fmt.Sprintf("policy %s: this broker serves sandbox %s alone and holds policy %s", key,
			s.cfg.sandbox, other), http.StatusConflict)
		return
	}
	ids := s.boundTo(key)
	// The same content under another version moves nobody: the rules are
	// the ones the sandboxes already have.
	if old := s.policies[key]; old == nil || old.digest != sp.digest {
		if err := s.rebind(ids, sp); err != nil {
			http.Error(w, fmt.Sprintf("policy %s: %v", key, err), http.StatusConflict)
			return
		}
		s.logger.Printf("policy %s: stored version %s, digest %s, for %d sandboxes", key, sp.version, sp.digest, len(ids))
	}
	s.policies[key] = sp
	ids = s.boundTo(key)
	reply(w, injector.PutPolicyResponse{Version: sp.version, Digest: sp.digest, Sandboxes: len(ids),
		PendingRoutes: s.pendingRoutes(ids, sp)})
}

// deletePolicy removes the policy stored under namespace/name. Every sandbox
// registered with it loses all its routes and stays registered with none, so
// it fails closed until it is removed. A policy stored again under the name
// gives such a sandbox nothing back: every route of it is a new one.
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
	ids := s.boundTo(key)
	for _, id := range ids {
		if err := s.handler.SetRoutes(id, nil); err != nil {
			s.logger.Printf("sandbox %s: cannot drop the routes of policy %s, and it is removed: %v", id, key, err)
			s.forget(id)
			continue
		}
		s.bound[id].routes = nil
	}
	if removed || len(ids) > 0 {
		s.logger.Printf("policy %s: removed, and %d sandboxes lost its routes", key, len(ids))
	}
	reply(w, injector.DeletePolicyResponse{Removed: removed, Sandboxes: len(ids)})
}

// listPolicies answers with every stored policy, sorted by key.
func (s *server) listPolicies(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := injector.PoliciesResponse{Policies: []injector.PolicyInfo{}}
	for _, key := range slices.Sorted(maps.Keys(s.policies)) {
		sp := s.policies[key]
		out.Policies = append(out.Policies, injector.PolicyInfo{Namespace: sp.ref.Namespace, Name: sp.ref.Name,
			Version: sp.version, Digest: sp.digest, Sandboxes: len(s.boundTo(key))})
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

// otherPolicy returns the key of a stored policy other than key, when the
// broker serves one sandbox, and "" otherwise. Such a broker holds the policy
// of its sandbox and no other, so a broker that leaks holds no other
// sandbox's rules. Called with mu held.
func (s *server) otherPolicy(key string) string {
	if s.cfg.sandbox == "" {
		return ""
	}
	for k := range s.policies {
		if k != key {
			return k
		}
	}
	return ""
}
