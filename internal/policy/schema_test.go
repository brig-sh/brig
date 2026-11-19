package policy

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// schemaPath is the JSON Schema an editor reads. It sits under api/ because
// it is the published shape of the document, and it is checked from here
// because a Go package under api/ could not import the checks it needs.
const schemaPath = "../../api/policy/v1alpha1/policy.schema.json"

func readSchema(t *testing.T) map[string]any {
	t.Helper()
	blob, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(blob, &schema); err != nil {
		t.Fatalf("the schema is not valid JSON: %v", err)
	}
	return schema
}

// jsonNames is the JSON field names of a struct type, sorted.
func jsonNames(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func object(t *testing.T, v any, where string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object in the schema", where)
	}
	return m
}

// The schema and the MCP types describe one document: each object's property
// names equal the JSON tags of its struct, and each refuses a key it does not
// list, as the decoder does. Only the MCP objects are pinned; routes and
// egress are typed loosely in the schema by design.
func TestThePolicySchemaMatchesTheMCPTypes(t *testing.T) {
	schema := readSchema(t)
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("$schema = %v", schema["$schema"])
	}
	defs := object(t, schema["$defs"], "$defs")
	for where, c := range map[string]struct {
		node any
		typ  reflect.Type
	}{
		"mcpServer": {defs["mcpServer"], reflect.TypeOf(v1alpha1.MCPServer{})},
		"mcpTools":  {defs["mcpTools"], reflect.TypeOf(v1alpha1.MCPTools{})},
	} {
		node := object(t, c.node, where)
		props := object(t, node["properties"], where+".properties")
		var got []string
		for k := range props {
			got = append(got, k)
		}
		sort.Strings(got)
		if want := jsonNames(c.typ); !slices.Equal(got, want) {
			t.Errorf("%s: schema properties %v, Go fields %v", where, got, want)
		}
		if node["additionalProperties"] != false {
			t.Errorf("%s: additionalProperties is %v, want false", where, node["additionalProperties"])
		}
	}
	server := object(t, object(t, defs["mcpServer"], "mcpServer")["properties"], "mcpServer.properties")
	transport := object(t, server["transport"], "transport")
	if got, _ := transport["enum"].([]any); !reflect.DeepEqual(got, []any{"http"}) {
		t.Errorf("transport enum = %v, want [http]", transport["enum"])
	}
}

// A server with no tool allowlist would reach every tool, so the schema
// makes an editor flag the omission as the validator does.
func TestThePolicySchemaRequiresTools(t *testing.T) {
	defs := object(t, readSchema(t)["$defs"], "$defs")
	required, _ := object(t, defs["mcpServer"], "mcpServer")["required"].([]any)
	for _, want := range []string{"name", "tools"} {
		if !slices.Contains(required, any(want)) {
			t.Errorf("mcpServer.required = %v, missing %s", required, want)
		}
	}
	tools := object(t, object(t, defs["mcpTools"], "mcpTools")["properties"], "mcpTools.properties")
	if got := object(t, tools["allow"], "allow")["minItems"]; got != float64(1) {
		t.Errorf("mcpTools.allow.minItems = %v, want 1", got)
	}
}

// A preset sends the credential in a header, so a preset server with no
// credential is refused at load. The schema makes an editor flag it too.
func TestThePolicySchemaRequiresACredentialWithAPreset(t *testing.T) {
	defs := object(t, readSchema(t)["$defs"], "$defs")
	branches, _ := object(t, defs["mcpServer"], "mcpServer")["oneOf"].([]any)
	for _, b := range branches {
		required, _ := object(t, b, "mcpServer.oneOf")["required"].([]any)
		if slices.Contains(required, any("preset")) && !slices.Contains(required, any("credential")) {
			t.Errorf("mcpServer.oneOf branch %v names preset without credential", required)
		}
	}
}

// The schema's preset list is what an editor offers, so a preset brig embeds
// that the schema leaves out would be flagged as an error in a valid file.
func TestThePolicySchemaListsTheMCPPresets(t *testing.T) {
	defs := object(t, readSchema(t)["$defs"], "$defs")
	server := object(t, object(t, defs["mcpServer"], "mcpServer")["properties"], "mcpServer.properties")
	enum, _ := object(t, server["preset"], "preset")["enum"].([]any)
	var got []string
	for _, v := range enum {
		got = append(got, v.(string))
	}
	sort.Strings(got)
	var want []string
	for _, n := range MCPPresetNames() {
		want = append(want, string(n))
	}
	if !slices.Equal(got, want) {
		t.Errorf("schema preset enum %v, embedded MCP presets %v", got, want)
	}
}
