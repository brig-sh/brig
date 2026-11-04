package broker

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// refusal is a request.allow miss. Its text is the body of the 403 and goes to
// the audit line.
type refusal struct {
	// field is the request.allow list that missed, such as "methods".
	field  string
	reason string
}

// allow checks a request against the route's request.allow, before any
// credential is fetched. rest is the path after the route prefix.
//
// It enforces the lists v1alpha1.RequestAllow defines: methods, paths and
// repos. Every list that is set must match. The models and tools lists of the
// spec read the request body, and the type does not carry them yet.
func allow(a *v1alpha1.RequestAllow, method, rest string) *refusal {
	if a == nil {
		return nil
	}
	if len(a.Methods) > 0 && !slices.Contains(a.Methods, method) {
		return &refusal{"methods", fmt.Sprintf("method %q is not in the allow list %s", method, list(a.Methods))}
	}
	if len(a.Paths) > 0 && !slices.ContainsFunc(a.Paths, func(g string) bool { return match(g, rest) }) {
		return &refusal{"paths", fmt.Sprintf("path %q is not in the allow list %s", rest, list(a.Paths))}
	}
	if len(a.Repos) > 0 {
		repo, ok := repoOf(rest)
		if !ok {
			return &refusal{"repos", fmt.Sprintf("path %q names no owner/name repository", rest)}
		}
		if !slices.ContainsFunc(a.Repos, func(g string) bool { return match(g, repo) }) {
			return &refusal{"repos", fmt.Sprintf("repo %q is not in the allow list %s", repo, list(a.Repos))}
		}
	}
	return nil
}

// match applies a path.Match glob, where * stops at a slash. A malformed glob
// matches nothing.
func match(glob, s string) bool {
	ok, err := path.Match(glob, s)
	return err == nil && ok
}

// repoOf returns the owner/name a git smart-HTTP path is for, such as
// brig-sh/brig for /brig-sh/brig.git/info/refs.
func repoOf(rest string) (string, bool) {
	parts := strings.SplitN(strings.TrimPrefix(rest, "/"), "/", 3)
	if len(parts) < 2 {
		return "", false
	}
	owner, name := parts[0], strings.TrimSuffix(parts[1], ".git")
	if owner == "" || name == "" {
		return "", false
	}
	return owner + "/" + name, true
}

func list(items []string) string {
	return "[" + strings.Join(items, ", ") + "]"
}

// refusalBody renders a refusal of rule as the 403 body.
func refusalBody(policy, route, rule, reason, sandbox string) string {
	return fmt.Sprintf("brig broker refused this request\n"+
		"policy:  %s\n"+
		"route:   %s\n"+
		"rule:    %s\n"+
		"reason:  %s\n"+
		"sandbox: %s\n", policy, route, rule, reason, sandbox)
}

// rule names the list that missed under owner, the spec path of the
// request.allow it belongs to: spec.routes[<route>], or
// spec.routes[<route>].services[<service>] for a service of a gcp route.
func (r *refusal) rule(owner string) string {
	return owner + ".request.allow." + r.field
}

// routeRule is the spec path of route.
func routeRule(route string) string {
	return "spec.routes[" + route + "]"
}
