//go:build darwin

package secret

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
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

func open() (Store, error) { return keychain{service: service}, nil }

func (k keychain) Kind() string { return "keychain" }

// maxLine is the buffer security(1) reads one interactive command into.
// Measured on macOS 15: a line of 4095 characters and its newline is accepted
// whole, and a longer one is truncated to that with no error anywhere. The
// ceiling is therefore a property of the whole command, not of the value --
// a longer secret name leaves fewer characters for the value it names.
const maxLine = 4096

// writePrefix is the write command up to and including the "-w " that the
// value follows. Splitting it out is what lets maxValue price the value
// against the command that will actually carry it.
//
// Quoting is safe to do by hand here because nothing variable on this line
// needs it: the service is a constant, the value is base64, and the name has
// been through ValidName, so it holds only letters, digits, - and _. The
// label is the one argument with a space in it, and its shape is fixed.
func (k keychain) writePrefix(name string, update bool) string {
	args := []string{
		"add-generic-password",
		"-s", k.service,
		"-a", name,
		// The label is what Keychain Access shows, so say whose item it is.
		"-l", `"brig: ` + name + `"`,
		"-D", `"brig secret"`,
	}
	if update {
		args = append(args, "-U")
	}
	// -w must be the last option: getopt stops at the first non-option, which
	// is also why no keychain can be named here and why the tests use the
	// default one.
	return strings.Join(append(args, "-w"), " ") + " "
}

// maxValue is the largest raw value that fits on the command line for this
// name. Base64 turns three bytes into four characters, so the budget divides
// by four and multiplies by three.
func (k keychain) maxValue(name string, update bool) int {
	return (maxLine - 1 - len(k.writePrefix(name, update))) / 4 * 3
}

// write stores a value, creating or updating.
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
func (k keychain) write(name string, value []byte, update bool) error {
	if max := k.maxValue(name, update); len(value) > max {
		return fmt.Errorf("the value for %q is %d bytes, and the keychain takes at most %d",
			name, len(value), max)
	}
	line := k.writePrefix(name, update) + base64.StdEncoding.EncodeToString(value)
	cmd := exec.Command("security", "-i")
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
	cmd := exec.Command("security", "find-generic-password",
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
	cmd := exec.Command("security", "find-generic-password",
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
	cmd := exec.Command("security", "delete-generic-password",
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
	cmd := exec.Command("security", "dump-keychain")
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
		list = append(list, Secret{Name: name, Modified: modified(block)})
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
