// Package jsonfind searches a JSON document for a field at any depth.
//
// It lives on its own rather than inside internal/creds because two packages
// need it and they must not depend on each other: creds resolves credentials
// on the run path, and the importer reads host sources on a path the run is
// architecturally forbidden to reach (see internal/hostsrc). One recursive
// search, no shared dependency.
package jsonfind

import (
	"encoding/json"
	"strings"
)

// String and Number find a field in the blob, so a credential wrapped in an
// envelope needs no path configured when its field is unique.
//
// A dotted name, such as claudeAiOauth.expiresAt, is first a path from the
// root. Next comes a key at the root that holds the whole name. Only then is
// the name searched at any depth: a key that holds the whole name, or the
// path below a key that holds its first segment. A search at depth that finds
// two different values is no match. A Claude Code credentials document holds
// expiresAt and accessToken once for the login and again for each MCP server
// signed in with OAuth, and map order used to pick one of them at random.
// This is the rule the broker applies to a route's field (internal/broker).
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
	if v, ok := walkPath(doc, strings.Split(field, ".")); ok {
		return v, true
	}
	if m, ok := doc.(map[string]any); ok {
		if v, ok := m[field]; ok {
			return v, true
		}
	}
	var found []any
	collect(doc, field, &found)
	if len(found) == 0 {
		return nil, false
	}
	for _, v := range found[1:] {
		if !same(found[0], v) {
			return nil, false
		}
	}
	return found[0], true
}

// walkPath follows the keys of path down from node.
func walkPath(node any, path []string) (any, bool) {
	for _, key := range path {
		m, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		if node, ok = m[key]; !ok {
			return nil, false
		}
	}
	return node, true
}

// collect appends every value below node that field selects: the value of a
// key equal to field, and for a dotted field the rest of the path below a key
// equal to its first segment. The order of found follows map order, which is
// why find accepts it only when every value in it is the same.
func collect(node any, field string, found *[]any) {
	head, rest, dotted := strings.Cut(field, ".")
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			if k == field {
				*found = append(*found, v)
			} else if dotted && k == head {
				if got, ok := walkPath(v, strings.Split(rest, ".")); ok {
					*found = append(*found, got)
				}
			}
			collect(v, field, found)
		}
	case []any:
		for _, v := range n {
			collect(v, field, found)
		}
	}
}

// same reports whether two found values are one answer. Only a scalar can
// be: an object or an array found twice is two answers, and comparing them
// with == would panic.
func same(a, b any) bool {
	switch a.(type) {
	case string, float64, bool:
		return a == b
	}
	return false
}
