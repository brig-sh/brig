package secret

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// testService prefixes every service name these tests invent, so a run that
// died before its cleanup can be swept up by the next one.
const testService = "sh.brig.secret.test."

// TestMain sweeps the namespace before the suite runs.
//
// t.Cleanup covers a test that finishes, including a failing one, but not a
// run killed part way -- a `go test` timeout, an editor stopping the run, a
// ^C. Those leave items behind, and a later test that counts what List
// returns then fails for a reason that has nothing to do with the code.
func TestMain(m *testing.M) {
	sweep()
	os.Exit(m.Run())
}

func sweep() {
	out, err := exec.Command("security", "dump-keychain").Output()
	if err != nil {
		return
	}
	for _, block := range strings.Split(string(out), "\nkeychain: ") {
		if !strings.Contains(block, `class: "genp"`) {
			continue
		}
		svc := attr(block, "svce")
		if !strings.HasPrefix(svc, testService) {
			continue
		}
		// Deleted through security rather than through Delete, which now
		// validates the name -- and a leftover from the planted-item test
		// carries a name on purpose outside the grammar.
		_ = exec.Command("security", "delete-generic-password",
			"-s", svc, "-a", attr(block, "acct")).Run()
	}
}

// testKeychain is a keychain scoped to a service name of its own, which
// removes every secret it created when the test ends.
//
// These tests use the real login keychain rather than a throwaway one on
// purpose. security reads the value from stdin only when -w is the last
// option, and getopt stops at the first non-option -- so naming a keychain and
// keeping the value off argv are mutually exclusive. Isolating the tests would
// mean exercising an add path production never runs, which is the one thing
// these tests exist to avoid.
type testKeychain struct {
	keychain
	t *testing.T
}

// The suffix is random rather than the pid, because macOS recycles pids and a
// leftover item under a recycled one would be indistinguishable from an item
// the running test created.
func testStore(t *testing.T) *testKeychain {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return &testKeychain{
		keychain: keychain{service: fmt.Sprintf("%s%s-%s", testService, hex.EncodeToString(b[:]), t.Name())},
		t:        t,
	}
}

// Create and Update register the cleanup before touching the keychain, so a
// secret is removed even if the assertions after it fail.
//
// Update registers too, though it is not supposed to create anything. That is
// the point: the test asserting Update never creates is exactly the test that
// leaves debris behind when it fails, and a failing test should not also
// litter the developer's keychain.
func (k *testKeychain) Create(name string, value []byte) error {
	k.cleanup(name)
	return k.keychain.Create(name, value)
}

func (k *testKeychain) Update(name string, value []byte) error {
	k.cleanup(name)
	return k.keychain.Update(name, value)
}

// Write registers cleanup the same way Create and Update do, so a test
// exercising the Annotator path directly does not litter the developer's
// keychain when it fails partway through.
func (k *testKeychain) Write(name string, value []byte, p Provenance, update bool) error {
	k.cleanup(name)
	return k.keychain.Write(name, value, p, update)
}

func (k *testKeychain) cleanup(name string) {
	k.t.Cleanup(func() { _ = k.keychain.Delete(name) })
}

func TestCreateAndReadRoundTrip(t *testing.T) {
	k := testStore(t)
	// Every shape the keychain's own command line cannot carry: a quote or a
	// backslash would be read as quoting by `security -i`, and a newline would
	// end the command. Base64 is what makes these survive.
	//
	// The sizes are the other half. The first three cases here encode to under
	// 128 characters, which is the whole range the prompt form used to work
	// in, so a suite of only those proved a round trip exactly where the
	// truncation could not show up.
	for _, tc := range []struct {
		name  string
		value []byte
	}{
		{"plain", []byte("token-value")},
		{"quotes", []byte(`a"b'c`)},
		{"backslash", []byte(`a\b\\c`)},
		{"multiline", []byte("-----BEGIN KEY-----\nline two\nline three\n")},
		{"binary", []byte{0x00, 0x01, 0xff, 0xfe, 0x00}},
		{"spaces", []byte("  leading and trailing  ")},
		// One byte past the 96 the prompt form could carry: the first size at
		// which the old path stored a short value and reported success.
		{"just_over_the_old_cap", randomBytes(t, 97)},
		// The value this package exists to hold. An API key is about 108
		// bytes and an SSH key is a couple of thousand.
		{"api_key", []byte("sk-ant-api03-" + strings.Repeat("x", 95))},
		{"rsa_private_key", rsaKeyPEM(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := k.Create(tc.name, tc.value); err != nil {
				t.Fatal(err)
			}
			got, err := k.Read(tc.name)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tc.value) {
				t.Errorf("round trip changed the value:\n got %q\nwant %q", got, tc.value)
			}
		})
	}
}

// find is List filtered to one name, for a test that wants a secret's
// metadata rather than its value: Read never returns Provenance or Modified,
// and List is the only path that does.
func find(t *testing.T, k *testKeychain, name string) Secret {
	t.Helper()
	list, err := k.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, s := range list {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("List did not include %q: %+v", name, list)
	return Secret{}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// A real key rather than a stub, because its size is the point: the PEM runs
// to about 1.7KB, well past every ceiling this file is about.
func rsaKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// The value must never reach argv, where `ps` would show it to any process on
// the host. internal/runtime makes the same guarantee for guest variables, and
// this is the line that keeps the store honest about it: the command carries
// no value at all, and the value travels on stdin.
func TestWriteKeepsTheValueOutOfArgv(t *testing.T) {
	k := testStore(t)
	const value = "argv-canary-value"
	if err := k.Create("canary", []byte(value)); err != nil {
		t.Fatal(err)
	}
	prefix, err := k.writePrefix("canary", false, Provenance{})
	if err != nil {
		t.Fatal(err)
	}
	// The whole of what security is invoked with. Anything the value could
	// hide in would have to be here.
	for _, a := range []string{"-i", prefix} {
		if strings.Contains(a, value) || strings.Contains(a, base64.StdEncoding.EncodeToString([]byte(value))) {
			t.Fatalf("the value reached argv: %q", a)
		}
	}
	if !strings.HasSuffix(prefix, "-w ") {
		t.Errorf("-w is not last, so security would not read the value: %q", prefix)
	}
}

// The write is priced against the command line that carries it, and a value
// that does not fit is refused rather than shortened. security answers a line
// over its buffer by truncating it and reporting success, so a value one byte
// past the limit used to be stored short, decode cleanly, and read back as a
// different secret than the one that went in.
func TestWriteRefusesAValueTooBigForTheCommandLine(t *testing.T) {
	k := testStore(t)
	max := k.MaxValueFor("big", false, Provenance{})
	if max < 1024 {
		t.Fatalf("MaxValueFor = %d, too small to be carrying keys", max)
	}
	if err := k.Create("big", bytes.Repeat([]byte("a"), max)); err != nil {
		t.Fatalf("a value of exactly %d bytes was refused: %v", max, err)
	}
	got, err := k.Read("big")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != max {
		t.Errorf("the largest value read back as %d bytes, not %d", len(got), max)
	}
	err = k.Create("toobig", bytes.Repeat([]byte("a"), max+1))
	if err == nil {
		t.Fatal("a value one byte over the limit was accepted")
	}
	if !strings.Contains(err.Error(), "at most") {
		t.Errorf("refusal = %v, want it to say what the limit is", err)
	}
	// Refused before anything was written, so there is no short value left
	// behind under that name.
	if _, err := k.Read("toobig"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the refused write left something behind: %v", err)
	}
}

// The limit shrinks as the name grows, because the name is on the same line.
func TestMaxValueLeavesRoomForTheName(t *testing.T) {
	k := testStore(t)
	short := k.MaxValueFor("a", false, Provenance{})
	long := k.MaxValueFor(strings.Repeat("a", 41), false, Provenance{})
	if long >= short {
		t.Errorf("MaxValueFor did not fall with a longer name: %d then %d", short, long)
	}
	if diff := short - long; diff < 30 {
		t.Errorf("a name 40 characters longer only cost %d bytes", diff)
	}
}

// The comment attribute is read without decrypting, which is what lets
// ls report provenance with no keychain dialog.
func TestProvenanceSurvivesWriteAndList(t *testing.T) {
	k := testStore(t)
	want := Provenance{V: ProvenanceVersion, From: "keychain:Claude Code-credentials", ExpiresAt: 1755436980000}
	if err := k.Write("prov-a", []byte("value"), want, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := find(t, k, "prov-a")
	if got.Provenance != want {
		t.Errorf("provenance = %+v, want %+v", got.Provenance, want)
	}
}

// The update path is the one that loses metadata by default, and it is the
// path every re-import takes.
func TestProvenanceSurvivesUpdate(t *testing.T) {
	k := testStore(t)
	if err := k.Write("prov-b", []byte("v1"), Provenance{V: ProvenanceVersion, From: "file:/a"}, false); err != nil {
		t.Fatalf("create: %v", err)
	}
	second := Provenance{V: ProvenanceVersion, From: "keychain:svc", ExpiresAt: 42}
	if err := k.Write("prov-b", []byte("v2"), second, true); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := find(t, k, "prov-b").Provenance; got != second {
		t.Errorf("provenance after update = %+v, want %+v", got, second)
	}
}

// The gap the tests above leave: they only ever update WITH a provenance.
// security's -U rewrites the attributes named on the line and leaves the rest
// alone, so an update carrying no -j keeps whatever comment the previous
// value had -- and `brig secret update` is exactly that call. An imported
// credential renewed by hand would otherwise keep the old expiresAt for good,
// reporting as expired for as long as it existed.
func TestUpdateWithNoProvenanceClearsTheOldOne(t *testing.T) {
	k := testStore(t)
	stale := Provenance{V: ProvenanceVersion, From: "keychain:svc", ExpiresAt: 1}
	if err := k.Write("prov-c", []byte("imported"), stale, false); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := k.Update("prov-c", []byte("renewed-by-hand")); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := find(t, k, "prov-c")
	if !got.Provenance.IsZero() {
		t.Errorf("provenance after a hand update = %+v, want the zero value: "+
			"the comment still describes the value that was replaced", got.Provenance)
	}
	if stored, err := k.Read("prov-c"); err != nil || string(stored) != "renewed-by-hand" {
		t.Errorf("value = %q, %v; want the updated one", stored, err)
	}
}

// MaxValue promises a caller that has not chosen a provenance yet a ceiling
// Write will not undercut, for any From up to assumedFromLen. ExpiresAt is
// omitempty, so leaving it zero in the assumed provenance dropped it out of
// the encoded document and broke that promise from about 105 characters on --
// the caller was told a value fit and then refused.
//
// Past assumedFromLen no fixed assumption can hold, and the failure there is
// a spurious refusal carrying Write's own accurate ceiling, never a truncated
// write. That boundary is the thing worth pinning.
func TestMaxValueIsNeverLargerThanWhatWriteApplies(t *testing.T) {
	k := keychain{service: "sh.brig.test"}
	for _, n := range []int{len("keychain:svc"), 105, assumedFromLen} {
		real := Provenance{V: ProvenanceVersion, From: strings.Repeat("x", n), ExpiresAt: 1755436980000}
		for _, update := range []bool{false, true} {
			if got, want := k.MaxValueFor("n", update, real), k.MaxValue("n", update); got < want {
				t.Errorf("From=%d update=%v: Write's ceiling %d is below MaxValue's %d",
					n, update, got, want)
			}
		}
	}
}

// A hand-created secret carries none, and that has to read as absent rather
// than as an empty provenance that claims a source of "".
func TestHandCreatedSecretHasNoProvenance(t *testing.T) {
	k := testStore(t)
	if err := k.Create("plain", []byte("v")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := find(t, k, "plain").Provenance; !got.IsZero() {
		t.Errorf("provenance = %+v, want the zero value", got)
	}
}

// The size ceiling is priced against the whole command line, and the comment
// now rides on it -- so MaxValue has to account for the comment or the
// pre-check passes a write that security silently truncates.
func TestMaxValueAccountsForTheComment(t *testing.T) {
	k := keychain{service: "sh.brig.test"}
	long := Provenance{V: ProvenanceVersion, From: "keychain:" + strings.Repeat("x", 200)}
	if k.MaxValueFor("n", false, long) >= k.MaxValueFor("n", false, Provenance{}) {
		t.Error("a longer comment did not reduce the value budget")
	}
}

// Step 5c: the write path must price its ceiling against the provenance it is
// actually attaching, not the provenance-free ceiling MaxValue offers a
// caller that has not chosen one yet. Wiring Write to that number while still
// appending -j <encoded> is the obvious minimal edit, and it is wrong: the
// line exceeds security's buffer, security truncates it silently on a
// four-byte boundary, the short value still base64-decodes and still
// resolves, and verify cannot roll back an update. The assertion that matters
// is on the stored bytes -- here, that nothing was stored at all -- not on
// the error alone.
func TestWriteRefusesWhenProvenanceOverflowsTheLine(t *testing.T) {
	k := testStore(t)
	// A value that exactly fills the provenance-free budget: the old
	// maxValue would have waved this through.
	bare := k.MaxValueFor("prov-big", false, Provenance{})
	value := bytes.Repeat([]byte("a"), bare)
	long := Provenance{V: ProvenanceVersion, From: "keychain:" + strings.Repeat("x", 200)}

	err := k.Write("prov-big", value, long, false)
	if err == nil {
		t.Fatal("a value plus provenance that overflows the line was accepted")
	}
	if !strings.Contains(err.Error(), "at most") {
		t.Errorf("refusal = %v, want it to say what the limit is", err)
	}
	// The point of 5c: nothing was silently truncated and stored under a name
	// that now resolves to a value shorter than the one asked for.
	if _, err := k.Read("prov-big"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the refused write left something behind")
	}
}

// Create refuses a name that is taken, and leaves the existing value alone.
// security returns 45 for this, so no read-then-write race stands between the
// check and the decision.
func TestCreateRefusesACollision(t *testing.T) {
	k := testStore(t)
	if err := k.Create("dup", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := k.Create("dup", []byte("second")); !errors.Is(err, ErrExists) {
		t.Fatalf("second Create = %v, want ErrExists", err)
	}
	got, err := k.Read("dup")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first" {
		t.Errorf("the collision overwrote the value: %q", got)
	}
}

// Update replaces, and refuses to create. -U on its own would create the item
// silently, which would turn a typo into a new secret nobody meant to make.
func TestUpdateReplacesButNeverCreates(t *testing.T) {
	k := testStore(t)
	if err := k.Create("up", []byte("before")); err != nil {
		t.Fatal(err)
	}
	if err := k.Update("up", []byte("after")); err != nil {
		t.Fatal(err)
	}
	got, err := k.Read("up")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "after" {
		t.Errorf("Update did not replace: %q", got)
	}
	if err := k.Update("absent", []byte("x")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update of an absent secret = %v, want ErrNotFound", err)
	}
	if _, err := k.Read("absent"); !errors.Is(err, ErrNotFound) {
		t.Error("Update created a secret it should have refused")
	}
}

func TestReadAndDeleteReportAbsence(t *testing.T) {
	k := testStore(t)
	if _, err := k.Read("ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Read = %v, want ErrNotFound", err)
	}
	if err := k.Delete("ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete = %v, want ErrNotFound", err)
	}
	if err := k.Create("real", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := k.Delete("real"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Read("real"); !errors.Is(err, ErrNotFound) {
		t.Error("Delete left the secret readable")
	}
}

// List returns brig's own items and nothing else. The login keychain these
// tests run against holds hundreds of other items, so this doubles as the
// check that the service filter works.
func TestListReturnsOnlyOurNamespace(t *testing.T) {
	k := testStore(t)
	for _, n := range []string{"beta", "alpha", "gamma"} {
		if err := k.Create(n, []byte("v-"+n)); err != nil {
			t.Fatal(err)
		}
	}
	list, err := k.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("List returned %d secrets, want 3: %+v", len(list), list)
	}
	// Sorted by name, so output is stable between runs.
	for i, want := range []string{"alpha", "beta", "gamma"} {
		if list[i].Name != want {
			t.Errorf("list[%d] = %q, want %q", i, list[i].Name, want)
		}
	}
	if list[0].Modified.IsZero() {
		t.Error("the modification time was not parsed")
	}
}

// The grammar belongs to the store, not to whoever calls it.
//
// Without the check here, Create("é") succeeded and Read("é") returned the
// value, but the keychain renders a non-printable account as hex, so attr read
// it as "" and List never showed it. The secret existed and nothing could find
// it again.
func TestNamesAreValidatedAtTheStoreBoundary(t *testing.T) {
	k := testStore(t)
	for _, name := range []string{"", "é", "a b", "a.b", "-lead"} {
		if err := k.Create(name, []byte("v")); err == nil {
			t.Errorf("Create(%q) was accepted", name)
		}
		if _, err := k.Read(name); err == nil {
			t.Errorf("Read(%q) was accepted", name)
		}
		if err := k.Update(name, []byte("v")); err == nil {
			t.Errorf("Update(%q) was accepted", name)
		}
		if err := k.Delete(name); err == nil {
			t.Errorf("Delete(%q) was accepted", name)
		}
	}
	// And nothing was stored under any of them.
	list, err := k.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("List = %+v, want nothing", list)
	}
}

// An item another process put in brig's namespace is not brig's to explain
// away. The service name is a label, not proof of authorship -- see the
// comment on service -- so this is reachable, and the caller should be told
// where the bad value came from rather than handed a byte offset into a string
// they never supplied.
func TestReadExplainsAValueBrigDidNotWrite(t *testing.T) {
	k := testStore(t)
	plant(t, k.service, "planted", "!!! not base64 !!!")
	_, err := k.Read("planted")
	if err == nil {
		t.Fatal("a value that is not brig's encoding was read as one")
	}
	for _, want := range []string{"planted", "brig's encoding"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Read = %v, want it to mention %q", err, want)
		}
	}
	// The name is still brig's grammar, so List shows it: brig can see that
	// something is there, and says so honestly when asked to read it.
	list, err := k.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "planted" {
		t.Errorf("List = %+v, want the planted item", list)
	}
}

// A name outside the grammar cannot be created through the store, so the only
// way one exists is that something else wrote it. List leaves it alone rather
// than offering a secret every other verb would then refuse.
func TestListSkipsNamesOutsideTheGrammar(t *testing.T) {
	k := testStore(t)
	plant(t, k.service, "not a brig name", "dg==")
	if err := k.Create("mine", []byte("v")); err != nil {
		t.Fatal(err)
	}
	list, err := k.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "mine" {
		t.Errorf("List = %+v, want only \"mine\"", list)
	}
}

// plant writes an item directly, bypassing the store, to stand in for whatever
// else on the host might write into brig's namespace. An optional comment
// stands in for a hostile icmt attribute -- the same namespace any process
// running as this user can write into carries the comment too.
func plant(t *testing.T, service, name, value string, comment ...string) {
	t.Helper()
	t.Cleanup(func() {
		_ = exec.Command("security", "delete-generic-password", "-s", service, "-a", name).Run()
	})
	line := fmt.Sprintf("add-generic-password -s %s -a %q", service, name)
	if len(comment) > 0 {
		line += fmt.Sprintf(" -j %q", comment[0])
	}
	line += fmt.Sprintf(" -w %q\n", value)
	cmd := exec.Command("security", "-i")
	cmd.Stdin = strings.NewReader(line)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
}

// Step 5b, end to end: the comment attribute is not brig's private space,
// and this is what it looks like when something else uses it. A From that
// survived unsanitised would be printed by `brig secret ls` and by a warning
// telling the user to run a command -- exactly where a hidden escape
// sequence would do its work.
func TestListSanitisesAHostilePlantedComment(t *testing.T) {
	k := testStore(t)
	plant(t, k.service, "hostile", "dg==", "brig1:"+base64.RawURLEncoding.EncodeToString(
		[]byte(`{"v":1,"from":"`+"\x1b[31mnot a color\x1b[0m"+`","expiresAt":1}`)))
	got := find(t, k, "hostile")
	if got.Provenance.From != "" {
		t.Errorf("From = %q, want the planted escape sequence rejected to the zero value", got.Provenance.From)
	}
}

// securityError keeps the line that explains something and drops the tool's
// own noise around it, and it wraps rather than replaces, so the exit code
// stays reachable.
func TestSecurityErrorKeepsTheExplanationAndTheCause(t *testing.T) {
	cause := &exec.ExitError{}
	// The prompt form wrote its prompts with no newline before the message,
	// so the first line held both; interactive mode adds a line after it.
	const stderr = "password data for new item: retype password for new item: " +
		"security: SecKeychainItemCreateFromContent (<default>): The specified item already exists in the keychain.\n" +
		"add-generic-password: returned -25299\n"
	err := securityError(cause, stderr)
	if strings.Contains(err.Error(), "password data for new item") {
		t.Errorf("the prompt was kept: %v", err)
	}
	if !strings.Contains(err.Error(), "already exists in the keychain") {
		t.Errorf("the explanation was dropped: %v", err)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Errorf("%v does not unwrap to the *exec.ExitError", err)
	}
	// With nothing of security's own to keep, the cause is returned as it is.
	if got := securityError(cause, "  \n"); got != error(cause) {
		t.Errorf("securityError with empty stderr = %v, want the cause", got)
	}
}

// The dump is parsed rather than grepped, so an item of another class or
// another service cannot be mistaken for one of brig's.
func TestParseDumpIgnoresOtherItems(t *testing.T) {
	const dump = `keychain: "/Users/x/Library/Keychains/login.keychain-db"
version: 512
class: "genp"
attributes:
    "acct"<blob>="not-ours"
    "mdat"<timedate>=0x32303236303831343136323333305A00  "20260814162330Z\000"
    "svce"<blob>="com.apple.assistant"
keychain: "/Users/x/Library/Keychains/login.keychain-db"
version: 512
class: "genp"
attributes:
    "acct"<blob>="ours"
    "mdat"<timedate>=0x32303236303831343136323333305A00  "20260814162330Z\000"
    "svce"<blob>="sh.brig.secret"
keychain: "/Users/x/Library/Keychains/login.keychain-db"
version: 512
class: 0x0000000F
attributes:
    "svce"<blob>="sh.brig.secret"
`
	got := parseDump(dump, "sh.brig.secret")
	if len(got) != 1 || got[0].Name != "ours" {
		t.Fatalf("parseDump = %+v, want one secret named \"ours\"", got)
	}
	if got[0].Modified.Format("2006-01-02") != "2026-08-14" {
		t.Errorf("modified = %v, want 2026-08-14", got[0].Modified)
	}
}

// A backend that cannot supply a modification time returns the zero value,
// and nothing downstream may invent one. See Secret.Modified.
func TestParseDumpToleratesAMissingDate(t *testing.T) {
	const dump = `keychain: "/Users/x/Library/Keychains/login.keychain-db"
version: 512
class: "genp"
attributes:
    "acct"<blob>="dateless"
    "svce"<blob>="sh.brig.secret"
`
	got := parseDump(dump, "sh.brig.secret")
	if len(got) != 1 || got[0].Name != "dateless" {
		t.Fatalf("parseDump = %+v, want one secret named \"dateless\"", got)
	}
	if !got[0].Modified.IsZero() {
		t.Errorf("modified = %v, want the zero time", got[0].Modified)
	}
}

// Every call in this file drives security(1), and it used to be named as a
// bare word: resolved through whatever $PATH the invoking shell was carrying.
// That is a real swap and not a theoretical one -- the write path pipes the
// encoded secret to this program's stdin and the read path takes its stdout as
// the value, so a file called `security` earlier in $PATH is handed
// credentials by brig itself and answers with whatever it likes. A shim
// planted this way captured a real value during review.
//
// The test plants exactly that shim, with a $PATH holding nothing else, and
// asserts brig never ran it.
func TestSecurityIsNotResolvedThroughPath(t *testing.T) {
	dir := t.TempDir()
	marker := dir + "/it-ran"
	shim := "#!/bin/sh\necho \"$@\" >> " + marker + "\ncat >> " + marker + "\nexit 0\n"
	if err := os.WriteFile(dir+"/security", []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	k := testStore(t)
	// One call per verb that shells out, so a single missed call site is not
	// hidden by the others. What each one returns is beside the point: the
	// only assertion is that the shim was never the thing that answered.
	_ = k.Create("shimmed", []byte("a-real-value"))
	_, _ = k.Read("shimmed")
	_ = k.Update("shimmed", []byte("another-real-value"))
	_, _ = k.List()
	_ = k.Delete("shimmed")

	if _, err := os.Stat(marker); err == nil {
		captured, _ := os.ReadFile(marker)
		t.Fatalf("brig ran a `security` planted on $PATH, which was handed: %s", captured)
	}
}
