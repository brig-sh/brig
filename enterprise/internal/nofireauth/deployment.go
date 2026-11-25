// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package nofireauth

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

const (
	// ClientID is the public OAuth client id Brig signs in with.
	ClientID = "brig"
	// DefaultURL is the deployment used when neither the flag nor EnvURL
	// names one.
	DefaultURL = "https://my.nofire.ai"
	// EnvURL names the environment variable that selects a deployment.
	EnvURL = "BRIG_NOFIRE_URL"
)

// Deployment is a validated, normalised NOFire AI deployment URL. The
// normalised form is also the issuer Brig expects in discovery metadata
// (RFC 8414), so two spellings of one deployment must give one string and a
// lookalike must be refused, not rewritten.
type Deployment struct {
	u *url.URL
}

// ParseDeployment validates raw and normalises it: https only (http is
// accepted for a loopback IP address, for local development), a lower-case
// ASCII host, no default port, no trailing slash. It refuses what makes a URL
// look like another host or carry more than an origin and a path prefix:
// user info, a query, a fragment, a non-ASCII or punycode host, a host that
// ends in a number, and a path segment with anything but unreserved
// characters. localhost
// is an ordinary host name: it gets no exception for http, which is for
// loopback IP addresses only.
func ParseDeployment(raw string) (Deployment, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Deployment{}, errors.New("deployment URL is empty. Pass a URL such as https://my.nofire.ai")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	// url.Parse drops an empty "?" or "#", so look at the text first.
	if strings.Contains(raw, "?") {
		return Deployment{}, errors.New("deployment URL has a query. Remove everything from the ? on")
	}
	if strings.Contains(raw, "#") {
		return Deployment{}, errors.New("deployment URL has a fragment. Remove everything from the # on")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Deployment{}, fmt.Errorf("deployment URL is not valid: %w", err)
	}
	if u.User != nil {
		return Deployment{}, errors.New("deployment URL has a user name or password. Remove it")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return Deployment{}, fmt.Errorf("deployment URL scheme %q is not supported. Use https", u.Scheme)
	}

	host, err := normaliseHost(u.Hostname())
	if err != nil {
		return Deployment{}, err
	}
	if u.Scheme == "http" && !isLoopbackIP(host) {
		return Deployment{}, fmt.Errorf("deployment URL uses http. Use https; http is allowed only for a loopback IP address such as 127.0.0.1, not %q", host)
	}

	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return Deployment{}, fmt.Errorf("deployment URL port %q is not valid. Use a number from 1 to 65535", port)
		}
		port = strconv.Itoa(n)
		if port == defaultPort(u.Scheme) {
			port = ""
		}
	}

	// Strip exactly one trailing slash; any other empty segment is a typo or
	// an attempt at a different path, so it is refused, as is a dot segment.
	// A segment holds only unreserved characters and no percent-encoding: a
	// proxy that decodes %2F or %5C would fetch the metadata from another
	// path than the one Brig checked, and ";" or "@" make a path read like
	// another host.
	path := strings.TrimSuffix(u.EscapedPath(), "/")
	if path != "" {
		for _, seg := range strings.Split(path[1:], "/") {
			if seg == "" {
				return Deployment{}, fmt.Errorf("deployment URL path %q has an empty segment. Remove the extra /", u.EscapedPath())
			}
			if strings.IndexFunc(seg, notUnreserved) >= 0 {
				return Deployment{}, fmt.Errorf("deployment URL path %q has a character other than letters, digits and . _ ~ -. Remove it, and write any percent-encoded character as itself", u.EscapedPath())
			}
			if seg == "." || seg == ".." {
				return Deployment{}, fmt.Errorf("deployment URL path %q has a . or .. segment. Remove it", u.EscapedPath())
			}
		}
	}

	h := host
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	if port != "" {
		h += ":" + port
	}
	return Deployment{u: &url.URL{Scheme: u.Scheme, Host: h, Path: path}}, nil
}

// notUnreserved reports whether r is outside the RFC 3986 unreserved set.
func notUnreserved(r rune) bool {
	return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
		r == '.' || r == '_' || r == '~' || r == '-')
}

func defaultPort(scheme string) string {
	if scheme == "http" {
		return "80"
	}
	return "443"
}

func isLoopbackIP(host string) bool {
	a, err := netip.ParseAddr(host)
	return err == nil && a.IsLoopback()
}

// normaliseHost lower-cases a host and refuses anything that is not plain
// ASCII DNS or an IP literal. A byte >= 0x80 or an "xn--" label is how a
// lookalike host is written, so neither is accepted.
func normaliseHost(h string) (string, error) {
	if h == "" {
		return "", errors.New("deployment URL has no host. Add one, such as my.nofire.ai")
	}
	for i := 0; i < len(h); i++ {
		if h[i] >= 0x80 {
			return "", errors.New("deployment URL host is not ASCII, and brig refuses it because it can imitate another host. Use a host name of ASCII letters, digits and hyphens")
		}
	}
	h = strings.ToLower(h)
	if a, err := netip.ParseAddr(h); err == nil {
		if a.Zone() != "" {
			return "", errors.New("deployment URL host has an IPv6 zone. Remove the % and what follows it")
		}
		return a.String(), nil
	}
	for _, label := range strings.Split(h, ".") {
		if strings.HasPrefix(label, "xn--") {
			return "", errors.New("deployment URL host uses punycode (xn--), and brig refuses it because it can imitate another host. Use a host name without xn-- labels")
		}
		if label == "" {
			return "", errors.New("deployment URL host has an empty label. Remove the extra dot")
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return "", fmt.Errorf("deployment URL host has the character %q, which a host name cannot contain. Remove it", c)
			}
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", fmt.Errorf("deployment URL host label %q starts or ends with a hyphen. Remove the hyphen", label)
		}
	}
	// A host whose last label is a number is an IPv4 address to the URL
	// standard and to inet_aton, which getaddrinfo uses: 2130706433, 0x7f.1
	// and 0177.0.0.1 all dial 127.0.0.1. netip accepted none of them above,
	// so Brig and the resolver would disagree about the host.
	if last := h[strings.LastIndex(h, ".")+1:]; isNumber(last) {
		return "", fmt.Errorf("deployment URL host %q ends in a number but is not an IP address in dotted-decimal form. Write an IP address as four numbers, such as 127.0.0.1, or use a host name", h)
	}
	return h, nil
}

// isNumber reports whether label is a decimal, octal or 0x hexadecimal
// number, as the URL standard's IPv4 parser reads one.
func isNumber(label string) bool {
	digits := "0123456789"
	if strings.HasPrefix(label, "0x") {
		label, digits = label[2:], "0123456789abcdef"
		if label == "" {
			return true
		}
	}
	return label != "" && strings.Trim(label, digits) == ""
}

// String returns the normalised URL, which is also the issuer Brig expects.
func (d Deployment) String() string {
	if d.u == nil {
		return ""
	}
	return d.u.String()
}

// SameOrigin reports whether raw is an absolute URL with the scheme, host and
// effective port of d. The path is ignored. Brig uses it to refuse discovery
// endpoints that point off the deployment.
func (d Deployment) SameOrigin(raw string) bool {
	if d.u == nil {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
		return false
	}
	origin := func(u *url.URL) (string, string, string) {
		p := u.Port()
		if p == "" {
			p = defaultPort(u.Scheme)
		}
		return u.Scheme, strings.ToLower(u.Hostname()), p
	}
	s1, h1, p1 := origin(d.u)
	s2, h2, p2 := origin(u)
	return s1 == s2 && h1 == h2 && p1 == p2
}

// URLSource says where the deployment URL came from.
type URLSource int

const (
	FromArg URLSource = iota
	FromEnv
	FromDefault
)

func (s URLSource) String() string {
	switch s {
	case FromArg:
		return "from the command line"
	case FromEnv:
		return "from " + EnvURL
	default:
		return "default"
	}
}

// ResolveDeployment picks the deployment: the argument, else the environment
// value, else DefaultURL. A value from the environment that normalises to the
// default still reports FromEnv, so the caller can tell it was chosen.
func ResolveDeployment(arg, env string) (Deployment, URLSource, error) {
	raw, src := DefaultURL, FromDefault
	switch {
	case arg != "":
		raw, src = arg, FromArg
	case env != "":
		raw, src = env, FromEnv
	}
	d, err := ParseDeployment(raw)
	if err != nil {
		return Deployment{}, src, fmt.Errorf("deployment URL %s: %w", src, err)
	}
	return d, src, nil
}
