package broker

import (
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// checkDivertedRoutes refuses two routes besides endpoint and host on one host and
// port. The runtime diverts the host and port of the upstream of such a
// route, and a divert handler gets a diverted connection by the host and
// port alone, so it could not tell the two routes apart.
func checkDivertedRoutes(routes map[string]*route) error {
	names := make([]string, 0, len(routes))
	for name, rt := range routes {
		if !rt.spec.Mode.AtBrigInternal() {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	seen := map[string]string{}
	for _, name := range names {
		u := routes[name].upstream
		addr := net.JoinHostPort(strings.ToLower(u.Hostname()), strconv.Itoa(upstreamPort(u)))
		if other, dup := seen[addr]; dup {
			return fmt.Errorf("routes %s and %s are both diverted on %s, and a diverted connection "+
				"is bound by its host and port", other, name, addr)
		}
		seen[addr] = name
	}
	return nil
}

// upstreamPort returns the port of an https:// upstream.
func upstreamPort(u *url.URL) int {
	if p, err := strconv.Atoi(u.Port()); err == nil {
		return p
	}
	return 443
}
