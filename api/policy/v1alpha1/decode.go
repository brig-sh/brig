package v1alpha1

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
	// The YAML parser that sigs.k8s.io/yaml wraps, imported through the same
	// module so go.mod gains nothing. Only its Decoder reads a stream of
	// documents.
	goyaml "sigs.k8s.io/yaml/goyaml.v2"
)

// Decode decodes one policy document, in YAML or JSON, without validating it.
// It refuses what UnmarshalStrict refuses. Call Validate on the result.
func Decode(doc []byte) (*Policy, error) {
	var p Policy
	if err := UnmarshalStrict(doc, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// UnmarshalStrict decodes one YAML or JSON document into into, a pointer to a
// type of this package or to a struct with json tags of the same kind.
//
// It refuses a stream of more than one document, a key given twice, a block
// or a list with no value, and a key that is not the exact json name of a
// field. sigs.k8s.io/yaml decodes through encoding/json, which matches a key
// to a field without regard to case, and folds ſ to s and the Kelvin sign to
// k. A key such as Spec or upſtream would then set the field it resembles,
// and win over the real key beside it. So the document is decoded a second
// time into maps, and every key is compared byte for byte with the field
// names of the target type.
func UnmarshalStrict(doc []byte, into any) error {
	if err := singleDocument(doc); err != nil {
		return err
	}
	if err := yaml.UnmarshalStrict(doc, into); err != nil {
		return err
	}
	var tree any
	if err := yaml.Unmarshal(doc, &tree); err != nil {
		return err
	}
	return exactKeys(tree, reflect.TypeOf(into), "")
}

// UnmarshalFolded decodes one document the way the retired flat policy form
// was always decoded: a key matches a field without regard to case, as
// encoding/json matches it, so a file that spells Egress: still loads. It
// refuses a stream of more than one document, an unknown key, and two keys
// that name one field, such as name: and Name:, where encoding/json would
// let one win over the other unseen. Every other document uses
// UnmarshalStrict.
func UnmarshalFolded(doc []byte, into any) error {
	if err := singleDocument(doc); err != nil {
		return err
	}
	if err := yaml.UnmarshalStrict(doc, into); err != nil {
		return err
	}
	var tree any
	if err := yaml.Unmarshal(doc, &tree); err != nil {
		return err
	}
	return foldedKeys(tree, reflect.TypeOf(into), "")
}

// singleDocument refuses a stream with a second document after the first.
// yaml.UnmarshalStrict reads the first document and ignores the rest. A
// later document that holds nothing, such as a trailing --- with only
// comments after it, drops nothing, so it is not counted: older versions
// loaded a file that ends in ---, and it still loads.
func singleDocument(doc []byte) error {
	d := goyaml.NewDecoder(bytes.NewReader(doc))
	for n := 0; ; n++ {
		var v any
		switch err := d.Decode(&v); {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		}
		// An empty first document followed by one that is not is refused
		// too: the decode would read the empty one and drop the other.
		if n > 0 && v != nil {
			return errors.New("the file holds more than one YAML document; one file is one document")
		}
	}
}

// exactKeys walks a decoded tree beside the type it was decoded into, and
// refuses an object key that is not the json name of a field of that type.
func exactKeys(v any, t reflect.Type, path string) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		obj, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		fields := jsonFields(t)
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			ft, ok := fields[k]
			if !ok {
				return fmt.Errorf("unknown field %q", join(path, k))
			}
			// A block or a list with no value decodes as if it were left
			// out. Under request: that turns an allow list whose rules are
			// all commented out into a route that allows every request. A
			// string with no value reads as empty, and is left alone.
			if obj[k] == nil && (ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice) {
				return fmt.Errorf("field %q has no value; give it one, or leave the key out", join(path, k))
			}
			if err := exactKeys(obj[k], ft, join(path, k)); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		list, ok := v.([]any)
		if !ok {
			return nil
		}
		for i, item := range list {
			if err := exactKeys(item, t.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		obj, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		for k, item := range obj {
			if err := exactKeys(item, t.Elem(), join(path, k)); err != nil {
				return err
			}
		}
	}
	return nil
}

// foldedKeys walks a decoded tree beside the type it was decoded into, and
// refuses two object keys that match one field without regard to case.
func foldedKeys(v any, t reflect.Type, path string) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		obj, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		fields := jsonFields(t)
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		seen := map[string]string{}
		for _, k := range keys {
			name := ""
			for f := range fields {
				if f == k || (name == "" && strings.EqualFold(f, k)) {
					name = f
				}
			}
			if name == "" {
				return fmt.Errorf("unknown field %q", join(path, k))
			}
			if other, ok := seen[name]; ok {
				return fmt.Errorf("fields %q and %q both set %q; keep one", join(path, other), join(path, k), join(path, name))
			}
			seen[name] = k
			if err := foldedKeys(obj[k], fields[name], join(path, k)); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		list, ok := v.([]any)
		if !ok {
			return nil
		}
		for i, item := range list {
			if err := foldedKeys(item, t.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		obj, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		for k, item := range obj {
			if err := foldedKeys(item, t.Elem(), join(path, k)); err != nil {
				return err
			}
		}
	}
	return nil
}

// jsonFields maps the json name of each field of t to the field's type, the
// fields of an embedded struct with no name of its own included.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			for k, ft := range jsonFields(f.Type) {
				out[k] = ft
			}
			continue
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
