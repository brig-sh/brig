package redact

import (
	"strings"
	"testing"
)

// A report is attached to public issues, so a planted credential that comes
// back out is a leak. Each case names the value that must not appear and the
// kind its mask must carry.
func TestRedactMasksEachShape(t *testing.T) {
	mac := strings.Repeat("Ab0_-", 8) + "xyz" // 43 characters: an HMAC-SHA256 in raw base64url
	cases := []struct {
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
	}
	for _, c := range cases {
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
	} {
		if got := String(line); got != line {
			t.Errorf("String(%q) = %q, want it unchanged", line, got)
		}
	}
}
