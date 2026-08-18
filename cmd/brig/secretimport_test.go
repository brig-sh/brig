package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/hostsrc"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/secret"
)

// fakeAnnotator is a store that carries provenance and a size ceiling, which
// is what the importer type-asserts for. calls records every write so that
// "one Update, never delete-then-create" is an assertion rather than a hope.
type fakeAnnotator struct {
	*fakeStore
	prov  map[string]secret.Provenance
	max   int
	calls []string
}

var (
	_ secret.Annotator = (*fakeAnnotator)(nil)
	_ secret.Sizer     = (*fakeAnnotator)(nil)
)

func newAnnotating(t *testing.T) *fakeAnnotator {
	t.Helper()
	base := &fakeStore{items: map[string][]byte{}, provenance: map[string]secret.Provenance{}}
	// One map behind both names, not two. The importer reads provenance the
	// only way a Store exposes it -- List -- so a test that seeded prov into a
	// map List never reads would be asserting on something the code cannot
	// see.
	f := &fakeAnnotator{fakeStore: base, prov: base.provenance, max: 3000}
	old := openStore
	openStore = func() (secret.Store, error) { return f, nil }
	t.Cleanup(func() { openStore = old })
	return f
}

func (f *fakeAnnotator) Write(name string, value []byte, p secret.Provenance, update bool) error {
	f.calls = append(f.calls, fmt.Sprintf("write:%s:%t", name, update))
	if len(value) > f.max {
		return fmt.Errorf("the value for %q is %d bytes, and the store takes at most %d",
			name, len(value), f.max)
	}
	if _, exists := f.items[name]; !exists {
		f.order = append(f.order, name)
	}
	f.items[name], f.prov[name] = value, p
	return nil
}

func (f *fakeAnnotator) MaxValue(string, bool) int { return f.max }

func (f *fakeAnnotator) Delete(name string) error {
	f.calls = append(f.calls, "delete:"+name)
	return f.fakeStore.Delete(name)
}

// fakeHost is the host itself: one blob per locator, and a count of how many
// times each was actually touched. It outlives any one reader, which is what
// makes the dialog economy testable -- see fakeHostReader.
type fakeHost struct {
	values map[string][]byte
	reads  map[string]int
}

// fakeHostReader stands in for a hostsrc.Reader, memo and all. The memo is
// part of the contract Reader.Read documents -- at most one read per locator
// per reader -- so a double without it would answer differently from the real
// thing for the exact question these tests ask.
//
// A fresh one per newHostReader() call, over a shared fakeHost, is what turns
// "build the reader once for the whole run" into an assertion: a reader per
// secret would give each its own memo and touch the host twice, which is two
// approval dialogs for the user.
type fakeHostReader struct {
	host *fakeHost
	seen map[string]hostsrc.Value
}

func (r *fakeHostReader) Read(s profile.Source) (hostsrc.Value, bool, error) {
	loc := s.Locator()
	if v, ok := r.seen[loc]; ok {
		return v, v.From != "", nil
	}
	r.host.reads[loc]++
	blob, ok := r.host.values[loc]
	var v hostsrc.Value
	if ok {
		v = hostsrc.Value{Bytes: blob, From: loc}
	}
	r.seen[loc] = v
	return v, ok, nil
}

// useHost installs a host double and returns it.
func useHost(t *testing.T, values map[string][]byte) *fakeHost {
	t.Helper()
	h := &fakeHost{values: values, reads: map[string]int{}}
	old := newHostReader
	newHostReader = func() hostReader {
		return &fakeHostReader{host: h, seen: map[string]hostsrc.Value{}}
	}
	t.Cleanup(func() { newHostReader = old })
	return h
}

// loadProfiles points the registry at a directory holding the given profile
// bodies and loads it, so a test decides what `profile.Lookup` and
// `profile.All` answer.
func loadProfiles(t *testing.T, bodies ...string) {
	t.Helper()
	dir := t.TempDir()
	for i, body := range bodies {
		path := filepath.Join(dir, fmt.Sprintf("p%d.yaml", i))
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("BRIG_PROFILE_DIR", dir)
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
}

// importable registers a profile with one importable secret and one
// hand-created one, which is the mixed case most of these tests need.
func importable(t *testing.T) {
	t.Helper()
	t.Setenv("BRIG_PROFILE_DIR", writeProfile(t, `
name: mytool
image: ghcr.io/brig-sh/mytool:latest
guestHome: /home/mytool
binary: mytool
mem: 1024
cpus: 1
secrets:
  - name: mytool-token
    required: false
    expiryField: expiresAt
    sources:
      - from: keychain
        service: Mytool-credentials
  - name: mytool-manual
    required: false
`))
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
}

// A secret with no sources is skipped and reported, and does NOT fail the
// command: otherwise a wholly successful import of a profile mixing imported
// and hand-created secrets reports failure, and `import && run` breaks.
func TestHandCreatedSecretsAreReportedAndExitZero(t *testing.T) {
	importable(t)
	store := newAnnotating(t)
	useHost(t, map[string][]byte{
		"keychain:Mytool-credentials": []byte(`{"token":"tok","expiresAt":1755436980000}`),
	})

	var out bytes.Buffer
	if err := importSecrets(&out, []string{"mytool"}); err != nil {
		t.Fatalf("import failed over a hand-created secret: %v", err)
	}
	if _, ok := store.items["mytool-token"]; !ok {
		t.Error("the importable secret was not stored")
	}
	if !strings.Contains(out.String(), "brig secret create mytool-manual") {
		t.Errorf("the hand-created secret was not reported:\n%s", out.String())
	}
}

// With [name...], a named secret that has no sources IS an error: the user
// asked for something that cannot be imported, and silently skipping it would
// report success for work that did not happen.
func TestNamingAHandCreatedSecretIsAnError(t *testing.T) {
	importable(t)
	newAnnotating(t)
	useHost(t, nil)

	err := importSecrets(&bytes.Buffer{}, []string{"mytool", "mytool-manual"})
	if err == nil {
		t.Fatal("naming a hand-created secret succeeded")
	}
	if !strings.Contains(err.Error(), "brig secret create mytool-manual") {
		t.Errorf("the error does not say how to supply it: %v", err)
	}
}

// security truncates an over-long line silently on a four-byte boundary,
// so the short value still base64-decodes and still resolves. verify catches
// that on create but explicitly cannot roll back an update -- so the size is
// checked BEFORE writing, which is what stops a re-import destroying a good
// value and leaving a resolvable bad one behind.
func TestOversizeValueIsRefusedBeforeWriting(t *testing.T) {
	importable(t)
	store := newAnnotating(t)
	store.max = 16
	store.seed("mytool-token", "the-good-value")
	useHost(t, map[string][]byte{
		"keychain:Mytool-credentials": bytes.Repeat([]byte("x"), 64),
	})

	if err := importSecrets(&bytes.Buffer{}, []string{"mytool"}); err == nil {
		t.Fatal("an oversize value was accepted")
	}
	if len(store.calls) != 0 {
		t.Errorf("the store was written to before the size was checked: %v", store.calls)
	}
	if string(store.items["mytool-token"]) != "the-good-value" {
		t.Error("the previous good value was destroyed")
	}
}

// A byte-identical value is skipped, not rewritten: otherwise Modified comes
// to mean "an import last ran" rather than "the value last changed", and it is
// the freshness signal users read in `brig secret ls`.
func TestIdenticalValueIsNotRewritten(t *testing.T) {
	importable(t)
	store := newAnnotating(t)
	blob := []byte(`{"token":"tok"}`)
	store.seed("mytool-token", string(blob))
	useHost(t, map[string][]byte{"keychain:Mytool-credentials": blob})

	if err := importSecrets(&bytes.Buffer{}, []string{"mytool"}); err != nil {
		t.Fatal(err)
	}
	if len(store.calls) != 0 {
		t.Errorf("an unchanged value was rewritten: %v", store.calls)
	}
}

// Replace is a single Update (-U), never delete-then-create: that window lets
// a concurrent run fail with "missing secret -- import it" while that very
// command is running, and lets one of two concurrent imports read back the
// other's value in verify and delete it.
func TestReplaceIsASingleUpdate(t *testing.T) {
	importable(t)
	store := newAnnotating(t)
	store.seed("mytool-token", "old")
	store.prov["mytool-token"] = secret.Provenance{V: secret.ProvenanceVersion, From: "keychain:Mytool-credentials"}
	useHost(t, map[string][]byte{"keychain:Mytool-credentials": []byte("new")})

	if err := importSecrets(&bytes.Buffer{}, []string{"mytool"}); err != nil {
		t.Fatal(err)
	}
	if len(store.calls) != 1 || store.calls[0] != "write:mytool-token:true" {
		t.Errorf("calls = %v; want one update", store.calls)
	}
}

// A secret the user created by hand is not replaced without -y. Provenance is
// what makes the distinction possible: no From means brig did not write it,
// and overwriting somebody's own value is not something to do quietly.
func TestHandCreatedSecretIsNotReplacedWithoutYes(t *testing.T) {
	importable(t)
	store := newAnnotating(t)
	store.seed("mytool-token", "mine")
	useHost(t, map[string][]byte{"keychain:Mytool-credentials": []byte("theirs")})

	err := importSecrets(&bytes.Buffer{}, []string{"mytool"})
	if err == nil {
		t.Fatal("a hand-created secret was replaced without -y")
	}
	if string(store.items["mytool-token"]) != "mine" {
		t.Error("the hand-created value was overwritten anyway")
	}
	if !strings.Contains(err.Error(), "-y") {
		t.Errorf("the error does not name the flag that answers it: %v", err)
	}
	if err := importSecrets(&bytes.Buffer{}, []string{"mytool", "-y"}); err != nil {
		t.Fatalf("-y did not answer it: %v", err)
	}
	if string(store.items["mytool-token"]) != "theirs" {
		t.Error("-y did not replace the value")
	}
}

// The dialog economy, at the level the user feels it: two secrets naming the
// same keychain item raise one approval dialog, so the importer reads the
// locator once for the whole run.
func TestOneLocatorIsReadOncePerImport(t *testing.T) {
	// Two secrets over one keychain item, one extracting a field and one
	// storing the document verbatim -- the shape a file-shaped credential and
	// its token-shaped sibling actually take.
	loadProfiles(t, `
name: mytool
image: ghcr.io/brig-sh/mytool:latest
guestHome: /home/mytool
binary: mytool
mem: 1024
cpus: 1
secrets:
  - name: mytool-credentials
    required: false
    sources:
      - from: keychain
        service: Mytool-credentials
  - name: mytool-token
    required: false
    field: token
    sources:
      - from: keychain
        service: Mytool-credentials
`)
	store := newAnnotating(t)
	host := useHost(t, map[string][]byte{
		"keychain:Mytool-credentials": []byte(`{"token":"tok"}`),
	})

	if err := importSecrets(&bytes.Buffer{}, []string{"mytool"}); err != nil {
		t.Fatal(err)
	}
	if got := host.reads["keychain:Mytool-credentials"]; got != 1 {
		t.Errorf("the host was read %d times; want 1, or the user answers two dialogs", got)
	}
	if string(store.items["mytool-token"]) != "tok" {
		t.Errorf("the field: secret stored %q", store.items["mytool-token"])
	}
	if string(store.items["mytool-credentials"]) != `{"token":"tok"}` {
		t.Errorf("the verbatim secret stored %q", store.items["mytool-credentials"])
	}
}

// --from-command fills a secret with NO sources: at all, and takes precedence
// over the declared ones for that name -- the path a credential in an external
// secret manager takes without putting `sh -c` in profile data, where it would
// be a shareable artifact that runs a host command.
func TestFromCommandOverridesTheDeclaredSources(t *testing.T) {
	importable(t)
	store := newAnnotating(t)
	host := useHost(t, map[string][]byte{
		"keychain:Mytool-credentials": []byte("from-the-keychain"),
	})

	// mytool-manual declares no sources at all.
	if err := importSecrets(&bytes.Buffer{}, []string{
		"mytool", "mytool-manual", "--from-command", "printf %s from-the-command"}); err != nil {
		t.Fatalf("--from-command could not fill a secret with no sources: %v", err)
	}
	if got := string(store.items["mytool-manual"]); got != "from-the-command" {
		t.Errorf("stored %q; want the command's stdout", got)
	}

	// And for a secret that does declare one, the command wins and the
	// declared source is not read at all.
	if err := importSecrets(&bytes.Buffer{}, []string{
		"mytool", "mytool-token", "--from-command", "printf %s from-the-command"}); err != nil {
		t.Fatal(err)
	}
	if got := string(store.items["mytool-token"]); got != "from-the-command" {
		t.Errorf("stored %q; want the command to outrank the declared source", got)
	}
	if host.reads["keychain:Mytool-credentials"] != 0 {
		t.Error("the declared source was read even though a command was given")
	}
}

// With zero or two names it errors, because nothing says which secret the
// command answers for and guessing stores a credential under the wrong name.
func TestFromCommandNeedsExactlyOneName(t *testing.T) {
	importable(t)
	store := newAnnotating(t)
	useHost(t, nil)

	for _, args := range [][]string{
		{"mytool", "--from-command", "printf %s v"},
		{"mytool", "mytool-token", "mytool-manual", "--from-command", "printf %s v"},
	} {
		err := importSecrets(&bytes.Buffer{}, args)
		if err == nil {
			t.Fatalf("%v was accepted", args)
		}
		if !strings.Contains(err.Error(), "one name") {
			t.Errorf("%v: the error does not say what is wrong: %v", args, err)
		}
	}
	if len(store.calls) != 0 {
		t.Errorf("a credential was stored under a guessed name: %v", store.calls)
	}
}

// A dry run reads and does not write. It reads because a dry run that read
// nothing could neither confirm a source exists nor report an expiry, which is
// the whole of what it is for.
func TestDryRunReadsButDoesNotWrite(t *testing.T) {
	importable(t)
	store := newAnnotating(t)
	const expiry = 1755436980000
	host := useHost(t, map[string][]byte{
		"keychain:Mytool-credentials": fmt.Appendf(nil, `{"token":"tok","expiresAt":%d}`, expiry),
	})

	var out bytes.Buffer
	if err := importSecrets(&out, []string{"mytool", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if len(store.calls) != 0 {
		t.Errorf("a dry run wrote to the store: %v", store.calls)
	}
	if _, ok := store.items["mytool-token"]; ok {
		t.Error("a dry run stored a value")
	}
	if host.reads["keychain:Mytool-credentials"] != 1 {
		t.Error("a dry run did not read the source, so it confirmed nothing")
	}
	if !strings.Contains(out.String(), "keychain:Mytool-credentials") {
		t.Errorf("the dry run does not name the source:\n%s", out.String())
	}
	when := time.UnixMilli(expiry).Local().Format("2006-01-02 15:04")
	if !strings.Contains(out.String(), when) {
		t.Errorf("the dry run does not report the expiry it saw (%s):\n%s", when, out.String())
	}
}

// The report names the profile actually loaded, not the word typed: `claude`
// is an alias and a user's own file shadows a built-in, so either one makes
// the typed word the wrong thing to report back.
func TestOutputNamesTheCanonicalProfile(t *testing.T) {
	// A file of the user's own, shadowing the built-in, reached by the alias:
	// both indirections at once.
	loadProfiles(t, `
name: claude-code
image: docker.io/me/mine:latest
guestHome: /home/claude
binary: claude
mem: 1024
cpus: 1
secrets:
  - name: claude-credentials
    required: false
    sources:
      - from: keychain
        service: Claude Code-credentials
`)
	newAnnotating(t)
	useHost(t, map[string][]byte{"keychain:Claude Code-credentials": []byte("v")})

	var out bytes.Buffer
	if err := importSecrets(&out, []string{"claude"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "claude-code:") {
		t.Errorf("the output does not name the canonical profile:\n%s", out.String())
	}
}

// Secret names are flat and global, so importing for one profile fills the
// name for every profile that declares it. That is the point of a flat
// namespace and it is also its blast radius, so the output says so.
func TestOutputNamesTheOtherProfilesAffected(t *testing.T) {
	loadProfiles(t, `
name: mytool
image: ghcr.io/brig-sh/mytool:latest
guestHome: /home/mytool
binary: mytool
mem: 1024
cpus: 1
secrets:
  - name: mytool-token
    required: false
    sources:
      - from: keychain
        service: Mytool-credentials
`, `
name: othertool
image: ghcr.io/brig-sh/othertool:latest
guestHome: /home/othertool
binary: othertool
mem: 1024
cpus: 1
secrets:
  - name: mytool-token
    required: false
`)
	newAnnotating(t)
	useHost(t, map[string][]byte{"keychain:Mytool-credentials": []byte("v")})

	var out bytes.Buffer
	if err := importSecrets(&out, []string{"mytool"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "othertool") {
		t.Errorf("the output does not say which other profile this fills:\n%s", out.String())
	}
}

// Exit non-zero only when a name an importer covers could not be imported. A
// hand-created secret is an informational line and exits zero -- otherwise a
// wholly successful import of a mixed profile reports failure and breaks
// `brig secret import x && brig run x`.
func TestExitStatusIsNonZeroWhenAnImporterCouldNotFill(t *testing.T) {
	t.Run("the host holds nothing for an importable secret", func(t *testing.T) {
		importable(t)
		newAnnotating(t)
		useHost(t, nil)
		if err := importSecrets(&bytes.Buffer{}, []string{"mytool"}); err == nil {
			t.Fatal("an import that filled nothing reported success")
		}
	})
	t.Run("the only unfilled name is hand-created", func(t *testing.T) {
		importable(t)
		newAnnotating(t)
		useHost(t, map[string][]byte{"keychain:Mytool-credentials": []byte("v")})
		if err := importSecrets(&bytes.Buffer{}, []string{"mytool"}); err != nil {
			t.Fatalf("a wholly successful import reported failure: %v", err)
		}
	})
}

// Names, locators and dates are the whole of the output. A value never
// appears, in any mode -- the same rule `brig secret ls` follows, and the one
// that makes it safe to run this in a terminal whose scrollback outlives it.
func TestNoOutputEverHoldsAValue(t *testing.T) {
	const value = "s3cr3t-value"
	blob := fmt.Sprintf(`{"token":%q,"expiresAt":1755436980000}`, value)

	// Every mode above, in the order that also exercises store, unchanged and
	// replace: import, import again (unchanged), dry run, and --from-command.
	runs := [][]string{
		{"mytool"},
		{"mytool"},
		{"mytool", "--dry-run"},
		{"mytool", "mytool-manual", "--from-command", "printf %s " + value},
	}
	importable(t)
	newAnnotating(t)
	useHost(t, map[string][]byte{"keychain:Mytool-credentials": []byte(blob)})
	for _, args := range runs {
		var out bytes.Buffer
		if err := importSecrets(&out, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if strings.Contains(out.String(), value) {
			t.Errorf("%v printed a value:\n%s", args, out.String())
		}
		if strings.Contains(out.String(), blob) {
			t.Errorf("%v printed the source document:\n%s", args, out.String())
		}
	}
}

// Provenance is optional, so a plain Store still imports: the value is what
// the run needs, and an absent provenance is the same zero value a
// hand-created secret carries. That is the optional-interface contract, and
// without this test the fallback arm is never executed.
func TestStoreWithoutAnnotatorStillImports(t *testing.T) {
	importable(t)
	store := newFake(t) // a plain secret.Store: no Annotator, no Sizer
	useHost(t, map[string][]byte{"keychain:Mytool-credentials": []byte("v")})

	if err := importSecrets(&bytes.Buffer{}, []string{"mytool"}); err != nil {
		t.Fatal(err)
	}
	if got := string(store.items["mytool-token"]); got != "v" {
		t.Errorf("stored %q; want the value", got)
	}
	if p := store.provenance["mytool-token"]; !p.IsZero() {
		t.Errorf("a store that cannot carry provenance recorded %+v", p)
	}
}

// import is the only secret verb whose first argument is not a secret name, so
// `brig secret import claude-credentials` -- typed by someone who has just
// read a message naming that secret -- is the mistake people actually make.
// Answer it the way every other store error does: name the command they meant.
func TestImportNamesTheProfileWhenGivenASecret(t *testing.T) {
	importable(t)
	newAnnotating(t)
	useHost(t, nil)

	err := importSecrets(&bytes.Buffer{}, []string{"mytool-token"})
	if err == nil {
		t.Fatal("a secret name was accepted as a profile")
	}
	if !strings.Contains(err.Error(), "brig secret import mytool mytool-token") {
		t.Errorf("the error does not name the command they meant: %v", err)
	}
}

// `brig import claude` is someone reaching for `brig secret import claude`:
// this verb takes a file and that one takes a profile. Without the branch the
// answer is "no such file or directory" for a command that was nearly right.
func TestProfileImportPointsAtTheSecretVerb(t *testing.T) {
	importable(t)
	err := importProfile([]string{"mytool"})
	if err == nil {
		t.Fatal("a profile name was read as a file")
	}
	if !strings.Contains(err.Error(), "brig secret import mytool") {
		t.Errorf("the error does not name the verb they meant: %v", err)
	}
}
