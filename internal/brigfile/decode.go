package brigfile

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"unicode"

	"sigs.k8s.io/yaml"
)

var rawMessage = reflect.TypeOf(json.RawMessage{})

// checkTree walks a Brigfile decoded into plain maps and lists beside the
// type it describes, and refuses two things the typed decode lets through
// or reports without a path.
//
// An unknown key. encoding/json reports one as `unknown field "network"`,
// which does not say under which target. The walk names the whole path,
// targets.dev.overrides.network, and compares keys byte for byte, so a key
// in the wrong case is refused here too.
//
// A scalar that is not a string where the type wants one. sigs.k8s.io/yaml
// converts a YAML scalar to the type of the field it lands in, so an
// unquoted 0600 reaches a string field as "384", and yes as "true". In a
// guest argument or a file path that changes what the file says without a
// word. In the plain tree those values are still a number and a boolean, so
// the walk can see them.
//
// An inline document is not walked: it is a profile or a policy, and its own
// parser decides what it may hold.
func checkTree(v any, t reflect.Type, path string) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == rawMessage || v == nil {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		obj, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
			fields[name] = t.Field(i).Type
		}
		for _, k := range sortedKeys(obj) {
			ft, ok := fields[k]
			if !ok {
				return fmt.Errorf("unknown field %q", join(path, k))
			}
			if err := checkTree(obj[k], ft, join(path, k)); err != nil {
				return err
			}
		}
	case reflect.Map:
		obj, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		for _, k := range sortedKeys(obj) {
			if err := checkTree(obj[k], t.Elem(), join(path, k)); err != nil {
				return err
			}
		}
	case reflect.Slice:
		list, ok := v.([]any)
		if !ok {
			return nil
		}
		for i, item := range list {
			if err := checkTree(item, t.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case reflect.String:
		switch v.(type) {
		case string:
		case bool:
			return fmt.Errorf("field %q is %v, which YAML reads as a boolean, not a string; quote it", path, v)
		case float64:
			return fmt.Errorf("field %q is %v, which YAML reads as a number, not a string; quote it", path, v)
		default:
			return fmt.Errorf("field %q is not a string", path)
		}
	}
	return nil
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// noControl refuses a control character in any key or string of a profile
// or policy document the repository wrote, a newline and a tab included, and
// names where it is.
//
// The strings of these documents reach the reader's terminal: brig plan
// prints the guest home, every variable name, every file path and every
// secret name. An escape sequence in one of them could move the cursor and
// clear the rows above it, which are the rows that say where a route sends
// your secret, and a newline could print a row of its own. A field the
// profile or policy parser checks already, such as a secret name, is
// checked here as well, so no field is left to a check that does not exist.
func noControl(doc []byte) error {
	var tree any
	if err := yaml.Unmarshal(doc, &tree); err != nil {
		// The parser that runs next reports it, with its own words.
		return nil
	}
	return walkControl(tree, "")
}

func walkControl(v any, path string) error {
	switch v := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(v) {
			if strings.IndexFunc(k, unicode.IsControl) >= 0 {
				return fmt.Errorf("the key %q under %q holds a control character", k, path)
			}
			if err := walkControl(v[k], join(path, k)); err != nil {
				return err
			}
		}
	case []any:
		for i, item := range v {
			if err := walkControl(item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case string:
		if strings.IndexFunc(v, unicode.IsControl) >= 0 {
			return fmt.Errorf("%s: %q holds a control character", path, v)
		}
	}
	return nil
}
