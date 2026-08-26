package profile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// Custom profiles.
//
// A profile is data, so there is no reason the set of them has to be the one
// compiled in. Any Linux CLI in an OCI image already runs under brig; a
// profile just saves you spelling out the image, the home directory and the
// credential variables every time.
//
// They live as one file per profile in a directory, which makes them
// diffable, reviewable and easy to share -- and means `brig export` followed
// by an edit is the way to write one, rather than starting from a blank file
// and a guess about the field names.
//
// YAML or JSON, read the same way: JSON is a subset of YAML, so one parser
// handles both and nothing has to guess at a format. Export writes YAML,
// because a profile is a file a person edits and YAML has comments; JSON
// stays available for anything generating profiles programmatically. An
// imported file is stored byte for byte as you wrote it, so comments and
// ordering survive the round trip.
//
// See https://github.com/brig-sh/community-images/blob/main/docs/bring-your-own-image.md
// for how to build an image for one.

// BringYourOwnImageDoc is where to send someone who wants their own image.
const BringYourOwnImageDoc = "https://github.com/brig-sh/community-images/blob/main/docs/bring-your-own-image.md"

// Dir is where a user's own profiles live.
//
// This follows the XDG Base Directory Specification, version 0.8:
// https://specifications.freedesktop.org/basedir/latest/
//
// Three of its clauses matter here, and none is obvious from the code:
//
//   - "If $XDG_CONFIG_HOME is either not set or empty, a default equal to
//     $HOME/.config should be used." Empty counts as unset, which is why the
//     check in configHome is `!= ""` rather than a lookup for presence. That
//     reads like an oversight and is in fact the rule.
//   - "All paths set in these environment variables must be absolute. If an
//     implementation encounters a relative path in any of these variables it
//     should consider the path invalid and ignore it." This one brig would
//     otherwise get wrong, and it matters more here than in most tools: brig
//     runs from whatever project directory you happen to be in, so honouring
//     a relative value would resolve profiles against the current directory
//     and quietly find a different set per project.
//   - "If, when attempting to write a file, the destination directory is
//     non-existent an attempt should be made to create it with permission
//     0700." See Import.
//
// BRIG_PROFILE_DIR is brig's own variable rather than an XDG one, so it is
// taken as given: an explicit override is a deliberate act and does not get
// second-guessed for absoluteness. BRIG_TEMPLATE_DIR is the older spelling,
// honoured for one release.
func Dir() string {
	if dir := os.Getenv("BRIG_PROFILE_DIR"); dir != "" {
		return dir
	}
	if dir := os.Getenv("BRIG_TEMPLATE_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(configHome(), "brig")
}

// configHome resolves $XDG_CONFIG_HOME per the spec cited on Dir.
func configHome() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" && filepath.IsAbs(dir) {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// This is the one case where the relative path Dir warns about above
		// is returned deliberately: there is no home directory to anchor to,
		// and failing outright would make brig unusable rather than merely
		// misdirected. os.UserHomeDir failing means $HOME is unset, which is
		// close to impossible on the platforms brig supports.
		return ".config"
	}
	return filepath.Join(home, ".config")
}

// isProfileFile reports whether a directory entry is a profile. Anything
// else in the directory -- a README, an editor's backup -- is ignored rather
// than reported as broken.
func isProfileFile(name string) bool {
	switch filepath.Ext(name) {
	case ".yaml", ".yml", ".json":
		return true
	}
	return false
}

// Read reads and validates one profile file.
func Read(path string) (Profile, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, err
	}
	return Parse(blob)
}

// Parse decodes and validates a profile, in YAML or JSON.
//
// One path serves both because JSON is a subset of YAML. The strict decoder
// refuses unknown fields rather than ignoring them: a misspelled "forwards"
// would otherwise decode into nothing and silently forward no credentials,
// which looks exactly like a broken sandbox.
func Parse(blob []byte) (Profile, error) {
	var p Profile
	if err := yaml.UnmarshalStrict(blob, &p); err != nil {
		return Profile{}, err
	}
	if err := p.normaliseKind(); err != nil {
		return Profile{}, err
	}
	if err := p.normaliseForward(); err != nil {
		return Profile{}, err
	}
	return p, p.Validate()
}

// Validate checks a profile is usable and safe.
func (p Profile) Validate() error {
	if p.Name == "" {
		return fmt.Errorf("name is required")
	}
	// The name reaches a workspace path and a sandbox name, so it has to be
	// safe in both. Same rule as a session name, for the same reason.
	if !safeName(p.Name) {
		return fmt.Errorf("name %q may use only lowercase letters, digits, dot, "+
			"dash and underscore, and must start with a letter or digit", p.Name)
	}
	if p.Image == "" {
		return fmt.Errorf("image is required (see %s)", BringYourOwnImageDoc)
	}
	if p.GuestHome == "" || !strings.HasPrefix(p.GuestHome, "/") {
		return fmt.Errorf("guestHome is required and must be absolute, e.g. /home/%s", p.Name)
	}
	if p.Binary == "" && p.kind() == KindAgent {
		return fmt.Errorf("binary is required for kind: agent " +
			"(a shell or gui profile has nothing to pass arguments to)")
	}
	// validateBindings checks the new keys (env, secrets); the deprecated
	// forward: has already been folded into Env by Parse before Validate runs.
	if err := p.validateBindings(); err != nil {
		return err
	}
	if p.Mem <= 0 || p.CPUs <= 0 {
		return fmt.Errorf("mem and cpus must be greater than zero")
	}
	// Checked here rather than left to the runtime. A typo would otherwise
	// travel all the way to a boot that fails naming a backend nobody meant,
	// which is a long way from the line that caused it.
	switch p.Hypervisor {
	case "", "vz", "hvi", "qemu":
	default:
		return fmt.Errorf("hypervisor %q is not one of vz, hvi or qemu", p.Hypervisor)
	}
	switch p.RootfsType {
	case "", "block", "virtiofs", "9pfs":
	default:
		return fmt.Errorf("rootfsType %q is not one of block, virtiofs or 9pfs", p.RootfsType)
	}
	return nil
}

func safeName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '.' || r == '-' || r == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}

// Import validates a profile and writes it into the profile directory,
// where the next brig invocation will find it. It returns the path written.
//
// The bytes are stored exactly as they came in, rather than re-serialised
// from the parsed struct. A profile is a file someone wrote, and
// round-tripping it through a marshaller would drop their comments and
// reorder their fields for no gain -- brig has already checked it parses.
func Import(blob []byte, dir string) (Profile, string, error) {
	t, err := Parse(blob)
	if err != nil {
		return Profile{}, "", err
	}
	if owner, reserved := Reserved(t.Name); reserved && t.Name != owner {
		return Profile{}, "", fmt.Errorf("name %q collides with the %s profile", t.Name, owner)
	}
	if reservedName(t.Name) {
		return Profile{}, "", fmt.Errorf("name %q is reserved: a file of that name in the "+
			"profile directory is brig's own, so the profile would never load", t.Name)
	}
	// 0700 per the XDG spec cited on Dir, and the better default anyway for a
	// directory of files that name credential variables.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Profile{}, "", err
	}
	path := filepath.Join(dir, t.Name+extensionFor(blob))
	// A profile of this name may already be here in the other format, and
	// leaving both would make which one wins a matter of directory order.
	for _, ext := range []string{".yaml", ".yml", ".json"} {
		other := filepath.Join(dir, t.Name+ext)
		if other != path {
			_ = os.Remove(other)
		}
	}
	if len(blob) > 0 && blob[len(blob)-1] != '\n' {
		blob = append(blob, '\n')
	}
	return t, path, os.WriteFile(path, blob, 0o644)
}

// extensionFor names the file after the format it is actually in, so what is
// on disk reads the way it was written.
func extensionFor(blob []byte) string {
	if len(strings.TrimSpace(string(blob))) > 0 && strings.TrimSpace(string(blob))[0] == '{' {
		return ".json"
	}
	return ".yaml"
}

// firstHeaderLine is how Export recognises its own header in bytes that have
// already been through it, so a file that goes round more than once does not
// accumulate one header per trip. It is a prefix of exportHeader's actual
// first line rather than a copy of it -- the line itself also carries the
// import hint -- so the invariant that matters is that it is a prefix, and
// that it appears exactly once; TestFirstHeaderLineIsAPrefixOfExportHeader
// pins both rather than leaving them to be eyeballed after the next edit.
const firstHeaderLine = "# A brig profile."

// exportHeader is what makes an exported profile a starting point rather
// than a puzzle. It is the one thing JSON could not carry, and the reason
// export writes YAML: the fields are explained where you are editing them.
const exportHeader = firstHeaderLine + ` Edit it, then: brig profile import <this file>
#
#   name       the profile name. Also the workspace directory and the sandbox
#              name, so: lowercase letters, digits, dot, dash, underscore
#   kind       agent (the default), shell or gui. An agent needs a binary; the
#              other two have nothing to pass arguments to
#   image      the guest image to boot. brig can only verify the signature of
#              an image published under ghcr.io/brig-sh; anything else boots
#              with a warning
#   guestHome  where the workspace is mounted. The agent's state lands here,
#              which is what makes the workspace the unit of persistence
#   binary     the CLI inside the guest, for kind: agent
#   secrets    names this profile needs from brig's own store. A bare name is
#              required: a missing one fails the run. {name: x, required: false}
#              warns instead and boots. sources: says where brig can find it on
#              your host -- from: keychain|file|env, first hit wins
#   env        what the guest sees. Each entry has a name and exactly one of
#              value: (a literal), ref: or refs: -- secrets.<name> reads brig's
#              store, env.<name> reads brig's own environment, and a refs:
#              chain takes the first that resolves
#   files      credential files the guest sees. Each entry has a ref:, a path:
#              under guestHome and an optional mode: (default "0600"). The
#              target must sit inside a tmpfs volume below, so the bytes stay
#              in memory and never reach the workspace
#   volumes    what is mounted inside guestHome, one primitive per entry:
#              {kind: tmpfs, path: x} makes x memory-only, so nothing written
#              there reaches host disk, and {kind: hostmount, path: x/y} names
#              an exception kept across boots. A tmpfs takes an optional
#              size: ("256m", default "64m") -- it is a ceiling the guest
#              hits as ENOSPC, so size it for what the mount holds. Parents
#              mount before children by path depth, so the order you write
#              them in is taste
#   forward    deprecated: forward: [X] now means env: [{name: X, ref: env.X}],
#              which says the same thing. Still read, still works
#   deny       variables never bound, whatever env or forward says. This is the
#              billing guard: a metered API key that outranks a subscription
#              token belongs here
#   statePaths deprecated: volumes: above says the same thing and is acted on.
#              A profile may declare one or the other, never both
#   hypervisor macOS backend to boot on: vz (the default, the one with a
#              graphical console), hvi or qemu. BRIG_HYPERVISOR wins over it
#   runtimeBin the runtime binary to drive instead of the one on PATH. About
#              your machine rather than the workload, so it does not travel
#              usefully to anyone else
#   rootfsType how the guest root reaches the VM: block, virtiofs or 9pfs.
#              Unset lets the runtime choose; block gives a writable disk with
#              room to install packages
#   genericBoot
#              the image was never built to be a guest -- a plain OCI image
#              with no kernel in it. The runtime supplies the kernel and
#              initrd and boots it unmodified
#   reserved   marks a profile that owns the workspace a session name could
#              otherwise slug onto. The trailing word is reserved too, so
#              claude-desktop reserves "desktop" as well as its own name
#
# Building an image for a profile:
# ` + BringYourOwnImageDoc + `
`

// SourceBytes returns the bytes a profile was read from, which is what export
// hands back: a profile is a file someone wrote, and re-serialising it from
// the parsed struct would drop their comments and reorder their fields for
// no gain.
//
// Named SourceBytes rather than Source because Source is now the type naming
// one place `brig secret import` may read a value from, and a package cannot
// have both. The type wins the short name: it is written by hand in every
// profile that declares a source, while this is called once, here.
func SourceBytes(name string) ([]byte, bool) {
	e, ok := registry[name]
	if !ok || len(e.source) == 0 {
		return nil, false
	}
	// Cloned rather than handed back the registry's own slice: a caller that
	// appended to or wrote into it (as Import does to a blob it owns) would
	// otherwise corrupt the entry that every later Export reads from.
	return bytes.Clone(e.source), true
}

// Path returns the file a profile was read from, empty for an embedded one.
func Path(name string) (string, bool) {
	e, ok := registry[name]
	if !ok || e.path == "" {
		return "", false
	}
	return e.path, true
}

// Files returns every file backing a profile of your own, the one that loaded
// last. Empty for a built-in: it is compiled in, so there is no file, and the
// caller has a better sentence for that than "no such file".
//
// More than one file is possible because a file need not be named after the
// profile it declares, so two of them in one directory can claim the same
// name. Load reports that; it does not prevent it.
func Files(name string) []string {
	e, ok := registry[name]
	if !ok || e.path == "" {
		return nil
	}
	return append(append([]string{}, e.shadowed...), e.path)
}

// Remove deletes every file backing a profile of your own, and returns what it
// deleted.
//
// Every file, not only the one that loaded. Deleting the winner alone promotes
// whichever file was shadowed by it, so `brig profile rm codex` would report
// success and leave codex listed exactly as before -- the one outcome an rm
// must not have.
func Remove(name string) ([]string, error) {
	files := Files(name)
	removed := make([]string, 0, len(files))
	for _, f := range files {
		if err := os.Remove(f); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, err
		}
		removed = append(removed, f)
	}
	return removed, nil
}

// LegacyDir is where brig kept custom templates before profiles.
//
// Built from the home directory rather than through configHome, and that is
// deliberate: the directory this names is the one the pre-profile code
// created, and that code joined os.UserHomeDir with .config unconditionally,
// never reading $XDG_CONFIG_HOME. Resolving it the modern way would name a
// directory that never existed, for exactly the users who set that variable --
// the ones a migration hint is least able to afford being wrong for.
func LegacyDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "brig", "templates")
}

// LegacyHint is the line brig prints when profiles are sitting in the
// pre-profile directory. Nothing is moved: a migration that copies files
// behind your back is worse than one that tells you where they are, because
// the files name credential variables and there is no safe guess about which
// of them you still want.
//
// Empty when there is nothing to say, so the caller can print it
// unconditionally.
func LegacyHint() string {
	old := LegacyDir()
	if old == "" || old == Dir() {
		return ""
	}
	entries, err := os.ReadDir(old)
	if err != nil {
		return ""
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && isProfileFile(e.Name()) {
			n++
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d profile(s) in %s are not read any more; they live in %s now. "+
		"`brig profile import %s` moves one across.",
		n, old, Dir(), filepath.Join(old, "<file>"))
}

// ExportPath resolves an export destination into a file inside the profile
// directory.
//
// A destination is a name, never a path. The profile directory is the only
// place a profile file does anything, so a path was busywork with a wrong
// answer available: export anywhere else and the file is inert. Export writes
// exactly one directory, which is also what makes it a command that cannot
// scatter files across the host on a typo.
//
// Wanting a copy somewhere else is a real thing to want, and it already has a
// spelling: with no destination export writes stdout, so `brig profile export
// codex > mine.yaml` puts it wherever you like, under the shell's rules rather
// than brig's.
//
// An empty destination stays empty: that is the stdout case, and the caller
// decides what to do with it.
func ExportPath(dest string, asJSON bool) (string, error) {
	if dest == "" {
		return "", nil
	}
	if looksLikePath(dest) {
		return "", fmt.Errorf("%q is a path, and export takes a name: it writes into %s "+
			"and nowhere else. Use `brig profile export <profile> > %s` to put a copy "+
			"somewhere of your own", dest, Dir(), dest)
	}
	base := dest
	if !isProfileFile(base) {
		if asJSON {
			base += ".json"
		} else {
			base += ".yaml"
		}
	}
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if !safeName(stem) {
		return "", fmt.Errorf("%q is not a usable profile file name: lowercase letters, "+
			"digits, dot, dash and underscore, starting with a letter or digit", dest)
	}
	if reservedBasenames[base] {
		return "", fmt.Errorf("%s in the profile directory is brig's own, so a profile "+
			"written there would never load", base)
	}
	return filepath.Join(Dir(), base), nil
}

// looksLikePath is how ExportPath tells a destination someone typed as a path
// from one they typed as a name, so it can refuse the first rather than turn
// it into a file name with a separator in it. A separator settles it; so does
// a leading dot or tilde, which is how the shell leaves a path it could not
// expand.
func looksLikePath(s string) bool {
	return strings.ContainsRune(s, '/') || strings.ContainsRune(s, filepath.Separator) ||
		strings.HasPrefix(s, ".") || strings.HasPrefix(s, "~")
}

// Export renders a profile as the file it came from, with the field
// reference prepended.
//
// Verbatim, because starting from the closest existing profile is the
// documented way to write one: `brig profile export claude-code` should hand
// you the annotated file brig ships, not an alphabetised struct dump with
// every comment stripped. A profile built in Go rather than read from a file
// has no bytes to return, so that case still marshals.
//
// Only p.Name is read: a modified copy exports as the file on record, not as
// the copy. ExportJSON marshals the value it is given.
func Export(p Profile) ([]byte, error) {
	body, ok := SourceBytes(p.Name)
	if !ok {
		var err error
		if body, err = yaml.Marshal(p); err != nil {
			return nil, err
		}
	}
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte(firstHeaderLine)) {
		return body, nil
	}
	return append([]byte(exportHeader), body...), nil
}

// ExportAs renders a profile under a name of the caller's choosing, so a file
// written as mytool.yaml declares the mytool profile rather than the one it
// was copied from.
//
// Starting from the closest existing profile is the documented way to write
// one, and the recipe brig prints for it stopped at its second step until this
// existed: export wrote the file under the name you gave it while the profile
// inside kept the name it came from, so `brig profile edit mytool` had no such
// profile to open and the only command that could reach the file named a
// different profile and deleted it without asking.
//
// The rename is a rewrite of one line rather than a re-serialisation of the
// parsed struct, because the comments are the reason export writes YAML at
// all: why a deny list is what it is, which credential outranks which. Setting
// Name and marshalling would produce a correct profile with every one of those
// sentences gone.
//
// An empty name, or the profile's own, renders unchanged -- that is the plain
// `brig profile export claude-code` case, where nothing was asked to be
// renamed.
func ExportAs(p Profile, name string) ([]byte, error) {
	blob, err := Export(p)
	if err != nil {
		return nil, err
	}
	if name == "" || name == p.Name {
		return blob, nil
	}
	// Parsed again rather than trusted: the line-based rewrite below is a
	// text edit on a format that has more than one way to write the same
	// mapping, so the returned bytes have to be checked to actually declare
	// the name they were asked for. A body it cannot find a top-level name in
	// -- a profile of your own that was written as JSON, a flow mapping on one
	// line -- falls through to marshalling, which loses the comments and is
	// still better than a file that lies about its own name.
	if renamed, ok := renameField(blob, name); ok {
		if got, err := Parse(renamed); err == nil && got.Name == name {
			return renamed, nil
		}
	}
	p.Name = name
	body, err := yaml.Marshal(p)
	if err != nil {
		return nil, err
	}
	return append([]byte(exportHeader), body...), nil
}

// renameField rewrites the value of the top-level name: line and leaves every
// other byte of the file alone.
//
// Column 0 is the whole test, and it is what keeps the rewrite off the two
// other places the word appears in an export: the `- name:` entries under
// secrets:, env: and files:, which are indented, and the field reference in
// the header, where every line starts with a #. Only the first match is
// rewritten -- a second top-level name: is a duplicate key, which the parser
// rejects on the way back out.
//
// Anything written after the value is kept, comment included: `name: mine
// # keep in sync with the workspace` is a note someone wrote, and a rename is
// not a reason to drop it. It may of course now be wrong, but so is deleting
// it, and only one of those is visible to the person editing the file.
func renameField(blob []byte, name string) ([]byte, bool) {
	lines := bytes.Split(blob, []byte("\n"))
	for i, line := range lines {
		key, rest, ok := bytes.Cut(line, []byte(":"))
		if !ok || string(key) != "name" {
			continue
		}
		lines[i] = append([]byte("name: "+name), trailingComment(rest)...)
		return bytes.Join(lines, []byte("\n")), true
	}
	return nil, false
}

// trailingComment is what a value line carries after its value, from the
// whitespace that separates the two so the alignment someone chose survives
// the rename.
func trailingComment(rest []byte) []byte {
	i := bytes.IndexByte(rest, '#')
	if i < 0 {
		return nil
	}
	for i > 0 && (rest[i-1] == ' ' || rest[i-1] == '\t') {
		i--
	}
	return rest[i:]
}

// ExportJSON renders a profile as JSON, for anything generating or consuming
// profiles programmatically.
func ExportJSON(p Profile) ([]byte, error) {
	blob, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(blob, '\n'), nil
}

// ExportJSONAs is ExportAs for the JSON spelling. JSON carries no comments, so
// there is nothing to preserve and the name is simply set before marshalling.
func ExportJSONAs(p Profile, name string) ([]byte, error) {
	if name != "" {
		p.Name = name
	}
	return ExportJSON(p)
}

// IsCustom reports whether a name is served by a file rather than by an
// embedded spec, so listings can say which profiles are not brig's own.
func IsCustom(name string) bool {
	e, ok := registry[name]
	return ok && e.origin == OriginFile
}
