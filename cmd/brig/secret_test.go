package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/secret"
	"github.com/brig-sh/brig/internal/ttytest"
)

// fakeStore stands in for the keychain. The keychain itself is covered in
// internal/secret against the real thing; these tests are about the CLI, and
// running them against the real store would make `go test ./...` write to the
// developer's login keychain from two packages instead of one.
type fakeStore struct {
	items map[string][]byte
	order []string
}

// The double is the thing most likely to drift: adding a method to Store
// breaks the keychain loudly, but a stale fake is only caught if some test
// happens to exercise the new method. This makes that a build failure.
// Pointer receivers, so the assertion is on *fakeStore.
var _ secret.Store = (*fakeStore)(nil)

func newFake(t *testing.T) *fakeStore {
	t.Helper()
	f := &fakeStore{items: map[string][]byte{}}
	old := openStore
	openStore = func() (secret.Store, error) { return f, nil }
	t.Cleanup(func() { openStore = old })
	return f
}

// seed puts a secret in the store as if it had been created there, keeping the
// map and the insertion order in step -- List reads the order, so a test that
// only wrote the map would see nothing.
func (f *fakeStore) seed(name, value string) {
	f.items[name] = []byte(value)
	f.order = append(f.order, name)
}

func (f *fakeStore) Kind() string { return "fake" }

func (f *fakeStore) Create(name string, value []byte) error {
	if _, ok := f.items[name]; ok {
		return secret.ErrExists
	}
	f.seed(name, string(value))
	return nil
}

func (f *fakeStore) Read(name string) ([]byte, error) {
	v, ok := f.items[name]
	if !ok {
		return nil, secret.ErrNotFound
	}
	return v, nil
}

func (f *fakeStore) Update(name string, value []byte) error {
	if _, ok := f.items[name]; !ok {
		return secret.ErrNotFound
	}
	f.items[name] = value
	return nil
}

func (f *fakeStore) Delete(name string) error {
	if _, ok := f.items[name]; !ok {
		return secret.ErrNotFound
	}
	delete(f.items, name)
	return nil
}

func (f *fakeStore) List() ([]secret.Secret, error) {
	var out []secret.Secret
	for _, n := range f.order {
		if _, ok := f.items[n]; ok {
			out = append(out, secret.Secret{
				Name:     n,
				Modified: time.Date(2026, 8, 14, 16, 23, 30, 0, time.UTC),
			})
		}
	}
	return out, nil
}

// pipeStdin points os.Stdin at a file holding the given bytes, so a test can
// exercise the default input path rather than only -f. A file rather than a
// pipe on purpose: it is not a terminal either, which is the only property the
// code under test asks about.
func pipeStdin(t *testing.T, data string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old; f.Close() })
}

// terminalStdin points os.Stdin at a real pseudo-terminal, so a test can
// exercise the branch a user hits when they type the command with nothing
// piped into it.
//
// /dev/null stood in for one before, on the strength of being a character
// device, which is all the old wrap.IsTerminal asked. It is not a terminal,
// and the tests below are about what brig does when there IS somebody at the
// other end -- a stand-in that a correct check rejects tests nothing. The
// master end comes back for a test that wants to type an answer.
func terminalStdin(t *testing.T) *os.File {
	t.Helper()
	return ttytest.AsStdin(t)
}

// `echo tok | brig secret create x` is the line people type, and a stored
// newline fails later in a way that reads like a bad token.
func TestCreateStripsOneTrailingNewlineFromStdin(t *testing.T) {
	f := newFake(t)
	pipeStdin(t, "tok\n")
	if err := secretCmd(&bytes.Buffer{}, []string{"create", "gh"}); err != nil {
		t.Fatal(err)
	}
	if got := string(f.items["gh"]); got != "tok" {
		t.Errorf("stored %q, want %q", got, "tok")
	}
}

// A value that arrived over something CRLF-terminated would otherwise keep the
// \r, which fails an auth header in the same way the \n does and is harder to
// see in a bug report.
func TestCreateStripsACRLFFromStdin(t *testing.T) {
	f := newFake(t)
	pipeStdin(t, "tok\r\n")
	if err := secretCmd(&bytes.Buffer{}, []string{"create", "gh"}); err != nil {
		t.Fatal(err)
	}
	if got := string(f.items["gh"]); got != "tok" {
		t.Errorf("stored %q, want %q", got, "tok")
	}
}

// Only a line ending is stripped. A lone trailing \r has no `echo` that
// produces it by accident, so it is part of the value.
func TestALoneCarriageReturnIsPartOfTheValue(t *testing.T) {
	f := newFake(t)
	pipeStdin(t, "tok\r")
	if err := secretCmd(&bytes.Buffer{}, []string{"create", "gh"}); err != nil {
		t.Fatal(err)
	}
	if got := string(f.items["gh"]); got != "tok\r" {
		t.Errorf("stored %q, want %q", got, "tok\r")
	}
}

// `-f "$KEYFILE"` with KEYFILE unset is the way this gets typed. Falling back
// to stdin there stores whatever the script had on it under the name and
// reports success.
func TestAnEmptyFilePathIsRefused(t *testing.T) {
	f := newFake(t)
	pipeStdin(t, "value-from-stdin")
	err := secretCmd(&bytes.Buffer{}, []string{"create", "gh", "-f", ""})
	if err == nil {
		t.Fatal("an empty -f was accepted")
	}
	if !strings.Contains(err.Error(), "-f") {
		t.Errorf("the error does not name the flag: %v", err)
	}
	if _, ok := f.items["gh"]; ok {
		t.Error("stdin was stored under the name anyway")
	}
}

// Every other verb refuses what it cannot use. ls accepting a name silently is
// the same problem one step further on: `brig secret ls gh-token` is someone
// reaching for read, and it printed the whole list and exited 0.
func TestListRefusesArguments(t *testing.T) {
	f := newFake(t)
	f.seed("gh", "v")
	var out bytes.Buffer
	err := secretCmd(&out, []string{"ls", "gh-token"})
	if err == nil {
		t.Fatal("ls accepted a name")
	}
	if !strings.Contains(err.Error(), "brig secret read gh-token") {
		t.Errorf("the error does not name the verb that does narrow: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("ls listed anyway: %q", out.String())
	}
	if err := secretCmd(&bytes.Buffer{}, []string{"list", "-f", "/etc/passwd"}); err == nil {
		t.Error("ls accepted a flag")
	}
}

// A file's bytes are what the file holds. Stripping here would corrupt a PEM
// key, whose final newline belongs to it.
func TestCreateFromAFileIsVerbatim(t *testing.T) {
	f := newFake(t)
	path := filepath.Join(t.TempDir(), "key.pem")
	const pem = "-----BEGIN KEY-----\nabc\n-----END KEY-----\n"
	if err := os.WriteFile(path, []byte(pem), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := secretCmd(&bytes.Buffer{}, []string{"create", "key", "-f", path}); err != nil {
		t.Fatal(err)
	}
	if got := string(f.items["key"]); got != pem {
		t.Errorf("stored %q, want %q", got, pem)
	}
}

func TestCreateRefusesACollisionWithAHelpfulMessage(t *testing.T) {
	f := newFake(t)
	f.seed("gh", "old")
	pipeStdin(t, "new")
	err := secretCmd(&bytes.Buffer{}, []string{"create", "gh"})
	if err == nil {
		t.Fatal("create overwrote an existing secret")
	}
	if !strings.Contains(err.Error(), "brig secret update gh") {
		t.Errorf("the error does not name the fix: %v", err)
	}
	if string(f.items["gh"]) != "old" {
		t.Error("the value was replaced")
	}
}

func TestReadWritesTheValueVerbatim(t *testing.T) {
	f := newFake(t)
	f.seed("gh", "tok")
	var out bytes.Buffer
	if err := secretCmd(&out, []string{"read", "gh"}); err != nil {
		t.Fatal(err)
	}
	// No trailing newline: the output is piped far more often than it is read,
	// and a newline brig added would travel into whatever consumes it.
	if out.String() != "tok" {
		t.Errorf("read wrote %q, want %q", out.String(), "tok")
	}
}

func TestReadReportsAbsenceByName(t *testing.T) {
	newFake(t)
	err := secretCmd(&bytes.Buffer{}, []string{"read", "ghost"})
	if err == nil {
		t.Fatal("reading an absent secret succeeded")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("the error does not name the secret: %v", err)
	}
	if !strings.Contains(err.Error(), "brig secret ls") {
		t.Errorf("the error does not say how to find the name: %v", err)
	}
}

// A dot would make `ref: secrets.a.b` ambiguous once profiles can reference
// secrets, so the grammar rejects it here rather than at the point of use.
func TestCreateRejectsABadName(t *testing.T) {
	newFake(t)
	pipeStdin(t, "v")
	if err := secretCmd(&bytes.Buffer{}, []string{"create", "a.b"}); err == nil {
		t.Error("a dotted name was accepted")
	}
}

// The name is checked before the value is read, so a bad name is reported
// without the secret ever being handled.
func TestABadNameIsRejectedBeforeTheValueIsRead(t *testing.T) {
	newFake(t)
	if err := secretCmd(&bytes.Buffer{}, []string{"create", "a.b", "-f", "/nonexistent"}); err == nil {
		t.Fatal("a dotted name was accepted")
	} else if strings.Contains(err.Error(), "/nonexistent") {
		t.Errorf("the file was read before the name was checked: %v", err)
	}
}

// On a host with no store, the platform is what the user needs to hear, and
// they need it before they have gone looking for a token to pipe in. Reading
// the value first meant `brig secret create x` at a prompt on Linux answered
// "no value on stdin. Pipe one in", and only said there was nowhere to put it
// once they had.
func TestAHostWithNoStoreSaysSoBeforeAskingForAValue(t *testing.T) {
	old := openStore
	openStore = func() (secret.Store, error) { return nil, secret.ErrUnsupported }
	t.Cleanup(func() { openStore = old })
	terminalStdin(t)

	err := secretCmd(&bytes.Buffer{}, []string{"create", "gh"})
	if !errors.Is(err, secret.ErrUnsupported) {
		t.Errorf("create reported %v, want the platform", err)
	}
}

func TestCreateRejectsAnEmptyValue(t *testing.T) {
	newFake(t)
	pipeStdin(t, "")
	if err := secretCmd(&bytes.Buffer{}, []string{"create", "empty"}); err == nil {
		t.Error("an empty secret was stored")
	}
}

func TestUnknownSubcommandNamesTheRealOnes(t *testing.T) {
	newFake(t)
	err := secretCmd(&bytes.Buffer{}, []string{"frobnicate"})
	if err == nil || !strings.Contains(err.Error(), "create") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// A bare `brig secret` is a question, not a mistake: say what the verbs are.
func TestNoSubcommandNamesTheVerbs(t *testing.T) {
	newFake(t)
	err := secretCmd(&bytes.Buffer{}, nil)
	if err == nil || !strings.Contains(err.Error(), "create") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestUpdateReplacesAndRefusesAnAbsentSecret(t *testing.T) {
	f := newFake(t)
	f.seed("gh", "old")
	pipeStdin(t, "new\n")
	if err := secretCmd(&bytes.Buffer{}, []string{"update", "gh"}); err != nil {
		t.Fatal(err)
	}
	if got := string(f.items["gh"]); got != "new" {
		t.Errorf("stored %q, want %q", got, "new")
	}

	pipeStdin(t, "x")
	err := secretCmd(&bytes.Buffer{}, []string{"update", "ghost"})
	if err == nil {
		t.Fatal("update created a secret that did not exist")
	}
	if !strings.Contains(err.Error(), "brig secret create ghost") {
		t.Errorf("the error does not name the fix: %v", err)
	}
	if _, ok := f.items["ghost"]; ok {
		t.Error("update created the secret anyway")
	}
}

func TestDeleteRemovesAndReportsAbsence(t *testing.T) {
	f := newFake(t)
	f.seed("gh", "v")
	var out bytes.Buffer
	if err := secretCmd(&out, []string{"delete", "gh", "-y"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.items["gh"]; ok {
		t.Error("delete left the secret in place")
	}
	if !strings.Contains(out.String(), "deleted gh") {
		t.Errorf("delete said nothing useful: %q", out.String())
	}
	if err := secretCmd(&bytes.Buffer{}, []string{"delete", "gh", "-y"}); err == nil {
		t.Error("deleting an absent secret succeeded")
	}
}

// rm is the spelling brig uses elsewhere -- `brig rm`, `brig profile rm` --
// so it works here too.
func TestDeleteAndRmAreSynonyms(t *testing.T) {
	f := newFake(t)
	f.seed("gh", "v")
	if err := secretCmd(&bytes.Buffer{}, []string{"rm", "gh", "-y"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.items["gh"]; ok {
		t.Error("rm did not delete")
	}
}

// A deleted secret cannot be recovered, so a delete nobody confirmed must not
// go through. Without a terminal there is nobody to ask, which is the shape a
// script has -- and a script that means it says so with -y.
func TestDeleteWithoutATerminalRefusesUnlessTold(t *testing.T) {
	f := newFake(t)
	f.seed("gh", "v")
	pipeStdin(t, "y\n")
	err := secretCmd(&bytes.Buffer{}, []string{"delete", "gh"})
	if err == nil {
		t.Fatal("delete went through with nobody to confirm it")
	}
	// The answer is not read off stdin: a value being piped in is not somebody
	// saying yes.
	if _, ok := f.items["gh"]; !ok {
		t.Error("the secret was deleted anyway")
	}
	if !strings.Contains(err.Error(), "-y") {
		t.Errorf("the refusal does not name the flag that answers it: %v", err)
	}
}

// The question defaults to no, so a terminal that says nothing -- here one
// whose other end has hung up -- leaves the secret alone.
func TestDeleteAtATerminalDefaultsToNo(t *testing.T) {
	f := newFake(t)
	f.seed("gh", "v")
	// A real terminal with nobody at the far end: closing the master is how
	// the read ends, where a pipe would simply reach EOF.
	_ = terminalStdin(t).Close()
	if err := secretCmd(&bytes.Buffer{}, []string{"delete", "gh"}); err == nil {
		t.Fatal("an unanswered delete went through")
	}
	if _, ok := f.items["gh"]; !ok {
		t.Error("an unanswered delete removed the secret")
	}
}

// -y is the answer given in advance, so it is taken wherever it is written.
func TestDeleteTakesYesBeforeOrAfterTheName(t *testing.T) {
	for _, args := range [][]string{
		{"delete", "-y", "gh"},
		{"delete", "gh", "-y"},
		{"delete", "gh", "--yes"},
	} {
		f := newFake(t)
		f.seed("gh", "v")
		terminalStdin(t)
		if err := secretCmd(&bytes.Buffer{}, args); err != nil {
			t.Fatalf("%q: %v", args, err)
		}
		if _, ok := f.items["gh"]; ok {
			t.Errorf("%q did not delete", args)
		}
	}
}

// The flag delete does take must not turn every other flag into a name.
func TestDeleteRejectsAnUnknownFlag(t *testing.T) {
	newFake(t)
	err := secretCmd(&bytes.Buffer{}, []string{"delete", "-f", "gh"})
	if err == nil {
		t.Fatal("delete accepted an unknown flag")
	}
	if !strings.Contains(err.Error(), "-f") {
		t.Errorf("the error does not name the flag: %v", err)
	}
}

// captureStderr points os.Stderr at a file and returns what was written to it.
// The terminal on the other side is a real one, allocated by the caller: brig
// asks the terminal driver now, so a stand-in that is merely a character
// device is not a terminal and never reaches the branch under test.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stderr")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = f
	fn()
	os.Stderr = old
	f.Close()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// create refuses a value typed at a terminal because the scrollback keeps it.
// read prints one to that same terminal, so it says so rather than leaving the
// two halves of the same argument disagreeing.
func TestReadToATerminalSaysItIsInTheScrollback(t *testing.T) {
	f := newFake(t)
	f.seed("gh", "tok")
	_, tty := ttytest.Pair(t)
	msg := captureStderr(t, func() {
		if err := secretCmd(tty, []string{"read", "gh"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(msg, "scrollback") {
		t.Errorf("read to a terminal said nothing about the scrollback: %q", msg)
	}
	// The name, not the value: a warning that repeated the secret would put a
	// second copy of it on the screen it is warning about.
	if strings.Contains(msg, "tok") {
		t.Errorf("the warning repeated the value: %q", msg)
	}
}

// A pipe keeps nothing, so there is nothing to say, and saying it would land
// in whatever is reading stderr.
func TestReadToAPipeIsSilent(t *testing.T) {
	f := newFake(t)
	f.seed("gh", "tok")
	var out bytes.Buffer
	msg := captureStderr(t, func() {
		if err := secretCmd(&out, []string{"read", "gh"}); err != nil {
			t.Fatal(err)
		}
	})
	if msg != "" {
		t.Errorf("read to a pipe wrote to stderr: %q", msg)
	}
	if out.String() != "tok" {
		t.Errorf("read to a pipe is not byte-exact: %q", out.String())
	}
}

// Asking for help is not a mistake, so it is answered with help rather than
// with an error -- and every verb answers, not only the group.
func TestHelpIsAnsweredWithHelp(t *testing.T) {
	for _, args := range [][]string{
		{"--help"},
		{"-h"},
		{"help"},
		{"create", "--help"},
		{"update", "-h"},
		{"read", "--help"},
		{"delete", "--help"},
		{"ls", "--help"},
	} {
		newFake(t)
		var out bytes.Buffer
		if err := secretCmd(&out, args); err != nil {
			t.Errorf("%q: %v", args, err)
			continue
		}
		for _, want := range []string{"brig secret create", "brig secret delete", "-f, --file"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%q: help is missing %q:\n%s", args, want, out.String())
			}
		}
	}
}

// The flag package spells a help request as an error, and that spelling must
// not reach anyone: it reads as a failure and it exits non-zero.
func TestHelpDoesNotLeakTheFlagPackagesWording(t *testing.T) {
	newFake(t)
	var out bytes.Buffer
	if err := secretCmd(&out, []string{"create", "--help"}); err != nil {
		t.Fatalf("asking for help failed: %v", err)
	}
	if strings.Contains(out.String(), "help requested") {
		t.Errorf("the flag package's wording reached the user:\n%s", out.String())
	}
}

func TestListShowsNamesAndDates(t *testing.T) {
	f := newFake(t)
	f.seed("alpha", "a")
	f.seed("beta", "b")
	var out bytes.Buffer
	if err := secretCmd(&out, []string{"ls"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NAME", "UPDATED", "alpha", "beta", "2026-08-14"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("ls output is missing %q:\n%s", want, out.String())
		}
	}
	// Values are never listed: ls reads attributes, not secrets.
	for _, never := range []string{"a", "b"} {
		for _, line := range strings.Split(out.String(), "\n") {
			if strings.TrimSpace(line) == never {
				t.Errorf("ls printed a value: %q", line)
			}
		}
	}
}

func TestListWithNoSecretsSaysHowToAddOne(t *testing.T) {
	newFake(t)
	var out bytes.Buffer
	if err := secretCmd(&out, []string{"ls"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "brig secret create") {
		t.Errorf("an empty list is unhelpful: %q", out.String())
	}
}

func TestWriteRejectsAnUnknownFlag(t *testing.T) {
	newFake(t)
	pipeStdin(t, "v")
	err := secretCmd(&bytes.Buffer{}, []string{"create", "gh", "--fyle", "x"})
	if err == nil {
		t.Fatal("an unknown flag was accepted")
	}
	if !strings.Contains(err.Error(), "--fyle") {
		t.Errorf("the error drops the second dash the user typed: %v", err)
	}
}

// read and delete take no flags at all, so a flag is a mistake to name rather
// than a name to look up -- otherwise the error is `no secret named "-f"`.
func TestReadRejectsAFlag(t *testing.T) {
	newFake(t)
	err := secretCmd(&bytes.Buffer{}, []string{"read", "-f", "/dev/null"})
	if err == nil {
		t.Fatal("read accepted a flag")
	}
	if !strings.Contains(err.Error(), "-f") {
		t.Errorf("the error does not name the flag: %v", err)
	}
}

// --stdin is the default rather than a mode, but the docs name it, so writing
// it out loud has to work.
func TestStdinIsAcceptedExplicitly(t *testing.T) {
	f := newFake(t)
	pipeStdin(t, "tok")
	if err := secretCmd(&bytes.Buffer{}, []string{"create", "gh", "--stdin"}); err != nil {
		t.Fatal(err)
	}
	if got := string(f.items["gh"]); got != "tok" {
		t.Errorf("stored %q, want %q", got, "tok")
	}
}

// `-f -` is the conventional spelling of "the file is stdin", and the README
// uses it, so it has to mean stdin rather than a file called "-".
func TestDashAsTheFileMeansStdin(t *testing.T) {
	f := newFake(t)
	pipeStdin(t, "tok")
	if err := secretCmd(&bytes.Buffer{}, []string{"create", "gh", "-f", "-"}); err != nil {
		t.Fatal(err)
	}
	if got := string(f.items["gh"]); got != "tok" {
		t.Errorf("stored %q, want %q", got, "tok")
	}
}

// Two sources at once has no reading that can be guessed at, and guessing
// would silently store the wrong one.
func TestStdinAndFileTogetherAreRefused(t *testing.T) {
	newFake(t)
	pipeStdin(t, "tok")
	err := secretCmd(&bytes.Buffer{}, []string{"create", "gh", "--stdin", "-f", "/dev/null"})
	if err == nil {
		t.Fatal("two sources were accepted")
	}
}

// The name comes before the flag in the line the docs use, and Parse stops at
// the first bare word, so this is the case the lift-and-continue loop exists
// for.
func TestFlagAfterTheNameIsHonoured(t *testing.T) {
	f := newFake(t)
	path := filepath.Join(t.TempDir(), "v")
	if err := os.WriteFile(path, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := secretCmd(&bytes.Buffer{}, []string{"create", "gh", "-f", path}); err != nil {
		t.Fatal(err)
	}
	if got := string(f.items["gh"]); got != "from-file" {
		t.Errorf("stored %q, want %q", got, "from-file")
	}
}

// A second bare word is a typo -- a name with a space in it, most likely --
// and storing the first while dropping the second would hide it.
func TestASecondNameIsRefused(t *testing.T) {
	newFake(t)
	pipeStdin(t, "v")
	if err := secretCmd(&bytes.Buffer{}, []string{"create", "gh", "token"}); err == nil {
		t.Error("a second bare word was accepted")
	}
}
