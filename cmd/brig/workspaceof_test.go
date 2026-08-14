package main

import "testing"

// An unnamed sandbox is named after its template alone, and reporting the
// workspace of a session that happens to be called after the template is a
// quiet lie: `brig ls` sent people to ~/brig/claude-code-claude-cod, which is
// not where the running sandbox lives.
func TestWorkspaceOfUnnamedSandbox(t *testing.T) {
	for _, c := range []struct{ vm, want string }{
		{"brig-claude-code", "/claude-code"},
		{"brig-claude-code-skilltest", "/claude-code-skilltest"},
		{"brig-ubuntu", "/ubuntu"},
	} {
		got := workspaceOf(c.vm, nil)
		if got == "" {
			t.Fatalf("%s: no workspace resolved", c.vm)
		}
		if len(got) < len(c.want) || got[len(got)-len(c.want):] != c.want {
			t.Errorf("%s -> %s, want it to end in %s", c.vm, got, c.want)
		}
	}
}
