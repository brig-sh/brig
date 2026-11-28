package controlroom

import (
	"regexp"
	"unicode/utf8"
)

// redactions are the secret patterns the introspection collector applies
// before a capture leaves a node, in its order. The Reporter applies the same
// ones, so the ledger and the SIEM get the same text.
var redactions = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----(?s:.*?)-----END [A-Z ]*PRIVATE KEY-----`), "[REDACTED:private-key]"},
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`), "[REDACTED:jwt]"},
	{regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{16,}`), "Bearer [REDACTED]"},
	{regexp.MustCompile(`\b(AKIA|ASIA)[A-Z0-9]{16}\b`), "[REDACTED:aws-key-id]"},
	{regexp.MustCompile(`(?i)(aws_secret_access_key|aws_session_token|secretaccesskey|sessiontoken|x-amz-security-token|x-amz-signature)(\\?["']?\s*[:=]\s*\\?["']?)[A-Za-z0-9/+=%]{16,}`), "${1}${2}[REDACTED]"},
	{regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})`), "[REDACTED:github-token]"},
	{regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`), "[REDACTED:slack-token]"},
	{regexp.MustCompile(`\bsk-(ant-|proj-)?[A-Za-z0-9_-]{20,}`), "[REDACTED:api-key]"},
	{regexp.MustCompile(`(?i)(password|passwd|api[_-]?key|client[_-]?secret|secret[_-]?key|private[_-]?key|access[_-]?token|refresh[_-]?token|auth[_-]?token)(\\?["']?\s*[:=]\s*\\?["']?)[^\s"'\\,}&]{8,}`), "${1}${2}[REDACTED]"},
}

// Redact returns s with every secret the collector's patterns match replaced.
func Redact(s string) string {
	for _, r := range redactions {
		s = r.re.ReplaceAllString(s, r.with)
	}
	return s
}

// capBytes returns s cut to at most n bytes on a rune boundary, and whether it
// was cut.
func capBytes(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}
