package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
)

// captureStdout runs fn with os.Stdout replaced, and returns what it wrote.
// listProfiles prints straight to stdout rather than taking a writer -- it is
// the top of the call chain, with nothing further up to hand one in -- so a
// pipe is the only seam a test has.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	fnErr := fn()
	w.Close()
	os.Stdout = orig

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String(), fnErr
}

// writeProfile writes one profile body into a fresh directory and returns it,
// for the tests that need a profile the registry will load. A directory of its
// own per call, because the registry reads every yaml in it and a leftover
// from another test would silently join the merged set.
func writeProfile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "profile.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// export with a destination writes the file, verbatim: the comments are the
// reason export writes YAML at all.
func TestExportProfileToAFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}

	if err := exportProfile([]string{"claude-code", "mine"}); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(dir, "mine.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// The file and the profile in it agree: a destination is the name the
	// exported profile carries, not only the name of the file it lands in.
	if !strings.Contains(string(blob), "name: mine") {
		t.Errorf("the export is not a profile called mine:\n%s", blob)
	}
	if strings.Contains(string(blob), "name: claude-code") {
		t.Errorf("mine.yaml still declares the profile it was copied from:\n%s", blob)
	}
	if !strings.Contains(string(blob), "outrank Claude Code's subscription credential") {
		t.Error("the destination file lost the comments")
	}
}

// Export writes the profile directory and nothing else. A destination that is
// a path is refused rather than honoured -- otherwise a typo, or a profile
// name arriving from somewhere else, decides where brig writes on the host.
func TestExportRefusesAPathDestination(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "mine.yaml")
	for _, dest := range []string{elsewhere, "./mine.yaml", "../mine.yaml", "~/mine.yaml"} {
		if err := exportProfile([]string{"claude-code", dest}); err == nil {
			t.Errorf("export wrote to %q", dest)
		}
	}
	if _, err := os.Stat(elsewhere); !os.IsNotExist(err) {
		t.Error("export wrote outside the profile directory")
	}
}

// With no destination, export prints and writes nothing. That is what keeps
// `brig profile export x | brig profile import -` working, and what stops the
// command touching the profile directory when it was not asked to.
func TestExportWithNoDestinationWritesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	// No destination: this prints, and leaves the directory alone.
	if err := exportProfile([]string{"claude-code"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("export wrote into the profile directory: %v", entries)
	}
}

// The listing answers "where did this profile come from", which is the same
// question as "why is brig booting that image".
func TestListProfilesMarksOrigin(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	override := []byte("name: claude-code\nimage: docker.io/me/mine:latest\n" +
		"guestHome: /home/claude\nbinary: claude\nmem: 1\ncpus: 1\n")
	if err := os.WriteFile(filepath.Join(dir, "claude-code.yaml"), override, 0o644); err != nil {
		t.Fatal(err)
	}
	own := []byte("name: mine\nimage: i\nguestHome: /home/mine\nbinary: m\nmem: 1\ncpus: 1\n")
	if err := os.WriteFile(filepath.Join(dir, "mine.yaml"), own, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}

	if !profile.OverridesBuiltIn("claude-code") {
		t.Error("the override is not reported as one")
	}
	if profile.OverridesBuiltIn("mine") {
		t.Error("a profile of your own is reported as overriding a built-in")
	}
	if profile.IsCustom("codex") {
		t.Error("an embedded profile is reported as file-backed")
	}
}

// The listing reports what a run actually needs: bindings by name and
// secrets by name, with the command that creates a missing one. forward: is
// what this replaces -- C1 folds every forward: entry into env: at parse
// time, so a listing that still looked at Forward would go blank the moment
// that lands.
func TestListProfilesReportsBindingsAndRequiredSecrets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	blob := []byte("name: mine\nimage: i\nguestHome: /home/mine\nbinary: m\nmem: 1\ncpus: 1\n" +
		"secrets:\n  - gh_token\n" +
		"env:\n  - name: FOO\n    value: not-a-secret\n  - name: GH_TOKEN\n    ref: secrets.gh_token\n")
	if err := os.WriteFile(filepath.Join(dir, "mine.yaml"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, listProfiles)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out, "environment: FOO GH_TOKEN") {
		t.Errorf("listing does not report the env bindings by name:\n%s", out)
	}
	if !strings.Contains(out, "secrets: gh_token") {
		t.Errorf("listing does not name the required secret:\n%s", out)
	}
	// gh_token declares no sources, so it is hand-created by definition and
	// the listing points at the one command that can supply it. A secret that
	// declares sources gets the other line instead -- which is the split the
	// old single "(brig secret create <name>)" suffix could not make.
	if !strings.Contains(out, "by hand: brig secret create <name> (gh_token)") {
		t.Errorf("listing does not say how to create the hand-created secret:\n%s", out)
	}
	// Scoped to this profile's own name: the shipped claude-code declares an
	// importable secret and prints the other line, so an unscoped search for
	// "secret import" asserts nothing about mine and fails on someone else's
	// correct output.
	if strings.Contains(out, "brig secret import mine") {
		t.Errorf("a secret with no sources is offered as importable:\n%s", out)
	}
	// "never forwarded:" is the deny list, untouched by this change, so the
	// check is for the removed "forwards:" label specifically.
	if strings.Contains(out, "forwards:") {
		t.Errorf("listing still reports the deprecated forward list:\n%s", out)
	}
	// The literal env value must never leak into the listing either -- a
	// listing is names, not values, whatever the source.
	if strings.Contains(out, "not-a-secret") {
		t.Errorf("listing printed a bound value:\n%s", out)
	}
}

// ls and list are the same listing. There is no reason to make anyone guess
// which spelling brig wants.
func TestProfileLsAndListAreSynonyms(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	for _, verb := range []string{"ls", "list"} {
		if err := profileCmd([]string{verb}); err != nil {
			t.Errorf("brig profile %s: %v", verb, err)
		}
	}
	if err := profileCmd([]string{"nonsense"}); err == nil {
		t.Error("an unknown subcommand was accepted")
	}
}

// The profile directory may not exist yet: `brig profile edit` sends a
// first-time user straight to this command, and on a fresh install nothing has
// created it.
func TestExportCreatesTheProfileDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "brig")
	t.Setenv("BRIG_PROFILE_DIR", dir)
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	if err := exportProfile([]string{"claude-code", "claude-code"}); err != nil {
		t.Fatalf("the flow `brig profile edit` prints does not work: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("no directory created: %v", err)
	}
	// 0700 per the XDG spec, and the right mode for files naming credentials.
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("profile directory mode = %04o, want 0700", perm)
	}
	if _, err := os.Stat(filepath.Join(dir, "claude-code.yaml")); err != nil {
		t.Errorf("no file written: %v", err)
	}
}

// A destination is a name, not a path: the profile directory is the only place
// a profile file does anything, so typing it out was busywork with a wrong
// answer. This is the whole flow -- export, then edit -- with no path in it.
func TestExportBareNameLandsInTheProfileDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "brig")
	t.Setenv("BRIG_PROFILE_DIR", dir)
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	if err := exportProfile([]string{"claude-code", "mine"}); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(dir, "mine.yaml"))
	if err != nil {
		t.Fatalf("export did not write into the profile directory: %v", err)
	}
	if !strings.Contains(string(blob), "name: mine") {
		t.Errorf("the export is not a profile called mine:\n%s", blob)
	}
	// And brig reads it back on the next load, which is the point of putting
	// it there rather than wherever the user happened to be standing. Under a
	// name of its own it is a profile of your own, not an override: overriding
	// the built-in is what exporting under the built-in's name does.
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	if !profile.IsCustom("mine") {
		t.Error("the exported file is not picked up as a profile of your own")
	}
	if profile.OverridesBuiltIn("claude-code") {
		t.Error("an export under a new name shadowed the profile it was copied from")
	}
	if err := exportProfile([]string{"claude-code", "claude-code"}); err != nil {
		t.Fatal(err)
	}
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	if !profile.OverridesBuiltIn("claude-code") {
		t.Error("an export under the built-in's own name is not picked up as an override")
	}
	// --json names the file after the format it is in.
	if err := exportProfile([]string{"codex", "robot", "--json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "robot.json")); err != nil {
		t.Errorf("--json did not write robot.json: %v", err)
	}
}

// An export is generated bytes: it has none of the edits that are the whole
// reason the file exists. Overwriting one silently is how an afternoon of
// tuning a deny list disappears.
func TestExportRefusesToOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(dir, "mine.yaml")
	edited := []byte("name: claude-code\nimage: docker.io/me/hand-tuned:latest\n" +
		"guestHome: /home/claude\nbinary: claude\nmem: 1\ncpus: 1\n")
	if err := os.WriteFile(mine, edited, 0o644); err != nil {
		t.Fatal(err)
	}

	err := exportProfile([]string{"claude-code", "mine"})
	if err == nil {
		t.Fatal("export overwrote an existing file without being asked")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("the error does not name the way through: %v", err)
	}
	if got, _ := os.ReadFile(mine); string(got) != string(edited) {
		t.Error("the file was overwritten anyway")
	}

	if err := exportProfile([]string{"claude-code", "mine", "--force"}); err != nil {
		t.Fatalf("--force did not overwrite: %v", err)
	}
	if got, _ := os.ReadFile(mine); string(got) == string(edited) {
		t.Error("--force left the old file in place")
	}
}

// A mistyped flag must not become a file name. Without this, `brig profile
// export codex --jsonn` reports success and leaves a file called --jsonn.
func TestExportRejectsAnUnknownFlag(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	if err := exportProfile([]string{"codex", "--jsonn"}); err == nil {
		t.Fatal("a mistyped flag was taken as a destination")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a file was written for a mistyped flag: %v", entries)
	}
}

// Export says what the run line says. Left alone, the flag package answers in
// its own voice -- `invalid boolean value "x" for -json: parse error` -- which
// names the flag with one dash and ends in a phrase about the parser.
func TestExportSpeaksBrigsWordingForABadValue(t *testing.T) {
	err := exportProfile([]string{"claude-code", "--json=x"})
	if err == nil {
		t.Fatal("--json=x was accepted")
	}
	if !strings.Contains(err.Error(), "--json is either given or not") ||
		strings.Contains(err.Error(), "parse error") {
		t.Errorf("--json=x: %v, want brig's own wording naming --json", err)
	}
}

// Two files in the profile directory can declare one profile, so rm has to
// take both. Taking only the one that loaded promotes the other, and the
// command reports success with the profile still listed.
func TestRemoveProfileTakesEveryFileDeclaringTheName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	blob := []byte("name: codex\nimage: docker.io/me/mine:latest\n" +
		"guestHome: /home/codex\nbinary: codex\nmem: 1\ncpus: 1\n")
	for _, base := range []string{"codex.yaml", "pinned.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, base), blob, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = profile.Load(profile.Dir()) // duplicates are reported, not fatal

	// -y because pinned.yaml is not the file the argument names, and rm asks
	// before deleting one of those. The second file is the whole point here,
	// so answering the question in advance is the way to reach the case.
	if err := removeProfile([]string{"codex", "-y"}); err != nil {
		t.Fatal(err)
	}
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatalf("a file was left behind: %v", err)
	}
	if profile.IsCustom("codex") {
		t.Error("codex is still file-backed after rm")
	}
}

// rm resolves through the registry, so it removes the file that actually backs
// a profile -- whatever the file is called -- and it understands aliases.
func TestRemoveProfileResolvesFileAndAlias(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	blob := []byte("name: claude-code\nimage: docker.io/me/pinned:latest\n" +
		"guestHome: /home/claude\nbinary: claude\nmem: 1\ncpus: 1\n")
	// A basename that says nothing about which profile it serves, which is what
	// `brig profile export claude-code pinned.yaml` produces.
	pinned := filepath.Join(dir, "pinned.yaml")
	if err := os.WriteFile(pinned, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	// By alias, not even by the profile's own name -- which is a file the
	// argument did not name, so the answer comes with it.
	if err := removeProfile([]string{"claude", "-y"}); err != nil {
		t.Fatalf("could not remove an override by alias: %v", err)
	}
	if _, err := os.Stat(pinned); !os.IsNotExist(err) {
		t.Error("the override file is still there")
	}
	// And a built-in still reports as one rather than as missing.
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	err := removeProfile([]string{"codex"})
	if err == nil || !strings.Contains(err.Error(), "built-in") {
		t.Errorf("wrong error for a built-in: %v", err)
	}
}

// The other half of the split: a secret that declares where brig could read it
// on the host is not something you have to mint by hand, and the listing names
// the command that fills it. That command takes the PROFILE, so the line reads
// `brig secret import mine (from-the-host)` -- one command, then the names it
// covers -- against the create line's name at a time.
func TestListProfilesSeparatesImportableSecretsFromHandCreatedOnes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	blob := []byte("name: mine\nimage: i\nguestHome: /home/mine\nbinary: m\nmem: 1\ncpus: 1\n" +
		"secrets:\n" +
		"  - name: from-the-host\n    from: keychain\n    service: Some Service\n" +
		"  - by-hand\n")
	if err := os.WriteFile(filepath.Join(dir, "mine.yaml"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, listProfiles)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "from your host: brig secret import mine (from-the-host)") {
		t.Errorf("listing does not report the importable secret:\n%s", out)
	}
	if !strings.Contains(out, "by hand: brig secret create <name> (by-hand)") {
		t.Errorf("listing does not report the hand-created secret:\n%s", out)
	}
	// One import line for the profile, not one per importable name: the
	// command is the same command for every name on it.
	if n := strings.Count(out, "brig secret import mine"); n != 1 {
		t.Errorf("the import command appears %d times:\n%s", n, out)
	}
	// A keychain service name is not a value, but it is the user's own
	// vocabulary rather than brig's, and a listing is names.
	if strings.Contains(out, "Some Service") {
		t.Errorf("listing printed a source locator:\n%s", out)
	}
}

// The recipe brig prints has to work as written: export the closest built-in
// under a name of your own, edit it, run it, remove it by the name you chose.
// Every step addresses that name, which is what did not work while the file
// was called mytool.yaml and the profile inside it was still claude-code --
// edit had no such profile to open, and rm reached the file only under the
// name of the profile it was copied from. The run leg is script/smoke.sh,
// which has a runtime to boot against; what a run needs from here is that the
// name resolves to this file, with the settings it was copied from.
func TestExportedProfileIsEditableAndRemovableByItsNewName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	if err := exportProfile([]string{"claude-code", "mytool"}); err != nil {
		t.Fatal(err)
	}
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}

	p, ok := profile.Lookup("mytool")
	if !ok {
		t.Fatal("the exported profile is not there under the name it was exported as")
	}
	built, _ := profile.Lookup("claude-code")
	if p.Name != "mytool" || p.Image != built.Image || p.GuestHome != built.GuestHome {
		t.Errorf("mytool is not the profile it was copied from, renamed: %+v", p)
	}
	if path, ok := profile.Path("mytool"); !ok || path != filepath.Join(dir, "mytool.yaml") {
		t.Errorf("Path(mytool) = %q, %v", path, ok)
	}
	// Renamed and nothing else: the comments are why anyone starts from an
	// existing profile rather than a blank file.
	blob, err := os.ReadFile(filepath.Join(dir, "mytool.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), "outrank Claude Code's subscription credential") {
		t.Errorf("the rename cost the file its comments:\n%s", blob)
	}

	stubEditor(t, `printf '\n# tuned by hand\n' >> "$1"`)
	if err := editProfile([]string{"mytool"}); err != nil {
		t.Fatalf("editing the exported profile by its own name failed: %v", err)
	}
	if after, err := os.ReadFile(filepath.Join(dir, "mytool.yaml")); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(after), "tuned by hand") {
		t.Errorf("the edit is not on disk:\n%s", after)
	}

	// And out again by the same name, with nothing asked: this is the file the
	// argument names.
	if err := removeProfile([]string{"mytool"}); err != nil {
		t.Fatalf("removing the exported profile by its own name failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mytool.yaml")); !os.IsNotExist(err) {
		t.Error("the file is still there after rm")
	}
}

// rm resolves through the registry, so the file it deletes need not be the one
// the argument spells. That is worth keeping -- it is what makes rm work on an
// alias -- but it must not delete a file nobody named without a word: the bug
// this guards was `brig profile rm claude-code` removing mytool.yaml, silently
// and with exit 0.
func TestRemoveProfileAsksBeforeDeletingAFileYouDidNotName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	pinned := filepath.Join(dir, "pinned.yaml")
	blob := []byte("name: mytool\nimage: i\nguestHome: /home/x\nbinary: x\nmem: 1\ncpus: 1\n")
	if err := os.WriteFile(pinned, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	// A stdin of the test's own, closed: nothing to ask on, and an answer that
	// is not yes if anything did ask. `go test` usually hands the binary
	// /dev/null, but a test binary run from a terminal would otherwise sit
	// waiting for someone to type.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	orig := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = orig; r.Close() }()

	err = removeProfile([]string{"mytool"})
	if err == nil {
		t.Fatal("rm deleted a file the argument did not name, without asking")
	}
	if !strings.Contains(err.Error(), "-y") || !strings.Contains(err.Error(), pinned) {
		t.Errorf("the refusal names neither the file nor the way through: %v", err)
	}
	if _, statErr := os.Stat(pinned); statErr != nil {
		t.Error("the file was removed anyway")
	}

	// -y is that question answered in advance, the way the secret verbs spell
	// it.
	if err := removeProfile([]string{"mytool", "-y"}); err != nil {
		t.Fatalf("-y did not answer the question: %v", err)
	}
	if _, err := os.Stat(pinned); !os.IsNotExist(err) {
		t.Error("-y left the file in place")
	}
}
