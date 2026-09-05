// Package jsonfind searches a JSON document for a field at any depth.
//
// It lives on its own rather than inside internal/creds because two packages
// need it and they must not depend on each other: creds resolves credentials
// on the run path, and the importer reads host sources on a path the run is
// architecturally forbidden to reach (see internal/hostsrc). One recursive
// search, no shared dependency.
package jsonfind

import "encoding/json"

// String and Number search the blob for a field at any depth, so a credential
// wrapped in an envelope needs no path configured. The first match in
// document order wins.
func String(blob []byte, field string) (string, bool) {
	v, ok := find(blob, field)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func Number(blob []byte, field string) (int64, bool) {
	v, ok := find(blob, field)
	if !ok {
		return 0, false
	}
	n, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int64(n), true
}

func find(blob []byte, field string) (any, bool) {
	var doc any
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, false
	}
	return walk(doc, field)
}

func walk(node any, field string) (any, bool) {
	switch n := node.(type) {
	case map[string]any:
		if v, ok := n[field]; ok {
			return v, true
		}
		// Map iteration order is random, so a nested hit is only stable when
		// the field appears once below this level. Profiles name fields that
		// are unique in their blob, which is the case this serves.
		for _, v := range n {
			if got, ok := walk(v, field); ok {
				return got, true
			}
		}
	case []any:
		for _, v := range n {
			if got, ok := walk(v, field); ok {
				return got, true
			}
		}
	}
	return nil, false
}
