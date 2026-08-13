package config

import (
	"reflect"
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

func TestKindString(t *testing.T) {
	// Kind names reach the user in "expected bool, got ..." errors, so they are
	// part of the interface rather than debug output.
	for kind, want := range map[Kind]string{
		Bool: "bool", Int: "int", String: "string",
		Enum: "enum", StringList: "list", EnvVar: "env var name",
	} {
		if got := kind.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", int(kind), got, want)
		}
	}
}
