package redact

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A report is attached to public issues, so a planted credential that comes
// back out is a leak. Each case names the value that must not appear and the
// kind its mask must carry.
func TestRedactMasksEachShape(t *testing.T) {
	for _, c := range shapes {
		got := String(c.line)
		for _, p := range c.planted {
			if strings.Contains(got, p) {
				t.Errorf("%s: %q still holds %q after masking: %q", c.kind, c.line, p, got)
			}
		}
		if want := "[masked: " + c.kind + "]"; !strings.Contains(got, want) {
			t.Errorf("%s: %q masked to %q, want it to carry %q", c.kind, c.line, got, want)
		}
	}
}

// mac is 43 characters: an HMAC-SHA256 in raw base64url.
var mac = strings.Repeat("Ab0_-", 8) + "xyz"

var shapes = []struct {
	kind    string
	line    string
	planted []string
}{
	{"anthropic-key", "key=sk-ant-api03-AbCdEf0123456789_xyz", []string{"sk-ant-api03-AbCdEf0123456789_xyz"}},
	{"github-token", "token ghp_0123456789abcdefABCDEF0123456789abcd", []string{"ghp_0123456789abcdefABCDEF0123456789abcd"}},
	{"github-token", "github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz", []string{"11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz"}},
	{"authorization", "Authorization: Bearer abc.def-ghi_jkl0123456789", []string{"abc.def-ghi_jkl0123456789"}},
	{"authorization", "curl -H 'Authorization: Basic dXNlcjpwYXNzd29yZDEyMw=='", []string{"dXNlcjpwYXNzd29yZDEyMw=="}},
	{"jwt", "id_token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.c2lnbmF0dXJlMDEyMw", []string{"eyJzdWIiOiIxMjM0NTY3ODkwIn0", "c2lnbmF0dXJlMDEyMw"}},
	{"brig-token", "token=eyJzYW5kYm94IjoiYnJpZy1jbGF1ZGUifQ." + mac, []string{"eyJzYW5kYm94IjoiYnJpZy1jbGF1ZGUifQ", mac}},
	{"json-secret", `{"accessToken":"opaque-value-1","refreshToken":"opaque-value-2"}`, []string{"opaque-value-1", "opaque-value-2"}},
	{"env-secret", "GH_TOKEN=opaquevalue123 ANTHROPIC_API_KEY=other456", []string{"opaquevalue123", "other456"}},
	{"aws-key-id", "aws_access_key_id AKIAABCDEFGHIJKLMNOP", []string{"AKIAABCDEFGHIJKLMNOP"}},
	{"env-secret", `GH_TOKEN="opaquevalue123"`, []string{"opaquevalue123"}},
	{"env-secret", `export MY_API_KEY='opaque value 456'`, []string{"opaque value 456", "value 456"}},
	{"json-secret", `{"token":"[abc]"}`, []string{"[abc]"}},
	{"json-secret", `{"password":"start\"weird"}`, []string{"start", "weird"}},
	{"authorization", "Authorization:\tBearer abc.def-ghi_jkl0123456789", []string{"abc.def-ghi_jkl0123456789"}},
	// A line cut at a buffer boundary, or a value that goes on to the next
	// line, leaves the quote open.
	{"env-secret", `API_SECRET="abcdef0123456789`, []string{"abcdef0123456789"}},
	{"env-secret", "API_SECRET='abcdef0123456789\nnext", []string{"abcdef0123456789"}},
	{"json-secret", `{"refreshToken":"opaque-value-cut`, []string{"opaque-value-cut"}},
}

// The names stay so a reader can tell which variable or field held a value.
func TestRedactKeepsTheName(t *testing.T) {
	got := String(`GH_TOKEN=opaquevalue123 {"refreshToken":"opaque"}`)
	for _, name := range []string{"GH_TOKEN=", `"refreshToken":`} {
		if !strings.Contains(got, name) {
			t.Errorf("masking dropped %q: %q", name, got)
		}
	}
}

// A report that masks ordinary text is unreadable, and a reader stops
// trusting the masks. Each line comes back unchanged.
func TestRedactKeepsOrdinaryText(t *testing.T) {
	for _, line := range []string{
		"docs(broker): Document the Basic challenge on a refused token",
		"refs: [env.GH_TOKEN, secrets.gh-token]",
		"sha256:de5ba1cecf90b306c7b3576194fce8c421cbbfc26556a53752e7992d76b4170d",
		"742b49f8-21b6-4f6b-8e3c-582486c5c544",
		"virtio-net: gvisor-tap gateway relay via /Users/stelioschar/.brig/sandbox-brig-claude-code.sock.qemu",
		"[17538.451910] Out of memory: Killed process 21272 (node) total-vm:13653136kB",
		"registry-1.docker.io api.anthropic.com",
		"Authorization: none",
		`{"expiresAt":1793967384,"scopes":["user:inference"]}`,
		"MEM=4096 CPUS=4 BRIG_VERIFY=warn",
		"the basic responsibilities of the broker",
		// A header printed with no value: the next line is not its value.
		"Authorization:\nsandbox-brig-claude-code.sock",
		"retrying with Bearer\nsandbox-brig-claude-code.sock",
	} {
		if got := String(line); got != line {
			t.Errorf("String(%q) = %q, want it unchanged", line, got)
		}
	}
}

// Masking twice changes nothing, and a report of many lines masks each line as
// it would alone. A rule that re-masked a mask, or ran across lines, fails
// one of the two.
func TestRedactIsIdempotentAcrossLines(t *testing.T) {
	var all, want []string
	for _, c := range shapes {
		once := String(c.line)
		if twice := String(once); twice != once {
			t.Errorf("%s: masking %q twice gave %q, once gave %q", c.kind, c.line, twice, once)
		}
		all = append(all, c.line)
		want = append(want, once)
	}
	if got := String(strings.Join(all, "\n")); got != strings.Join(want, "\n") {
		t.Errorf("masking the lines together differs from masking each:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// The character that ends a broker token comes back whole, even when it is
// more than one byte.
func TestRedactGivesBackTheCharacterAfterAToken(t *testing.T) {
	got := String("token=eyJzYW5kYm94IjoiYSJ9." + mac + "état")
	if want := "token=[masked: brig-token]état"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if !utf8.ValidString(got) {
		t.Errorf("masking left invalid UTF-8: %q", got)
	}
}
