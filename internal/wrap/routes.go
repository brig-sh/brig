package wrap

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/profile"
)

// routeServer registers a sandbox's routes and mints the session tokens of
// one exec. brig-broker is the one a run uses; a test passes its own.
type routeServer interface {
	// Register registers s, starting the broker when none is running, and
	// returns the loopback port the broker serves routes on.
	Register(s injector.Sandbox) (int, error)
	// Mint returns a session token for one route of a registered sandbox.
	Mint(r injector.MintRequest) (string, error)
	// Forget removes a sandbox, which refuses its tokens from then on, and
	// stops the broker when no sandbox is left. It starts nothing.
	Forget(id string) error
	// Status is what a running broker reports, and false when none answers.
	// A broker whose process is alive and does not answer reports its PID
	// alone, with false. It starts nothing.
	Status() (injector.Status, bool)
}

// brokerProcess is the routeServer of a run: brig-broker, in the broker
// directory under ~/.brig/host.
type brokerProcess struct {
	client *injector.Client
}

func (b *brokerProcess) Register(s injector.Sandbox) (int, error) {
	dir, err := injector.Dir()
	if err != nil {
		return 0, err
	}
	running, err := injector.Register(dir, s)
	if err != nil {
		return 0, err
	}
	b.client = &running.Client
	return running.Status.Port, nil
}

func (b *brokerProcess) Mint(r injector.MintRequest) (string, error) {
	if b.client == nil {
		return "", injector.ErrNotRunning
	}
	return b.client.Mint(r)
}

func (b *brokerProcess) Forget(id string) error {
	dir, err := injector.Dir()
	if err != nil {
		return err
	}
	return injector.Forget(dir, id)
}

func (b *brokerProcess) Status() (injector.Status, bool) {
	dir, err := injector.Dir()
	if err != nil {
		return injector.Status{}, false
	}
	running, ok := injector.Running(dir)
	if !ok {
		pid, _ := injector.LivePID(dir)
		return injector.Status{PID: pid}, false
	}
	return running.Status, true
}

// routeBroker is c.broker, or brig-broker.
func (c *Config) routeBroker() routeServer {
	if c.broker == nil {
		c.broker = &brokerProcess{}
	}
	return c.broker
}

// routesOf merges the routes of the bound policies, and names the policy that
// declares each.
func routesOf(bound []policy.Policy) ([]v1alpha1.Route, map[string]string, error) {
	routes, err := policy.Routes(bound)
	if err != nil {
		return nil, nil, err
	}
	owner := map[string]string{}
	for _, p := range bound {
		for _, r := range p.Spec.Routes {
			owner[r.Name] = p.Metadata.Name
		}
	}
	return routes, owner, nil
}

// routeRefusal is why this run cannot serve its routes, or nil. A microVM or
// a container reaches the broker at brig.internal, and no runtime routes that
// name from a guest to the host yet (#265), so a run with a route is refused
// rather than booted without the routes its policy names.
//
// Read-only, like sessionRefusal, which returns it.
func (c *Config) routeRefusal() error {
	if c.routesErr != nil {
		return fmt.Errorf("the policies bound to %s cannot apply together: %w", c.Profile.Name, c.routesErr)
	}
	if len(c.routes) == 0 || c.Runtime == nil {
		return nil
	}
	// TODO(#265): route brig.internal from the guest to the broker on hull's
	// network gateway and on the nerdctl bridge, deliver the brig CA as
	// /brig/ca/brig.pem, and lift this refusal.
	return fmt.Errorf("policy %s declares routes, and brig.internal routing is not available on this "+
		"runtime (%s) yet. Refusing the run rather than booting without them. Detach the policy",
		c.routeList(), c.Runtime.Kind())
}

// routeList names each policy with its routes, the way a refusal does.
func (c *Config) routeList() string {
	byPolicy := map[string][]string{}
	var order []string
	for _, r := range c.routes {
		p := c.routePolicy[r.Name]
		if _, ok := byPolicy[p]; !ok {
			order = append(order, p)
		}
		byPolicy[p] = append(byPolicy[p], r.Name)
	}
	var out []string
	for _, p := range order {
		out = append(out, fmt.Sprintf("%s (route %s)", p, strings.Join(byPolicy[p], ", ")))
	}
	return strings.Join(out, ", ")
}

// serveRoutes registers this sandbox's routes with the broker, mints a
// session token per route for this exec, and records what the guest is
// handed. Called on every exec: a token is minted per exec, and the same
// registration again keeps the tokens of an agent already running valid.
func (c *Config) serveRoutes(set creds.Set) error {
	if len(c.routes) == 0 {
		return nil
	}
	b := c.routeBroker()
	s := injector.Sandbox{ID: c.VMName, Profile: c.Profile.Name}
	for _, r := range c.routes {
		s.Routes = append(s.Routes, injector.Route{Policy: c.routePolicy[r.Name], Route: r})
	}
	if _, err := b.Register(s); err != nil {
		return fmt.Errorf("could not register the routes of %s with the credential broker: %w", c.VMName, err)
	}
	tokens := map[string]string{}
	for _, r := range c.routes {
		token, err := b.Mint(injector.MintRequest{
			Sandbox:    c.VMName,
			Session:    c.RawName,
			Route:      r.Name,
			TTLSeconds: int64(injector.TokenTTL / time.Second),
		})
		if err != nil {
			return fmt.Errorf("could not mint a session token for route %s: %w", r.Name, err)
		}
		tokens[r.Name] = token
	}
	ep := injector.HostEndpoint{Transport: injector.TransportBrigInternal}
	vars, err := ep.GuestEnv(c.VMName, c.routes, tokens, set.Vars)
	if err != nil {
		return err
	}
	c.routeVars = vars
	return nil
}

// forgetRoutes revokes this sandbox's session tokens: the broker stops
// serving it, and stops itself when no sandbox is left. A failure is said
// and does not fail the stop.
func (c *Config) forgetRoutes() {
	if err := c.routeBroker().Forget(c.VMName); err != nil {
		c.warnf("the credential broker could not revoke the session tokens of %s: %v", c.VMName, err)
	}
}

// RevokeRoutes removes the sandbox vmName from the credential broker, which
// refuses its session tokens from the next request on, and stops the broker
// when no sandbox is left. It starts nothing. brig rm --all and brig reset
// call it for each sandbox they remove, beside ForgetSandbox, because they
// remove sandboxes without a Config.
func RevokeRoutes(vmName string) error { return (&brokerProcess{}).Forget(vmName) }

// RevokeAllRoutes removes every sandbox the credential broker still has
// registered, and stops it. It returns the ids it removed. brig rm --all
// and brig reset call it after their own removals, for the registrations no
// runtime list reaches. It starts nothing.
func RevokeAllRoutes() ([]string, error) {
	dir, err := injector.Dir()
	if err != nil {
		return nil, err
	}
	return injector.ForgetAll(dir)
}

// brokeredSecrets are the secrets the routes inject. Each stays on the host,
// whatever the profile binds it to.
func brokeredSecrets(routes []v1alpha1.Route) map[string]string {
	out := map[string]string{}
	for _, r := range routes {
		if r.Credential != nil && r.Credential.SecretRef.Name != "" {
			out[r.Credential.SecretRef.Name] = r.Name
		}
	}
	return out
}

// namesBrokered reports whether a binding's chain names a brokered secret.
func namesBrokered(refs []string, brokered map[string]string) bool {
	for _, raw := range refs {
		r, err := profile.ParseRef(raw)
		if err == nil && r.Namespace == profile.NamespaceSecrets {
			if _, ok := brokered[r.Name]; ok {
				return true
			}
		}
	}
	return false
}

// withRoutes returns the profile and the env bindings a run with routes
// delivers. A secret a route injects is never delivered to the guest: a
// files: binding of it is dropped, an env binding whose chain names it is
// dropped whole, so an ambient value cannot stand in for it either, and it
// leaves the profile's secrets list, so brig does not read it. The broker
// reads it on the host. skipped names each secret a route injects, whether
// or not the profile delivers it.
func withRoutes(t profile.Profile, bindings []profile.EnvBinding, routes []v1alpha1.Route) (
	profile.Profile, []profile.EnvBinding, []string) {
	brokered := brokeredSecrets(routes)
	if len(brokered) == 0 {
		return t, bindings, nil
	}
	var files []profile.FileBinding
	for _, b := range t.Files {
		if namesBrokered([]string{b.Ref}, brokered) {
			continue
		}
		files = append(files, b)
	}
	keepBinding := func(in []profile.EnvBinding) []profile.EnvBinding {
		var out []profile.EnvBinding
		for _, b := range in {
			if namesBrokered(b.RefList(), brokered) {
				continue
			}
			out = append(out, b)
		}
		return out
	}
	var secrets []profile.SecretDecl
	for _, d := range t.Secrets {
		if _, ok := brokered[d.Name]; !ok {
			secrets = append(secrets, d)
		}
	}
	t.Files, t.Secrets = files, secrets
	t.Env = keepBinding(t.Env)
	bindings = keepBinding(bindings)
	return t, bindings, slices.Sorted(maps.Keys(brokered))
}
