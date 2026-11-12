// Package pointerkind tells a stored pointer document from a raw credential
// by its kind field.
//
// The broker sends a raw credential upstream or to the guest, and a pointer
// names the user's cloud profile, role or account, so a document that names
// a pointer kind must never pass for a raw credential, even when it is not
// valid JSON. awscred and gcpcred both need the check and do not depend on
// each other, so it lives here.
package pointerkind

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Trim returns doc without the bytes around it that the JSON decoder
// refuses and an editor may add: a byte order mark, Unicode spaces, format
// characters and invalid UTF-8. Each such byte before the object would make
// the decoder refuse the whole document, so it would pass for a raw
// credential. Is and the broker's check for a document that does not decode
// both read doc through Trim, so the two cannot disagree about where the
// object starts.
func Trim(doc []byte) []byte {
	return bytes.TrimFunc(doc, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError
	})
}

// Is reports whether doc is a JSON object whose top-level kind field is
// want. It reads the object a token at a time, so a document cut short after
// its kind, or with data after the object, still counts. A whole-document
// json.Unmarshal refuses both, and the document would then pass for a raw
// credential. A document cut short before its kind field names no kind and
// is not matched.
//
// The field name is matched without regard to case because encoding/json
// matches it that way, so ParsePointer takes "Kind" for "kind". The value is
// matched the same way although ParsePointer compares it exactly: the match
// only decides whether a document is held back, so matching more is the safe
// direction. Every kind field is read, not only the first, because
// ParsePointer keeps the last of a duplicate key.
func Is(doc []byte, want string) bool {
	dec := json.NewDecoder(bytes.NewReader(Trim(doc)))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return false
	}
	for {
		t, err := dec.Token()
		if err != nil {
			return false
		}
		key, ok := t.(string)
		if !ok {
			return false // the end of the object
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return false
		}
		if !strings.EqualFold(key, "kind") {
			continue
		}
		var kind string
		if json.Unmarshal(value, &kind) == nil && strings.EqualFold(kind, want) {
			return true
		}
	}
}
