package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/profile"
)

func writePolicyFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name+".yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const noNetBody = "apiVersion: brig.sh/v1alpha1\nname: no-net\ndesc: nothing outbound\n" +
	"egress:\n  default: deny\n  allow:\n    - host: api.anthropic.com\n"

// A fresh install has no policy directory or policies in it. ls says so
// rather than printing nothing, so the reader knows the emptiness is
// expected and how to fix it.
func TestListPoliciesEmpty(t *testing.T) {
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	out, err := captureStdout(t, listPolicies)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no policies yet") {
		t.Errorf("empty directory did not say so: %q", out)
	}
}

func TestListPoliciesSortedAndSkipsABadFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "b", "apiVersion: brig.sh/v1alpha1\nname: zzz\negress:\n  default: allow\n")
	writePolicyFile(t, dir, "a", noNetBody)
	writePolicyFile(t, dir, "bad", "apiVersion: brig.sh/v1alpha1\nname: broken\nunexpected: true\negress:\n  default: deny\n")

	// listPolicies never fails the command over one bad file: the failure
	// shows up on stderr, not as a returned error, so one broken policy
	// does not hide the ones that work.
	var out string
	warning := captureStderr(t, func() {
		var err error
		out, err = captureStdout(t, listPolicies)
		if err != nil {
			t.Fatalf("listPolicies returned an error over one bad file: %v", err)
		}
	})
	if !strings.Contains(warning, "bad.yaml") {
		t.Errorf("the broken file was not named on stderr: %q", warning)
	}
	if !strings.Contains(out, "no-net") || !strings.Contains(out, "zzz") {
		t.Errorf("both good policies were not listed: %q", out)
	}
	if strings.Index(out, "no-net") > strings.Index(out, "zzz") {
		t.Errorf("policies are not sorted by name: %q", out)
	}
}

// A policy nothing binds gets no second line: the common case stays one
// line per policy, the way it always has.
func TestListPoliciesOmitsBoundToWhenNothingBinds(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)

	out, err := captureStdout(t, listPolicies)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "bound to") {
		t.Errorf("an unbound policy printed a bound to line: %q", out)
	}
}

func TestListPoliciesShowsAProfileAttachment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)
	if err := attachPolicy([]string{"no-net", "claude-code"}); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, listPolicies)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "bound to: claude-code") {
		t.Errorf("the profile attachment was not listed: %q", out)
	}
}

func TestListPoliciesShowsASessionAttachment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)
	if err := attachPolicy([]string{"no-net", "claude-code", "-n", "work"}); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, listPolicies)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "bound to: claude-code -n work") {
		t.Errorf("the session attachment was not listed: %q", out)
	}
}

func TestListPoliciesShowsAnInlineDeclaration(t *testing.T) {
	policyDir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", policyDir)
	writePolicyFile(t, policyDir, "no-net", noNetBody)
	t.Setenv("BRIG_PROFILE_DIR", writeProfile(t, `
name: mytool
image: ghcr.io/brig-sh/mytool:latest
guestHome: /home/mytool
binary: mytool
mem: 1024
cpus: 1
policy: [no-net]
`))
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, listPolicies)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "bound to: mytool (inline)") {
		t.Errorf("the inline declaration was not listed: %q", out)
	}
}

// Listing policies never depended on attachments.yaml before attach
// existed, and it must not start failing outright just because that file
// is broken -- diagnosing a policy problem is exactly when brig policies
// still needs to work.
func TestListPoliciesToleratesAMalformedAttachmentsFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)
	if err := os.WriteFile(filepath.Join(dir, "attachments.yaml"),
		[]byte("profiles: [this is not valid: yaml structure"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out string
	warning := captureStderr(t, func() {
		var err error
		out, err = captureStdout(t, listPolicies)
		if err != nil {
			t.Fatalf("listPolicies returned an error over a broken attachments.yaml: %v", err)
		}
	})
	if !strings.Contains(out, "no-net") {
		t.Errorf("the policy was not listed: %q", out)
	}
	if warning == "" {
		t.Error("a broken attachments.yaml was not reported on stderr")
	}
}

// A directory listPolicies cannot even read is a different failure from an
// empty one: LoadAll returns a nil map only for that case, and "no policies
// yet" would be the wrong thing to say -- the truth is brig could not look.
func TestListPoliciesReturnsAnErrorForAnUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode-000 directory, so this cannot be reproduced")
	}
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)

	out, err := captureStdout(t, listPolicies)
	if err == nil {
		t.Fatal("listPolicies returned nil for an unreadable directory")
	}
	if strings.Contains(out, "no policies yet") {
		t.Errorf("an unreadable directory was reported as having no policies: %q", out)
	}
}

// create writes a starter document and opens it in the editor -- a no-op
// editor here, since the point of this test is what got written to disk.
func TestCreatePolicyWritesAStarter(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	stubEditor(t, `true`)

	if err := createPolicy([]string{"no-net"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "no-net.yaml")
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("nothing was written: %v", err)
	}
	if !strings.Contains(string(blob), "name: no-net") {
		t.Errorf("the starter is not a policy named no-net:\n%s", blob)
	}
	if _, err := readPolicyFile(path); err != nil {
		t.Errorf("the starter does not parse and validate on its own: %v", err)
	}
}

// attachments.yaml is the attachment record, and LoadAll skips it, so a
// policy by that name would be invisible to every command and writing it
// would destroy whatever the record held. Refused either way: --force
// replaces a policy you own, not brig's own bookkeeping.
func TestCreatePolicyRefusesAReservedName(t *testing.T) {
	for _, force := range []string{"", "--force"} {
		name := "plain"
		if force != "" {
			name = "force"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("BRIG_POLICY_DIR", dir)
			loadTestProfiles(t)
			if err := attachPolicy([]string{"no-net", "claude-code"}); err == nil {
				t.Fatal("expected the attach to fail: no-net does not exist here")
			}
			// A real record to destroy, written the way attach writes one.
			var a policy.Attachments
			a.AttachToProfile("no-net", "claude-code")
			if err := a.Save(dir); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(dir, "attachments.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			stubEditor(t, `echo should not run >&2; exit 1`)

			args := []string{"attachments"}
			if force != "" {
				args = append(args, force)
			}
			err = createPolicy(args)
			if err == nil {
				t.Fatalf("create %v was accepted", args)
			}
			if !strings.Contains(err.Error(), "reserved") {
				t.Errorf("the error does not say the name is reserved: %v", err)
			}
			after, readErr := os.ReadFile(filepath.Join(dir, "attachments.yaml"))
			if readErr != nil {
				t.Fatalf("the attachment record was removed: %v", readErr)
			}
			if string(after) != string(before) {
				t.Errorf("the attachment record was overwritten:\n%s", after)
			}
		})
	}
}

// A starter document is generated bytes, but the file it would replace might
// carry real edits -- create refuses to overwrite one without --force.
func TestCreatePolicyRefusesAnExistingFileWithoutForce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	path := writePolicyFile(t, dir, "no-net", noNetBody)
	stubEditor(t, `echo should not run >&2; exit 1`)

	if err := createPolicy([]string{"no-net"}); err == nil {
		t.Fatal("create overwrote an existing file without --force")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != noNetBody {
		t.Errorf("the existing file was changed:\n%s", after)
	}
}

// --force overrides the refusal above, and works whether it is typed before
// or after the name -- flag.Parse stops at the first bare word, so a flag
// after the name has to be lifted and re-parsed rather than left in Args.
func TestCreatePolicyForceOverwritesFlagAfterName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	stubEditor(t, `sed -i.bak 's/^desc:$/desc: replaced/' "$1"`)

	if err := createPolicy([]string{"no-net", "--force"}); err != nil {
		t.Fatalf("create --force after the name failed: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "no-net.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "replaced") {
		t.Errorf("the starter did not replace the old file:\n%s", after)
	}
}

// When the editor fails, create is left half done unless it also removes
// the starter it just wrote -- otherwise the command reports failure but
// brig policies lists the policy anyway, and a retry refuses because the
// file is already there.
func TestCreatePolicyRemovesTheStarterWhenTheEditorFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	stubEditor(t, `exit 1`)

	if err := createPolicy([]string{"no-net"}); err == nil {
		t.Fatal("create succeeded despite the editor failing")
	}
	if _, err := os.Stat(filepath.Join(dir, "no-net.yaml")); !os.IsNotExist(err) {
		t.Errorf("the starter was left on disk after the editor failed (stat err = %v)", err)
	}
}

// --force's overwrite is a different case: the file the editor failed to
// touch already existed before create ran, so removing it would leave the
// user with no file at all where they had one a moment ago.
func TestCreatePolicyForceKeepsTheFileWhenTheEditorFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	path := writePolicyFile(t, dir, "no-net", noNetBody)
	stubEditor(t, `exit 1`)

	if err := createPolicy([]string{"no-net", "--force"}); err == nil {
		t.Fatal("create succeeded despite the editor failing")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("--force's file was removed after the editor failed, leaving none at all: %v", err)
	}
	if string(after) != noNetBody {
		t.Errorf("--force overwrote the real file with the starter before the editor even ran, "+
			"so the failing editor left the starter behind instead of the original content:\n%s", after)
	}
}

// The starter used to be written straight over path before the editor ran,
// so a --force whose editor then failed left the real content replaced by
// the blank starter -- the command reported failure, but the original was
// already gone. Written beside path instead and only renamed into place
// once the save parses, so a failing editor can never destroy it.
func TestCreatePolicyForceDoesNotLoseContentWhenTheEditorFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	const original = "apiVersion: brig.sh/v1alpha1\nname: no-net\n" +
		"desc: MY REAL CONTENT, DO NOT LOSE THIS\negress:\n  default: deny\n"
	path := writePolicyFile(t, dir, "no-net", original)
	stubEditor(t, `exit 1`)

	if err := createPolicy([]string{"no-net", "--force"}); err == nil {
		t.Fatal("create succeeded despite the editor failing")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Errorf("the real content was lost:\nwant %q\ngot  %q", original, after)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a temp file was left behind: %v", e.Name())
		}
	}
}

// The same content must survive an editor that runs but produces something
// that does not parse: --force still must not touch the real file until
// the new content is known to be good.
func TestCreatePolicyForceDoesNotLoseContentOnABrokenSave(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	const original = "apiVersion: brig.sh/v1alpha1\nname: no-net\n" +
		"desc: MY REAL CONTENT, DO NOT LOSE THIS\negress:\n  default: deny\n"
	path := writePolicyFile(t, dir, "no-net", original)
	stubEditor(t, `printf 'name: [unclosed\n' > "$1"`)

	err := createPolicy([]string{"no-net", "--force"})
	if err == nil {
		t.Fatal("create succeeded despite an unparseable save")
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != original {
		t.Errorf("the real content was lost:\nwant %q\ngot  %q", original, after)
	}
	scratchPath := strings.TrimSpace(strings.SplitAfter(err.Error(), "your edit is still at ")[1])
	if _, statErr := os.Stat(scratchPath); statErr != nil {
		t.Errorf("the error names a scratch file that does not exist: %v", statErr)
	}
}

// The name lives inside the file, not the filename, so a duplicate is
// possible even when the target path itself is free. create checks the
// name against every policy that already loads, not just the path it is
// about to write, and --force does not help here: forcing would only add
// a second file declaring the same name.
func TestCreatePolicyRefusesANameTakenByAnotherFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	other := writePolicyFile(t, dir, "custom", noNetBody) // declares name: no-net
	stubEditor(t, `echo should not run >&2; exit 1`)

	for _, args := range [][]string{{"no-net"}, {"no-net", "--force"}} {
		if err := createPolicy(args); err == nil {
			t.Fatalf("createPolicy(%v) = nil, want an error: no-net is custom.yaml's name", args)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "no-net.yaml")); err == nil {
		t.Error("create wrote no-net.yaml despite the name being taken by custom.yaml")
	}
	after, err := os.ReadFile(other)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != noNetBody {
		t.Errorf("custom.yaml was changed:\n%s", after)
	}
}

// A directory create cannot even read is a different failure from a clean
// namespace to write into: LoadAll returns a nil map only for that case,
// and create must surface it rather than press on toward MkdirAll/WriteFile.
func TestCreatePolicyReturnsAnErrorForAnUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode-000 directory, so this cannot be reproduced")
	}
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	stubEditor(t, `echo should not run >&2; exit 1`)

	if err := createPolicy([]string{"no-net"}); err == nil {
		t.Fatal("createPolicy returned nil for an unreadable directory")
	}
}

// show prints YAML by default, and works whether --json is typed before or
// after the name -- the same flag-after-a-bare-word case create's force
// flag has.
func TestShowPolicyJSONFlagAfterName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)

	out, err := captureStdout(t, func() error { return showPolicy([]string{"no-net"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "name: no-net") {
		t.Errorf("plain show is not YAML naming the policy: %q", out)
	}

	out, err = captureStdout(t, func() error { return showPolicy([]string{"no-net", "--json"}) })
	if err != nil {
		t.Fatalf("show --json after the name failed: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("show --json did not print valid JSON: %v\n%s", err, out)
	}
	if decoded["name"] != "no-net" {
		t.Errorf("show --json printed the wrong policy: %v", decoded)
	}
}

// create checks the name before it ever builds a path from it, so a name
// like "../../notes" is refused rather than joined into a path that escapes
// the policy directory.
func TestCreatePolicyRefusesAnUnsafeNameBeforeTouchingDisk(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	stubEditor(t, `echo should not run >&2; exit 1`)

	if err := createPolicy([]string{"../../escape"}); err == nil {
		t.Fatal("an unsafe name was accepted")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("create wrote something despite the unsafe name: %v", entries)
	}
}

// The starter template writes the name into the document unquoted, so a
// name YAML would read back as something else must be refused here, before
// create ever writes the file -- not just when a document is later parsed.
func TestCreatePolicyRefusesAnAmbiguousNameBeforeTouchingDisk(t *testing.T) {
	for _, name := range []string{"no", "0x10"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("BRIG_POLICY_DIR", dir)
			stubEditor(t, `echo should not run >&2; exit 1`)

			if err := createPolicy([]string{name}); err == nil {
				t.Fatal("an ambiguous name was accepted")
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("create wrote something despite the ambiguous name: %v", entries)
			}
		})
	}
}

func TestShowUnknownPolicy(t *testing.T) {
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	err := showPolicy([]string{"ghost"})
	if err == nil || !strings.Contains(err.Error(), "unknown policy") {
		t.Errorf("wrong error for an unknown name: %v", err)
	}
}

// lookupPolicy backs show, edit and rm alike, so an unreadable directory
// must surface as that failure through all three, not as "unknown policy"
// -- which would point the user at `brig policies`, and that would also
// fail to read the same directory.
func TestShowEditRemoveReportAnUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode-000 directory, so this cannot be reproduced")
	}
	for _, run := range []struct {
		name string
		call func() error
	}{
		{"show", func() error { return showPolicy([]string{"no-net"}) }},
		{"edit", func() error { return editPolicy([]string{"no-net"}) }},
		{"rm", func() error { return removePolicy([]string{"no-net"}) }},
	} {
		t.Run(run.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("BRIG_POLICY_DIR", dir)
			if err := os.Chmod(dir, 0o000); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(dir, 0o700)

			err := run.call()
			if err == nil {
				t.Fatal("returned nil for an unreadable directory")
			}
			if strings.Contains(err.Error(), "unknown policy") {
				t.Errorf("an unreadable directory was reported as an unknown policy: %v", err)
			}
		})
	}
}

func TestEditPolicyOpensAndSavesChanges(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	path := writePolicyFile(t, dir, "no-net", noNetBody)
	stubEditor(t, `awk '!/^desc: nothing outbound$/' "$1" > "$1.new" && mv "$1.new" "$1" && `+
		`printf 'desc: edited\n' >> "$1"`)

	if err := editPolicy([]string{"no-net"}); err != nil {
		t.Fatalf("editing failed: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "desc: edited") {
		t.Errorf("the editor's changes are not on disk:\n%s", after)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("the scratch copy was not cleaned up: %v", entries)
	}
}

// The final write goes through a temp file in the same directory, then a
// rename, rather than truncating entry.Path in place -- so a crash between
// those two steps cannot leave it half written. The rename must not change
// the file's mode, and must leave no .tmp file behind either.
func TestEditPolicyWriteBackPreservesModeAndLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	path := writePolicyFile(t, dir, "no-net", noNetBody)
	stubEditor(t, `awk '!/^desc: nothing outbound$/' "$1" > "$1.new" && mv "$1.new" "$1" && `+
		`printf 'desc: edited\n' >> "$1"`)

	if err := editPolicy([]string{"no-net"}); err != nil {
		t.Fatalf("editing failed: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("file mode changed to %v after edit, want 0644", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a temp file was left behind: %v", e.Name())
		}
	}
}

// The rename replaces the file outright, so a mode other than the 0644
// every policy starts with has to be carried over by hand -- the previous
// test alone would not catch a regression here, since 0644 in and 0644 out
// looks identical whether the mode was preserved or just hardcoded again.
func TestEditPolicyPreservesANonDefaultMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	path := writePolicyFile(t, dir, "no-net", noNetBody)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	stubEditor(t, `awk '!/^desc: nothing outbound$/' "$1" > "$1.new" && mv "$1.new" "$1" && `+
		`printf 'desc: edited\n' >> "$1"`)

	if err := editPolicy([]string{"no-net"}); err != nil {
		t.Fatalf("editing failed: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode changed to %v after edit, want 0600 preserved", info.Mode().Perm())
	}
}

// edit validates before saving: a broken save does not reach the real file
// at all, and the edit itself is not lost -- it stays in the scratch copy
// the error names.
func TestEditPolicyLeavesTheRealFileUntouchedOnABrokenSave(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	path := writePolicyFile(t, dir, "no-net", noNetBody)
	stubEditor(t, `printf 'name: [unclosed\n' > "$1"`)

	err := editPolicy([]string{"no-net"})
	if err == nil {
		t.Fatal("a broken save was accepted")
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("the file was removed: %v", readErr)
	}
	if string(after) != noNetBody {
		t.Errorf("the real file was changed by a save that did not validate:\n%s", after)
	}
	scratchPath := strings.TrimSpace(strings.SplitAfter(err.Error(), "your edit is still at ")[1])
	scratch, readErr := os.ReadFile(scratchPath)
	if readErr != nil {
		t.Fatalf("the scratch copy the error named does not exist: %v", readErr)
	}
	if !strings.Contains(string(scratch), "unclosed") {
		t.Errorf("the scratch copy does not hold the edit:\n%s", scratch)
	}
}

// create refuses a reserved name, so edit has to as well: a rename is the
// other way to reach that name, and two entry points with different rules
// is the shape of a bug. Saving this one would destroy nothing -- the file
// is not the reserved one -- but it would leave a policy under a name
// create cannot produce and brig keeps for its own records.
func TestEditPolicyRefusesARenameToAReservedName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	path := writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)
	stubEditor(t, `sed -i.bak 's/^name: no-net$/name: attachments/' "$1"`)

	err := editPolicy([]string{"no-net"})
	if err == nil {
		t.Fatal("a rename to the reserved name was saved")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Errorf("the error does not say the name is reserved: %v", err)
	}
	// The real file is untouched, and the error says where the edit went.
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(after), "name: no-net") {
		t.Errorf("the file was renamed despite the refusal:\n%s", after)
	}
	if !strings.Contains(err.Error(), "your edit is still at") {
		t.Errorf("the error does not say where the edit was left: %v", err)
	}
}

// A save that renames the policy onto a name a different file already
// declares must be refused: otherwise two files end up claiming one name,
// and nothing says so until the next listing.
func TestEditPolicyRefusesARenameIntoAnotherFilesName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	path := writePolicyFile(t, dir, "no-net", noNetBody)
	otherBody := "apiVersion: brig.sh/v1alpha1\nname: staging\negress:\n  default: allow\n"
	other := writePolicyFile(t, dir, "staging", otherBody)
	stubEditor(t, `sed -i.bak 's/^name: no-net$/name: staging/' "$1"`)

	err := editPolicy([]string{"no-net"})
	if err == nil {
		t.Fatal("a rename onto another file's name was accepted")
	}
	if !strings.Contains(err.Error(), "staging") {
		t.Errorf("the error does not name the colliding policy: %v", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != noNetBody {
		t.Errorf("no-net.yaml was changed by a save that was refused:\n%s", after)
	}
	afterOther, readErr := os.ReadFile(other)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(afterOther) != otherBody {
		t.Errorf("staging.yaml was changed by an edit to a different file:\n%s", afterOther)
	}
}

// A rename to a name nothing else uses must still succeed: the check
// above must not mistake this file for a collision with itself.
func TestEditPolicyAllowsARenameToAFreeName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	path := writePolicyFile(t, dir, "no-net", noNetBody)
	stubEditor(t, `sed -i.bak 's/^name: no-net$/name: totally-new/' "$1"`)

	if err := editPolicy([]string{"no-net"}); err != nil {
		t.Fatalf("a rename to an unused name was refused: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "name: totally-new") {
		t.Errorf("the rename did not save:\n%s", after)
	}
}

// A rename leaves whatever attach bound to the old name pointing at a
// name this file no longer declares -- edit refuses it, the same way rm
// refuses to delete a bound policy outright.
func TestEditPolicyRefusesARenameThatWouldOrphanAnAttachment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	path := writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)
	if err := attachPolicy([]string{"no-net", "claude-code"}); err != nil {
		t.Fatal(err)
	}
	stubEditor(t, `sed -i.bak 's/^name: no-net$/name: totally-new/' "$1"`)

	err := editPolicy([]string{"no-net"})
	if err == nil {
		t.Fatal("a rename that would orphan an attachment was accepted")
	}
	if !strings.Contains(err.Error(), "claude-code") {
		t.Errorf("the error does not name what is bound to the old name: %v", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(after), "name: no-net") {
		t.Errorf("the file was renamed despite the refusal:\n%s", after)
	}
}

// --force renames it anyway, leaving the (now dangling) attachment in
// place -- the same trade edit's --force already makes for other checks.
func TestEditPolicyForceRenamesAnAttachedPolicy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	path := writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)
	if err := attachPolicy([]string{"no-net", "claude-code"}); err != nil {
		t.Fatal(err)
	}
	stubEditor(t, `sed -i.bak 's/^name: no-net$/name: totally-new/' "$1"`)

	if err := editPolicy([]string{"no-net", "--force"}); err != nil {
		t.Fatalf("--force still refused the rename: %v", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(after), "name: totally-new") {
		t.Errorf("the rename did not save:\n%s", after)
	}
}

// The same refusal applies to a name declared only inline: renaming it
// away leaves the profile's own policy: entry pointing at nothing.
func TestEditPolicyRefusesARenameThatWouldOrphanAnInlineDeclaration(t *testing.T) {
	policyDir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", policyDir)
	writePolicyFile(t, policyDir, "no-net", noNetBody)
	t.Setenv("BRIG_PROFILE_DIR", writeProfile(t, `
name: mytool
image: ghcr.io/brig-sh/mytool:latest
guestHome: /home/mytool
binary: mytool
mem: 1024
cpus: 1
policy: [no-net]
`))
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	stubEditor(t, `sed -i.bak 's/^name: no-net$/name: totally-new/' "$1"`)

	err := editPolicy([]string{"no-net"})
	if err == nil {
		t.Fatal("a rename that would orphan an inline declaration was accepted")
	}
	if !strings.Contains(err.Error(), "mytool (inline)") {
		t.Errorf("the error does not say it is declared inline: %v", err)
	}
}

// Renaming to a different name is not the only kind of save -- one that
// keeps the same name (a rule change, a desc: edit) must not even ask
// whether anything is bound, since the file the binding points at is
// still right here either way.
func TestEditPolicyDoesNotCheckAttachmentsWhenTheNameIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)
	if err := attachPolicy([]string{"no-net", "claude-code"}); err != nil {
		t.Fatal(err)
	}
	stubEditor(t, `sed -i.bak 's/^desc:.*$/desc: updated/' "$1"`)

	if err := editPolicy([]string{"no-net"}); err != nil {
		t.Fatalf("an edit that keeps the same name was refused: %v", err)
	}
}

func TestEditUnknownPolicy(t *testing.T) {
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	err := editPolicy([]string{"ghost"})
	if err == nil || !strings.Contains(err.Error(), "unknown policy") {
		t.Errorf("wrong error for an unknown name: %v", err)
	}
}

func TestRemovePolicyDeletesTheFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	path := writePolicyFile(t, dir, "no-net", noNetBody)

	if err := removePolicy([]string{"no-net"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the file is still there")
	}
}

// A policy attach has bound to a profile would leave attachments.yaml
// naming a policy that no longer resolves to anything -- rm refuses that,
// unless --force, and removes nothing either way until it decides.
func TestRemovePolicyRefusesOneAttachedToAProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	path := writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)
	if err := attachPolicy([]string{"no-net", "claude-code"}); err != nil {
		t.Fatal(err)
	}

	err := removePolicy([]string{"no-net"})
	if err == nil {
		t.Fatal("rm of an attached policy was accepted")
	}
	if !strings.Contains(err.Error(), "bound to") || !strings.Contains(err.Error(), "claude-code") {
		t.Errorf("the error does not say what it is bound to: %v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("the file was removed despite the refusal: %v", statErr)
	}
}

// The same refusal applies to a session-level attach, described the same
// way attach and brig policies already print it.
func TestRemovePolicyRefusesOneAttachedToASession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)
	if err := attachPolicy([]string{"no-net", "claude-code", "-n", "work"}); err != nil {
		t.Fatal(err)
	}

	err := removePolicy([]string{"no-net"})
	if err == nil || !strings.Contains(err.Error(), "claude-code -n work") {
		t.Errorf("wrong error for a session-attached policy: %v", err)
	}
}

// --force removes it anyway, leaving the (now dangling) attachment in
// place: rm's job is the file, not attachments.yaml.
func TestRemovePolicyForceRemovesAnAttachedPolicy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	path := writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)
	if err := attachPolicy([]string{"no-net", "claude-code"}); err != nil {
		t.Fatal(err)
	}

	if err := removePolicy([]string{"no-net", "--force"}); err != nil {
		t.Fatalf("--force still refused: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("the file is still there")
	}
}

// Deleting the file would leave the profile's own policy: entry pointing
// at nothing, exactly the dangling reference this check exists to
// prevent -- attach never having touched attachments.yaml here changes
// nothing about that.
func TestRemovePolicyRefusesOneDeclaredInline(t *testing.T) {
	policyDir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", policyDir)
	path := writePolicyFile(t, policyDir, "no-net", noNetBody)
	t.Setenv("BRIG_PROFILE_DIR", writeProfile(t, `
name: mytool
image: ghcr.io/brig-sh/mytool:latest
guestHome: /home/mytool
binary: mytool
mem: 1024
cpus: 1
policy: [no-net]
`))
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}

	err := removePolicy([]string{"no-net"})
	if err == nil {
		t.Fatal("rm of an inline-declared policy was accepted")
	}
	if !strings.Contains(err.Error(), "mytool (inline)") {
		t.Errorf("the error does not say it is declared inline: %v", err)
	}
	// detach explicitly refuses to touch an inline entry (see
	// TestDetachRefusesAnInlineDeclaredPolicy), so telling the user to
	// "detach it" here would send them in a circle.
	if strings.Contains(err.Error(), "Detach it") {
		t.Errorf("the error tells the user to detach something detach refuses to touch: %v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("the file was removed despite the refusal: %v", statErr)
	}

	if err := removePolicy([]string{"no-net", "--force"}); err != nil {
		t.Fatalf("--force still refused: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("the file is still there")
	}
}

// A policy that is both declared inline and attached needs both kinds of
// guidance, not just one: "detach it" alone would leave the inline entry
// unmentioned, and vice versa.
func TestRemovePolicyGivesBothKindsOfGuidanceWhenBothApply(t *testing.T) {
	policyDir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", policyDir)
	writePolicyFile(t, policyDir, "no-net", noNetBody)
	t.Setenv("BRIG_PROFILE_DIR", writeProfile(t, `
name: mytool
image: ghcr.io/brig-sh/mytool:latest
guestHome: /home/mytool
binary: mytool
mem: 1024
cpus: 1
policy: [no-net]
`))
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	if err := attachPolicy([]string{"no-net", "claude-code"}); err != nil {
		t.Fatal(err)
	}

	err := removePolicy([]string{"no-net"})
	if err == nil {
		t.Fatal("rm of a policy that is both inline and attached was accepted")
	}
	if !strings.Contains(err.Error(), "Detach it and edit the profile's policy: list") {
		t.Errorf("the error does not give both kinds of guidance: %v", err)
	}
}

func TestRemoveUnknownPolicy(t *testing.T) {
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	err := removePolicy([]string{"ghost"})
	if err == nil || !strings.Contains(err.Error(), "unknown policy") {
		t.Errorf("wrong error for an unknown name: %v", err)
	}
}

// parseNameAndForce takes one name; a second bare word is a mistake worth
// reporting, not a silently ignored extra argument -- the same rule
// create and show already hold rm to now that they share the parser.
func TestRemovePolicyRejectsASecondName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)

	err := removePolicy([]string{"no-net", "staging"})
	if err == nil || !strings.Contains(err.Error(), `takes one name, not "staging"`) {
		t.Errorf("wrong error for a second bare word: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "no-net.yaml")); statErr != nil {
		t.Errorf("the file was removed despite the rejected extra argument: %v", statErr)
	}
}

// rm cannot tell you it is safe to delete something when it cannot read
// the record of what points at it -- unlike listPolicies, which only
// degrades a display, rm without --force refuses outright.
func TestRemovePolicyFailsClosedOnAMalformedAttachmentsFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)
	if err := os.WriteFile(filepath.Join(dir, "attachments.yaml"),
		[]byte("profiles: [this is not valid: yaml structure"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := removePolicy([]string{"no-net"}); err == nil {
		t.Error("rm proceeded despite a broken attachments.yaml it could not check")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "no-net.yaml")); statErr != nil {
		t.Errorf("the file was removed despite the unreadable attachment record: %v", statErr)
	}

	// --force is the documented way past a check rm cannot perform, the
	// same as it is past a check that ran and refused.
	if err := removePolicy([]string{"no-net", "--force"}); err != nil {
		t.Fatalf("--force still refused: %v", err)
	}
}

// loadTestProfiles points the profile registry at a fresh, empty directory
// and loads it, so a test sees only the built-ins (claude-code, ubuntu, ...)
// with nothing left over from another test.
func loadTestProfiles(t *testing.T) {
	t.Helper()
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
}

// "attached" is printed only once the record is on disk. A dir that cannot
// be written would otherwise put success on stdout beside a non-zero exit,
// and a script reading stdout would believe the binding landed.
func TestAttachSaysNothingWhenTheSaveFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a mode-500 directory, so this cannot be reproduced")
	}
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)
	// Readable, so the policy still loads and every check passes; not
	// writable, so only the Save fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)

	out, err := captureStdout(t, func() error {
		return attachPolicy([]string{"no-net", "claude-code"})
	})
	if err == nil {
		t.Fatal("attach reported success with an unwritable policy directory")
	}
	if strings.Contains(out, "attached") {
		t.Errorf("stdout claimed the attach landed while the command failed: %q", out)
	}
}

func TestAttachBindsAPolicyToAProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)

	if err := attachPolicy([]string{"no-net", "claude-code"}); err != nil {
		t.Fatalf("attach failed: %v", err)
	}
	a, err := policy.LoadAttachments(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Profiles["claude-code"]; len(got) != 1 || got[0] != "no-net" {
		t.Errorf("Profiles[claude-code] = %v, want [no-net]", got)
	}
}

func TestAttachWithNameBindsASessionInstead(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)

	if err := attachPolicy([]string{"no-net", "claude-code", "-n", "work"}); err != nil {
		t.Fatalf("attach failed: %v", err)
	}
	a, err := policy.LoadAttachments(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Sessions["claude-code"]["work"]; len(got) != 1 || got[0] != "no-net" {
		t.Errorf("Sessions[claude-code][work] = %v, want [no-net]", got)
	}
	if len(a.Profiles["claude-code"]) != 0 {
		t.Errorf("Profiles[claude-code] = %v, want none: -n binds the session, not the profile",
			a.Profiles["claude-code"])
	}
}

// claude-code -n work and codex -n work are different sandboxes, so the
// confirmation has to name the profile, not just the session: "attached
// to session work" alone cannot tell the two apart.
func TestAttachWithNameNamesTheProfileInItsConfirmation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)

	out, err := captureStdout(t, func() error {
		return attachPolicy([]string{"no-net", "claude-code", "-n", "work"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "claude-code -n work") {
		t.Errorf("the confirmation does not name the profile: %q", out)
	}
}

// A session's sandbox and workspace are named from its slug, so a -n value
// Slug would rewrite addresses a session that will never exist under the
// name given: `-n "My Work"` would record "My Work" while `run -n "My
// Work"` starts my-work. Refused rather than rewritten, the same rule
// ParseRef holds the strict agent@label form to, so an attachment stays an
// address instead of one spelling standing in for another.
func TestAttachRefusesASessionNameThatIsNotSlugClean(t *testing.T) {
	for _, c := range []struct{ name, session, wants string }{
		{"needs rewriting", "My Work", "my-work"},
		{"nothing usable", "!!!", "no usable characters"},
		{"another profile's workspace", "desktop", "claude-desktop"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("BRIG_POLICY_DIR", dir)
			writePolicyFile(t, dir, "no-net", noNetBody)
			loadTestProfiles(t)

			err := attachPolicy([]string{"no-net", "claude-code", "-n", c.session})
			if err == nil {
				t.Fatalf("attach -n %q was accepted", c.session)
			}
			if !strings.Contains(err.Error(), c.wants) {
				t.Errorf("the error does not say what is wrong or what to type: %v", err)
			}
			// Nothing is written: the refusal happens while parsing, before
			// any record is touched.
			if _, statErr := os.Stat(filepath.Join(dir, "attachments.yaml")); !os.IsNotExist(statErr) {
				t.Errorf("a record was written despite the refusal (stat err = %v)", statErr)
			}
		})
	}
}

// A value that is already slug-clean is what run would start, so it binds.
func TestAttachAcceptsASlugCleanSessionName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)

	if err := attachPolicy([]string{"no-net", "claude-code", "-n", "my-work"}); err != nil {
		t.Fatalf("a slug-clean session was refused: %v", err)
	}
	a, err := policy.LoadAttachments(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Sessions["claude-code"]["my-work"]; len(got) != 1 || got[0] != "no-net" {
		t.Errorf("Sessions[claude-code][my-work] = %v, want [no-net]", got)
	}
}

// The strict rule is attach's alone. detach and check read the record, and
// have to be able to name whatever key is in it -- a hand edit, or an
// earlier build, can put a spelling there that attach would now refuse.
// Holding the readers to the same rule would print a binding in ls that
// nothing could then inspect or remove.
func TestDetachAndCheckAcceptASessionNameAttachWouldRefuse(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)
	// Written directly: attach is exactly what will not produce this.
	var a policy.Attachments
	a.AttachToSession("no-net", "claude-code", "My Work")
	if err := a.Save(dir); err != nil {
		t.Fatal(err)
	}

	// The listing names it, so the listing's own spelling has to work.
	listed, err := captureStdout(t, listPolicies)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listed, "claude-code -n My Work") {
		t.Fatalf("the listing does not name the binding: %q", listed)
	}

	seen, err := captureStdout(t, func() error {
		return checkPolicy([]string{"claude-code", "-n", "My Work"})
	})
	if err != nil {
		t.Fatalf("check could not inspect a recorded binding: %v", err)
	}
	if !strings.Contains(seen, "no-net") {
		t.Errorf("check did not report the recorded binding: %q", seen)
	}

	if err := detachPolicy([]string{"no-net", "claude-code", "-n", "My Work"}); err != nil {
		t.Fatalf("detach could not remove a recorded binding: %v", err)
	}
	after, err := policy.LoadAttachments(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Sessions) != 0 {
		t.Errorf("the binding survived detach: %+v", after.Sessions)
	}
}

// -n given but empty -- a literal `-n ""`, or `-n "$SESSION"` with an
// unset $SESSION -- is not the same as -n omitted. Reading it as omitted
// would silently attach to every run of the profile instead of the one
// session a caller who passed -n at all meant.
func TestAttachRejectsAnEmptySessionName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)

	err := attachPolicy([]string{"no-net", "claude-code", "-n", ""})
	if err == nil {
		t.Fatal("attach with an empty -n value was accepted")
	}
	a, loadErr := policy.LoadAttachments(dir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(a.Profiles) != 0 || len(a.Sessions) != 0 {
		t.Errorf("attach wrote something despite the empty -n being refused: %+v", a)
	}
}

// ubuntu is kind: shell in the built-ins: no agent, so no tool-call surface
// an egress rule could hook into. attach has to refuse before it writes
// anything.
func TestAttachRefusesAShellProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)

	err := attachPolicy([]string{"no-net", "ubuntu"})
	if err == nil {
		t.Fatal("attach to a shell profile was accepted")
	}
	if !strings.Contains(err.Error(), "shell") {
		t.Errorf("the error does not say why: %v", err)
	}
	a, loadErr := policy.LoadAttachments(dir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(a.Profiles) != 0 {
		t.Errorf("attach wrote something despite refusing: %v", a.Profiles)
	}
}

func TestAttachUnknownPolicy(t *testing.T) {
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	loadTestProfiles(t)
	err := attachPolicy([]string{"ghost", "claude-code"})
	if err == nil || !strings.Contains(err.Error(), "unknown policy") {
		t.Errorf("wrong error for an unknown policy: %v", err)
	}
}

func TestAttachUnknownProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)
	err := attachPolicy([]string{"no-net", "ghost"})
	if err == nil || !strings.Contains(err.Error(), "unknown profile") {
		t.Errorf("wrong error for an unknown profile: %v", err)
	}
	// The same "unknown profile" message every other command in this
	// codebase reports via notFoundf, which exitCode reads for exit 3 --
	// a plain fmt.Errorf here would look identical on stderr but exit 1.
	if got := exitCode(err); got != exitNotFound {
		t.Errorf("exitCode = %d, want %d (exitNotFound)", got, exitNotFound)
	}
}

func TestDetachUndoesAnAttach(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)

	if err := attachPolicy([]string{"no-net", "claude-code"}); err != nil {
		t.Fatal(err)
	}
	if err := detachPolicy([]string{"no-net", "claude-code"}); err != nil {
		t.Fatalf("detach failed: %v", err)
	}
	a, err := policy.LoadAttachments(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Profiles) != 0 {
		t.Errorf("Profiles = %v, want none after detach", a.Profiles)
	}
}

// attach resolves a profile alias to its canonical name before writing, so
// the binding lands under the one name every other lookup (including
// detach) agrees on -- not under whatever spelling was typed.
func TestAttachResolvesAnAliasToTheCanonicalProfileName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)

	if err := attachPolicy([]string{"no-net", "claude"}); err != nil {
		t.Fatalf("attach failed: %v", err)
	}
	a, err := policy.LoadAttachments(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Profiles["claude-code"]; len(got) != 1 || got[0] != "no-net" {
		t.Errorf("Profiles[claude-code] = %v, want [no-net]: attach must key by the canonical "+
			"name, not the alias %q", got, "claude")
	}
	if len(a.Profiles["claude"]) != 0 {
		t.Errorf("Profiles[claude] = %v, want none: the alias itself must not become a key",
			a.Profiles["claude"])
	}
}

// detach has to resolve the same alias attach did, or `attach ... claude`
// followed by `detach ... claude` would leave the binding in place.
func TestDetachResolvesAnAliasToMatchWhatAttachStored(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)

	if err := attachPolicy([]string{"no-net", "claude"}); err != nil {
		t.Fatal(err)
	}
	if err := detachPolicy([]string{"no-net", "claude"}); err != nil {
		t.Fatalf("detach failed: %v", err)
	}
	a, err := policy.LoadAttachments(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Profiles) != 0 {
		t.Errorf("Profiles = %v, want none after detach by the same alias attach used", a.Profiles)
	}
}

// Detaching something that was never attached is a no-op, not an error --
// there is nothing to validate against, only a binding to remove if it is
// there.
func TestDetachSomethingNeverAttachedIsANoOp(t *testing.T) {
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	if err := detachPolicy([]string{"ghost", "nowhere"}); err != nil {
		t.Errorf("detaching a name that was never attached failed: %v", err)
	}
}

// A no-op detach must say so, not "detached" -- and must not write
// attachments.yaml, or create the policy directory, over a run that
// changed nothing.
func TestDetachSomethingNeverAttachedSaysSoAndWritesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "policies")
	t.Setenv("BRIG_POLICY_DIR", dir)

	out, err := captureStdout(t, func() error {
		return detachPolicy([]string{"ghost", "nowhere"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "was not attached") {
		t.Errorf("a no-op detach did not say so: %q", out)
	}
	if strings.Contains(out, "detached ") {
		t.Errorf("a no-op detach claimed a removal that did not happen: %q", out)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Errorf("a no-op detach created the policy directory: %v", statErr)
	}
}

// A policy the profile already declares inline binds every run already --
// attach would only write a redundant entry, one detach could never remove
// (detach refuses to touch a name the inline list declares), so attach has
// to refuse it too, before that entry exists.
func TestAttachRefusesAPolicyAlreadyDeclaredInline(t *testing.T) {
	policyDir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", policyDir)
	writePolicyFile(t, policyDir, "no-net", noNetBody)
	t.Setenv("BRIG_PROFILE_DIR", writeProfile(t, `
name: mytool
image: ghcr.io/brig-sh/mytool:latest
guestHome: /home/mytool
binary: mytool
mem: 1024
cpus: 1
policy: [no-net]
`))
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}

	err := attachPolicy([]string{"no-net", "mytool"})
	if err == nil {
		t.Fatal("attach of an already-inline-declared policy was accepted")
	}
	if !strings.Contains(err.Error(), "inline") {
		t.Errorf("the error does not say the policy is already inline: %v", err)
	}
	a, loadErr := policy.LoadAttachments(policyDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(a.Profiles) != 0 {
		t.Errorf("attach wrote something despite refusing: %v", a.Profiles)
	}
}

// A policy a profile declares inline was never attach's to add, so detach
// has to refuse to remove it rather than reporting success over a no-op.
func TestDetachRefusesAnInlineDeclaredPolicy(t *testing.T) {
	policyDir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", policyDir)
	writePolicyFile(t, policyDir, "no-net", noNetBody)
	t.Setenv("BRIG_PROFILE_DIR", writeProfile(t, `
name: mytool
image: ghcr.io/brig-sh/mytool:latest
guestHome: /home/mytool
binary: mytool
mem: 1024
cpus: 1
policy: [no-net]
`))
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}

	err := detachPolicy([]string{"no-net", "mytool"})
	if err == nil {
		t.Fatal("detach of an inline-declared policy was accepted")
	}
	if !strings.Contains(err.Error(), "inline") {
		t.Errorf("the error does not say the policy is inline: %v", err)
	}
}

// -n scopes a detach to one session, a narrower thing than the inline list
// that binds every run -- the inline refusal above must not reach here.
func TestDetachWithNameIgnoresTheInlineRefusal(t *testing.T) {
	policyDir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", policyDir)
	writePolicyFile(t, policyDir, "no-net", noNetBody)
	t.Setenv("BRIG_PROFILE_DIR", writeProfile(t, `
name: mytool
image: ghcr.io/brig-sh/mytool:latest
guestHome: /home/mytool
binary: mytool
mem: 1024
cpus: 1
policy: [no-net]
`))
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}

	if err := detachPolicy([]string{"no-net", "mytool", "-n", "work"}); err != nil {
		t.Errorf("session-scoped detach was refused over an unrelated inline policy: %v", err)
	}
}

// claude-code -n work and codex -n work are different sandboxes
// (brig-claude-code-work vs brig-codex-work), so a session name is only
// unique within its profile. attach/detach must not let one profile's
// "work" session see or remove what is bound to another profile's session
// of the same name.
func TestAttachSessionDoesNotCrossProfiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	writePolicyFile(t, dir, "staging", "apiVersion: brig.sh/v1alpha1\nname: staging\negress:\n  default: allow\n")
	loadTestProfiles(t)

	if err := attachPolicy([]string{"no-net", "claude-code", "-n", "work"}); err != nil {
		t.Fatal(err)
	}
	if err := attachPolicy([]string{"staging", "codex", "-n", "work"}); err != nil {
		t.Fatal(err)
	}
	if err := detachPolicy([]string{"staging", "codex", "-n", "work"}); err != nil {
		t.Fatal(err)
	}

	a, err := policy.LoadAttachments(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Sessions["claude-code"]["work"]; len(got) != 1 || got[0] != "no-net" {
		t.Errorf("Sessions[claude-code][work] = %v, want [no-net]: codex's session "+
			"was attached to and detached from, claude-code's must be untouched", got)
	}
	if len(a.Sessions["codex"]["work"]) != 0 {
		t.Errorf("Sessions[codex][work] = %v, want none after detach", a.Sessions["codex"]["work"])
	}
}

// parseWords falls back to rewriteFlagError for anything other than an
// unknown flag, the same as nameAndYes and nameAndFile already do, so a
// malformed flag value reads the same way across every brig verb.
func TestParseWordsPolishesAMalformedBoolFlag(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	stubEditor(t, `true`)

	err := editPolicy([]string{"no-net", "--force=oops"})
	if err == nil {
		t.Fatal("a malformed --force value was accepted")
	}
	if !strings.Contains(err.Error(), "takes true or false") {
		t.Errorf("the raw flag-package error leaked through unpolished: %v", err)
	}
}

func TestPolicyCmdDispatch(t *testing.T) {
	if err := policyCmd(nil); err == nil {
		t.Error("no subcommand was accepted")
	}
	if err := policyCmd([]string{"bogus"}); err == nil {
		t.Error("an unknown subcommand was accepted")
	}
}

// Asking for help is not a mistake: `brig policy -h` and a verb's own -h
// both print usage and exit clean, the same translation profileCmd and
// secretCmd already make for flag.ErrHelp.
func TestPolicyCmdPrintsUsageForHelp(t *testing.T) {
	out, err := captureStdout(t, func() error { return policyCmd([]string{"-h"}) })
	if err != nil {
		t.Fatalf("bare -h returned an error: %v", err)
	}
	if !strings.Contains(out, "brig policy --") {
		t.Errorf("bare -h did not print usage: %q", out)
	}

	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	out, err = captureStdout(t, func() error { return policyCmd([]string{"edit", "-h"}) })
	if err != nil {
		t.Fatalf("a verb's own -h returned an error: %v", err)
	}
	if !strings.Contains(out, "brig policy --") {
		t.Errorf("a verb's own -h did not print usage: %q", out)
	}
}

// --force on rm (or on edit's rename check) can leave a name bound with no
// policy behind it -- nothing can enforce what is not there, so check must
// not report it as something that applies and exit 0.
func TestCheckFailsWhenABoundPolicyDoesNotExist(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)
	if err := attachPolicy([]string{"no-net", "claude-code"}); err != nil {
		t.Fatal(err)
	}
	if err := removePolicy([]string{"no-net", "--force"}); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error { return checkPolicy([]string{"claude-code"}) })
	if err == nil {
		t.Fatal("check accepted a binding to a policy that no longer exists")
	}
	if !strings.Contains(err.Error(), "no-net") || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
	if !strings.Contains(out, "no such policy") {
		t.Errorf("check did not mark the missing name in its listing: %q", out)
	}
}

// "attached" on its own reads as a rule that is now in force, and nothing
// in the guest reads these bindings yet. The docs say so; so does attach,
// because the terminal is where someone acts on it.
func TestAttachSaysThePolicyIsNotEnforcedYet(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)

	// On stderr, where the CLI puts every advisory, so stdout stays the
	// command's answer alone.
	var out string
	note := captureStderr(t, func() {
		var err error
		out, err = captureStdout(t, func() error {
			return attachPolicy([]string{"no-net", "claude-code"})
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(note, policy.NotEnforcedNote) {
		t.Errorf("attach did not say the binding is not enforced: %q", note)
	}
	if strings.Contains(out, policy.NotEnforcedNote) {
		t.Errorf("the note went to stdout, where it is not the answer: %q", out)
	}
}

// check is the verb that means "confirm this is in force", and it prints
// the names and exits zero -- the one answer most likely to be read as a
// verdict, so it carries the note too.
func TestCheckSaysThePolicyIsNotEnforcedYet(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)
	if err := attachPolicy([]string{"no-net", "claude-code"}); err != nil {
		t.Fatal(err)
	}

	// check prints one policy name per line, so the note has to stay off
	// stdout: anything looping over it would read the note as a name.
	var out string
	note := captureStderr(t, func() {
		var err error
		out, err = captureStdout(t, func() error { return checkPolicy([]string{"claude-code"}) })
		if err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(note, policy.NotEnforcedNote) {
		t.Errorf("check did not say the binding is not enforced: %q", note)
	}
	if strings.TrimSpace(out) != "no-net" {
		t.Errorf("stdout is not just the policy names: %q", out)
	}
}

// Where nothing is bound, or where the answer is already a refusal naming
// what cannot be enforced, the note is noise -- there is nothing to read as
// a verdict in the first place.
func TestCheckOmitsTheNoteWhenThereIsNothingToMisread(t *testing.T) {
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	loadTestProfiles(t)

	nothingBound, err := captureStdout(t, func() error { return checkPolicy([]string{"claude-code"}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(nothingBound, policy.NotEnforcedNote) {
		t.Errorf("the note was printed with nothing bound: %q", nothingBound)
	}

	refused, _ := captureStdout(t, func() error { return checkPolicy([]string{"ubuntu"}) })
	if strings.Contains(refused, policy.NotEnforcedNote) {
		t.Errorf("the note was printed beside a refusal: %q", refused)
	}
}

func TestCheckListsTheEffectivePolicies(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)
	if err := attachPolicy([]string{"no-net", "claude-code"}); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error { return checkPolicy([]string{"claude-code"}) })
	if err != nil {
		t.Fatalf("check failed: %v", err)
	}
	if !strings.Contains(out, "no-net") {
		t.Errorf("check did not list the attached policy: %q", out)
	}
}

// -n narrows to one session's effective set -- the profile-level policy
// above must not leak into a check for a profile that never got it, and a
// session-only attach must show up only with -n.
func TestCheckWithNameChecksOnlyThatSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)
	if err := attachPolicy([]string{"no-net", "claude-code", "-n", "work"}); err != nil {
		t.Fatal(err)
	}

	without, err := captureStdout(t, func() error { return checkPolicy([]string{"claude-code"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(without, "no policy applies") {
		t.Errorf("check without -n saw the session-only attach: %q", without)
	}

	with, err := captureStdout(t, func() error { return checkPolicy([]string{"claude-code", "-n", "work"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(with, "no-net") {
		t.Errorf("check -n work did not see the session attach: %q", with)
	}
}

func TestCheckFailsOnAShellProfile(t *testing.T) {
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	loadTestProfiles(t)

	err := checkPolicy([]string{"ubuntu"})
	if err == nil {
		t.Fatal("check on a shell profile was accepted")
	}
	if !strings.Contains(err.Error(), "shell") {
		t.Errorf("the error does not say why: %v", err)
	}
}

func TestCheckUnknownProfile(t *testing.T) {
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	loadTestProfiles(t)

	err := checkPolicy([]string{"ghost"})
	if err == nil || !strings.Contains(err.Error(), "unknown profile") {
		t.Errorf("wrong error for an unknown profile: %v", err)
	}
	if got := exitCode(err); got != exitNotFound {
		t.Errorf("exitCode = %d, want %d (exitNotFound)", got, exitNotFound)
	}
}

func TestCheckRejectsAnEmptySessionName(t *testing.T) {
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	loadTestProfiles(t)

	err := checkPolicy([]string{"claude-code", "-n", ""})
	if err == nil {
		t.Error("check with an empty -n value was accepted")
	}
}

func TestCheckCmdDispatch(t *testing.T) {
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	loadTestProfiles(t)
	if err := policyCmd([]string{"check", "claude-code"}); err != nil {
		t.Errorf("policy check dispatch failed: %v", err)
	}
}
