package wrap

import (
	"fmt"
	"strings"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/runtime"
)

// tlsProperty names inject.tls in a refusal.
const tlsProperty runtime.Property = "route inject.tls"

// TLSRefusal is why a run cannot serve the routes that set inject.tls, or nil
// when none does. The policy schema accepts inject.tls, but this build of
// brig does not yet present the client certificate or pin the CA, so a run
// would forward the request with no certificate and with the system roots
// trusted instead of ca. That is a route the policy asked to be different,
// served as if it were not. The refusal comes before boot, as a
// CapabilityError, so brig exits 7 like any policy a run cannot enforce.
//
// owner names the policy a route came from. path is the run path, or the
// zero value where no runtime is known (brig policy check names none).
func TLSRefusal(routes []v1alpha1.Route, owner func(route string) string, path runtime.RunPath) error {
	var names []string
	for _, r := range routes {
		if r.Inject != nil && r.Inject.TLS != nil {
			names = append(names, fmt.Sprintf("%s (route %s)", owner(r.Name), r.Name))
		}
	}
	if len(names) == 0 {
		return nil
	}
	if path == (runtime.RunPath{}) {
		path = runtime.RunPath{Runtime: "brig"}
	}
	err := &runtime.CapabilityError{Property: tlsProperty, Path: path, State: runtime.CannotEnforce,
		Why:    "inject.tls is not served by this build of brig",
		Remedy: "Remove inject.tls from the route, or detach the policy"}
	return fmt.Errorf("policy %s sets inject.tls: %w", strings.Join(names, ", "), err)
}

// tlsRefusal is TLSRefusal for this run's routes.
func (c *Config) tlsRefusal() error {
	var path runtime.RunPath
	if c.Runtime != nil {
		path = c.routePath()
	}
	return TLSRefusal(c.routes, func(route string) string { return c.routePolicy[route] }, path)
}
