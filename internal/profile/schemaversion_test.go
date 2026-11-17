package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// versionFields are the fields of a profile that passes every rule, bar its
// name, for a test to put a schemaVersion in front of.
const versionFields = "image: example.com/x:1\nguestHome: /home/x\nbinary: x\nmem: 512\ncpus: 1\n"

const versionBase = "name: v\n" + versionFields

// A file that names no schemaVersion is read as version 1, and so is one that
// names 1, wherever in the file the key sits and in JSON as well as YAML. The
// key is optional: nothing writes it yet, and a profile written before it
// existed must load exactly as it did.
func TestAProfileIsReadAsSchemaVersionOne(t *testing.T) {
	for name, tc := range map[string]struct {
		doc  string
		said int
	}{
		"no key":                    {versionBase, 0},
		"the key first":             {"schemaVersion: 1\n" + versionBase, 1},
		"the key last":              {versionBase + "schemaVersion: 1\n", 1},
		"the key in JSON":           {`{"schemaVersion": 1, "name": "v", "image": "example.com/x:1", "guestHome": "/home/x", "binary": "x", "mem": 512, "cpus": 1}`, 1},
		"a float that is whole":     {"schemaVersion: 1.0\n" + versionBase, 1},
		"the key in the wrong case": {"SCHEMAVERSION: 1\n" + versionBase, 1},
	} {
		t.Run(name, func(t *testing.T) {
			p, err := Parse([]byte(tc.doc))
			if err != nil {
				t.Fatalf("Parse = %v", err)
			}
			if p.SchemaVersion != tc.said {
				t.Errorf("SchemaVersion = %d, want %d: the profile records what the file said", p.SchemaVersion, tc.said)
			}
			if p.Schema() != 1 {
				t.Errorf("Schema() = %d, want 1", p.Schema())
			}
		})
	}
}

// A version this brig does not know is refused naming the version, and the
// version is what the error is about. The reason is a file written for a newer
// brig: it will carry fields this brig has never heard of, and the strict
// decoder would say `unknown field "future"` -- an accurate message about the
// wrong thing, which is how the reader learns about a schema change today.
func TestAnUnknownSchemaVersionIsNamedBeforeAnythingElseIsRefused(t *testing.T) {
	for name, doc := range map[string]string{
		"on its own":                        "schemaVersion: 2\n" + versionBase,
		"in JSON":                           `{"schemaVersion": 2, "name": "v"}`,
		"beside a field this brig lacks":    "schemaVersion: 2\nfuture: true\n" + versionBase,
		"beside a key v0.5.0 removed":       "schemaVersion: 2\nstatePaths: [.x]\n" + versionBase,
		"beside a file that is not valid":   "schemaVersion: 2\nname: Not A Name\n",
		"after the fields it contradicts":   versionBase + "future: true\nschemaVersion: 2\n",
		"in the wrong case":                 "SchemaVersion: 2\n" + versionBase,
		"a number a later brig will not be": "schemaVersion: 7\n" + versionBase,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(doc))
			if err == nil {
				t.Fatal("a schemaVersion this brig does not read was accepted")
			}
			for _, want := range []string{"is not supported", "this brig reads schemaVersion 1", "newer brig"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q:\n%s", want, err)
				}
			}
			for _, wrong := range []string{"unknown field", "was removed in", "name "} {
				if strings.Contains(err.Error(), wrong) {
					t.Errorf("the refusal is about something else (%q) and not the version:\n%s", wrong, err)
				}
			}
		})
	}
}

// The number is in the message, so the reader can tell which version the file
// asked for from the error alone.
func TestTheRefusalNamesTheVersionTheFileAskedFor(t *testing.T) {
	_, err := Parse([]byte("schemaVersion: 41\n" + versionBase))
	if err == nil || !strings.Contains(err.Error(), "schemaVersion 41 is not supported") {
		t.Fatalf("Parse = %v, want it to name schemaVersion 41", err)
	}
}

// A value that is not a whole number of 1 or more is not a version at all, and
// is refused with one message that shows what to write, not with a decoder's
// remark about types. A quoted "1" is in the list on purpose: YAML makes it a
// string, and brig does not guess that a string was meant as a number.
func TestASchemaVersionThatIsNotAWholeNumberIsRefused(t *testing.T) {
	for name, doc := range map[string]string{
		"zero":              "schemaVersion: 0\n",
		"negative":          "schemaVersion: -1\n",
		"a fraction":        "schemaVersion: 1.5\n",
		"a string":          "schemaVersion: \"1\"\n",
		"a bool":            "schemaVersion: true\n",
		"empty":             "schemaVersion:\n",
		"null":              "schemaVersion: null\n",
		"a list":            "schemaVersion: [1]\n",
		"a map":             "schemaVersion: {v: 1}\n",
		"too large to read": "schemaVersion: 99999999999999999999\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(doc + versionBase))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), "schemaVersion must be a whole number of 1 or more") ||
				!strings.Contains(err.Error(), "schemaVersion: 1") {
				t.Errorf("the refusal does not say what a version is, or show one:\n%s", err)
			}
		})
	}
}

// The probe that reads the version may only ever say "this is a version this
// brig does not read". Anything else it cannot make out, a file that does not
// parse as YAML or a key given twice, it leaves to the strict decoder, which
// owns every other error: the reader gets the real problem and not a version
// message made up for a file that has no readable version in it.
func TestAFileTheProbeCannotReadIsLeftToTheStrictDecoder(t *testing.T) {
	for name, doc := range map[string]string{
		"not YAML":      "schemaVersion: 1\nname: [unclosed\n",
		"a key twice":   "schemaVersion: 1\nschemaVersion: 1\n" + versionBase,
		"not a mapping": "- schemaVersion\n- 2\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(doc))
			if err == nil {
				t.Fatal("accepted")
			}
			if strings.Contains(err.Error(), "is not supported") {
				t.Errorf("a version message for a file with no readable version:\n%s", err)
			}
		})
	}
}

// A file with no key must not meet any new failure: it is every profile
// written before the key existed, and every shipped one.
func TestTheShippedProfilesNeedNoSchemaVersion(t *testing.T) {
	reset(t)
	for _, name := range []string{"claude-code", "codex", "gemini", "ubuntu", "claude-desktop"} {
		p, _, ok := BuiltIn(name)
		if !ok {
			t.Fatalf("%s is not built in", name)
		}
		if p.SchemaVersion != 0 || p.Schema() != 1 {
			t.Errorf("%s: SchemaVersion %d, Schema() %d: a shipped profile names no version and reads as 1",
				name, p.SchemaVersion, p.Schema())
		}
	}
}

// A file with a version this brig does not read is one more way a file in the
// profile directory can be unusable, and it is treated like the others: named,
// skipped, and the rest still load. A new refusal that took the directory down
// would be worse than the one it replaces.
func TestAFileWithAnUnknownSchemaVersionIsSkippedNotFatal(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	write := func(name, doc string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("future.yaml", "schemaVersion: 2\n"+strings.Replace(versionBase, "name: v", "name: future", 1))
	write("fine.yaml", strings.Replace(versionBase, "name: v", "name: fine", 1))

	err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "future.yaml") ||
		!strings.Contains(err.Error(), "schemaVersion 2 is not supported") {
		t.Errorf("Load = %v, want it to name future.yaml and the version", err)
	}
	if _, ok := Lookup("fine"); !ok {
		t.Error("a good profile was not loaded because its neighbour was from a newer brig")
	}
	if _, ok := Lookup("future"); ok {
		t.Error("a profile this brig cannot read was registered")
	}
}

// The export header is the list docs/stability.md calls the profile schema, so
// a key that is not in it is not a promise. It says what the key is, that it is
// optional, and when to add it.
func TestTheExportHeaderDocumentsSchemaVersion(t *testing.T) {
	for _, want := range []string{"schemaVersion", "optional", "version 1", "does not know"} {
		if !strings.Contains(exportHeader, want) {
			t.Errorf("the export header does not mention %q", want)
		}
	}
}

// The fast path that skips the probe for a file that cannot name the key must
// not skip one that can. Each spelling here reaches the strict decoder as the
// schemaVersion field, so each has to reach the probe too: an escape in a
// double-quoted key, a hex escape, a JSON escape, and the long s, whose case
// folds to the s the decoder is looking for. A version this brig does not read
// is refused for all of them, and refused as the version.
func TestEveryWayOfSpellingTheKeyReachesTheProbe(t *testing.T) {
	for name, doc := range map[string]string{
		"a unicode escape in a quoted key": "\"schema\\u0056ersion\": 2\nfuture: true\n" + versionBase,
		"a hex escape in a quoted key":     "\"\\x73chemaVersion\": 2\nfuture: true\n" + versionBase,
		"an escape in a JSON string":       `{"schema\u0056ersion": 2, "future": true}`,
		"a long s":                         "\u017fchemaVersion: 2\nfuture: true\n" + versionBase,
		"upper case":                       "SCHEMAVERSION: 2\nfuture: true\n" + versionBase,
		"a quoted key":                     "\"schemaVersion\": 2\nfuture: true\n" + versionBase,
	} {
		t.Run(name, func(t *testing.T) {
			if !couldNameSchemaVersion([]byte(doc)) {
				t.Fatal("the fast path would skip a file that names the key")
			}
			_, err := Parse([]byte(doc))
			if err == nil || !strings.Contains(err.Error(), "schemaVersion 2 is not supported") {
				t.Errorf("Parse = %v, want the version named", err)
			}
		})
	}
}

// And the fast path is taken where it should be: no shipped profile names the
// key, so none of them is decoded twice. A path that always answered "maybe"
// would pass every test above and give back none of the time it exists to save.
func TestTheShippedProfilesTakeTheFastPath(t *testing.T) {
	reset(t)
	n := 0
	for _, p := range All() {
		if !Embedded(p.Name) {
			continue
		}
		_, blob, ok := BuiltIn(p.Name)
		if !ok {
			t.Fatalf("%s is not built in", p.Name)
		}
		n++
		if couldNameSchemaVersion(blob) {
			t.Errorf("%s would be decoded twice: the fast path says it may name the key", p.Name)
		}
	}
	if n < 9 {
		t.Fatalf("checked %d shipped profiles, want at least 9", n)
	}
}
