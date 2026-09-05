//go:build linux

package secret

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// service is brig's own namespace in the keyring. Every item this package
// touches carries it as an attribute, so brig never reaches outside its own
// namespace and List can search by it and find nothing else.
//
// That is the same narrow guarantee the keychain's service name buys, and the
// difference matters to anyone building a trust decision on it: it is a label,
// not an authenticity check. Any process on the session bus can add an item
// under it, and brig would then read, update and delete that item as if it
// were its own. What the namespace buys is containment of brig, not provenance
// of what it finds there.
const service = "sh.brig.secret"

// The Secret Service lives at a well-known name and path on the session bus.
const (
	secretsName = "org.freedesktop.secrets"
	secretsPath = dbus.ObjectPath("/org/freedesktop/secrets")
)

// The interfaces and members this backend speaks, named as constants so a typo
// is a build error at the const rather than a runtime "unknown method" from the
// far side of the bus.
const (
	serviceIface = "org.freedesktop.Secret.Service"
	collIface    = "org.freedesktop.Secret.Collection"
	itemIface    = "org.freedesktop.Secret.Item"
	promptIface  = "org.freedesktop.Secret.Prompt"

	methodOpenSession = serviceIface + ".OpenSession"
	methodReadAlias   = serviceIface + ".ReadAlias"
	methodSearchItems = serviceIface + ".SearchItems"
	methodUnlock      = serviceIface + ".Unlock"
	methodCreateItem  = collIface + ".CreateItem"
	methodGetSecret   = itemIface + ".GetSecret"
	methodSetSecret   = itemIface + ".SetSecret"
	methodDeleteItem  = itemIface + ".Delete"
	methodPrompt      = promptIface + ".Prompt"
	memberCompleted   = "Completed"

	propAttributes = itemIface + ".Attributes"
	propModified   = itemIface + ".Modified"
)

// defaultAlias is the collection brig stores into: whichever one the keyring
// has aliased as the user's default (gnome-keyring calls it "login"). The alias
// path is where ReadAlias points when a default is set.
const (
	defaultAlias     = "default"
	defaultAliasPath = dbus.ObjectPath("/org/freedesktop/secrets/aliases/default")
)

// noObject is the object path the Secret Service returns to mean "none": no
// prompt is needed, or no default collection exists. It is a bare "/".
const noObject = dbus.ObjectPath("/")

// The item attribute keys brig writes. service and name identify an item as
// brig's and are what List and the existence checks search on; provenance
// carries the encoded document provenance.go already defines, so a re-import's
// expiry survives exactly as it does in the keychain comment on macOS.
const (
	attrKeyService    = "service"
	attrKeyName       = "name"
	attrKeyProvenance = "provenance"
)

// contentType labels the stored bytes on the wire. Values are arbitrary bytes
// -- an SSH key, a binary blob -- so it is octet-stream rather than text.
const contentType = "application/octet-stream"

// dbusSecret is the (oayays) struct the Secret Service passes values in: the
// session the value is (un)encrypted against, the algorithm parameters (empty
// for the plain session brig opens), the value itself, and its content type.
type dbusSecret struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

// secretService is the Store backed by the freedesktop Secret Service.
type secretService struct {
	service string
	conn    *dbus.Conn
	session dbus.ObjectPath
}

// Caught at build time rather than wherever a secretService first gets assigned
// to a Store: a method that stops matching the interface fails here, in the
// file that has to change. Sizer is deliberately absent -- the value travels
// over D-Bus, not on a command line, so there is no line-length ceiling to
// price against, and secretimport.go falls back cleanly when a backend is not a
// Sizer.
var (
	_ Store     = (*secretService)(nil)
	_ Annotator = (*secretService)(nil)
)

// dialBus and hasSecretService are the two steps open() can fail at, held as
// variables so a test can force either failure. The executor these tests run on
// has no session bus, so open() could otherwise never reach its own error
// messages.
var (
	dialBus = func() (*dbus.Conn, error) { return dbus.SessionBus() }

	hasSecretService = func(conn *dbus.Conn) bool {
		var has bool
		if err := conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, secretsName).Store(&has); err != nil {
			return false
		}
		if has {
			return true
		}
		// A keyring can be installed and D-Bus-activatable without being
		// running yet; the daemon will start it on first use. Treat that as
		// present so brig prompts rather than declines.
		var names []string
		if err := conn.BusObject().Call("org.freedesktop.DBus.ListActivatableNames", 0).Store(&names); err != nil {
			return false
		}
		return slices.Contains(names, secretsName)
	}
)

func open() (Store, error) { return openService(service) }

// openService is open() with the namespace as an argument, so the integration
// test can run under a service value of its own and never touch a real brig
// secret.
func openService(service string) (*secretService, error) {
	conn, err := dialBus()
	if err != nil {
		return nil, errNoBus(err)
	}
	if !hasSecretService(conn) {
		return nil, errNoService()
	}
	s := &secretService{service: service, conn: conn}
	if err := s.openSession(); err != nil {
		return nil, err
	}
	return s, nil
}

// errNoBus and errNoService are the two shapes of the same refusal, and each
// says which half is missing and both ways out: install a keyring, or bind the
// secret to a command that holds no plaintext at rest. Both fixes appear in one
// message the way the darwin messages read, because a user who cannot install a
// keyring still has the second door.
func errNoBus(err error) error {
	return fmt.Errorf("%w: no D-Bus session bus to reach a keyring on. Install a keyring "+
		"(gnome-keyring or KWallet, both speak the Secret Service API) and log in to a session "+
		"that starts it, or bind the secret to a command instead with "+
		"`brig secret import <profile> --from-command '<sh>'`, which holds no plaintext at rest: %v",
		ErrUnsupported, err)
}

func errNoService() error {
	return fmt.Errorf("%w: a D-Bus session bus is running but no Secret Service answers on it. "+
		"Install a keyring (gnome-keyring or KWallet, both speak the Secret Service API) and log in "+
		"to a session that starts it, or bind the secret to a command instead with "+
		"`brig secret import <profile> --from-command '<sh>'`, which holds no plaintext at rest",
		ErrUnsupported)
}

func (s *secretService) Kind() string { return "secret-service" }

// openSession opens a plain (unencrypted) transport session. brig is a local
// process talking to a local keyring over the user's own session bus, so the
// DH-encrypted session the API also offers guards a channel that is already
// inside the trust boundary; the plain session keeps the code the value round
// trips through small, which is the property that matters for a credential.
func (s *secretService) openSession() error {
	var output dbus.Variant
	var session dbus.ObjectPath
	err := s.service0().Call(methodOpenSession, 0, "plain", dbus.MakeVariant("")).Store(&output, &session)
	if err != nil {
		return fmt.Errorf("opening a Secret Service session: %w", err)
	}
	s.session = session
	return nil
}

// service0 is the Secret Service object; item0 and collection0 are an item and
// a collection by path. The names keep the call sites reading as the interface
// they speak rather than as bus plumbing.
func (s *secretService) service0() dbus.BusObject {
	return s.conn.Object(secretsName, secretsPath)
}

func (s *secretService) object(path dbus.ObjectPath) dbus.BusObject {
	return s.conn.Object(secretsName, path)
}

// Create stores a new secret with no provenance, and returns ErrExists if the
// name is taken.
func (s *secretService) Create(name string, value []byte) error {
	if err := ValidName(name); err != nil {
		return err
	}
	return s.write(name, value, Provenance{}, false)
}

// Update replaces an existing value, keeping no provenance, and returns
// ErrNotFound rather than creating: an update naming a secret that is not there
// is a typo, and creating it silently would hide that.
func (s *secretService) Update(name string, value []byte) error {
	if err := ValidName(name); err != nil {
		return err
	}
	return s.write(name, value, Provenance{}, true)
}

// Write is the Annotator arm: create or update carrying provenance, so an
// imported credential's source and expiry ride on the item's attributes the way
// they ride in the keychain comment on macOS. A zero Provenance writes no
// provenance attribute, which is how a hand-created secret reads back as absent.
func (s *secretService) Write(name string, value []byte, p Provenance, update bool) error {
	if err := ValidName(name); err != nil {
		return err
	}
	return s.write(name, value, p, update)
}

// write is the shared create/update path: unlock the collection, decide against
// what is already there, then either create a fresh item or set the value and
// attributes of the existing one.
//
// Create and Update are separate calls rather than one CreateItem(replace)
// because the Secret Service matches "the same item" on the whole attribute
// set: an update that changed the provenance would not match the old item and
// would leave two behind. Setting the value and attributes of the item found by
// name is what keeps an update in place -- and never the delete-then-create
// shape secretimport.go warns against.
func (s *secretService) write(name string, value []byte, p Provenance, update bool) error {
	collPath, err := s.defaultCollection()
	if err != nil {
		return err
	}
	if err := s.unlock(collPath); err != nil {
		return err
	}
	existing, err := s.find(name)
	if err != nil {
		return err
	}
	switch {
	case update && existing == noObject:
		return ErrNotFound
	case !update && existing != noObject:
		return ErrExists
	}
	attrs, err := attributes(s.service, name, p)
	if err != nil {
		return err
	}
	sec := dbusSecret{Session: s.session, Parameters: []byte{}, Value: value, ContentType: contentType}
	if update {
		return s.replace(existing, attrs, sec)
	}
	return s.create(collPath, name, attrs, sec)
}

func (s *secretService) create(collPath dbus.ObjectPath, name string, attrs map[string]string, sec dbusSecret) error {
	props := map[string]dbus.Variant{
		// The label is what a keyring UI shows, so say whose item it is.
		itemIface + ".Label":      dbus.MakeVariant("brig: " + name),
		itemIface + ".Attributes": dbus.MakeVariant(attrs),
	}
	var item, prompt dbus.ObjectPath
	// replace=false: the collision has already been ruled out above, and a
	// concurrent create racing this one should surface rather than clobber.
	err := s.object(collPath).Call(methodCreateItem, 0, props, sec, false).Store(&item, &prompt)
	if err != nil {
		return fmt.Errorf("storing %q: %w", name, err)
	}
	return s.await(prompt)
}

// replace sets the value and rewrites the whole attribute set of an existing
// item. Rewriting all the attributes -- not only the value -- is what clears a
// stale provenance on a hand update: attrs omits the provenance key when the
// Provenance is zero, so the old expiry does not outlive the value it described,
// matching the keychain's clear-on-update behaviour.
func (s *secretService) replace(item dbus.ObjectPath, attrs map[string]string, sec dbusSecret) error {
	obj := s.object(item)
	if err := obj.Call(methodSetSecret, 0, sec).Err; err != nil {
		return fmt.Errorf("updating the value: %w", err)
	}
	if err := obj.SetProperty(propAttributes, dbus.MakeVariant(attrs)); err != nil {
		return fmt.Errorf("updating the attributes: %w", err)
	}
	return nil
}

// Read returns the value, or ErrNotFound.
func (s *secretService) Read(name string) ([]byte, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	item, err := s.find(name)
	if err != nil {
		return nil, err
	}
	if item == noObject {
		return nil, ErrNotFound
	}
	if err := s.unlock(item); err != nil {
		return nil, err
	}
	var sec dbusSecret
	if err := s.object(item).Call(methodGetSecret, 0, s.session).Store(&sec); err != nil {
		return nil, fmt.Errorf("reading %q: %w", name, err)
	}
	return sec.Value, nil
}

// Delete removes a secret, or returns ErrNotFound.
func (s *secretService) Delete(name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	item, err := s.find(name)
	if err != nil {
		return err
	}
	if item == noObject {
		return ErrNotFound
	}
	var prompt dbus.ObjectPath
	if err := s.object(item).Call(methodDeleteItem, 0).Store(&prompt); err != nil {
		return fmt.Errorf("deleting %q: %w", name, err)
	}
	return s.await(prompt)
}

// List returns every secret in brig's namespace, sorted by name. It searches on
// the service attribute alone, reads each item's attributes and Modified
// property -- which need no unlock, so listing raises no prompt -- and never
// reads a value.
func (s *secretService) List() ([]Secret, error) {
	items, err := s.search(map[string]string{attrKeyService: s.service})
	if err != nil {
		return nil, err
	}
	var list []Secret
	for _, path := range items {
		attrs, err := s.attributes(path)
		if err != nil {
			return nil, err
		}
		name := attrs[attrKeyName]
		// A name outside brig's grammar is one brig did not write, and one it
		// would refuse to read or remove. Listing it would offer the reader a
		// secret every other verb then declines to touch.
		if ValidName(name) != nil {
			continue
		}
		list = append(list, Secret{
			Name:       name,
			Modified:   s.modified(path),
			Provenance: provenanceFromAttributes(attrs),
		})
	}
	slices.SortFunc(list, func(a, b Secret) int { return strings.Compare(a.Name, b.Name) })
	return list, nil
}

// find returns the item matching brig's service and this name, or noObject when
// there is none. A name is unique within the namespace, so the first match is
// the answer.
func (s *secretService) find(name string) (dbus.ObjectPath, error) {
	items, err := s.search(map[string]string{attrKeyService: s.service, attrKeyName: name})
	if err != nil {
		return noObject, err
	}
	if len(items) == 0 {
		return noObject, nil
	}
	return items[0], nil
}

// search returns every item, locked or not, matching the given attributes.
// Locked items count: existence and listing must not depend on the collection
// being unlocked, or a locked keyring would read as empty.
func (s *secretService) search(attrs map[string]string) ([]dbus.ObjectPath, error) {
	var unlocked, locked []dbus.ObjectPath
	if err := s.service0().Call(methodSearchItems, 0, attrs).Store(&unlocked, &locked); err != nil {
		return nil, fmt.Errorf("searching the keyring: %w", err)
	}
	return append(unlocked, locked...), nil
}

// attributes reads an item's attribute map through the Properties interface,
// which does not decrypt the value and raises no prompt.
func (s *secretService) attributes(item dbus.ObjectPath) (map[string]string, error) {
	v, err := s.object(item).GetProperty(propAttributes)
	if err != nil {
		return nil, fmt.Errorf("reading item attributes: %w", err)
	}
	attrs, ok := v.Value().(map[string]string)
	if !ok {
		return map[string]string{}, nil
	}
	return attrs, nil
}

// modified reads the item's Modified property -- Unix seconds -- and answers the
// zero time for an item that carries none. A backend that cannot supply a
// modification time returns the zero value rather than inventing one; see
// Secret.Modified.
func (s *secretService) modified(item dbus.ObjectPath) time.Time {
	v, err := s.object(item).GetProperty(propModified)
	if err != nil {
		return time.Time{}
	}
	secs, ok := v.Value().(uint64)
	if !ok || secs == 0 {
		return time.Time{}
	}
	return time.Unix(int64(secs), 0)
}

// defaultCollection is the path of the collection brig stores into: the alias
// the keyring points at, or the well-known alias path when ReadAlias reports
// none, which is the path the keyring will have created the default under.
func (s *secretService) defaultCollection() (dbus.ObjectPath, error) {
	var path dbus.ObjectPath
	if err := s.service0().Call(methodReadAlias, 0, defaultAlias).Store(&path); err != nil {
		return noObject, fmt.Errorf("finding the default keyring collection: %w", err)
	}
	if path == noObject {
		return defaultAliasPath, nil
	}
	return path, nil
}

// unlock unlocks an object if it is locked, prompting the user through their
// keyring UI when the service asks for one. Unlock on an already-unlocked
// object is a no-op that returns no prompt, so this is safe to call before
// every operation.
func (s *secretService) unlock(obj dbus.ObjectPath) error {
	var unlocked []dbus.ObjectPath
	var prompt dbus.ObjectPath
	if err := s.service0().Call(methodUnlock, 0, []dbus.ObjectPath{obj}).Store(&unlocked, &prompt); err != nil {
		return fmt.Errorf("unlocking the keyring: %w", err)
	}
	return s.await(prompt)
}

// await drives a prompt to completion when the service returns one, and is a
// no-op for noObject. The prompt is where the keyring UI asks the user to
// unlock or to confirm; brig waits on its Completed signal rather than
// returning while the dialog is still open, because the operation that asked
// for the prompt has not happened until it closes.
func (s *secretService) await(prompt dbus.ObjectPath) error {
	if prompt == noObject {
		return nil
	}
	if err := s.conn.AddMatchSignal(
		dbus.WithMatchObjectPath(prompt),
		dbus.WithMatchInterface(promptIface),
		dbus.WithMatchMember(memberCompleted),
	); err != nil {
		return fmt.Errorf("watching the keyring prompt: %w", err)
	}
	ch := make(chan *dbus.Signal, 1)
	s.conn.Signal(ch)
	defer s.conn.RemoveSignal(ch)

	if err := s.object(prompt).Call(methodPrompt, 0, "").Err; err != nil {
		return fmt.Errorf("showing the keyring prompt: %w", err)
	}
	for sig := range ch {
		if sig.Path != prompt || sig.Name != promptIface+"."+memberCompleted {
			continue
		}
		// Completed carries (dismissed bool, result Variant). A dismissed
		// prompt is the user declining, which is a refusal rather than a
		// backend error.
		if len(sig.Body) > 0 {
			if dismissed, ok := sig.Body[0].(bool); ok && dismissed {
				return fmt.Errorf("the keyring prompt was dismissed, so the keyring stayed locked")
			}
		}
		return nil
	}
	return nil
}

// attributes maps a secret's name and provenance to the item attribute map, and
// back. Kept a free function, and tested both ways without a bus, because the
// mapping is the part a keyring never sees change but every verb depends on.
func attributes(service, name string, p Provenance) (map[string]string, error) {
	attrs := map[string]string{
		attrKeyService: service,
		attrKeyName:    name,
	}
	if !p.IsZero() {
		encoded, err := p.Encode()
		if err != nil {
			return nil, err
		}
		attrs[attrKeyProvenance] = encoded
	}
	return attrs, nil
}

// provenanceFromAttributes reads the provenance attribute back through
// DecodeProvenance, which answers false for anything brig did not write -- an
// item another process planted in the namespace, or one written before this
// field existed. Either way the zero value is what callers see, the same
// contract Modified follows.
func provenanceFromAttributes(attrs map[string]string) Provenance {
	p, ok := DecodeProvenance(attrs[attrKeyProvenance])
	if !ok {
		return Provenance{}
	}
	return p
}
