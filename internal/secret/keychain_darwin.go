//go:build darwin

package secret

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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

type keychain struct {
	service string
	// beforeSeal, when set, runs before every sealed write. Tests use it to
	// make that write fail at the one point a create has to roll back.
	beforeSeal func() error
}

// Caught at build time rather than wherever a keychain first gets assigned to
// a Store: a method that stops matching the interface fails here, in the file
// that has to change, instead of at some distant call site.
var _ Store = keychain{}
var _ Annotator = keychain{}

func open() (Store, error) { return keychain{service: service}, nil }

func (k keychain) Kind() string { return "keychain" }

// maxLine is the buffer security(1) reads one interactive command into.
// Measured on macOS 15: a line of 4095 characters and its newline is accepted
// whole, and a longer one is truncated to that with no error anywhere. The
// value used to ride this line, which put a ceiling of about 3 KB on it. It
// no longer does: the line carries a fixed-size key (see sealed_darwin.go),
// and TestKeyLineNeverNearsTheBuffer pins that the longest possible key line
// stays well inside this.
const maxLine = 4096

// writePrefix is the write command up to and including the "-w " that the
// key line follows. It is a function of its own because putItem builds the
// line in two parts, and TestKeyLineNeverNearsTheBuffer measures this part
// against security's buffer without a key.
//
// Quoting is safe to do by hand here because nothing variable on this line
// needs it: the service is a constant, the key is base64, and the name has
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

// Write stores a value together with its provenance, creating or updating.
// Create and Update are this with the zero Provenance: on a create that is no
// -j at all, so a hand-created secret's comment is empty rather than a
// zero-value JSON document; on an update it is an empty -j, which clears any
// comment the previous value had. DecodeProvenance reads both back as absent.
//
// The value does not go on security's command line, in either direction.
// The key item is written through `security -i` on stdin, the same path
// every value used to take, and the sealed item through argv, where the
// line has no ceiling and what stands there is ciphertext. See
// sealed_darwin.go.
//
// The order of the two writes is different on a create and an update, and
// each order is chosen for what a failure between them leaves behind.
//
// A create writes the key item first. security refuses a duplicate with its
// own exit code, so a taken name is refused before anything else is
// touched. If the sealed item then cannot be written, the key item is
// removed again: a caller told the create failed expects nothing to be
// there.
//
// An update writes the sealed item first, under the key the item already
// holds, and only then rewrites the key item to carry the new provenance.
// If the second step fails the value is new and the provenance is old. That
// is the safer direction: an old provenance can only make the stale-
// credential warning fire early, while a new provenance over an old value
// would keep it quiet. The key never changes on an update, so the sealed
// item's replacement, which security -U does whole, is the one step that
// changes the value.
//
// An item written before this change holds the value itself. An update of
// one draws a key, writes the sealed item, and then replaces the item with
// the key. If that last step fails the old item still holds the old value
// and Read still returns it, and the sealed item is replaced by the next
// update. Two updates of such an item at once can draw two keys and land
// them crosswise, key item under one and sealed item under the other; both
// writers then fail their read-back, Read reports the secret as damaged,
// and one more update or import repairs it. Once an item holds a key every
// concurrent update shares it, so the window is that first move only.
func (k keychain) Write(name string, value []byte, p Provenance, update bool) error {
	key, err := k.keyFor(name, update)
	if err != nil {
		return err
	}
	blob, err := seal(name, key, value)
	if err != nil {
		return err
	}
	if update {
		if err := k.putSealed(name, blob); err != nil {
			return err
		}
		if err := k.putItem(name, keyItem(key), p, true); err != nil {
			return err
		}
		return k.verify(name, value, true)
	}
	if err := k.putItem(name, keyItem(key), p, false); err != nil {
		return err
	}
	if err := k.putSealed(name, blob); err != nil {
		_ = k.deleteItem(name)
		return err
	}
	return k.verify(name, value, false)
}

// keyFor is the key a write seals under: the one the item already holds
// on an update, a fresh one on a create. An update of a pre-sealing item,
// or of a marked item that holds no key, draws a fresh one too: the
// sealed item written under it is what makes the secret whole again.
func (k keychain) keyFor(name string, update bool) ([]byte, error) {
	if !update {
		return newKey()
	}
	line, err := k.readItem(name)
	if err != nil {
		return nil, err
	}
	if key, _ := keyFromItem(line); key != nil {
		return key, nil
	}
	return newKey()
}

// security runs one security(1) command and returns its stdout. stdin is
// nil for a command that reads none. A failure carries security's own
// explanation and, through errors.As, its exit code, which status reads.
func (k keychain) security(stdin io.Reader, args ...string) (string, error) {
	cmd := exec.Command(securityBin, args...)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", securityError(err, errb.String())
	}
	return out.String(), nil
}

// putItem stores line as the keychain item for name, through `security -i`
// on stdin, so it stays out of argv exactly as the value used to.
//
// The line is a marker and a base64 key, so it holds only letters, digits,
// +, / and = beyond the colon: nothing on security's command line needs
// quoting, and no byte of it can end the line early. The prompt form is
// not used because it truncates at 128 characters.
func (k keychain) putItem(name, line string, p Provenance, update bool) error {
	prefix, err := k.writePrefix(name, update, p)
	if err != nil {
		return err
	}
	_, err = k.security(strings.NewReader(prefix+line+"\n"), "-i")
	if status(err) == codeDuplicate {
		return ErrExists
	}
	return err
}

// sealedArgs is the argv of the sealed write: security's own command, with
// the base64 ciphertext as the last argument. Always -U, so a sealed item
// an interrupted create left behind is replaced rather than refused.
// Nothing on this line needs quoting: it is argv, not a shell line.
func (k keychain) sealedArgs(name string, blob []byte) []string {
	return []string{
		"add-generic-password",
		"-s", k.service,
		"-a", sealedAccount(name),
		"-l", "brig: " + name + " (sealed)",
		"-D", "brig sealed value",
		"-U",
		"-w", base64.StdEncoding.EncodeToString(blob),
	}
}

// putSealed stores blob as the sealed item for name, through argv.
func (k keychain) putSealed(name string, blob []byte) error {
	if k.beforeSeal != nil {
		if err := k.beforeSeal(); err != nil {
			return err
		}
	}
	_, err := k.security(nil, k.sealedArgs(name, blob)...)
	return err
}

// write is Create and Update's path: the zero Provenance, so a plain secret
// created before this field existed -- or created by hand -- carries no
// comment at all rather than one describing an absent source.
func (k keychain) write(name string, value []byte, update bool) error {
	return k.Write(name, value, Provenance{}, update)
}

// verify reads back what write just stored.
//
// The write is two items, and this is the one check that both landed and
// agree: the key item holds the key that opens the sealed item, and the
// sealed item holds the value that was given. One extra decrypt on a write
// a person typed is a cheap way to make "stored" mean it.
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
	return fmt.Errorf("%q read back as %d bytes, not the %d given, so the write did not land",
		name, len(stored), len(value))
}

func (k keychain) Create(name string, value []byte) error {
	if err := ValidName(name); err != nil {
		return err
	}
	return k.write(name, value, false)
}

func (k keychain) Read(name string) ([]byte, error) {
	line, err := k.readItem(name)
	if err != nil {
		return nil, err
	}
	key, err := keyFromItem(line)
	if err != nil {
		return nil, fmt.Errorf("%q %w: %v. Store it again: brig secret update %s, or brig secret import",
			name, ErrDamaged, err, name)
	}
	if key == nil {
		// Written before this change: the item is the value.
		value, err := base64.StdEncoding.DecodeString(line)
		if err != nil {
			// Reachable for an item something other than brig put in the
			// namespace. Without the name and the "brig's encoding" part,
			// the caller gets a byte offset into a string they never
			// supplied.
			return nil, fmt.Errorf("the value stored for %q is not in brig's encoding, so brig did not write it: %w",
				name, err)
		}
		return value, nil
	}
	sealedLine, err := k.readLine(sealedAccount(name))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Not ErrNotFound: the secret exists, and half of it is gone.
			// Reporting absence would let an import write a new key over
			// this one without anyone learning the sealed item had been
			// removed.
			return nil, fmt.Errorf("%q %w: the key is in the keychain, but its sealed value is missing. "+
				"Store it again: brig secret update %s, or brig secret import", name, ErrDamaged, name)
		}
		return nil, err
	}
	blob, err := base64.StdEncoding.DecodeString(sealedLine)
	if err == nil {
		blob, err = unseal(name, key, blob)
	}
	switch {
	case err == nil:
		return blob, nil
	case errors.Is(err, errNotSealed) || !isAuthError(err):
		return nil, fmt.Errorf("%q %w: the sealed item is %v, so something other than brig put it there. "+
			"Store it again: brig secret update %s, or brig secret import", name, ErrDamaged, errNotSealed, name)
	default:
		return nil, fmt.Errorf("%q %w: the sealed item does not open with the key stored for it, so one of "+
			"them was changed outside brig. Store it again: brig secret update %s, or brig secret import",
			name, ErrDamaged, name)
	}
}

// readItem returns the line the key item holds for name, as stored: a
// marked key for a sealed secret, base64 of the value for one written
// before sealing.
func (k keychain) readItem(name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	return k.readLine(name)
}

// readLine is the -w read of one account.
func (k keychain) readLine(account string) (string, error) {
	out, err := k.security(nil, "find-generic-password", "-s", k.service, "-a", account, "-w")
	if status(err) == codeNotFound {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	// -w prints the value and a newline of its own.
	return strings.TrimRight(out, "\n"), nil
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
	// No existence probe of its own: Write reads the key item first, and
	// an absent one comes back as ErrNotFound from there.
	return k.write(name, value, true)
}

// Delete removes the key item and the sealed item. The key item goes
// first: once it is gone the sealed item is ciphertext nothing can open, so
// a failure between the two leaves nothing readable behind. A sealed item
// with no key item, left by a create that failed between its two steps, is
// swept up here too, and the caller still learns there was no secret.
func (k keychain) Delete(name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	itemErr := k.deleteItem(name)
	if itemErr != nil && !errors.Is(itemErr, ErrNotFound) {
		return itemErr
	}
	if err := k.deleteItem(sealedAccount(name)); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return itemErr
}

// isAuthError reports whether an unseal failure is the cipher refusing the
// key or the bytes, rather than a shape the cipher never saw.
func isAuthError(err error) bool {
	return err != nil && !errors.Is(err, errNotSealed)
}

// deleteItem removes one account's item.
func (k keychain) deleteItem(account string) error {
	_, err := k.security(nil, "delete-generic-password", "-s", k.service, "-a", account)
	if status(err) == codeNotFound {
		return ErrNotFound
	}
	return err
}

// List reads brig's namespace out of the keychain dump.
//
// dump-keychain without -d prints attributes only and raises no access
// prompt, which is what lets brig list secrets without an index file of its
// own -- and so without anything that could drift out of step with the store.
func (k keychain) List() ([]Secret, error) {
	out, err := k.security(nil, "dump-keychain")
	if err != nil {
		return nil, err
	}
	return parseDump(out, k.service), nil
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
