package brigfile

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// schemaPath is the JSON Schema an editor reads. It sits under api/ because
// it is the published shape of the document, and it is checked from here
// because a Go package under api/ could not import this one.
const schemaPath = "../../api/project/v1alpha1/brigfile.schema.json"

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

// The schema and the Go types describe one document. Every object's property
// names equal the JSON tags of its struct, and every object refuses a key it
// does not list, as the decoder does. Change one without the other and this
// fails.
func TestTheSchemaMatchesTheTypes(t *testing.T) {
	schema := readSchema(t)
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("$schema = %v", schema["$schema"])
	}
	defs := object(t, schema["$defs"], "$defs")
	for where, c := range map[string]struct {
		node any
		typ  reflect.Type
	}{
		"the project":  {schema, reflect.TypeOf(Project{})},
		"profileAlias": {defs["profileAlias"], reflect.TypeOf(ProfileAlias{})},
		"policyAlias":  {defs["policyAlias"], reflect.TypeOf(PolicyAlias{})},
		"target":       {defs["target"], reflect.TypeOf(Target{})},
		"overrides":    {defs["overrides"], reflect.TypeOf(Overrides{})},
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
		if node["type"] != "object" {
			t.Errorf("%s: type is %v, want object", where, node["type"])
		}
	}

	props := object(t, schema["properties"], "properties")
	if got := object(t, props["apiVersion"], "apiVersion")["const"]; got != APIVersion {
		t.Errorf("apiVersion const = %v, want %s", got, APIVersion)
	}
	for _, def := range []string{"profileAlias", "policyAlias"} {
		inline := object(t, object(t, object(t, defs[def], def)["properties"], def)["inline"], def+".inline")
		if inline["type"] != "object" {
			t.Errorf("%s.inline type = %v, want object", def, inline["type"])
		}
	}
}
