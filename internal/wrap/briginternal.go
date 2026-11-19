package wrap

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	pathpkg "path"
	"strings"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/runtime"
)

// A guest of hull on hvi or of nerdctl reaches its endpoint routes at
// https://brig.internal/<sandbox>/r/<route>. The runtime answers the name
// with the sandbox gateway's own address and diverts its port 443 to the
// broker's divert socket, where the broker serves TLS with a certificate the
// brig CA signs. The guest trusts that CA, and only for brig.internal: the CA
// is constrained to that one name. brig writes the CA certificate into the
// guest, never its key, and leaves the image's own trust store as it is.
// docs/broker.md ("Transports") describes it.

// caBundleSources are where an image keeps its system roots, in the order
// they are tried: Debian and Alpine, Fedora and RHEL, then the rest.
var caBundleSources = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/cert.pem",
}

// caCreateScript creates the CA file $2 empty, root's and 0644, in the
// directory $1, root's and 0755, and removes the bundle $3. A link at $1 or
// its parent is refused rather than followed. set -C makes the shell's >
// create the file new, so nothing that reappeared between the rm and the
// create is written through.
const caCreateScript = `set -e; for d in "$(dirname "$1")" "$1"; do [ ! -L "$d" ] || { echo "$d is a link" >&2; exit 1; }; done; ` +
	`mkdir -p -m 0755 "$1"; chown root "$1"; chmod 0755 "$1"; rm -f -- "$2" "$3" "$3.new"; umask 022; set -C; : > "$2"`

// guestFileCreateScript creates the file $2 empty, root's and 0644, in the
// directory $1, root's and 0755, as caCreateScript does for the CA, with no
// bundle beside it.
const guestFileCreateScript = `set -e; for d in "$(dirname "$1")" "$1"; do [ ! -L "$d" ] || { echo "$d is a link" >&2; exit 1; }; done; ` +
	`mkdir -p -m 0755 "$1"; chown root "$1"; chmod 0755 "$1"; rm -f -- "$2"; umask 022; set -C; : > "$2"`

// caSizeScript exits non-zero when the file $1 is not $2 bytes. The answer is
// its exit status: urunc's exec can drop the output of a command that exits
// at once, and an empty answer would read as a short file.
const caSizeScript = `[ "$(stat -c %s "$1")" = "$2" ]`

// caBundleScript writes the bundle $1: the first of the root files after $2
// that exists, then the CA file $2. The image's roots come first, so a client
// that reads one file still trusts every site it trusted before. An image with
// none gets the CA alone.
const caBundleScript = `set -e; umask 022; set -C; out="$1"; ca="$2"; shift 2; : > "$out.new"; ` +
	`for f in "$@"; do if [ -f "$f" ]; then cat "$f" >> "$out.new"; echo >> "$out.new"; break; fi; done; ` +
	`cat "$ca" >> "$out.new"; mv -f "$out.new" "$out"`

// checkBrigCA returns ca when it is one PEM certificate of a CA constrained to
// brig.internal, and nothing else: no key, no second block. It is what the
// guest is handed, so anything more is refused rather than written there.
func checkBrigCA(ca string) (string, error) {
	block, rest := pem.Decode([]byte(ca))
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) > 0 {
		return "", errors.New("it is not one PEM certificate and nothing else")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	if !cert.IsCA || !cert.PermittedDNSDomainsCritical || len(cert.PermittedDNSDomains) != 1 ||
		cert.PermittedDNSDomains[0] != injector.BrigInternal {
		return "", fmt.Errorf("it is not a CA constrained to %s", injector.BrigInternal)
	}
	return string(pem.EncodeToMemory(block)), nil
}

// deliverBrigCA writes the brig CA certificate to injector.GuestCAPath, and
// a bundle of the image's own roots followed by it to
// injector.GuestCABundlePath, for a guest that reaches brig.internal. The
// variables of GuestEnv point the guest's TLS clients at them: Node adds the
// CA to its own roots, and OpenSSL, Go, Python requests and git, which take
// one file in place of their roots, read the bundle, so every other site
// still verifies.
//
// Both files are root's and 0644 in a directory of root's, written on every
// exec: the CA is the broker's, and a broker that started again since the
// boot may have made a new one. The certificate goes in on stdin, as a
// credential does, though it is public: one way of writing into the guest.
func (c *Config) deliverBrigCA() error {
	if c.brigCA == "" {
		return nil
	}
	dir := pathpkg.Dir(injector.GuestCAPath)
	if err := c.guestRoot("sh", "-c", caCreateScript, "sh", dir, injector.GuestCAPath, injector.GuestCABundlePath); err != nil {
		return fmt.Errorf("could not create %s in the sandbox: %w", injector.GuestCAPath, err)
	}
	if err := c.feedGuestFile(injector.GuestCAPath, c.brigCA); err != nil {
		return err
	}
	args := append([]string{"sh", "-c", caBundleScript, "sh", injector.GuestCABundlePath, injector.GuestCAPath},
		caBundleSources...)
	if err := c.guestRoot(args...); err != nil {
		return fmt.Errorf("could not write %s in the sandbox: %w", injector.GuestCABundlePath, err)
	}
	return nil
}

// feedGuestFile appends body to the guest file path, which exists, on stdin
// and in pieces the runtime can carry, and checks that the file holds what
// was written.
func (c *Config) feedGuestFile(path, body string) error {
	piece := len(body)
	if l, ok := c.Runtime.(runtime.FeedLimiter); ok && l.MaxFeed() > 0 && l.MaxFeed() < piece {
		piece = l.MaxFeed()
	}
	for start := 0; start < len(body); start += piece {
		end := min(start+piece, len(body))
		if err := c.Runtime.Feed(runtime.ExecSpec{Name: c.VMName, User: guestRootUser,
			Cmd: []string{"sh", "-c", `cat >> "$1"`, "sh", path}, Stdin: strings.NewReader(body[start:end])}); err != nil {
			return fmt.Errorf("could not write %s in the sandbox: %w", path, err)
		}
	}
	if err := c.guestRoot("sh", "-c", caSizeScript, "sh", path, fmt.Sprint(len(body))); err != nil {
		return fmt.Errorf("%s does not hold the %d bytes written: %w", path, len(body), err)
	}
	return nil
}

// guestBrigInternal is the address the guest of this sandbox resolves
// brig.internal to: its own gateway's, which the runtime diverts to the
// broker. The broker serves brig.internal on connections to that address
// alone.
func (c *Config) guestBrigInternal() (string, error) {
	router, ok := c.Runtime.(runtime.BrigInternalRouter)
	if !ok {
		return "", fmt.Errorf("the runtime %s cannot say where its guest reaches %s", c.Runtime.Kind(),
			injector.BrigInternal)
	}
	addr, err := router.BrigInternalAddr(c.VMName)
	if err != nil {
		return "", fmt.Errorf("could not find where the guest of %s reaches %s: %w", c.VMName,
			injector.BrigInternal, err)
	}
	return addr.String(), nil
}

// acceptBrigInternal checks what the broker answered a brig.internal
// registration of what ("the memory", "the MCP servers") and takes its CA.
// others names what this sandbox registered before it, for the message.
//
// The routes, the memory and the MCP servers of a sandbox share its divert
// socket and the broker's CA, so each answer is checked against what the
// gateway was handed and what the registrations before it took.
func (c *Config) acceptBrigInternal(what, others string, added injector.AddResponse) error {
	// The gateway is handed the socket before the broker answers, as for a
	// route. A broker that listens elsewhere would leave the guest's
	// connections reset.
	if want := c.diverts(); !want.Requested() || added.DivertSocket != want.Socket {
		return fmt.Errorf("the credential broker listens for the diverted connections of %s on %q, "+
			"and brig expected %q; stop the broker with `brig stop` on every sandbox and run again",
			c.VMName, added.DivertSocket, want.Socket)
	}
	ca, err := checkBrigCA(added.CA)
	if err != nil {
		return fmt.Errorf("the credential broker handed over a brig CA that brig will not put in the "+
			"guest: %w", err)
	}
	// One broker makes one CA. Another CA here means two brokers answered
	// the registrations of one sandbox, and the guest would trust one and
	// be served by the other.
	if c.brigCA != "" && c.brigCA != ca {
		return fmt.Errorf("the credential broker handed %s of %s a different brig CA from %s; "+
			"stop the broker with `brig stop` on every sandbox and run again", what, c.VMName, others)
	}
	c.brigCA = ca
	return nil
}

// brigInternalRefusal is why this run cannot serve subject to its guest at
// brig.internal, or nil: an offline run cannot reach it, and a run path
// that does not route that name to the broker cannot serve it. runs says
// where it runs, as a sentence's start ("Agent memory runs"), and wayOut is
// the other way past the refusal.
func (c *Config) brigInternalRefusal(subject, runs, wayOut string) error {
	if c.Network == NetOffline {
		return fmt.Errorf("%s is served to the guest at %s, which an offline run cannot "+
			"reach; run with --network isolated, or %s", subject, injector.BrigInternal, wayOut)
	}
	err := c.endpointCheck()
	if err == nil {
		return nil
	}
	why := err.Error()
	var ce *runtime.CapabilityError
	if errors.As(err, &ce) {
		why = ce.Why
		if ce.Err != nil {
			why += ": " + ce.Err.Error()
		}
	}
	return fmt.Errorf("%s is served to the guest at %s, and %s cannot route it: %s. "+
		"%s on hull with the hvi backend (BRIG_HYPERVISOR=hvi) and on nerdctl with urunc; "+
		"run there, or %s", subject, injector.BrigInternal, c.routePath(), why, runs, wayOut)
}

// mcpConfig is the MCP configuration that points Claude Code at what the
// broker serves sandbox vm at brig.internal: its memory store, when memory
// is on, and each of servers. Every token is named by its variable, which
// Claude Code expands from its environment when it reads the file, so the
// file holds no secret and is the same on every exec. One file for all of
// them, because --mcp-config is one flag and the agent is handed one path.
func mcpConfig(vm string, memory bool, servers []policy.MCPEntry) (string, error) {
	type server struct {
		Type    string            `json:"type"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	base := injector.HostEndpoint{}.Base() + "/" + vm
	entries := map[string]server{}
	if memory {
		entries[v1alpha1.MCPReservedName] = server{
			Type:    "http",
			URL:     base + "/memory",
			Headers: map[string]string{"Authorization": "Bearer ${" + memoryTokenVar + "}"},
		}
	}
	for _, e := range servers {
		entries[e.Server.Name] = server{
			Type:    "http",
			URL:     base + "/mcp/" + e.Server.Name,
			Headers: map[string]string{"Authorization": "Bearer ${" + mcpTokenVar + "}"},
		}
	}
	b, err := json.Marshal(struct {
		MCPServers map[string]server `json:"mcpServers"`
	}{entries})
	return string(b), err
}

// deliverMCPConfig writes the MCP configuration of this run to
// mcpConfigPath, for a run with memory or an MCP server, the way
// deliverBrigCA writes the CA: root's and 0644 in a directory of root's,
// outside the guest home, on stdin, on every exec. The agent reads it and
// cannot change it, so it cannot point itself at another server with a
// token.
func (c *Config) deliverMCPConfig() error {
	if c.MCPConfig() == "" {
		return nil
	}
	body, err := mcpConfig(c.VMName, c.memoryKey != "", c.mcp)
	if err != nil {
		return err
	}
	dir := pathpkg.Dir(mcpConfigPath)
	if err := c.guestRoot("sh", "-c", guestFileCreateScript, "sh", dir, mcpConfigPath); err != nil {
		return fmt.Errorf("could not create %s in the sandbox: %w", mcpConfigPath, err)
	}
	return c.feedGuestFile(mcpConfigPath, body)
}
