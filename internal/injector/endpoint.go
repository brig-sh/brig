package injector

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/runtime"
)

// Transport is how a guest reaches the broker.
type Transport string

const (
	// TransportBrigInternal is TLS to brig.internal, with a brig CA that
	// signs only that name trusted in the guest. It serves a microVM or a
	// container, which reaches the host through a gateway.
	TransportBrigInternal Transport = "brig.internal"
)

// BrigInternal is the name a microVM guest reaches the broker by.
const BrigInternal = runtime.BrigInternalName

// BrigInternalPort is the port a guest reaches brig.internal on. The runtime
// diverts this port of the brig.internal address alone.
const BrigInternalPort = runtime.BrigInternalPort

// GuestCAPath is where a guest that reaches brig.internal finds the brig CA,
// and nothing else.
const GuestCAPath = "/brig/ca/brig.pem"

// GuestCABundlePath is where it finds the image's own roots followed by the
// brig CA, for a client that takes one file in place of its roots.
const GuestCABundlePath = "/brig/ca/bundle.pem"

// HostEndpoint is where the guests of one runtime reach the broker, and what
// a guest is handed to use a route.
type HostEndpoint struct {
	Transport Transport
}

// Base is the origin a route URL starts with.
func (e HostEndpoint) Base() string {
	return "https://" + BrigInternal
}

// URL is the URL of one route of one sandbox.
func (e HostEndpoint) URL(sandbox, route string) string {
	return e.Base() + "/" + sandbox + "/r/" + route
}

// shellQuote quotes s for sh as one word.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// gitConfigCount is the GIT_CONFIG_COUNT the guest already gets, or 0. The
// routes' entries are numbered after it, so they add to the guest's own
// instead of replacing them.
func gitConfigCount(forwarded []runtime.Var) (int, error) {
	count := ""
	for _, v := range forwarded {
		if v.Name == "GIT_CONFIG_COUNT" {
			count = v.Value
		}
	}
	if count == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(count)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("the guest is handed GIT_CONFIG_COUNT=%q, which is not a count, and a git route "+
			"adds its own entries after it", count)
	}
	return n, nil
}

// caEnv names the variables that point a guest's TLS clients at the brig CA,
// and the file each one names: OpenSSL and Go, Node, Python requests and
// git. Node adds NODE_EXTRA_CA_CERTS to its roots, so it gets the CA alone.
// The others read their one file in place of their roots, so they get the
// bundle, and every other site still verifies for them.
var caEnv = []string{"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "GIT_SSL_CAINFO"}

// caFile is the file the CA variable name points at.
func caFile(name string) string {
	if name == "NODE_EXTRA_CA_CERTS" {
		return GuestCAPath
	}
	return GuestCABundlePath
}

// TokenVar is the variable that holds the session token of a git route, which
// the credential helper answers with. - reads as _, so the name maps to one
// variable only for a route name v1alpha1.CheckRouteName accepts, and
// GuestEnv refuses any other.
func TokenVar(route string) string {
	return "BRIG_ROUTE_" + strings.ToUpper(strings.ReplaceAll(route, "-", "_")) + "_TOKEN"
}

// GuestEnv is what the guest is handed to use routes: each route's guest.env
// with {route.url} and {session.token} filled in, and for a route with
// guest.git, git configuration through GIT_CONFIG_COUNT that rewrites the
// route's prefixes to it and answers its credential prompt with the token.
//
// Nothing is written into the guest home. A session token is marked Secret,
// so it never goes on a command line. tokens maps a route name to its token.
// forwarded is what the guest already gets: git entries of its own through
// GIT_CONFIG_COUNT are kept, and the routes' entries follow them.
func (e HostEndpoint) GuestEnv(sandbox string, routes []v1alpha1.Route, tokens map[string]string,
	forwarded []runtime.Var) ([]runtime.Var, error) {
	var vars []runtime.Var
	var git [][2]string
	sorted := append([]v1alpha1.Route(nil), routes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, r := range sorted {
		// The name goes into a URL path and a shell variable name. The
		// policy grammar refuses one that maps to another route's
		// variable, or that ends the variable early for the shell, and so
		// does this.
		if err := v1alpha1.CheckRouteName(r.Name); err != nil {
			return nil, err
		}
		token, ok := tokens[r.Name]
		if !ok || token == "" {
			return nil, fmt.Errorf("no session token for route %s", r.Name)
		}
		url := e.URL(sandbox, r.Name)
		if r.Guest == nil {
			continue
		}
		names := make([]string, 0, len(r.Guest.Env))
		for name := range r.Guest.Env {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			switch v := r.Guest.Env[name]; v {
			case v1alpha1.PlaceholderRouteURL:
				vars = append(vars, runtime.Var{Name: name, Value: url})
			case v1alpha1.PlaceholderSessionToken:
				vars = append(vars, runtime.Var{Name: name, Value: token, Secret: true})
			default:
				// Validate refuses a literal; one here was not expanded.
				return nil, fmt.Errorf("route %s: guest.env %s is %q, not a placeholder", r.Name, name, v)
			}
		}
		if r.Guest.Git == nil || len(r.Guest.Git.InsteadOf) == 0 {
			continue
		}
		tokenVar := TokenVar(r.Name)
		vars = append(vars, runtime.Var{Name: tokenVar, Value: token, Secret: true})
		user := "x-access-token"
		if r.Inject != nil && r.Inject.BasicAuth != nil && r.Inject.BasicAuth.Username != "" {
			user = r.Inject.BasicAuth.Username
		}
		prefix := url + "/"
		for _, from := range r.Guest.Git.InsteadOf {
			git = append(git, [2]string{"url." + prefix + ".insteadOf", from})
		}
		// The empty helper resets the list for the route's URL, so a helper
		// from the host's own git config, such as osxkeychain, is not asked
		// first. The token is read from the environment when git asks, and
		// is never in the configuration.
		// The username comes from the policy, and the helper is a shell
		// function, so it is quoted.
		git = append(git,
			[2]string{"credential." + prefix + ".helper", ""},
			[2]string{"credential." + prefix + ".helper", "!f() { test \"$1\" = get || exit 0; " +
				"printf '%s\\n' " + shellQuote("username="+user) + " \"password=$" + tokenVar + "\"; }; f"})
	}
	if len(git) > 0 {
		base, err := gitConfigCount(forwarded)
		if err != nil {
			return nil, err
		}
		vars = append(vars, runtime.Var{Name: "GIT_CONFIG_COUNT", Value: strconv.Itoa(base + len(git))})
		for i, kv := range git {
			n := strconv.Itoa(base + i)
			vars = append(vars,
				runtime.Var{Name: "GIT_CONFIG_KEY_" + n, Value: kv[0]},
				runtime.Var{Name: "GIT_CONFIG_VALUE_" + n, Value: kv[1]})
		}
	}
	if e.Transport == TransportBrigInternal && len(routes) > 0 {
		for _, name := range caEnv {
			vars = append(vars, runtime.Var{Name: name, Value: caFile(name)})
		}
	}
	return vars, nil
}
