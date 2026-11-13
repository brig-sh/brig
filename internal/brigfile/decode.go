package brigfile

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
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
