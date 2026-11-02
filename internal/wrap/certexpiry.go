package wrap

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/notice"
)

// certWarnAhead is how long before a client certificate lapses the run starts
// saying so. A lapsed certificate fails every request of the route at the
// TLS handshake, and nothing in the guest says why.
const certWarnAhead = 3 * 24 * time.Hour

// clientCertificateSecrets maps each secret that a route presents as its
// client certificate to the names of the routes that present it. Several
// routes may share one secret, and a warning about it names all of them.
func clientCertificateSecrets(routes []v1alpha1.Route) map[string][]string {
	out := map[string][]string{}
	for _, r := range routes {
		if r.Inject == nil || r.Inject.TLS == nil || r.Inject.TLS.ClientCertificate == nil {
			continue
		}
		if name := r.Inject.TLS.ClientCertificate.SecretRef.Name; name != "" {
			out[name] = append(out[name], r.Name)
		}
	}
	for _, names := range out {
		slices.Sort(names)
	}
	return out
}

// routeNames renders the routes that share an expiring certificate.
func routeNames(routes []string) string {
	if len(routes) == 1 {
		return "route " + routes[0]
	}
	return "routes " + strings.Join(routes, ", ")
}

// warnExpiringCertificates says that a client certificate expires within
// certWarnAhead, or has. The expiry is the one the store recorded when the
// certificate was stored; no value is read, and a certificate with no
// recorded expiry is not warned about: absence is not evidence.
func (c *Config) warnExpiringCertificates(st routeStore) {
	names := make([]string, 0, len(st.certExpiry))
	for name, exp := range st.certExpiry {
		if exp != 0 {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	routes := clientCertificateSecrets(c.routes)
	now := nowMilli()
	for _, name := range names {
		left := st.certExpiry[name] - now
		if left > certWarnAhead.Milliseconds() {
			continue
		}
		var heading *notice.Block
		if left < 0 {
			heading = notice.Newf("the client certificate %s (%s) expired %s", name, routeNames(routes[name]), ago(-left))
		} else {
			heading = notice.Newf("the client certificate %s (%s) expires in %s", name, routeNames(routes[name]), until(left))
		}
		heading.Do("store the renewed certificate, then", "brig secret update "+name+" --type tls")
		c.warnf("%s", heading)
	}
}

// until renders the time left coarsely, the way ago renders the time past.
func until(millis int64) string {
	d := time.Duration(millis) * time.Millisecond
	switch {
	case d < time.Hour:
		return "less than an hour"
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
