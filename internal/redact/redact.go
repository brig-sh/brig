// Package redact masks credentials in text by their shape.
//
// It exists for text that leaves the host: a diagnostic report a user
// attaches to a public issue. It knows no stored value and reads no secret
// store. Masking the exact values Brig holds would also catch a token of a
// shape listed nowhere here, but it would give the caller a path to every
// credential on the host, and a diagnostic command is the last place that
// path belongs. A token of an unknown shape therefore passes through, and the
// caller tells the reader to check the text before sharing it.
//
// internal/broker/redact.go is a different thing: it removes the exact values
// a route injected from responses on their way to the guest.
package redact

import (
	"regexp"
	"strings"
)

// mask is the text that replaces a value of the given kind.
func mask(kind string) string { return "[masked: " + kind + "]" }

// maskedPrefix starts a value an earlier rule already replaced. The rules
// that keep a name run after it is written and accept almost any value, so
// they check for it rather than mask a mask.
const maskedPrefix = "[masked:"

// A rule matches a whole credential, or a name followed by its value. In the
// second form the leading keep submatches are the name, the next one is the
// value, and any after that is text the match had to take to find the value's
// end, given back unchanged.
type rule struct {
	kind string
	re   *regexp.Regexp
	keep int
	// skipWords leaves a value made only of letters alone. Prose such as
	// "basic responsibilities" puts a long word after "basic"; a credential
	// has a digit or a symbol in it.
	skipWords bool
}

// The order matters. The whole-token shapes run first, so a JWT in an
// Authorization header is reported as a JWT, and the rules that keep a name
// run last, after every value they could swallow is already masked.
//
// Separators are spaces and tabs, never a newline: a header printed with no
// value must not take the start of the next line as its value.
var rules = []rule{
	{kind: "jwt", re: regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	// Brig's broker token: base64url(JSON claims) "." base64url(HMAC-SHA256),
	// and the signature is always 43 characters. The second group stops the
	// match from ending inside a longer dotted word.
	{kind: "brig-token", re: regexp.MustCompile(`(eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{43})([^A-Za-z0-9_.-]|$)`)},
	{kind: "anthropic-key", re: regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{10,}`)},
	{kind: "github-token", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`)},
	{kind: "aws-key-id", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)},
	// A credential is 16 characters or more, so prose such as "the Basic
	// challenge" and a header that says "Authorization: none" survive.
	{kind: "authorization", re: regexp.MustCompile(`(?i)(authorization:[ \t]*(?:(?:bearer|basic|token)[ \t]+)?)([A-Za-z0-9._~+/=-]{16,})`), keep: 1},
	{kind: "authorization", re: regexp.MustCompile(`(?i)(\b(?:bearer|basic)[ \t]+)([A-Za-z0-9._~+/=-]{16,})`), keep: 1, skipWords: true},
	// A JSON string value ends at the first quote that is not escaped.
	{kind: "json-secret", re: regexp.MustCompile(`("(?:accessToken|refreshToken|idToken|access_token|refresh_token|id_token|client_secret|apiKey|api_key|password|secret|token)"[ \t]*:[ \t]*)("(?:[^"\\\n]|\\.)+")`), keep: 1},
	// A value may be quoted, as in a .env file or a shell line.
	{kind: "env-secret", re: regexp.MustCompile(`\b([A-Z][A-Z0-9_]*(?:TOKEN|KEY|SECRET|PASSWORD|PASSWD)=)("[^"\n]+"|'[^'\n]+'|[^\s'"]+)`), keep: 1},
}

// String returns s with each credential-shaped value replaced by
// "[masked: <kind>]".
func String(s string) string {
	for _, r := range rules {
		s = r.re.ReplaceAllStringFunc(s, func(m string) string {
			return r.replace(m)
		})
	}
	return s
}

func (r rule) replace(m string) string {
	sub := r.re.FindStringSubmatch(m)
	value, rest := sub[0], []string(nil)
	if len(sub) > 1 {
		value, rest = sub[r.keep+1], sub[r.keep+2:]
	}
	inner := strings.Trim(value, `"'`)
	if strings.HasPrefix(inner, maskedPrefix) || r.skipWords && onlyLetters(inner) {
		return m
	}
	var b strings.Builder
	for _, s := range sub[1 : r.keep+1] {
		b.WriteString(s)
	}
	if r.kind == "json-secret" {
		b.WriteString(`"` + mask(r.kind) + `"`)
	} else {
		b.WriteString(mask(r.kind))
	}
	for _, s := range rest {
		b.WriteString(s)
	}
	return b.String()
}

func onlyLetters(s string) bool {
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
			return false
		}
	}
	return true
}
