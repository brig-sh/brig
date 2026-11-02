package wrap

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/secret"
)

// routeStore is what brig's secret store says about the secrets this run's
// routes inject. The broker reads them on the host on every request, so a
// secret the store does not hold, or a store that does not open, boots a
// sandbox whose routes answer 502 and whose agent retries for minutes.
type routeStore struct {
	// missing are the routed secrets the store does not list.
	missing []string
	// err is why the store could not be opened or listed.
	err error
	// certExpiry maps each clientCertificate secret to the expiry the store
	// recorded for it, in epoch milliseconds; 0 when none. It comes from the
	// same listing as missing: no value is read.
	certExpiry map[string]int64
}

// checkRouteStore lists the store for the secrets this run's routes inject,
// by name. The run never reads a routed secret's value: that is the
// broker's, on the host. A run with no routed secret opens nothing.
func (c *Config) checkRouteStore() routeStore {
	if len(c.routeSkipped) == 0 {
		return routeStore{}
	}
	open := c.OpenStore
	if open == nil {
		open = openStore
	}
	store, err := open()
	if err != nil {
		return routeStore{err: err}
	}
	// A store that cannot list is not read to learn whether a secret is
	// there. Every backend brig ships lists.
	l, ok := store.(lister)
	if !ok {
		return routeStore{}
	}
	list, err := l.List()
	if err != nil {
		return routeStore{err: err}
	}
	held := map[string]bool{}
	expiry := map[string]int64{}
	for _, s := range list {
		held[s.Name] = true
		expiry[s.Name] = s.Provenance.ExpiresAt
	}
	var st routeStore
	for name := range clientCertificateSecrets(c.routes) {
		if held[name] {
			if st.certExpiry == nil {
				st.certExpiry = map[string]int64{}
			}
			st.certExpiry[name] = expiry[name]
		}
	}
	for _, name := range c.routeSkipped {
		if !held[name] {
			st.missing = append(st.missing, name)
		}
	}
	return st
}

// routeStoreRefusal is why a run with these routes is refused before boot,
// or nil. It is a credential failure, exit 6, like a required secret the
// store does not hold.
func (c *Config) routeStoreRefusal(st routeStore) error {
	if st.err != nil {
		// The store's own message for a host with none offers to read a
		// secret from a command instead, which writes to the same store.
		cause, advice := st.err, ""
		if errors.Is(st.err, secret.ErrUnsupported) {
			cause = secret.ErrUnsupported
			advice = ". On Linux the store is a Secret Service: log in to a desktop session whose keyring " +
				"(gnome-keyring or KWallet) provides one, then run " + c.routeSecretFix(c.routeSkipped)
		}
		return fmt.Errorf("policy %s injects %s, which the broker reads from brig's secret store on the host, "+
			"and the store is not available: %w%s", c.routedBy(c.routeSkipped), strings.Join(c.routeSkipped, ", "),
			&creds.StoreUnavailableError{Cause: cause}, advice)
	}
	if len(st.missing) == 0 {
		return nil
	}
	var missing []creds.Missing
	for _, name := range st.missing {
		d := c.routeDecl(name)
		missing = append(missing, creds.Missing{Name: name, Required: true, Importable: d.Importable(),
			Reason: secret.ErrNotFound, Hint: d.HintText(), Create: c.routeCreateCommand(name)})
	}
	return fmt.Errorf("policy %s injects a secret that brig's secret store does not hold: %w",
		c.routedBy(st.missing), &creds.MissingSecretsError{Sandbox: c.VMName, Profile: c.Profile.Name, Missing: missing})
}

// routeStoreLine is the part of the BROKER row that names the secrets the
// routes inject, and what the store says about them.
func (c *Config) routeStoreLine(st routeStore) []string {
	if len(c.routeSkipped) == 0 {
		return nil
	}
	parts := []string{"kept on the host: " + strings.Join(c.routeSkipped, ", ")}
	switch {
	case st.err != nil:
		parts = append(parts, "brig's secret store is not available on this host, so the run is refused")
	case len(st.missing) > 0:
		parts = append(parts, fmt.Sprintf("not in brig's secret store: %s, so the run is refused; %s",
			strings.Join(st.missing, ", "), c.routeSecretFix(st.missing)))
	}
	return parts
}

// routeStoreFacts are the fields of the broker in `brig info --json` and
// `brig plan --json` that the store answers.
func (c *Config) routeStoreFacts(st routeStore) (missing []string, storeErr, refused string) {
	if st.err != nil {
		storeErr = st.err.Error()
	}
	if err := c.routeStoreRefusal(st); err != nil {
		refused = err.Error()
	}
	return st.missing, storeErr, refused
}

// routeDecl is the profile's declaration of a routed secret, or one that
// names it alone when the profile declares none.
func (c *Config) routeDecl(name string) profile.SecretDecl {
	for _, d := range c.routeDecls {
		if d.Name == name {
			return d
		}
	}
	return profile.SecretDecl{Name: name}
}

// routeSecretFix is the command that puts each of names in the store:
// import takes the profile, create the secret.
func (c *Config) routeSecretFix(names []string) string {
	var cmds []string
	imported := false
	for _, name := range names {
		switch {
		case !c.routeDecl(name).Importable():
			cmds = append(cmds, c.routeCreateCommand(name))
		case !imported:
			imported = true
			cmds = append(cmds, "brig secret import "+c.Profile.Name)
		}
	}
	return strings.Join(cmds, ", ")
}

// routeCreateCommand is the command that stores the secret name. A client
// certificate and a CA bundle are stored with --type, which checks them
// before they are written, so a missing one is not hinted as a plain
// secret: stored without --type, a certificate is only found wrong at the
// first request.
func (c *Config) routeCreateCommand(name string) string {
	typ := ""
	for _, r := range c.routes {
		if r.Inject == nil || r.Inject.TLS == nil {
			continue
		}
		t := r.Inject.TLS
		switch {
		case t.ClientCertificate != nil && t.ClientCertificate.SecretRef.Name == name:
			return "brig secret create " + name + " --type tls -f <file>"
		case t.CA != nil && t.CA.SecretRef.Name == name:
			typ = "ca"
		}
	}
	if typ != "" {
		return "brig secret create " + name + " --type " + typ + " -f <file>"
	}
	return "brig secret create " + name
}

// routedBy names each route that injects one of names, with the policy that
// declares it, the way a refusal does.
func (c *Config) routedBy(names []string) string {
	var out []string
	for _, r := range c.routes {
		routed := routeSecretNames(r)
		for _, name := range names {
			if slices.Contains(routed, name) {
				out = append(out, fmt.Sprintf("%s (route %s)", c.routePolicy[r.Name], r.Name))
				break
			}
		}
	}
	return strings.Join(out, ", ")
}
