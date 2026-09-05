//go:build linux

package secret

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
)

// The attribute mapping is the part that never touches a bus and every verb
// depends on: a secret's name and provenance to the item attributes and back.

func TestAttributesRoundTripWithProvenance(t *testing.T) {
	want := Provenance{V: ProvenanceVersion, From: "keychain:Claude Code-credentials", ExpiresAt: 1755436980000}
	attrs, err := attributes("sh.brig.test", "gh-token", want)
	if err != nil {
		t.Fatal(err)
	}
	if attrs[attrKeyService] != "sh.brig.test" {
		t.Errorf("service attribute = %q, want the namespace", attrs[attrKeyService])
	}
	if attrs[attrKeyName] != "gh-token" {
		t.Errorf("name attribute = %q, want the secret name", attrs[attrKeyName])
	}
	if got := provenanceFromAttributes(attrs); got != want {
		t.Errorf("provenance round trip = %+v, want %+v", got, want)
	}
}

// A zero Provenance writes no provenance attribute at all, and reads back as
// absent -- the same contract a hand-created secret carries.
func TestAttributesOmitZeroProvenance(t *testing.T) {
	attrs, err := attributes("sh.brig.test", "plain", Provenance{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := attrs[attrKeyProvenance]; ok {
		t.Errorf("a zero provenance still wrote an attribute: %q", attrs[attrKeyProvenance])
	}
	if got := provenanceFromAttributes(attrs); !got.IsZero() {
		t.Errorf("provenance = %+v, want the zero value", got)
	}
}

// An attribute another process planted -- not brig's encoding -- reads back as
// absent rather than as a provenance brig trusts, the same way DecodeProvenance
// answers false for it on macOS.
func TestProvenanceFromForeignAttributesIsAbsent(t *testing.T) {
	for _, attrs := range []map[string]string{
		{attrKeyProvenance: "not brig's encoding"},
		{}, // no attribute at all
	} {
		if got := provenanceFromAttributes(attrs); !got.IsZero() {
			t.Errorf("provenance from %v = %+v, want the zero value", attrs, got)
		}
	}
}

// The grammar belongs to the store: a name past maxName is refused before
// anything reaches the bus, so this holds with no keyring at all.
func TestNameValidationAgainstMaxName(t *testing.T) {
	ok := strings.Repeat("a", maxName)
	if err := ValidName(ok); err != nil {
		t.Errorf("a name of exactly %d characters was refused: %v", maxName, err)
	}
	tooLong := strings.Repeat("a", maxName+1)
	if err := ValidName(tooLong); err == nil {
		t.Errorf("a name of %d characters was accepted", maxName+1)
	}
	// And every verb refuses it at the boundary, before dereferencing a
	// connection -- the store has none here on purpose.
	s := &secretService{service: service}
	if err := s.Create(tooLong, []byte("v")); err == nil {
		t.Error("Create accepted an over-long name")
	}
	if _, err := s.Read(tooLong); err == nil {
		t.Error("Read accepted an over-long name")
	}
	if err := s.Update(tooLong, []byte("v")); err == nil {
		t.Error("Update accepted an over-long name")
	}
	if err := s.Delete(tooLong); err == nil {
		t.Error("Delete accepted an over-long name")
	}
}

// open()'s two failure branches, forced through the injectable seams, because
// the executor has no session bus to reach either of them naturally. Both wrap
// ErrUnsupported and both name the two ways out, so a caller can tell the
// platform-unsupported case apart and a reader is told how to fix it.

func TestOpenReportsNoBus(t *testing.T) {
	restore := swapSeams(func() (*dbus.Conn, error) { return nil, errors.New("dial refused") }, nil)
	defer restore()

	_, err := open()
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("open() = %v, want it to wrap ErrUnsupported", err)
	}
	for _, want := range []string{"session bus", "gnome-keyring", "--from-command"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("open() = %v, want it to mention %q", err, want)
		}
	}
}

func TestOpenReportsNoService(t *testing.T) {
	restore := swapSeams(
		func() (*dbus.Conn, error) { return nil, nil },
		func(*dbus.Conn) bool { return false },
	)
	defer restore()

	_, err := open()
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("open() = %v, want it to wrap ErrUnsupported", err)
	}
	for _, want := range []string{"Secret Service", "gnome-keyring", "--from-command"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("open() = %v, want it to mention %q", err, want)
		}
	}
}

// swapSeams replaces dialBus and hasSecretService for a test and returns a
// function that puts the originals back. A nil hasSecretService leaves the real
// one, which the no-bus test never reaches anyway.
func swapSeams(dial func() (*dbus.Conn, error), has func(*dbus.Conn) bool) func() {
	origDial, origHas := dialBus, hasSecretService
	dialBus = dial
	if has != nil {
		hasSecretService = has
	}
	return func() { dialBus, hasSecretService = origDial, origHas }
}

// The integration test runs only against a real keyring: DBUS_SESSION_BUS_ADDRESS
// set and a Secret Service answering. It stays under a service attribute value
// of its own so it never touches a real brig secret, and it removes what it
// creates.

// testService prefixes every service value the integration test invents, so a
// run that died before cleanup is distinguishable from a real brig namespace.
const testService = "sh.brig.secret.test."

func integrationStore(t *testing.T) *secretService {
	t.Helper()
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		t.Skip("no session bus: set DBUS_SESSION_BUS_ADDRESS and run a Secret Service keyring to exercise the backend")
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	s, err := openService(testService + hex.EncodeToString(b[:]))
	if err != nil {
		if errors.Is(err, ErrUnsupported) {
			t.Skipf("no Secret Service on the session bus: %v", err)
		}
		t.Fatalf("openService: %v", err)
	}
	return s
}

// cleanup deletes a name at test end, ignoring an already-absent one.
func cleanup(t *testing.T, s *secretService, name string) {
	t.Helper()
	t.Cleanup(func() {
		if err := s.Delete(name); err != nil && !errors.Is(err, ErrNotFound) {
			t.Logf("cleanup of %q: %v", name, err)
		}
	})
}

func TestIntegrationCreateReadUpdateDelete(t *testing.T) {
	s := integrationStore(t)
	cleanup(t, s, "roundtrip")

	value := []byte("-----BEGIN KEY-----\nline two\n\x00\xff\xfe")
	if err := s.Create("roundtrip", value); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.Read("roundtrip")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, value) {
		t.Errorf("Read = %q, want %q", got, value)
	}

	updated := []byte("second value")
	if err := s.Update("roundtrip", updated); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got, err := s.Read("roundtrip"); err != nil || !bytes.Equal(got, updated) {
		t.Errorf("Read after Update = %q, %v; want %q", got, err, updated)
	}

	if err := s.Delete("roundtrip"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Read("roundtrip"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Read after Delete = %v, want ErrNotFound", err)
	}
}

func TestIntegrationReadAndDeleteReportAbsence(t *testing.T) {
	s := integrationStore(t)
	if _, err := s.Read("ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Read of an absent secret = %v, want ErrNotFound", err)
	}
	if err := s.Delete("ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete of an absent secret = %v, want ErrNotFound", err)
	}
}

func TestIntegrationCreateRefusesACollision(t *testing.T) {
	s := integrationStore(t)
	cleanup(t, s, "dup")
	if err := s.Create("dup", []byte("first")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Create("dup", []byte("second")); !errors.Is(err, ErrExists) {
		t.Fatalf("second Create = %v, want ErrExists", err)
	}
	if got, err := s.Read("dup"); err != nil || string(got) != "first" {
		t.Errorf("Read = %q, %v; want the first value left intact", got, err)
	}
}

func TestIntegrationUpdateNeverCreates(t *testing.T) {
	s := integrationStore(t)
	if err := s.Update("absent", []byte("v")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update of an absent secret = %v, want ErrNotFound", err)
	}
	if _, err := s.Read("absent"); !errors.Is(err, ErrNotFound) {
		t.Error("Update created a secret it should have refused")
	}
}

func TestIntegrationListReturnsOnlyOurNamespace(t *testing.T) {
	s := integrationStore(t)
	for _, n := range []string{"beta", "alpha", "gamma"} {
		cleanup(t, s, n)
		if err := s.Create(n, []byte("v-"+n)); err != nil {
			t.Fatalf("Create %q: %v", n, err)
		}
	}
	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("List returned %d secrets, want 3: %+v", len(list), list)
	}
	// Sorted by name, so the output is stable between runs.
	for i, want := range []string{"alpha", "beta", "gamma"} {
		if list[i].Name != want {
			t.Errorf("list[%d] = %q, want %q", i, list[i].Name, want)
		}
	}
}

func TestIntegrationProvenanceRoundTrip(t *testing.T) {
	s := integrationStore(t)
	cleanup(t, s, "prov")

	want := Provenance{V: ProvenanceVersion, From: "keychain:Claude Code-credentials", ExpiresAt: 1755436980000}
	if err := s.Write("prov", []byte("value"), want, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := findSecret(t, s, "prov").Provenance; got != want {
		t.Errorf("provenance after Write = %+v, want %+v", got, want)
	}

	// A hand update carries no provenance, and the old one must not outlive the
	// value it described.
	if err := s.Update("prov", []byte("renewed-by-hand")); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := findSecret(t, s, "prov").Provenance; !got.IsZero() {
		t.Errorf("provenance after a hand Update = %+v, want the zero value", got)
	}
}

// findSecret is List filtered to one name, for a test that wants a secret's
// metadata rather than its value: Read returns neither Provenance nor Modified,
// and List is the only path that does.
func findSecret(t *testing.T, s *secretService, name string) Secret {
	t.Helper()
	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, sec := range list {
		if sec.Name == name {
			return sec
		}
	}
	t.Fatalf("List did not include %q: %+v", name, list)
	return Secret{}
}
