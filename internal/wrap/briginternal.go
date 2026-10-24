package wrap

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	pathpkg "path"
	"strings"

	"github.com/brig-sh/brig/internal/injector"
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
	piece := len(c.brigCA)
	if l, ok := c.Runtime.(runtime.FeedLimiter); ok && l.MaxFeed() > 0 && l.MaxFeed() < piece {
		piece = l.MaxFeed()
	}
	for start := 0; start < len(c.brigCA); start += piece {
		end := min(start+piece, len(c.brigCA))
		if err := c.Runtime.Feed(runtime.ExecSpec{Name: c.VMName, User: guestRootUser,
			Cmd: []string{"sh", "-c", `cat >> "$1"`, "sh", injector.GuestCAPath}, Stdin: strings.NewReader(c.brigCA[start:end])}); err != nil {
			return fmt.Errorf("could not write %s in the sandbox: %w", injector.GuestCAPath, err)
		}
	}
	if err := c.guestRoot("sh", "-c", caSizeScript, "sh", injector.GuestCAPath, fmt.Sprint(len(c.brigCA))); err != nil {
		return fmt.Errorf("%s does not hold the %d bytes written: %w", injector.GuestCAPath, len(c.brigCA), err)
	}
	args := append([]string{"sh", "-c", caBundleScript, "sh", injector.GuestCABundlePath, injector.GuestCAPath},
		caBundleSources...)
	if err := c.guestRoot(args...); err != nil {
		return fmt.Errorf("could not write %s in the sandbox: %w", injector.GuestCABundlePath, err)
	}
	return nil
}
