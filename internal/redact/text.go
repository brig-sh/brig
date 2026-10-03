package redact

import (
	"regexp"
	"sort"
	"strings"
)

// LeakError is a value Check found. It carries the kind, never the value.
type LeakError struct{ Kind string }

func (e *LeakError) Error() string { return "still contains a " + e.Kind + " value" }

// tokenPattern is a credential shape.
//   - values: submatches that hold the secret. A value of only placeholders
//     was written by the scrub; Text skips it and Check passes it.
//   - tails: submatches for text glued after a closing quote. A match with
//     a non-empty tail is never scrub output: "<VAR-1>"secret.
//   - bare: submatch of an unquoted value, which ends before a trailing run of
//     placeholders (PASSWORD=ab<VAR-1> redacts "ab"). 0 means none.
type tokenPattern struct {
	kind   string
	re     *regexp.Regexp
	repl   string
	values []int
	tails  []int
	bare   int
}

// Value syntax, read as a shell would. A quoted value runs to its closing
// quote, skipping backslash escapes, or to the end of the line if unclosed.
// Text glued after the closing quote ("abc"def) is part of the value, up to a
// space or a separator. A glued or unquoted part may open another quoted
// segment, and a space inside that segment does not end the value.
const (
	// quoted ends at its quote or at the end of the line, never earlier;
	// otherwise the closing quote in abc"b,c", x could open a new segment.
	quoted  = `"(?:[^"\\\n]|\\.)*(?:"|\\?(?m:$))|'(?:[^'\\\n]|\\.)*(?:'|\\?(?m:$))`
	dqValue = `"(?:[^"\\\n]|\\.)*"?` + glued
	sqValue = `'(?:[^'\\\n]|\\.)*'?` + glued
	glued   = `(?:[^\s"',;}\]]|` + quoted + `)*`
	// unquoted is the body of a value that does not open with a quote.
	unquoted = `(?:[^\s"']|` + quoted + `)`
	// unquotedEnd is the last character of an unquoted value. It excludes
	// separators, so "abc"; scrubs to <redacted:cli-flag>; and a second pass
	// reads the placeholder alone.
	unquotedEnd = `(?:[^\s"',;}\]]|` + quoted + `)`
)

// tokenPatterns are credential shapes, most specific first (sk-ant- before
// sk-, cli-flag before assignment). A replacement is not matched again: fixed
// shapes exclude '<', scrubbed skips placeholder-only values, and end keeps a
// trailing run of placeholders.
var tokenPatterns = []tokenPattern{
	// BEGIN to END, or to the end of input if truncated. Dashes are optional
	// so Check also refuses a bare BEGIN ... PRIVATE KEY marker.
	{kind: "private-key", re: regexp.MustCompile(`(?s)(?:-----)?BEGIN [A-Z0-9 ]*PRIVATE KEY(?:.*?END [A-Z0-9 ]*PRIVATE KEY-*|.*)`), repl: "<redacted:private-key>"},
	{kind: "anthropic", re: regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{10,}`), repl: "<redacted:anthropic>"},
	{kind: "openai", re: regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`), repl: "<redacted:openai>"},
	{kind: "github", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`), repl: "<redacted:github>"},
	{kind: "aws", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`), repl: "<redacted:aws>"},
	{kind: "slack", re: regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), repl: "<redacted:slack>"},
	{kind: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), repl: "<redacted:jwt>"},
	// Any HTTP auth scheme; Basic is base64 user:password. The credential runs
	// to the next space, or to a quote followed by a separator or the end of
	// the JSON string.
	{kind: "auth", re: regexp.MustCompile(`(?i)\b(bearer|basic|token|digest)\s+[A-Za-z0-9._~+/=-]{8,}(?:[^\s"]|"[^\s,}\]])*`), repl: "$1 <redacted:auth>"},
	// The password runs to the last '@' before the path and may hold '@' or
	// '<'. The user part excludes '<', so the replacement is not matched
	// again.
	{kind: "url-credentials", re: regexp.MustCompile(`://[^/\s:@<]+:[^/\s]+@`), repl: "://<redacted:url-credentials>@"},
	// --token value or --password=value, quoted or not. After a space, a
	// leading '-' is the next flag.
	{kind: "cli-flag", re: regexp.MustCompile(`(?i)(--[a-z0-9-]*(?:token|secret|password|passwd|api-?key|auth)[a-z0-9-]*)(=)(` + dqValue + `|` + sqValue + `|` + unquoted + `*` + unquotedEnd + `)`), repl: "${1}${2}<redacted:cli-flag>", values: []int{3}, bare: 3},
	{kind: "cli-flag", re: regexp.MustCompile(`(?i)(--[a-z0-9-]*(?:token|secret|password|passwd|api-?key|auth)[a-z0-9-]*)(\s+)(` + dqValue + `|` + sqValue + `|[^\s"'-](?:` + unquoted + `*` + unquotedEnd + `)?)`), repl: "${1}${2}<redacted:cli-flag>", values: []int{3}, bare: 3},
	// KEY=value, x-api-key: value, and JSON "token": "value". A quoted value
	// keeps its quotes. An unquoted value runs to the next space outside a
	// quoted segment and leaves a trailing separator, so "A=1, B=2" splits.
	// It does not start with a bracket: "forward_token_vars": [...] is kept.
	{kind: "assignment", re: regexp.MustCompile(`(?i)\b([A-Z0-9_-]*(?:TOKEN|SECRET|PASSWORD|PASSWD|API[_-]?KEY|ACCESS[_-]?KEY|PRIVATE[_-]?KEY|AUTH)[A-Z0-9_-]*)(["']?\s*[=:]\s*)(?:(")((?:[^"\\\n]|\\.)*)("?)(` + glued + `)|(')((?:[^'\\\n]|\\.)*)('?)(` + glued + `)|([^\s"'\[\]{},;](?:` + unquoted + `*` + unquotedEnd + `)?))`), repl: "${1}${2}${3}${7}<redacted:assignment>${5}${9}", values: []int{4, 8, 11}, tails: []int{6, 10}, bare: 11},
}

// placeholderKinds are the counted kinds. Keep in step with the Kind
// constants; TestPlaceholderShapeCoversEveryKind fails on a missing one.
var placeholderKinds = []Kind{
	KindUser, KindHost, KindWorkspace, KindPath, KindProfile, KindLabel, KindSandbox,
	KindPolicy, KindVar, KindRegistry, KindImage, KindTag, KindDomain, KindIP, KindEmail,
}

// placeholderSrc matches one placeholder this package writes: <user>,
// <host>, <mac>, <kind-N>, or <redacted:kind> for a known token kind. Other
// angle-bracketed text is ordinary text.
var placeholderSrc = func() string {
	ks := make([]string, len(placeholderKinds))
	for i, k := range placeholderKinds {
		ks[i] = string(k)
	}
	seen := map[string]bool{}
	var tk []string
	for _, p := range tokenPatterns {
		if !seen[p.kind] {
			seen[p.kind] = true
			tk = append(tk, p.kind)
		}
	}
	return `<(?:user|host|mac|(?:` + strings.Join(ks, "|") + `)-[0-9]+|redacted:(?:` + strings.Join(tk, "|") + `))>`
}()

var (
	placeholderRE      = regexp.MustCompile(`^` + placeholderSrc + `$`)
	placeholdersOnlyRE = regexp.MustCompile(`^(?:` + placeholderSrc + `)+$`)
	// placeholderTailRE finds where a trailing run of placeholders starts,
	// in one linear scan.
	placeholderTailRE = regexp.MustCompile(`(?:` + placeholderSrc + `)+$`)
)

// maxPlaceholder bounds how far insidePlaceholder looks for brackets. No
// placeholder is longer.
const maxPlaceholder = 40

// scrubbed reports whether match m of p is scrub output or an empty value,
// and has no glued tail.
func (p tokenPattern) scrubbed(s string, m []int) bool {
	for _, t := range p.tails {
		if m[2*t] < m[2*t+1] {
			return false
		}
	}
	for _, v := range p.values {
		if m[2*v] >= 0 {
			return m[2*v] == m[2*v+1] || placeholdersOnlyRE.MatchString(s[m[2*v]:m[2*v+1]])
		}
	}
	return false
}

// end is where match m of p stops being secret: the start of a trailing run
// of placeholders in an unquoted value, else the match's end. ab<cd and
// ab<VAR-1>cd are redacted whole. One scan, so the cost is linear.
func (p tokenPattern) end(s string, m []int) int {
	if p.bare == 0 || m[2*p.bare] < 0 {
		return m[1]
	}
	lo, hi := m[2*p.bare], m[2*p.bare+1]
	if s[lo] == '"' || s[lo] == '\'' {
		return m[1]
	}
	if loc := placeholderTailRE.FindStringIndex(s[lo+1 : hi]); loc != nil {
		return lo + 1 + loc[0]
	}
	return m[1]
}

// replace applies p to s, leaving matches the scrub already wrote.
func (p tokenPattern) replace(s string) (string, bool) {
	var b strings.Builder
	changed, last := false, 0
	for _, m := range p.re.FindAllStringSubmatchIndex(s, -1) {
		if p.scrubbed(s, m) {
			continue
		}
		b.WriteString(s[last:m[0]])
		b.Write(p.re.ExpandString(nil, p.repl, s, m))
		last, changed = p.end(s, m), true
	}
	if !changed {
		return s, false
	}
	b.WriteString(s[last:])
	return b.String(), true
}

// found reports whether s holds a match of p the scrub did not write.
func (p tokenPattern) found(s string) bool {
	for _, m := range p.re.FindAllStringSubmatchIndex(s, -1) {
		if !p.scrubbed(s, m) {
			return true
		}
	}
	return false
}

var (
	emailRE = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	macRE   = regexp.MustCompile(`\b[0-9A-Fa-f]{2}(?::[0-9A-Fa-f]{2}){5}\b`)
	ipv4RE  = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`)
	// Candidates only; netip.ParseAddr decides. Submatches 1 and 3 are the
	// boundaries either side, kept as they are.
	//
	// Left: anything but a letter or digit, so eth0:fe80::1 is found and the
	// e::f in core::fmt is not. In x2001:db8::1 the candidate is db8::1.
	//
	// Right: not a letter, digit or colon, so fe80::1zz is not a candidate
	// and a::b::c is not retried as a::b.
	ipv6RE = regexp.MustCompile(`(?i)(^|[^0-9a-z])([0-9a-f]{0,4}(?::[0-9a-f]{0,4}){2,7})($|[^0-9a-z:])`)
)

// Text scrubs free text in this order: token shapes, emails, registered
// values (longest first), MAC and IP addresses. Each pass must run before the
// next rewrites the text it matches: ACME_TOKEN=value is found by its key
// before the key becomes <VAR-1>, and zz@acme.io is replaced whole before acme
// is. Best-effort: an unlisted credential shape survives.
func (r *Redactor) Text(s string) Clean {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range tokenPatterns {
		if out, changed := p.replace(s); changed {
			s = out
			r.applied["token:"+p.kind] = true
		}
	}
	// Keyed lower-cased: ZZ@ACME.IO and zz@acme.io are one address.
	s = emailRE.ReplaceAllStringFunc(s, func(m string) string { return r.placeholder(KindEmail, asciiLower(m)) })
	for _, v := range r.searchOrder() {
		o := r.originals[v]
		if out, changed := replaceWord(s, v, o.placeholder, foldCase(o.kind)); changed {
			s = out
			r.applied[string(o.kind)] = true
		}
	}
	if macRE.MatchString(s) {
		s = macRE.ReplaceAllString(s, "<mac>")
		r.applied["mac"] = true
	}
	s = ipv4RE.ReplaceAllStringFunc(s, r.ip)
	s = r.ipv6(s)
	return Clean{s}
}

// Check reports the first registered value, token shape, email or MAC address
// still in b. An unregistered IP address is not refused, since Text keeps
// some. Run Text first: raw text can fail on shapes Text rewrites harmlessly,
// such as "secrets: keychain reachable".
func (r *Redactor) Check(b []byte) error {
	s := string(b)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range r.searchOrder() {
		if containsWord(s, v, foldCase(r.originals[v].kind)) {
			return &LeakError{Kind: string(r.originals[v].kind)}
		}
	}
	for _, p := range tokenPatterns {
		if p.found(s) {
			return &LeakError{Kind: p.kind}
		}
	}
	// No placeholder matches either shape.
	if emailRE.MatchString(s) {
		return &LeakError{Kind: string(KindEmail)}
	}
	if macRE.MatchString(s) {
		return &LeakError{Kind: "mac"}
	}
	return nil
}

// searchOrder is the registered values to search for, longest first. Caller
// holds mu.
func (r *Redactor) searchOrder() []string {
	out := make([]string, 0, len(r.originals))
	for v, o := range r.originals {
		if o.always || len(v) >= minTextLen {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// ipv6 replaces IPv6 candidates in s. A candidate that fails to parse is
// retried with colons at either end trimmed ("fe80::1: timeout"). Caller holds
// mu.
func (r *Redactor) ipv6(s string) string {
	var b strings.Builder
	last := 0
	// Resume at the character after the candidate, not after the match: it
	// may be the left side of the next one (2001:db8::1,2001:db8::2).
	for pos := 0; pos < len(s); {
		m := ipv6RE.FindStringSubmatchIndex(s[pos:])
		if m == nil {
			break
		}
		for i := range m {
			if m[i] >= 0 {
				m[i] += pos
			}
		}
		pos = m[5]
		c := s[m[4]:m[5]]
		if strings.Count(c, ":") < 2 {
			continue
		}
		out := r.ip(c)
		if out == c {
			lead := len(c) - len(strings.TrimLeft(c, ":"))
			core := strings.TrimRight(c[lead:], ":")
			if strings.Count(core, ":") >= 2 {
				out = c[:lead] + r.ip(core) + c[lead+len(core):]
			}
		}
		if out == c {
			continue
		}
		b.WriteString(s[last:m[4]])
		b.WriteString(out)
		last = m[5]
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// foldCase reports whether kind k matches case-insensitively: host names,
// domains and emails.
func foldCase(k Kind) bool {
	return k == KindHost || k == KindDomain || k == KindEmail
}

// asciiLower lowers ASCII letters only, so byte offsets are preserved.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// replaceWord replaces each occurrence of old that stands as a whole word,
// ignoring ASCII case when fold is set.
func replaceWord(s, old, repl string, fold bool) (string, bool) {
	hay, needle := s, old
	if fold {
		hay, needle = asciiLower(s), asciiLower(old)
	}
	var b strings.Builder
	changed, start := false, 0
	for {
		i := strings.Index(hay[start:], needle)
		if i < 0 {
			break
		}
		i += start
		j := i + len(needle)
		if bounded(s, i, j) {
			b.WriteString(s[start:i])
			b.WriteString(repl)
			changed = true
		} else {
			b.WriteString(s[start:j])
		}
		start = j
	}
	if !changed {
		return s, false
	}
	b.WriteString(s[start:])
	return b.String(), true
}

func containsWord(s, old string, fold bool) bool {
	hay, needle := s, old
	if fold {
		hay, needle = asciiLower(s), asciiLower(old)
	}
	for start := 0; ; {
		i := strings.Index(hay[start:], needle)
		if i < 0 {
			return false
		}
		i += start
		if bounded(s, i, i+len(needle)) {
			return true
		}
		start = i + len(needle)
	}
}

// bounded reports whether s[i:j] has no word character either side and is
// not inside a placeholder.
func bounded(s string, i, j int) bool {
	if i > 0 && isWord(s[i-1]) || j < len(s) && isWord(s[j]) {
		return false
	}
	return !insidePlaceholder(s, i, j)
}

// insidePlaceholder reports whether the nearest '<' before s[i:j] and '>'
// after it enclose text placeholderRE accepts.
func insidePlaceholder(s string, i, j int) bool {
	lt := strings.LastIndexByte(s[max(0, i-maxPlaceholder):i], '<')
	if lt < 0 {
		return false
	}
	lt += max(0, i-maxPlaceholder)
	gt := strings.IndexByte(s[j:min(len(s), j+maxPlaceholder)], '>')
	if gt < 0 {
		return false
	}
	gt += j
	return placeholderRE.MatchString(s[lt : gt+1])
}

func isWord(c byte) bool {
	return c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}
