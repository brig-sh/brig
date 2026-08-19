// Package hostsrc reads a credential from where the host already keeps it.
//
// This is the import path and nothing else. internal/wrap must never depend on
// it: the guarantee is that a run reads no
// host source, and that guarantee is unprovable by instrumentation -- if
// BuildEnv does not reference this package, there is nothing to instrument. So
// it is asserted as a dependency instead (see arch_test.go), which fails the
// moment someone adds a well-meaning "auto-import when the secret is missing"
// convenience.
//
// Absence is ordinary throughout. A source that holds nothing reports (_,
// false, nil), which is how a chain falls through from the macOS keychain to
// the Linux file without any per-platform predicate, and how a host that has
// never run the agent produces a warning rather than an error.
package hostsrc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/brig-sh/brig/internal/jsonfind"
	"github.com/brig-sh/brig/internal/profile"
)

// errNoSuchItem is ordinary absence for a keychain source: the host has never
// run the agent, or (on a platform with no keychain at all) there is nothing
// to read. Both readKeychain implementations -- darwin and otherwise -- return
// it for exactly that case, and only that case; anything else is a refusal
// and must not be confused with it.
var errNoSuchItem = errors.New("no such keychain item")

// errToolMissing is the keychain tool never having run at all, as opposed to
// running and refusing. It is neither absence nor a refusal: falling through
// to the next source would hide a broken host, and reporting it as a refusal
// tells somebody to approve a dialog that was never raised.
var errToolMissing = errors.New("the keychain tool could not be run")

// Value is what a source yielded, and which source that was.
type Value struct {
	Bytes []byte
	// From is the source's locator, e.g. "keychain:Claude Code-credentials".
	// It is what the import output names and what provenance records, so the
	// two cannot disagree.
	From string
}

// Reader reads sources, at most once per locator.
type Reader struct {
	seen map[string]result
	// Seams for the tests. The host reads are the one thing here that cannot
	// be exercised in CI, so they are replaceable rather than mocked through
	// an interface nobody else implements.
	readFile     func(path string) ([]byte, error)
	readKeychain func(service string) ([]byte, error)
	lookupEnv    func(name string) (string, bool)
}

type result struct {
	v   Value
	ok  bool
	err error
}

func NewReader() *Reader {
	return &Reader{
		seen:         map[string]result{},
		readFile:     os.ReadFile,
		readKeychain: readKeychain,
		lookupEnv:    os.LookupEnv,
	}
}

// Read resolves one source, or reports that it holds nothing.
//
// Deduped by locator: two secrets naming the same keychain item raise one
// approval dialog rather than two. That optimisation lives here, where the
// reads happen, rather than in the schema -- which is what let the design drop
// the top-level import: block that existed only to group them.
func (r *Reader) Read(s profile.Source) (Value, bool, error) {
	key := s.Locator()
	if got, ok := r.seen[key]; ok {
		return got.v, got.ok, got.err
	}
	v, ok, err := r.read(s)
	r.seen[key] = result{v, ok, err}
	return v, ok, err
}

func (r *Reader) read(s profile.Source) (Value, bool, error) {
	from := s.Locator()
	switch s.From {
	case profile.SourceKeychain:
		blob, err := r.readKeychain(s.Service)
		// Absence and refusal are different answers. The host's item is
		// ACL-scoped to the application that wrote it, so an approval dialog is
		// the expected first-import experience -- and treating a denial, or a
		// locked keychain over SSH, as "nothing here" falls through to the next
		// source and ends up telling somebody to log in on a host where they
		// already have.
		switch {
		case errors.Is(err, errNoSuchItem):
			// Not having run the agent on this host is the common case, and on
			// a platform with no keychain there is nothing to read. Absence.
			return Value{}, false, nil
		case errors.Is(err, errToolMissing):
			return Value{}, false, fmt.Errorf("the keychain read for %q could not run: %w", s.Service, err)
		case err != nil:
			return Value{}, false, fmt.Errorf("the keychain read for %q was refused: %w. "+
				"Approve the dialog, or unlock your login keychain, then run the import again",
				s.Service, err)
		case len(strings.TrimSpace(string(blob))) == 0:
			return Value{}, false, nil
		}
		// security -w prints the value and a newline of its own, which is the
		// tool's, not the credential's.
		return Value{Bytes: []byte(strings.TrimSuffix(string(blob), "\n")), From: from}, true, nil
	case profile.SourceFile:
		path, err := expand(s.Path)
		if err != nil {
			return Value{}, false, err
		}
		blob, err := r.readFile(path)
		if os.IsNotExist(err) {
			return Value{}, false, nil
		}
		if err != nil {
			// A file that is there and unreadable is not absence: the user has
			// a permissions problem, and silently falling through to the next
			// source would hide it.
			return Value{}, false, fmt.Errorf("reading %s: %w", path, err)
		}
		if len(blob) == 0 {
			return Value{}, false, nil
		}
		return Value{Bytes: blob, From: from}, true, nil
	case profile.SourceEnv:
		value, ok := r.lookupEnv(s.Var)
		if !ok || value == "" {
			return Value{}, false, nil
		}
		return Value{Bytes: []byte(value), From: from}, true, nil
	}
	return Value{}, false, fmt.Errorf("unknown source %q", s.From)
}

// expand resolves a leading ~, which is where a profile writes a home-relative
// path -- and which stays unexpanded in the profile itself, because a profile
// is a shareable artifact and must not carry one host's home directory.
func expand(p string) (string, error) {
	if !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot expand %q: no home directory: %w", p, err)
	}
	return filepath.Join(home, p[2:]), nil
}

// Extract turns what a source yielded into what gets stored, plus the expiry
// to record as provenance.
//
// No field: means verbatim, which is what a files: delivery needs: the host
// keychain blob IS the format the agent's credentials file takes, so nothing
// is extracted and no field brig does not understand is lost. It is also why
// there is no json: template -- expiresAt is a number and scopes an array, so
// a mapping of string-valued refs could not reproduce the document without
// growing a type system.
//
// field: extracts, because a secret bound to a variable must be the bare
// value: ref: secrets.<name> yields the whole stored value, so a document
// bound to a variable would forward an entire JSON blob as a token.
func Extract(v Value, d profile.SecretDecl) ([]byte, int64, error) {
	var expiry int64
	if d.ExpiryField != "" {
		expiry, _ = jsonfind.Number(v.Bytes, d.ExpiryField)
	}
	if d.Field == "" {
		return v.Bytes, expiry, nil
	}
	got, ok := jsonfind.String(v.Bytes, d.Field)
	if !ok || got == "" {
		return nil, 0, fmt.Errorf("%s holds no %q, so there is nothing to store for %q",
			v.From, d.Field, d.Name)
	}
	return []byte(got), expiry, nil
}
