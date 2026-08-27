package profile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Origin is where a profile came from, which is the question someone debugging
// "why is brig booting that image" actually has.
type Origin int

const (
	// OriginBuiltIn is a profile compiled into brig.
	OriginBuiltIn Origin = iota
	// OriginFile is a profile read from a directory on disk.
	OriginFile
)

// entry is one profile plus its provenance. The Profile struct is the schema;
// the bytes, the origin and the paths are facts about where it came from.
//
// The bytes are kept so that export can hand back the file as written rather
// than a re-marshalled struct: a profile carries comments explaining why its
// deny list is what it is, and marshalling drops every one of them.
type entry struct {
	p      Profile
	source []byte
	origin Origin
	path   string // the file this entry was read from; empty for a built-in

	// shadowed are the files that also claim this name and lost. Kept
	// because they are still on disk and still claim it: `brig profile rm`
	// has to take them too, or removing the winner just promotes the next
	// one and the profile appears not to have been removed at all.
	shadowed []string
}

// registry is the merged set, by name. Rebuilt by Load.
//
// Written with no synchronisation, which is safe only under Load's
// single-call-at-startup contract -- see the concurrency note there.
var registry = map[string]entry{}

// aliases are the short spellings people actually type.
var aliases = map[string]string{
	"claude":  "claude-code",
	"desktop": "claude-desktop",
}

// reservedBasenames are the files in the profile directory that are not
// profiles.
//
// The directory is flat -- $XDG_CONFIG_HOME/brig/<name>.yaml -- so every yaml
// in it would otherwise be read as a profile, and a settings file would be
// reported as a broken one. This list is what keeps that door open, and it has
// to grow if brig ever wants a settings file under another name.
var reservedBasenames = map[string]bool{
	"config.yaml": true, "config.yml": true, "config.json": true,
}

// reservedName reports whether a profile of this name could not be read back
// from the profile directory, because a file it would be written to is one the
// loader skips. Import refuses those rather than writing a file that nothing
// will ever load and no command can remove.
//
// Any extension, not every extension. Import names the file after the format
// the bytes are in, so a name reserved in only one spelling still means the
// import that picks that spelling writes a file nothing loads. Requiring all
// three to be reserved before refusing would let exactly that case through,
// and the only reason it does not today is that reservedBasenames happens to
// list all three spellings of the one name it reserves.
func reservedName(name string) bool {
	for _, ext := range []string{".yaml", ".yml", ".json"} {
		if reservedBasenames[name+ext] {
			return true
		}
	}
	return false
}

// Load builds the registry: the embedded specs first, then each directory in
// order, with a later source overriding an earlier one by name.
//
// Precedence is the order of the list rather than a branch in here, so
// overriding a built-in -- how you pin your own image for a profile brig
// already knows about -- costs no special case. Materialising the built-ins to
// disk instead of embedding them would be a change to the list, not to this
// function; OverridesBuiltIn is the one place that also reads builtinFS.
//
// A directory that does not exist is not an error: most installs have none.
//
// Called once at startup, before either binary serves a request. registry has
// no synchronisation of its own, so a future reload-on-signal or per-request
// Load would be a data race against brigd's concurrent readers -- and one that
// -race will not catch, since no test calls Load concurrently.
// loadProblems is what Load reports without stopping for. Two kinds, kept
// apart because they read as different sentences: a file that does not parse
// is unusable, and two files claiming one name are both perfectly good files
// that cannot both be that profile.
type loadProblems struct {
	unusable  []string
	duplicate []string
}

func (l *loadProblems) err() error {
	var parts []string
	if len(l.unusable) > 0 {
		parts = append(parts, fmt.Sprintf("ignoring %d unusable profile(s):\n  %s",
			len(l.unusable), strings.Join(l.unusable, "\n  ")))
	}
	if len(l.duplicate) > 0 {
		parts = append(parts, fmt.Sprintf("%d duplicate profile name(s):\n  %s",
			len(l.duplicate), strings.Join(l.duplicate, "\n  ")))
	}
	if len(parts) == 0 {
		return nil
	}
	return errors.New(strings.Join(parts, "\n"))
}

func Load(dirs ...string) error {
	registry = map[string]entry{}
	var problems loadProblems

	if err := loadFS(builtinFS, "specs", OriginBuiltIn, "", &problems); err != nil {
		// The embedded specs are compiled in, so a failure here is a build
		// problem rather than anything a user did.
		return fmt.Errorf("reading the built-in profiles: %w", err)
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if err := loadFS(os.DirFS(dir), ".", OriginFile, dir, &problems); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			// os.DirFS.ReadDir has already rewritten the *PathError's Path to
			// ".", so err alone would surface as "readdir .: permission
			// denied" with dir nowhere in it -- and this is the one error path
			// where the user has no other way to tell which directory that was.
			return fmt.Errorf("reading %s: %w", dir, err)
		}
	}
	// One bad file must not take the others down with it: report it and carry
	// on, so a typo in a profile you are not using does not stop the profile
	// you are.
	//
	// The same carrying-on also covers the case where the names are the same:
	// a broken file that shadows a name an earlier source already supplied
	// leaves that earlier definition registered, so brig runs it. A typo in
	// your own claude-code.yaml therefore does not stop brig -- it means brig
	// boots its own claude-code image rather than the one you meant to pin,
	// and this error naming the file is the only signal that happened.
	return problems.err()
}

// loadFS reads every profile in one directory of one filesystem. Both sources
// are an fs.FS -- embed.FS and os.DirFS -- so there is one reader rather than
// one per kind of source.
func loadFS(fsys fs.FS, dir string, origin Origin, hostDir string, problems *loadProblems) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !isProfileFile(e.Name()) || reservedBasenames[e.Name()] {
			continue
		}
		name := path.Join(dir, e.Name())
		blob, err := fs.ReadFile(fsys, name)
		if err != nil {
			problems.unusable = append(problems.unusable, fmt.Sprintf("%s: %v", e.Name(), err))
			continue
		}
		p, err := Parse(blob)
		if err != nil {
			where := e.Name()
			if hostDir != "" {
				where = filepath.Join(hostDir, e.Name())
			}
			problems.unusable = append(problems.unusable, fmt.Sprintf("%s: %v", where, err))
			continue
		}
		ent := entry{p: p, source: blob, origin: origin}
		if hostDir != "" {
			ent.path = filepath.Join(hostDir, e.Name())
		}
		// A name is the key, and a file does not have to be called after the
		// one it declares, so nothing stops two files in one directory from
		// claiming the same profile. Overriding is the point across sources --
		// that is how a file shadows a built-in -- but within one directory it
		// is a mistake with no winner worth having: the survivor is decided by
		// where the two names happen to sort, and the loser is a file the user
		// edits with no effect at all. Report it and register the last one, so
		// which file won is at least stated rather than merely determined.
		if prev, ok := registry[p.Name]; ok {
			ent.shadowed = append([]string{}, prev.shadowed...)
			if prev.path != "" {
				ent.shadowed = append(ent.shadowed, prev.path)
			}
			if hostDir != "" && filepath.Dir(prev.path) == hostDir {
				problems.duplicate = append(problems.duplicate, fmt.Sprintf(
					"%s and %s in %s both declare the profile %q; %s wins",
					filepath.Base(prev.path), e.Name(), hostDir, p.Name, e.Name()))
			}
		}
		registry[p.Name] = ent
	}
	return nil
}

// Lookup returns the profile for a name or alias.
//
// A copy, not the registry's own value: see Profile.clone for why a plain
// struct copy is not enough here.
func Lookup(name string) (Profile, bool) {
	if e, ok := registry[name]; ok {
		return e.p.clone(), true
	}
	if canonical, ok := aliases[name]; ok {
		if e, ok := registry[canonical]; ok {
			return e.p.clone(), true
		}
	}
	return Profile{}, false
}

// Alias reports the profile a short spelling stands for, and whether the word
// is a spelling at all.
//
// Export asks, because it writes the name into the file it creates. Lookup
// prefers a registry hit over an alias, so a profile actually called claude
// takes every `brig run claude` from claude-code, which is the profile that
// word has always meant -- and the built-in stays reachable only under its
// full name, by someone who has worked out what happened.
func Alias(name string) (string, bool) {
	canonical, ok := aliases[name]
	return canonical, ok
}

// Aliases is the other direction: the short spellings that reach a profile, in
// order. The listing asks, because someone who only ever types `brig run
// claude` has nowhere else to learn which profile that word runs.
//
// A spelling a profile has taken is left out rather than reported. Lookup
// prefers a registry hit, so a file-backed profile called claude wins the word
// outright, and listing it against claude-code would name the one profile that
// word no longer reaches.
func Aliases(canonical string) []string {
	var out []string
	for short, name := range aliases {
		if name != canonical {
			continue
		}
		if _, taken := registry[short]; taken {
			continue
		}
		out = append(out, short)
	}
	sort.Strings(out)
	return out
}

// All returns every profile, in name order. Copies, as Lookup returns.
func All() []Profile {
	out := make([]Profile, 0, len(registry))
	for _, e := range registry {
		out = append(out, e.p.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names returns every profile name, in order.
func Names() []string {
	out := make([]string, 0, len(registry))
	for _, p := range All() {
		out = append(out, p.Name)
	}
	return out
}

// Reserved reports whether a session slug collides with a profile that owns
// the workspace it would land on. Without this, `brig run claude --name
// desktop` puts a Claude Code session on the Desktop app's workspace.
//
// It reads the merged set rather than the built-ins alone, so a profile of
// your own can declare itself reserved. That is the honest reading of a field
// a file can now set.
func Reserved(slug string) (string, bool) {
	for _, p := range All() {
		if !p.Reserved {
			continue
		}
		if slug == p.Name {
			return p.Name, true
		}
		// The trailing word is what a slug of the profile name reads as:
		// claude-desktop is reserved as "desktop" too.
		if i := lastDash(p.Name); i >= 0 && slug == p.Name[i+1:] {
			return p.Name, true
		}
	}
	return "", false
}

// OverridesBuiltIn reports whether a file-backed profile is shadowing one of
// brig's own. Listings say so, because "the image is not what I expected" and
// "there is a file I forgot about" are the same question.
//
// This is the one accessor that reads builtinFS directly rather than through
// the registry, so it stays here rather than beside the other provenance
// accessors in file.go: nothing outside this file is meant to touch that
// variable, per the note on it.
func OverridesBuiltIn(name string) bool {
	return IsCustom(name) && Embedded(name)
}

// Embedded reports whether brig ships a spec of this name, whether or not a
// file is currently shadowing it. Export says so when it writes into the
// profile directory, because "this file now decides what that name runs" is
// the consequence worth stating.
func Embedded(name string) bool {
	_, err := builtinFS.ReadFile("specs/" + name + ".yaml")
	return err == nil
}

// BuiltInDeny is the denylist brig ships for a name, whatever a file of the
// same name currently says.
//
// It exists so a report can name what an override dropped. A file that
// overrides a built-in and omits deny: is a supported thing to write and a
// silent removal of the guard that keeps a metered API key from outranking a
// subscription token -- silent because the report printed the profile's own
// deny list, which in that case is empty. Comparing the two is the only way to
// say so, and the shipped list is the only place the answer exists.
//
// Nothing is denied for a name brig does not ship, and a spec that no longer
// parses is not worth failing a status report over: both answer nil.
func BuiltInDeny(name string) []string {
	blob, err := builtinFS.ReadFile("specs/" + name + ".yaml")
	if err != nil {
		return nil
	}
	p, err := Parse(blob)
	if err != nil {
		return nil
	}
	return p.Deny
}

func lastDash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '-' {
			return i
		}
	}
	return -1
}
