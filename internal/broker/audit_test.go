package broker

import (
	"net/http"
	"strings"
	"testing"
)

// Anything that reaches the broker's port can send a request without a
// token. Its audit line carries the method, the status and the check, and
// nothing the sender wrote into the path. A request with a valid token
// carries the path, cut to 256 bytes.
func TestAuditLineOfAnUnverifiedRequestCarriesNoPath(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	long := "/" + strings.Repeat("x", 4096)
	for _, path := range []string{"/brig-a/r/anthropic" + long, "/nobody/r/none" + long, long} {
		r.do("POST", path, nil, "")
		line := r.lastAudit()
		for _, absent := range []string{"path=", "sandbox=", "route=", "xxxx"} {
			if strings.Contains(line, absent) {
				t.Errorf("the audit line of an unverified request holds %q: %.200s", absent, line)
			}
		}
		wantAudit(t, line, "method=POST", "status=")
	}
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages"+long, bearer(r.mint("brig-a", "anthropic")), "{}")
	wantStatus(t, resp, http.StatusOK)
	line := r.lastAudit()
	wantAudit(t, line, "sandbox=brig-a", "route=anthropic", "path=/v1/messages/xxx")
	if len(line) > 1024 {
		t.Errorf("the audit line of a verified request is %d bytes, want the path cut to 256", len(line))
	}
}

// A sandbox id is a path segment and an audit field, so it is a plain name.
func TestSandboxIDsArePlainNames(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", "a%2f", "a b", "a\nb", "a\rb", strings.Repeat("a", 256), "é"} {
		if err := ValidID(bad); err == nil {
			t.Errorf("ValidID(%q) accepted it", bad)
		}
	}
	for _, good := range []string{"brig-claude-code", "brig-a.b_c", strings.Repeat("a", 255)} {
		if err := ValidID(good); err != nil {
			t.Errorf("ValidID(%q): %v", good, err)
		}
	}
}
