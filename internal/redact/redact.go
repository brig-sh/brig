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

// maskUnit matches one mask an earlier rule wrote. The value classes of the
// rules that keep a name take it as one unit, so a value that holds a mask
// and more, such as AKIA…:secret after the key id is masked, is masked
// whole rather than stopping at the mask's space.
const maskUnit = `\[masked: [a-z-]+\]`

// isMask reports whether s is exactly one mask. Only such a value is left
// alone, so masking twice changes nothing while a crafted value that merely
// starts like a mask is still masked.
var isMask = regexp.MustCompile(`^` + maskUnit + `$`).MatchString

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
	// quoted writes the mask inside the quote the value opened with, so
	// JSON, escaped or not, stays JSON.
	quoted bool
}

// Value classes of the rules that keep a name.
const (
	// headerValue is the character set of an HTTP credential.
	headerValue = `(?:` + maskUnit + `|[A-Za-z0-9._~+/=-])`
	// jsonValue ends at the first quote that is not escaped, or at the end
	// of a line that was cut before its closing quote.
	jsonValue = `"(?:[^"\\\n]|\\.)+"?`
	// escapedJSONValue is a JSON string inside another string, as a logged
	// request body prints it: its quotes are \".
	escapedJSONValue = `\\"(?:[^"\\\n]|\\[^"\n])+(?:\\")?`
	// envValue may be quoted, as in a .env file or a shell line. A double
	// quote can be escaped inside it, and a quote left open runs to the end
	// of the line, so a cut line still masks.
	envValue = `"(?:[^"\\\n]|\\.)+"?|'[^'\n]+'?|(?:` + maskUnit + `|[^\s'"])+`
	// secretName ends the way the name of a credential does, in any case:
	// GH_TOKEN, PASSWORD, aws_secret_access_key, SecretAccessKey.
	secretName = `(?i:[a-z0-9_]*(?:token|key|secret|password|passwd))`
	// jsonSecretName is narrower: a JSON document names many keys, and a
	// "key" alone is usually not a credential.
	jsonSecretName = `(?i:[a-z0-9_]*(?:token|secret|password|passwd|api_?key|access_?key|private_?key))`
)

// The order matters. The whole-token shapes run first, so a JWT in an
// Authorization header is reported as a JWT, and the rules that keep a name
// run last. A value they find that is or holds an earlier mask is masked
// whole, unless it is exactly one mask.
//
// Separators are spaces and tabs, never a newline: a header printed with no
// value must not take the start of the next line as its value.
var rules = []rule{
	{kind: "jwt", re: regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	// Brig's broker token: base64url(JSON claims) "." base64url(HMAC-SHA256),
	// and the signature is always 43 characters. The second group stops the
	// match from ending inside a longer word; a period after it, as at the
	// end of a sentence, is given back.
	{kind: "brig-token", re: regexp.MustCompile(`(eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{43})([^A-Za-z0-9_-]|$)`)},
	{kind: "anthropic-key", re: regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{10,}`)},
	{kind: "github-token", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`)},
	{kind: "aws-key-id", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)},
	// After an explicit scheme any value is a credential: Basic of a short
	// user:pass is 12 characters. Without one a credential is 16 characters
	// or more, so a header that says "Authorization: none" survives. The
	// optional [ is Go's %v of a header map: map[Authorization:[Bearer …]].
	{kind: "authorization", re: regexp.MustCompile(`(?i)(authorization:[ \t]*\[?(?:bearer|basic|token)[ \t]+)(` + headerValue + `+)`), keep: 1},
	{kind: "authorization", re: regexp.MustCompile(`(?i)(authorization:[ \t]*\[?)(` + headerValue + `{16,})`), keep: 1},
	// Prose says "the Basic challenge"; the floor and skipWords keep it.
	{kind: "authorization", re: regexp.MustCompile(`(?i)(\b(?:bearer|basic)[ \t]+)(` + headerValue + `{16,})`), keep: 1, skipWords: true},
	{kind: "json-secret", re: regexp.MustCompile(`("` + jsonSecretName + `"[ \t]*:[ \t]*)(` + jsonValue + `)`), keep: 1, quoted: true},
	{kind: "json-secret", re: regexp.MustCompile(`(\\"` + jsonSecretName + `\\"[ \t]*:[ \t]*)(` + escapedJSONValue + `)`), keep: 1, quoted: true},
	{kind: "env-secret", re: regexp.MustCompile(`\b(` + secretName + `[ \t]*=[ \t]*)(` + envValue + `)`), keep: 1},
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
	inner := strings.Trim(value, `\"'`)
	if isMask(inner) || r.skipWords && onlyLetters(inner) {
		return m
	}
	var b strings.Builder
	for _, s := range sub[1 : r.keep+1] {
		b.WriteString(s)
	}
	switch {
	case r.quoted && strings.HasPrefix(value, `\"`):
		b.WriteString(`\"` + mask(r.kind) + `\"`)
	case r.quoted:
		b.WriteString(`"` + mask(r.kind) + `"`)
	default:
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
