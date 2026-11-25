// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package nofireauth

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// maxServerText bounds what one server-supplied string can put on the
	// terminal.
	maxServerText = 200
	// maxMessage bounds a whole message that can hold server text, such as
	// an error brig login prints. Each server string in it is already cut to
	// maxServerText, so this cap is wider: it bounds the rest, such as a
	// long deployment URL, without cutting off the advice Brig puts after
	// the server's words.
	maxMessage = 1000
)

// CleanLine makes text from the server safe to print so it cannot forge
// Brig's output. It keeps only printable characters and the plain space. That
// drops C0 and C1 controls (U+009B is a one-byte CSI), format characters such
// as the bidi overrides that reorder what the user reads, U+2028 and U+2029
// line breaks, and U+FFFD, which invalid bytes decode to. It then cuts the
// result to 200 characters; the cut counts runes so it never splits a UTF-8
// sequence.
func CleanLine(text string) string { return clean(text, maxServerText) }

// CleanMessage is CleanLine for a whole message that may hold server or
// network text, such as an error: one line, cut to 1000 characters.
func CleanMessage(text string) string { return clean(text, maxMessage) }

func clean(text string, limit int) string {
	var b strings.Builder
	n := 0
	for _, r := range text {
		if r == utf8.RuneError || (r != ' ' && !unicode.IsPrint(r)) {
			continue
		}
		if n == limit {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// Clean returns text from server prefixed with the deployment URL, so the
// user can see the words are the server's and not Brig's.
func Clean(server Deployment, text string) string {
	return server.String() + " said: " + CleanLine(text)
}
