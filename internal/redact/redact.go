// Package redact replaces identifying values in diagnostics with placeholders
// and scrubs token shapes and addresses from free text. It does no I/O.
//
// Register values through the structured methods first, then call Text, so a
// value found by a later collector is removed from earlier output too. Check
// refuses registered values, token shapes, emails and MAC addresses; it does
// not refuse unregistered IP addresses.
//
// Placeholders are counters in order of first sight, not hashes, because a
// hash of a short name can be guessed. They are stable within one Redactor.
package redact

import (
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Kind is the word inside a placeholder and in the manifest.
type Kind string

const (
	KindUser      Kind = "user"
	KindHost      Kind = "host"
	KindWorkspace Kind = "workspace"
	KindPath      Kind = "path"
	KindProfile   Kind = "profile"
	KindLabel     Kind = "label"
	KindSandbox   Kind = "sandbox"
	KindPolicy    Kind = "policy"
	KindVar       Kind = "VAR"
	KindRegistry  Kind = "registry"
	KindImage     Kind = "image"
	KindTag       Kind = "tag"
	KindDomain    Kind = "domain"
	KindIP        Kind = "ip"
	KindEmail     Kind = "email"
)

// minTextLen is the shortest registered value Text searches for; "dev" would
// rewrite /dev/. The host's home, user and hostname are searched at any
// length.
const minTextLen = 4

// Clean is text the Redactor has scrubbed. The unexported field keeps other
// packages from building one from raw text.
type Clean struct{ s string }

func (c Clean) String() string { return c.s }

// Host holds the machine's own values. Each non-empty field is replaced
// wherever it appears.
type Host struct {
	Home     string
	User     string
	Hostname string
}

// Allow lists what stays readable: brig's own names and public
// infrastructure.
type Allow struct {
	// Names are profile and policy names brig ships.
	Names []string
	// Vars are variable names the built-in profiles forward.
	Vars []string
	// Images are image repositories without tag or digest.
	Images []string
	// Domains are exact hosts, or suffixes written with a leading dot.
	Domains []string
	// Files are file and directory names inside brig's own directories.
	Files []string
	// Prefixes are addresses kept beyond loopback and unspecified.
	Prefixes []netip.Prefix
}

type original struct {
	placeholder string
	kind        Kind
	// always marks the host's own values, searched at any length.
	always bool
}

// Redactor holds the placeholder table for one bundle. Safe for concurrent
// use: an abandoned collector's goroutine may still register values.
type Redactor struct {
	mu sync.Mutex
	// home is the cleaned $HOME, set once in New.
	home      string
	allow     Allow
	names     map[string]bool
	vars      map[string]bool
	images    map[string]bool
	files     map[string]bool
	originals map[string]original
	counts    map[Kind]int
	applied   map[string]bool
}

// New returns a Redactor that already replaces the host's home, user name and
// hostname, the short hostname included.
func New(h Host, a Allow) *Redactor {
	// Host names are compared lower-cased.
	a.Domains = append([]string(nil), a.Domains...)
	for i, d := range a.Domains {
		a.Domains[i] = strings.ToLower(d)
	}
	r := &Redactor{
		allow:     a,
		names:     set(a.Names),
		vars:      set(a.Vars),
		images:    set(a.Images),
		files:     set(a.Files),
		originals: map[string]original{},
		counts:    map[Kind]int{},
		applied:   map[string]bool{},
	}
	if h.Home != "" {
		r.home = filepath.Clean(h.Home)
		r.originals[r.home] = original{"$HOME", KindPath, true}
	}
	if h.User != "" {
		r.originals[h.User] = original{"<user>", KindUser, true}
	}
	if h.Hostname != "" {
		// Keyed lower-cased, as placeholder keys host names.
		hn := asciiLower(h.Hostname)
		r.originals[hn] = original{"<host>", KindHost, true}
		if short, _, ok := strings.Cut(hn, "."); ok && short != "" {
			r.originals[short] = original{"<host>", KindHost, true}
		}
	}
	return r
}

func set(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// placeholder returns v's placeholder, allocating the next one of kind k on
// first sight. An empty value returns "" and registers nothing.
//
// A case-insensitive kind is keyed lower-cased. A value seen again under
// another kind keeps its first placeholder, and Kinds lists both kinds.
// Caller holds mu.
func (r *Redactor) placeholder(k Kind, v string) string {
	if v == "" {
		return ""
	}
	if foldCase(k) {
		v = asciiLower(v)
	}
	if o, ok := r.originals[v]; ok {
		r.applied[string(k)] = true
		return o.placeholder
	}
	r.counts[k]++
	ph := fmt.Sprintf("<%s-%d>", k, r.counts[k])
	r.originals[v] = original{placeholder: ph, kind: k}
	r.applied[string(k)] = true
	return ph
}

// Name is a profile, policy, label or sandbox name: kept when brig ships it.
func (r *Redactor) Name(k Kind, s string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s == "" || r.names[s] {
		return s
	}
	return r.placeholder(k, s)
}

// Var is an environment variable name: kept when a built-in profile forwards
// it.
func (r *Redactor) Var(s string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s == "" || r.vars[s] {
		return s
	}
	return r.placeholder(KindVar, s)
}

// Workspace is a session's host directory. Always a placeholder, never a
// $HOME-relative path, since its name is usually the project's.
func (r *Redactor) Workspace(p string) string {
	if p == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.placeholder(KindWorkspace, filepath.Clean(p))
}

// brigDirs are brig's directories under $HOME. A name inside one is kept
// only if it is in Allow.Files.
var brigDirs = []string{".brig", ".config/brig", ".local/share/brig"}

// devPrefixes are device and kernel paths, kept whole.
var devPrefixes = []string{"/dev/", "/proc/", "/sys/"}

// installPrefixes are install locations. A path under one is kept only if
// every later segment is in installNames or Allow.Files, or is a version;
// otherwise the whole path becomes a placeholder.
var installPrefixes = []string{
	"/usr/", "/bin/", "/sbin/", "/opt/homebrew/", "/home/linuxbrew/.linuxbrew/",
	"/nix/store/", "/Applications/", "/Library/", "/System/", "/etc/", "/run/", "/var/run/",
}

// installNames are path segments an install layout uses on its own.
var installNames = map[string]bool{
	"bin": true, "sbin": true, "lib": true, "libexec": true, "local": true,
	"share": true, "Caskroom": true, "Cellar": true, "brig": true, "brigd": true,
	"hull": true, "nerdctl": true, "containerd": true, "buildkitd": true,
	"cosign": true, "oras": true, "security": true, "sw_vers": true,
}

// Path is a host path.
func (r *Redactor) Path(p string) string {
	if p == "" {
		return ""
	}
	p = filepath.Clean(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.home != "" {
		if p == r.home {
			return "$HOME"
		}
		if rest, ok := strings.CutPrefix(p, r.home+"/"); ok {
			return "$HOME/" + r.homeRel(rest)
		}
	}
	for _, pre := range devPrefixes {
		if strings.HasPrefix(p, pre) {
			return p
		}
	}
	for _, pre := range installPrefixes {
		rest, ok := strings.CutPrefix(p, pre)
		if !ok {
			continue
		}
		for _, s := range strings.Split(rest, "/") {
			if !installNames[s] && !r.files[s] && !versionTag.MatchString(s) {
				return r.placeholder(KindPath, p)
			}
		}
		return p
	}
	return r.placeholder(KindPath, p)
}

// homeRel is a path below $HOME. Caller holds mu.
func (r *Redactor) homeRel(rest string) string {
	for _, d := range brigDirs {
		if rest == d {
			return d
		}
		tail, ok := strings.CutPrefix(rest, d+"/")
		if !ok {
			continue
		}
		segs := strings.Split(tail, "/")
		for i, s := range segs {
			if !r.files[s] {
				segs[i] = r.placeholder(KindPath, s)
			}
		}
		return d + "/" + strings.Join(segs, "/")
	}
	return r.placeholder(KindPath, rest)
}

// publicRegistries are kept: they name a hosting service, not an
// organisation.
var publicRegistries = map[string]bool{
	"docker.io": true, "ghcr.io": true, "quay.io": true, "gcr.io": true,
	"public.ecr.aws": true, "registry.k8s.io": true, "mcr.microsoft.com": true,
}

// versionTag matches "latest" or a release version: optional v, up to four
// numbers, optional rc/alpha/beta/pre/dev suffix, optional hex build metadata.
var versionTag = regexp.MustCompile(`^(latest|v?[0-9]+(\.[0-9]+){0,3}(-(rc|alpha|beta|pre|dev)(\.?[0-9]+)?)?([-+][0-9a-fA-F]{7,40})?)$`)

// digestShape is a content digest: an algorithm name and at least 32 hex
// digits.
var digestShape = regexp.MustCompile(`^[a-z0-9]+:[0-9a-f]{32,}$`)

// Image is an image reference. An allowlisted repository is kept; the tag is
// kept only if it matches versionTag and the digest only if it matches
// digestShape, even on an allowlisted repository.
func (r *Redactor) Image(ref string) string {
	if ref == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	name, digest, _ := strings.Cut(ref, "@")
	tag := ""
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name, tag = name[:i], name[i+1:]
	}
	var b strings.Builder
	if r.images[name] {
		b.WriteString(name)
	} else {
		registry, repo := "", name
		if first, rest, ok := strings.Cut(name, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
			registry, repo = first, rest
		}
		if registry != "" {
			if publicRegistries[registry] {
				b.WriteString(registry)
			} else {
				b.WriteString(r.placeholder(KindRegistry, registry))
			}
			b.WriteString("/")
		}
		b.WriteString(r.placeholder(KindImage, repo))
	}
	if tag != "" {
		b.WriteString(":")
		if versionTag.MatchString(tag) {
			b.WriteString(tag)
		} else {
			b.WriteString(r.placeholder(KindTag, tag))
		}
	}
	if digest != "" {
		b.WriteString("@")
		if digestShape.MatchString(digest) {
			b.WriteString(digest)
		} else {
			b.WriteString(r.placeholder(KindImage, digest))
		}
	}
	return b.String()
}

// validPort reports whether s is all digits and at most 65535, so
// "acme.io:secret-env" is not split as host:port.
func validPort(s string) bool {
	if s == "" || len(s) > 5 {
		return false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
		n = n*10 + int(c-'0')
	}
	return n <= 65535
}

// splitHostPort splits "host:port". It returns false for a bare IPv6
// address and for a trailing part that is not a port.
func splitHostPort(h string) (host, port string, ok bool) {
	if !strings.Contains(h, ":") {
		return "", "", false
	}
	if strings.Count(h, ":") > 1 && !strings.HasPrefix(h, "[") {
		return "", "", false
	}
	host, port, err := net.SplitHostPort(h)
	if err != nil || !validPort(port) {
		return "", "", false
	}
	return host, port, true
}

// withPort reattaches :port, bracketing a host that contains a colon (a kept
// IPv6 address).
func withPort(host, port string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

// Domain is a host name, or an address written where a host would be. A
// trailing :port is kept; only the host is redacted.
func (r *Redactor) Domain(h string) string {
	if h == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if host, port, ok := splitHostPort(h); ok {
		return withPort(r.domain(host), port)
	}
	return r.domain(h)
}

// domain is Domain without the port split. Caller holds mu.
func (r *Redactor) domain(h string) string {
	if _, err := netip.ParseAddr(h); err == nil {
		return r.ip(h)
	}
	lower := strings.ToLower(h)
	for _, d := range r.allow.Domains {
		if lower == d || strings.HasPrefix(d, ".") && (strings.HasSuffix(lower, d) || lower == d[1:]) {
			return h
		}
	}
	return r.placeholder(KindDomain, h)
}

// IP is an address in structured input. It accepts a port or prefix width,
// and fails closed: a value that does not parse becomes a placeholder. The
// address is registered as written, not normalised, so Text finds the same
// spelling.
func (r *Redactor) IP(s string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if host, port, ok := splitHostPort(s); ok {
		if _, err := netip.ParseAddr(host); err == nil {
			return withPort(r.ip(host), port)
		}
	}
	if i := strings.LastIndex(s, "/"); i > 0 {
		if pfx, err := netip.ParsePrefix(s); err == nil {
			return fmt.Sprintf("%s/%d", r.ip(s[:i]), pfx.Bits())
		}
	}
	bare := strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	if _, err := netip.ParseAddr(bare); err == nil {
		return r.ip(bare)
	}
	return r.placeholder(KindIP, s)
}

// ip keeps loopback, unspecified and allowlisted addresses. It returns
// non-addresses unchanged: Text feeds it regex candidates such as 10:22:33.
// Caller holds mu.
func (r *Redactor) ip(s string) string {
	a, err := netip.ParseAddr(s)
	if err != nil || a.IsLoopback() || a.IsUnspecified() {
		return s
	}
	for _, p := range r.allow.Prefixes {
		if p.Contains(a) {
			return s
		}
	}
	return r.placeholder(KindIP, s)
}

// Kinds is every kind of value replaced so far, sorted, for the manifest.
func (r *Redactor) Kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.applied))
	for k := range r.applied {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
