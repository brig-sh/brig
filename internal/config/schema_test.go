package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestKeyParseValue(t *testing.T) {
	tests := []struct {
		name string
		key  Key
		raw  string
		want any
		bad  bool
	}{
		{name: "bool true", key: Key{Kind: Bool}, raw: "true", want: true},
		{name: "bool false", key: Key{Kind: Bool}, raw: "false", want: false},
		{name: "bool 1", key: Key{Kind: Bool}, raw: "1", want: true},
		{name: "bool 0", key: Key{Kind: Bool}, raw: "0", want: false},
		// Strict, unlike wrap.Env.Bool, which reads anything but "0" as true.
		{name: "bool rejects word", key: Key{Kind: Bool}, raw: "maybe", bad: true},
		{name: "bool rejects empty", key: Key{Kind: Bool}, raw: "", bad: true},

		{name: "int", key: Key{Kind: Int}, raw: "4096", want: 4096},
		{name: "int negative", key: Key{Kind: Int}, raw: "-1", want: -1},
		{name: "int rejects float", key: Key{Kind: Int}, raw: "1.5", bad: true},
		{name: "int rejects word", key: Key{Kind: Int}, raw: "lots", bad: true},

		{name: "string", key: Key{Kind: String}, raw: "npx", want: "npx"},
		{name: "string allows empty", key: Key{Kind: String}, raw: "", want: ""},

		{
			name: "enum in set",
			key:  Key{Kind: Enum, Enum: []string{"stdio", "http"}},
			raw:  "http", want: "http",
		},
		{
			name: "enum outside set",
			key:  Key{Kind: Enum, Enum: []string{"stdio", "http"}},
			raw:  "grpc", bad: true,
		},

		{name: "list", key: Key{Kind: StringList}, raw: "a,b,c", want: []string{"a", "b", "c"}},
		{name: "list trims", key: Key{Kind: StringList}, raw: "a , b", want: []string{"a", "b"}},
		{name: "list single", key: Key{Kind: StringList}, raw: "a", want: []string{"a"}},
		{name: "list empty is empty", key: Key{Kind: StringList}, raw: "", want: []string{}},
		{name: "list drops blanks", key: Key{Kind: StringList}, raw: "a,,b", want: []string{"a", "b"}},

		{name: "env var name", key: Key{Kind: EnvVar}, raw: "ACME_TOKEN", want: "ACME_TOKEN"},
		{name: "env var lowercase ok", key: Key{Kind: EnvVar}, raw: "_x1", want: "_x1"},
		// The guard that stops a token being pasted where a name belongs.
		{name: "env var rejects value", key: Key{Kind: EnvVar}, raw: "sk-ant-abc123", bad: true},
		{name: "env var rejects leading digit", key: Key{Kind: EnvVar}, raw: "1X", bad: true},
		{name: "env var rejects empty", key: Key{Kind: EnvVar}, raw: "", bad: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.key.ParseValue(tc.raw)
			if tc.bad {
				if err == nil {
					t.Fatalf("ParseValue(%q) = %v, want an error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseValue(%q): %v", tc.raw, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseValue(%q) = %#v, want %#v", tc.raw, got, tc.want)
			}
		})
	}
}

// testSchema stands in for the shipped table, which arrives with the loader
// that reads it. Declaring keys in the test is also the extensibility property
// under test: a section is rows of data, so a test can invent one.
var testSchema = Schema{
	{Path: "skills.import.auto", Kind: Bool, Default: false, Doc: "seed host skills"},
	{Path: "skills.deny", Kind: StringList, Doc: "never load these"},
	{Path: "mcp.servers.*.command", Kind: String, Doc: "the server binary"},
	{Path: "mcp.servers.*.transport", Kind: Enum, Enum: []string{"stdio", "http"}, Default: "stdio", Doc: "transport"},
	{Path: "features.*", Kind: Bool, Doc: "a feature flag"},
}

func TestSchemaLookup(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		want     string // the declaring Path, wildcards intact
		wantKind Kind
		absent   bool
	}{
		{name: "literal", path: "skills.import.auto", want: "skills.import.auto", wantKind: Bool},
		{name: "literal list", path: "skills.deny", want: "skills.deny", wantKind: StringList},

		{name: "wildcard instance", path: "mcp.servers.github.command", want: "mcp.servers.*.command", wantKind: String},
		{name: "wildcard instance with dashes", path: "mcp.servers.my-server.command", want: "mcp.servers.*.command", wantKind: String},
		{name: "wildcard instance enum", path: "mcp.servers.github.transport", want: "mcp.servers.*.transport", wantKind: Enum},
		{name: "trailing wildcard", path: "features.someFlag", want: "features.*", wantKind: Bool},

		{name: "unknown top level", path: "nope.at.all", absent: true},
		{name: "typo", path: "skills.import.auot", absent: true},
		// A wildcard segment matches exactly one segment, never several.
		{name: "too deep for wildcard", path: "features.a.b", absent: true},
		{name: "too shallow", path: "mcp.servers.github", absent: true},
		{name: "prefix is not a key", path: "skills.import", absent: true},
		{name: "empty wildcard segment", path: "mcp.servers..command", absent: true},
		{name: "empty path", path: "", absent: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := testSchema.Lookup(tc.path)
			if tc.absent {
				if ok {
					t.Fatalf("Lookup(%q) matched %q, want no match", tc.path, got.Path)
				}
				return
			}
			if !ok {
				t.Fatalf("Lookup(%q) found nothing, want %q", tc.path, tc.want)
			}
			if got.Path != tc.want {
				t.Errorf("Lookup(%q) = %q, want %q", tc.path, got.Path, tc.want)
			}
			if got.Kind != tc.wantKind {
				t.Errorf("Lookup(%q) kind = %s, want %s", tc.path, got.Kind, tc.wantKind)
			}
		})
	}
}

func TestSchemaLookupPrefersAnExactMatch(t *testing.T) {
	// A literal declaration must win over a wildcard that also admits the path,
	// whichever order they appear in. Without that rule the result depends on
	// slice position, so moving two rows apart silently changes a key's type.
	literal := Key{Path: "mcp.servers.github.command", Kind: Enum, Enum: []string{"gh"}}
	wild := Key{Path: "mcp.servers.*.command", Kind: String}

	for _, tc := range []struct {
		name string
		s    Schema
	}{
		{name: "wildcard declared first", s: Schema{wild, literal}},
		{name: "literal declared first", s: Schema{literal, wild}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.s.Lookup("mcp.servers.github.command")
			if !ok {
				t.Fatal("Lookup found nothing")
			}
			if got.Path != literal.Path {
				t.Errorf("Lookup = %q, want the exact match %q", got.Path, literal.Path)
			}
			if got.Kind != Enum {
				t.Errorf("Lookup kind = %s, want enum", got.Kind)
			}
		})
	}

	// Another name under the same wildcard still resolves through it.
	got, ok := Schema{wild, literal}.Lookup("mcp.servers.other.command")
	if !ok || got.Path != wild.Path {
		t.Errorf("Lookup(other) = %q ok=%v, want %q", got.Path, ok, wild.Path)
	}
}

func TestSchemaValidate(t *testing.T) {
	tests := []struct {
		name string
		s    Schema
		bad  string // substring the error must mention; "" means valid
	}{
		{
			name: "valid",
			s: Schema{
				{Path: "a.b", Kind: Bool, Default: false, Doc: "d"},
				{Path: "a.c", Kind: Enum, Enum: []string{"x", "y"}, Default: "x", Doc: "d"},
				{Path: "m.*.n", Kind: String, Doc: "d"},
			},
		},
		// The mistake the zero value used to hide.
		{name: "kind omitted", s: Schema{{Path: "a.b", Doc: "d"}}, bad: "kind"},
		{name: "empty path", s: Schema{{Path: "", Kind: Bool, Doc: "d"}}, bad: "path"},
		{name: "empty segment", s: Schema{{Path: "a..b", Kind: Bool, Doc: "d"}}, bad: "segment"},
		{
			name: "duplicate path",
			s: Schema{
				{Path: "a.b", Kind: Bool, Doc: "d"},
				{Path: "a.b", Kind: Int, Doc: "d"},
			},
			bad: "declared twice",
		},
		{name: "enum without values", s: Schema{{Path: "a.b", Kind: Enum, Doc: "d"}}, bad: "enum"},
		{
			name: "enum values on a non-enum kind",
			s:    Schema{{Path: "a.b", Kind: Bool, Enum: []string{"x"}, Doc: "d"}},
			bad:  "enum",
		},
		// Default is `any`, so nothing but this check stops a string default on
		// an int key reaching a consumer that asserts an int.
		{
			name: "default of the wrong type",
			s:    Schema{{Path: "a.b", Kind: Int, Default: "4096", Doc: "d"}},
			bad:  "default",
		},
		{
			name: "default outside the enum",
			s:    Schema{{Path: "a.b", Kind: Enum, Enum: []string{"x"}, Default: "z", Doc: "d"}},
			bad:  "default",
		},
		{
			name: "list default of the wrong type",
			s:    Schema{{Path: "a.b", Kind: StringList, Default: "x", Doc: "d"}},
			bad:  "default",
		},
		{name: "missing doc", s: Schema{{Path: "a.b", Kind: Bool}}, bad: "doc"},
		{name: "nil default is fine", s: Schema{{Path: "a.b", Kind: Int, Doc: "d"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.s.Validate()
			if tc.bad == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want no error", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %q", tc.bad)
			}
			if !strings.Contains(err.Error(), tc.bad) {
				t.Errorf("Validate() = %q, want it to mention %q", err, tc.bad)
			}
			// Every schema error has to name the offending key.
			if tc.s[0].Path != "" && !strings.Contains(err.Error(), tc.s[0].Path) {
				t.Errorf("Validate() = %q, does not name the key %q", err, tc.s[0].Path)
			}
		})
	}
}

func TestParseValueWithNoKindDeclared(t *testing.T) {
	// Reachable only from a schema that skipped Validate, but it must not read
	// as a successful bool.
	k := Key{Path: "a.b"}
	if _, err := k.ParseValue("true"); err == nil {
		t.Error("ParseValue on a Key with no Kind should fail, not return a bool")
	}
	if got := invalid.String(); got != "no kind" {
		t.Errorf("invalid.String() = %q, want %q", got, "no kind")
	}
}

func TestSchemaSections(t *testing.T) {
	want := []string{"features", "mcp", "skills"} // sorted, deduped
	got := testSchema.Sections()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Sections() = %v, want %v", got, want)
	}
}

func TestSchemaKeys(t *testing.T) {
	var paths []string
	for _, k := range testSchema.Keys("skills") {
		paths = append(paths, k.Path)
	}
	want := []string{"skills.deny", "skills.import.auto"} // sorted by path
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("Keys(\"skills\") = %v, want %v", paths, want)
	}
	if len(testSchema.Keys("nope")) != 0 {
		t.Error("Keys of an undeclared section should be empty")
	}
}

func TestKeyResolveExpandsBeforeTypeChecking(t *testing.T) {
	vars := env(map[string]string{"MEM": "8192", "FLAG": "true", "MODE": "http"})

	tests := []struct {
		name string
		key  Key
		raw  string
		want any
		bad  bool
	}{
		// The ordering that makes expressions usable on non-string keys.
		{name: "int from var", key: Key{Kind: Int}, raw: "${MEM}", want: 8192},
		{name: "int from default", key: Key{Kind: Int}, raw: "${NOPE:-4096}", want: 4096},
		{name: "bool from var", key: Key{Kind: Bool}, raw: "${FLAG}", want: true},
		{
			name: "enum from var",
			key:  Key{Kind: Enum, Enum: []string{"stdio", "http"}},
			raw:  "${MODE}", want: "http",
		},
		{name: "list from default", key: Key{Kind: StringList}, raw: "${NOPE:-a,b}", want: []string{"a", "b"}},
		{name: "plain value still works", key: Key{Kind: Int}, raw: "512", want: 512},

		// Expansion succeeds, the type check then fails: both errors reachable.
		{name: "expands to a bad int", key: Key{Kind: Int}, raw: "${MODE}", bad: true},
		{name: "expansion itself fails", key: Key{Kind: Int}, raw: "${NOPE}", bad: true},

		// EnvVar holds a name, so it is never expanded -- otherwise declaring a
		// credential reference would resolve the credential.
		{name: "env var kind not expanded", key: Key{Kind: EnvVar}, raw: "MEM", want: "MEM"},
		{name: "env var kind rejects an expression", key: Key{Kind: EnvVar}, raw: "${MEM}", bad: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.key.Resolve(tc.raw, vars)
			if tc.bad {
				if err == nil {
					t.Fatalf("Resolve(%q) = %#v, want an error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tc.raw, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Resolve(%q) = %#v, want %#v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestKeyResolveNeverExpandsAnEnvVarName(t *testing.T) {
	// The case the old tests could not see. This token's text is shaped like a
	// variable name -- letters, digits and underscores only -- so if Resolve
	// expanded EnvVar keys, the expansion would pass validVarName and the
	// secret would be stored in the slot that is supposed to hold a reference.
	//
	// Deleting the EnvVar guard in Resolve must fail this test. The two older
	// cases ("MEM" and "${MEM}") both survive without the guard, which is how
	// the rule went unpinned.
	vars := env(map[string]string{"ACME_TOKEN": "abc123def456"})
	k := Key{Path: "memories.providers.acme.credential.fromEnv", Kind: EnvVar}

	if got, err := k.Resolve("${ACME_TOKEN}", vars); err == nil {
		t.Fatalf("Resolve expanded an EnvVar key to %#v; it must reject the expression", got)
	}
	// The name itself still passes through untouched.
	got, err := k.Resolve("ACME_TOKEN", vars)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", "ACME_TOKEN", err)
	}
	if got != "ACME_TOKEN" {
		t.Errorf("Resolve(%q) = %#v, want the name unchanged", "ACME_TOKEN", got)
	}
}

func TestEnvVarErrorNeverEchoesTheValue(t *testing.T) {
	// This error exists to catch someone pasting a credential where a variable
	// name belongs, so it must not print the credential. creds.go:102-106 sets
	// the house rule: report the scheme, never the value.
	secrets := []string{
		"sk-live-51H8xKlSecretValue", // rejected on the dash
		"51H8xKlSecretValue",         // rejected on the leading digit
	}
	for _, secret := range secrets {
		k := Key{Path: "memories.providers.acme.credential.fromEnv", Kind: EnvVar}
		_, err := k.Resolve(secret, nil)
		if err == nil {
			t.Fatalf("Resolve(%q) succeeded; want it rejected", secret)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error echoes the whole secret: %q", err)
		}
		// Not even a recognisable fragment.
		for _, frag := range []string{"sk-live", "51H8", "SecretValue"} {
			if strings.Contains(err.Error(), frag) {
				t.Errorf("error leaks the fragment %q: %q", frag, err)
			}
		}
		// It still has to name the setting, or nobody can find the offending line.
		if !strings.Contains(err.Error(), "memories.providers.acme.credential.fromEnv") {
			t.Errorf("error does not name the key: %q", err)
		}
	}
}

func TestKeyResolveErrorNamesTheKey(t *testing.T) {
	// A type error has to say which setting is wrong; the raw text alone does
	// not tell anyone where to look in the file.
	k := Key{Path: "skills.import.auto", Kind: Bool}
	_, err := k.Resolve("maybe", env(nil))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "skills.import.auto") {
		t.Errorf("error %q does not name the key", err)
	}
}

func TestKindString(t *testing.T) {
	// Kind names reach the user in "expected bool, got ..." errors, so they are
	// part of the interface rather than debug output.
	for kind, want := range map[Kind]string{
		invalid: "no kind", Bool: "bool", Int: "int", String: "string",
		Enum: "enum", StringList: "list", EnvVar: "env var name",
	} {
		if got := kind.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", int(kind), got, want)
		}
	}
	// An out-of-range Kind can only come from a cast, but it must still print
	// as something rather than as an empty string in an error message.
	if got := Kind(99).String(); got != "kind(99)" {
		t.Errorf("Kind(99).String() = %q, want %q", got, "kind(99)")
	}
}
