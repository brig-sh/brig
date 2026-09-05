//go:build darwin

package secret

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// service is brig's own namespace in the keychain. Every item this package
// touches carries it, so brig never reaches outside its own namespace.
//
// That is a narrower guarantee than it may look, and the difference matters to
// anyone building a trust decision on it. The service name is a label, not an
// authenticity check: any process running as this user can add an item under
// it, and brig would then read, update and delete that item as if it were its
// own. What the namespace buys is containment of brig, not provenance of what
// it finds there.
const service = "sh.brig.secret"

// securityBin is the keychain tool, named by its absolute path.
//
// A bare "security" is resolved through $PATH, and $PATH is whatever the shell
// that invoked brig happened to be carrying -- so a file called `security`
// earlier in it stands in for the system tool at every one of the call sites
// below. That is not a theoretical swap: the write path pipes the encoded
// value to the tool's stdin and the read path takes its stdout as the secret,
// so a shim gets handed real credentials by brig itself and can hand back
// whatever it likes. The tool is part of macOS and lives at a fixed path, so
// there is nothing to look up.
const securityBin = "/usr/bin/security"

// The exit codes security(1) returns for the two outcomes worth telling
// apart. Measured rather than documented: 45 is errSecDuplicateItem and 44 is
// errSecItemNotFound.
const (
	codeDuplicate = 45
	codeNotFound  = 44
)

type keychain struct{ service string }

// Caught at build time rather than wherever a keychain first gets assigned to
// a Store: a method that stops matching the interface fails here, in the file
// that has to change, instead of at some distant call site.
var _ Store = keychain{}
var _ Annotator = keychain{}
var _ Sizer = keychain{}

func open() (Store, error) { return keychain{service: service}, nil }

func (k keychain) Kind() string { return "keychain" }

// maxLine is the buffer security(1) reads one interactive command into.
// Measured on macOS 15: a line of 4095 characters and its newline is accepted
// whole, and a longer one is truncated to that with no error anywhere. The
// ceiling is therefore a property of the whole command, not of the value --
// a longer secret name leaves fewer characters for the value it names.
const maxLine = 4096

// writePrefix is the write command up to and including the "-w " that the
// value follows. Splitting it out is what lets MaxValueFor price the value
// against the command that will actually carry it.
//
// Quoting is safe to do by hand here because nothing variable on this line
// needs it: the service is a constant, the value is base64, and the name has
// been through ValidName, so it holds only letters, digits, - and _. The
// label is the one argument with a space in it, and its shape is fixed. -j's
// argument is safe unquoted for the same reason: Encode's base64url output
// holds only letters, digits, - and _, which is the whole point of that
// encoding (see Provenance.Encode).
func (k keychain) writePrefix(name string, update bool, p Provenance) (string, error) {
	args := []string{
		"add-generic-password",
		"-s", k.service,
		"-a", name,
		// The label is what Keychain Access shows, so say whose item it is.
		"-l", `"brig: ` + name + `"`,
		"-D", `"brig secret"`,
	}
	switch {
	case !p.IsZero():
		encoded, err := p.Encode()
		if err != nil {
			return "", err
		}
		args = append(args, "-j", encoded)
	case update:
		// -U rewrites only the attributes named on the line, so leaving -j
		// off an update keeps whatever comment was already there. Measured on
		// macOS 15: the value changes and the comment does not. That is wrong
		// for every update, because the provenance describes the value being
		// replaced -- `brig secret update` on an imported credential would
		// otherwise keep the old expiresAt forever, and a freshly renewed
		// token would report as expired for as long as it existed. An empty
		// -j clears the comment to <NULL>, which DecodeProvenance already
		// reads back as absent.
		args = append(args, "-j", `""`)
	}
	if update {
		args = append(args, "-U")
	}
	// -w must be the last option: getopt stops at the first non-option, which
	// is also why no keychain can be named here and why the tests use the
	// default one.
	return strings.Join(append(args, "-w"), " ") + " ", nil
}

// assumedFromLen is what MaxValue prices against before it knows what
// provenance an import will attach: generous for a keychain service name or
// a file path, and pessimistic enough that Write's real ceiling --
// MaxValueFor, priced against the provenance actually being attached -- is
// never smaller than what this promised a caller that has not chosen one
// yet.
//
// That promise holds up to this length and no further, which is the honest
// limit of a fixed assumption. A longer locator makes MaxValueFor the smaller
// number, and the caller gets a refusal from Write carrying Write's own
// accurate ceiling -- never a write security truncates, which is the outcome
// this arithmetic exists to prevent.
const assumedFromLen = 128

// MaxValue is the Sizer's provenance-free ceiling: for a caller, such as a
// CLI pre-check, that wants to size a value before it has decided what
// provenance goes with it. It must not be the ceiling Write itself uses --
// see MaxValueFor and Write's own comment on why.
func (k keychain) MaxValue(name string, update bool) int {
	// ExpiresAt is set, not left at zero, because it is omitempty: a zero one
	// disappears from the encoded document and makes this ceiling LARGER than
	// the one Write will apply to a real import that carries an expiry --
	// which is the opposite of the promise assumedFromLen is written against.
	// The value is only a length; math.MaxInt64 is the longest it encodes to.
	return k.MaxValueFor(name, update, Provenance{
		V:         ProvenanceVersion,
		From:      strings.Repeat("x", assumedFromLen),
		ExpiresAt: math.MaxInt64,
	})
}

// MaxValueFor is the largest raw value that fits on the command line for this
// name together with this provenance. Base64 turns three bytes into four
// characters, so the budget divides by four and multiplies by three.
//
// Write prices against this, with the provenance it is actually about to
// attach, rather than against MaxValue's provenance-free number -- the
// comment now rides the same line as the value, so a ceiling that does not
// know about it would pass a write the line cannot carry.
func (k keychain) MaxValueFor(name string, update bool, p Provenance) int {
	prefix, err := k.writePrefix(name, update, p)
	if err != nil {
		// Encode fails only if json.Marshal does, which cannot happen for
		// this fixed shape. Reporting no room is safer than dividing by a
		// length that was never computed.
		return 0
	}
	return (maxLine - 1 - len(prefix)) / 4 * 3
}

// Write stores a value together with its provenance, creating or updating.
// Create and Update are this with the zero Provenance: on a create that is no
// -j at all, so a hand-created secret's comment is empty rather than a
// zero-value JSON document; on an update it is an empty -j, which clears any
// comment the previous value had. DecodeProvenance reads both back as absent.
//
// The value is base64-encoded because security's command line is line-based:
// a newline in a raw value would end the line early, so an SSH key or any
// binary blob could not round-trip. Encoding also makes the NUL byte and the
// non-UTF8 byte ordinary, and -- because base64 spells everything in letters,
// digits, +, / and = -- it leaves nothing on the line that would need quoting.
//
// The command goes to `security -i` on stdin rather than to argv, so the value
// stays out of `ps` exactly as the prompt form kept it out. The prompt form is
// not used because it truncates at 128 characters: an ANTHROPIC_API_KEY is
// about 108 bytes, which encodes to 144, and the excess was dropped silently
// on a multiple of four, so the short value still decoded cleanly and nothing
// failed anywhere. Interactive mode raises that ceiling to a whole line but
// does not remove it, which is why the length is checked below rather than
// trusted.
//
// The ceiling checked is MaxValueFor(name, update, p) -- priced against the
// provenance actually being attached -- and not MaxValue's provenance-free
// number. Pricing against the wrong one is the obvious minimal edit and it is
// wrong: the comment now shares the line with the value, so a value that
// alone fits the provenance-free budget can still push the whole line past
// security's buffer once a long provenance is appended. security answers
// that by truncating the line silently on a four-byte boundary, not by
// refusing it -- so the short value still base64-decodes, still resolves,
// and verify (below) cannot roll back an update that landed on top of a good
// value. Checking the real ceiling here is what turns that into a refusal
// before anything is sent to security at all.
func (k keychain) Write(name string, value []byte, p Provenance, update bool) error {
	if max := k.MaxValueFor(name, update, p); len(value) > max {
		return fmt.Errorf("the value for %q is %d bytes, and with its provenance the keychain takes at most %d",
			name, len(value), max)
	}
	prefix, err := k.writePrefix(name, update, p)
	if err != nil {
		return err
	}
	line := prefix + base64.StdEncoding.EncodeToString(value)
	cmd := exec.Command(securityBin, "-i")
	cmd.Stdin = strings.NewReader(line + "\n")
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &bytes.Buffer{}, &errb
	if err := cmd.Run(); err != nil {
		if status(err) == codeDuplicate {
			return ErrExists
		}
		return securityError(err, errb.String())
	}
	return k.verify(name, value, update)
}

// write is Create and Update's path: the zero Provenance, so a plain secret
// created before this field existed -- or created by hand -- carries no
// comment at all rather than one describing an absent source.
func (k keychain) write(name string, value []byte, update bool) error {
	return k.Write(name, value, Provenance{}, update)
}

// verify reads back what write just stored.
//
// The length check above rests on brig's arithmetic agreeing with security's,
// and security answers a line it cannot fit by shortening it rather than by
// refusing it -- so the failure this guards against is silent by construction.
// One extra decrypt on a write a person typed is a cheap way to make "stored"
// mean it.
//
// A create that stored the wrong thing is removed, because a caller told the
// write failed will reasonably expect nothing to be there. An update cannot be
// undone that way: the previous value is already gone, so it only reports.
func (k keychain) verify(name string, value []byte, update bool) error {
	stored, err := k.Read(name)
	if err != nil {
		return fmt.Errorf("%q was written but could not be read back: %w", name, err)
	}
	if bytes.Equal(stored, value) {
		return nil
	}
	if !update {
		_ = k.Delete(name)
	}
	return fmt.Errorf("the keychain stored %d bytes of the %d given for %q, so the value was truncated",
		len(stored), len(value), name)
}

func (k keychain) Create(name string, value []byte) error {
	if err := ValidName(name); err != nil {
		return err
	}
	return k.write(name, value, false)
}

func (k keychain) Read(name string) ([]byte, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	cmd := exec.Command(securityBin, "find-generic-password",
		"-s", k.service, "-a", name, "-w")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if status(err) == codeNotFound {
			return nil, ErrNotFound
		}
		return nil, securityError(err, errb.String())
	}
	// -w prints the value and a newline of its own.
	raw, err := base64.StdEncoding.DecodeString(strings.TrimRight(out.String(), "\n"))
	if err != nil {
		// Reachable for an item something other than brig put in the
		// namespace. Without the name and the "brig's encoding" part, the
		// caller gets a byte offset into a string they never supplied.
		return nil, fmt.Errorf("the value stored for %q is not in brig's encoding, so brig did not write it: %w",
			name, err)
	}
	return raw, nil
}

// Update refuses to create.
//
// -U on an absent item creates it silently, so the existence check has to
// happen here. The check-then-write window is real but harmless: the only way
// to lose it is to delete the secret between the two calls, and the outcome
// is a secret that exists again rather than damage.
func (k keychain) Update(name string, value []byte) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if err := k.exists(name); err != nil {
		return err
	}
	return k.write(name, value, true)
}

// exists reports whether a secret is there without decrypting it: no -w, so
// this reads attributes only.
func (k keychain) exists(name string) error {
	cmd := exec.Command(securityBin, "find-generic-password",
		"-s", k.service, "-a", name)
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &bytes.Buffer{}, &errb
	if err := cmd.Run(); err != nil {
		if status(err) == codeNotFound {
			return ErrNotFound
		}
		return securityError(err, errb.String())
	}
	return nil
}

func (k keychain) Delete(name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	cmd := exec.Command(securityBin, "delete-generic-password",
		"-s", k.service, "-a", name)
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &bytes.Buffer{}, &errb
	if err := cmd.Run(); err != nil {
		if status(err) == codeNotFound {
			return ErrNotFound
		}
		return securityError(err, errb.String())
	}
	return nil
}

// List reads brig's namespace out of the keychain dump.
//
// dump-keychain without -d prints attributes only and raises no access
// prompt, which is what lets brig list secrets without an index file of its
// own -- and so without anything that could drift out of step with the store.
func (k keychain) List() ([]Secret, error) {
	cmd := exec.Command(securityBin, "dump-keychain")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, securityError(err, errb.String())
	}
	return parseDump(out.String(), k.service), nil
}

// parseDump pulls the generic passwords of one service out of a keychain
// dump. Items are blocks introduced by a "keychain:" line.
func parseDump(dump, service string) []Secret {
	var list []Secret
	for _, block := range strings.Split(dump, "\nkeychain: ") {
		// Only generic passwords carry a service and an account; the other
		// classes render their attributes as bare hex keys.
		if !strings.Contains(block, `class: "genp"`) {
			continue
		}
		if attr(block, "svce") != service {
			continue
		}
		// A name outside brig's grammar is one brig did not write, and one it
		// would refuse to read or remove. Listing it would offer the reader a
		// secret that every other verb then declines to touch.
		name := attr(block, "acct")
		if ValidName(name) != nil {
			continue
		}
		list = append(list, Secret{Name: name, Modified: modified(block), Provenance: provenance(block)})
	}
	slices.SortFunc(list, func(a, b Secret) int { return strings.Compare(a.Name, b.Name) })
	return list
}

// attr reads a blob attribute: `    "svce"<blob>="value"`. An attribute that
// is NULL, or rendered as hex because it is not printable, yields "" -- and
// brig's own names are letters, digits, - and _, so they never are.
func attr(block, key string) string {
	for _, line := range strings.Split(block, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), `"`+key+`"`)
		if !ok {
			continue
		}
		_, value, ok := strings.Cut(rest, `="`)
		if !ok {
			return ""
		}
		return strings.TrimSuffix(value, `"`)
	}
	return ""
}

// provenance reads the icmt attribute through DecodeProvenance, which reads
// attributes only -- no -d, no decrypt, no keychain-access prompt -- and
// answers false for anything brig did not write: an item another process
// planted in the namespace, or one written before this field existed. Either
// way the zero value is what List reports, the same contract Modified
// follows below.
func provenance(block string) Provenance {
	p, ok := DecodeProvenance(attr(block, "icmt"))
	if !ok {
		return Provenance{}
	}
	return p
}

// modified reads the mdat attribute, which prints as hex followed by the same
// bytes rendered: `"mdat"<timedate>=0x3230...  "20260814162330Z\000"`. The hex
// is decoded rather than the rendering parsed, because the rendering is the
// part that would change if security ever tidied its output.
//
// An item without the attribute yields the zero time, which is a value
// callers must render as absent rather than replace with a guess. See
// Secret.Modified.
func modified(block string) time.Time {
	for _, line := range strings.Split(block, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), `"mdat"`) {
			continue
		}
		_, rest, ok := strings.Cut(line, "=0x")
		if !ok {
			return time.Time{}
		}
		raw, _, _ := strings.Cut(rest, " ")
		b, err := hex.DecodeString(strings.TrimSpace(raw))
		if err != nil {
			return time.Time{}
		}
		t, err := time.Parse("20060102150405Z", strings.TrimRight(string(b), "\x00"))
		if err != nil {
			return time.Time{}
		}
		return t
	}
	return time.Time{}
}

// status is the exit code of a failed command, or -1 when it never ran.
func status(err error) int {
	var e *exec.ExitError
	if errors.As(err, &e) {
		return e.ExitCode()
	}
	return -1
}

// securityError keeps security's own explanation, which is the only account
// of anything brig does not have a code for -- a locked keychain, a denied
// access dialog.
//
// The last "security: " line is the one worth keeping. Taking the first line
// instead picked up whatever shared it: the prompt form wrote "password data
// for new item: retype password for new item: " with no newline before the
// real message, and interactive mode follows the message with its own
// "add-generic-password: returned -25299".
//
// The original error is wrapped rather than replaced, so the *exec.ExitError
// and its code stay reachable through errors.As for a caller that wants to
// tell one failure from another.
func securityError(err error, stderr string) error {
	// Searched within the line rather than anchored at its start, because the
	// prompt form printed no newline before its message and so shared a line
	// with it.
	var msg string
	for _, line := range strings.Split(stderr, "\n") {
		if i := strings.LastIndex(line, "security: "); i >= 0 {
			msg = strings.TrimSpace(line[i+len("security: "):])
		}
	}
	if msg == "" {
		return err
	}
	return fmt.Errorf("security: %s: %w", msg, err)
}
