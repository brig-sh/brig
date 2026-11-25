package wrap

import (
	"strings"
	"testing"
)

// A run that no service adds an entry to writes no MCP configuration into
// the guest, and the agent is pointed at none.
func TestMCPConfigIsNotWrittenWithoutEntries(t *testing.T) {
	g := &caGuest{files: map[string]string{}}
	c := &Config{Runtime: g, VMName: "brig-a"}
	if err := c.deliverMCPConfig(); err != nil {
		t.Fatal(err)
	}
	if len(g.argv)+len(g.feeds) != 0 {
		t.Errorf("a run with no entry ran %q and fed %d", g.argv, len(g.feeds))
	}
	if got := c.MCPConfig(); got != "" {
		t.Errorf("MCPConfig() = %q, want none", got)
	}
}

// The configuration names the token by its variable, which the agent expands
// from its own environment, so the file holds no secret. It goes in on stdin,
// never on a command line.
func TestMCPConfigHoldsTheVariableAndNoToken(t *testing.T) {
	const token = "echo-session-token-value"
	t.Setenv("BRIG_ECHO_TOKEN", token)
	g := &caGuest{files: map[string]string{}}
	c := &Config{Runtime: g, VMName: "brig-a"}
	if err := c.addMCPEntry(MCPEntry{Name: "echo", URL: "https://brig.internal/brig-a/echo", TokenVar: "BRIG_ECHO_TOKEN"}); err != nil {
		t.Fatal(err)
	}
	if err := c.deliverMCPConfig(); err != nil {
		t.Fatal(err)
	}
	want := `{"mcpServers":{"echo":{"type":"http","url":"https://brig.internal/brig-a/echo",` +
		`"headers":{"Authorization":"Bearer ${BRIG_ECHO_TOKEN}"}}}}`
	got, ok := g.files[GuestMCPConfigPath]
	if !ok {
		t.Fatalf("%s was not written: %v", GuestMCPConfigPath, g.files)
	}
	if got != want {
		t.Errorf("%s holds\n%s\nwant\n%s", GuestMCPConfigPath, got, want)
	}
	if strings.Contains(got, token) {
		t.Error("the token value reached the guest file")
	}
	if len(g.files) != 1 {
		t.Errorf("delivery wrote %v", g.files)
	}
	for _, a := range g.argv {
		if strings.Contains(a, "mcpServers") {
			t.Errorf("the configuration was on a command line: %q", a)
		}
	}
	if got := c.MCPConfig(); got != GuestMCPConfigPath {
		t.Errorf("MCPConfig() = %q, want %s", got, GuestMCPConfigPath)
	}
}

// Two services under one name would leave the agent with the one written
// last; the second is refused instead.
func TestMCPConfigRefusesTwoEntriesWithOneName(t *testing.T) {
	c := &Config{}
	if err := c.addMCPEntry(MCPEntry{Name: "echo", URL: "https://brig.internal/brig-a/echo", TokenVar: "BRIG_ECHO_TOKEN"}); err != nil {
		t.Fatal(err)
	}
	err := c.addMCPEntry(MCPEntry{Name: "echo", URL: "https://brig.internal/brig-a/other", TokenVar: "BRIG_OTHER_TOKEN"})
	if err == nil || err.Error() != "two MCP servers are named echo" {
		t.Fatalf("addMCPEntry = %v", err)
	}
	if len(c.mcpEntries) != 1 || c.mcpEntries[0].URL != "https://brig.internal/brig-a/echo" {
		t.Errorf("entries = %+v, want the first alone", c.mcpEntries)
	}
}
