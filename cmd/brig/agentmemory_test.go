package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/wrap"
)

// --agent-memory alone is the project's store, and it takes no value from
// the next word: in `brig run --agent-memory claude dir` the ref is claude.
func TestAgentMemoryFlagBoolForm(t *testing.T) {
	o, ref, tail, err := parse("run", []string{"--agent-memory", "claude", "dir"})
	if err != nil {
		t.Fatal(err)
	}
	if ref != "claude" || o.load.Project != "dir" || len(tail) != 0 {
		t.Errorf("ref = %q, project = %q, tail = %q", ref, o.load.Project, tail)
	}
	if o.load.Memory != "project" || o.load.NoMemory {
		t.Errorf("memory = %q, no-memory = %v; want project", o.load.Memory, o.load.NoMemory)
	}
}

func TestAgentMemoryFlagNamed(t *testing.T) {
	o, ref, _, err := parse("run", []string{"--agent-memory=go-services", "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if ref != "claude" || o.load.Memory != "go-services" {
		t.Errorf("ref = %q, memory = %q", ref, o.load.Memory)
	}
	// Off unless asked for.
	if o, _, _, _ = parse("run", []string{"claude"}); o.load.Memory != "" || o.load.NoMemory {
		t.Errorf("memory defaulted to %q (no-memory %v)", o.load.Memory, o.load.NoMemory)
	}
	// After the agent's arguments start it is the agent's word, not brig's.
	if o, _, _, _ = parse("run", []string{"claude", "-p", "hi", "--agent-memory"}); o.load.Memory != "" {
		t.Error("--agent-memory was read out of the agent's own arguments")
	}
}

func TestNoAgentMemoryFlag(t *testing.T) {
	o, _, _, err := parse("run", []string{"--agent-memory=team", "--no-agent-memory", "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.load.NoMemory {
		t.Error("--no-agent-memory did not set it")
	}
	// A boolean form written with false means off, as --skills=false does.
	if o, _, _, err = parse("run", []string{"--agent-memory=false", "claude"}); err != nil || !o.load.NoMemory || o.load.Memory != "" {
		t.Errorf("--agent-memory=false: memory %q, no-memory %v, %v", o.load.Memory, o.load.NoMemory, err)
	}
	// An empty name names nothing.
	if _, _, _, err = parse("run", []string{"--agent-memory=", "claude"}); err == nil ||
		!strings.Contains(err.Error(), "--agent-memory") {
		t.Errorf("--agent-memory= was accepted or refused without naming the flag: %v", err)
	}
}

// The memory flags are read on brig's own line, left of the ref. After the
// ref they are the agent's words, as they were before brig had them: an
// agent with a flag of that name keeps it, and the word after it is not
// read as the project.
func TestAgentMemoryFlagsAfterTheRefAreTheAgents(t *testing.T) {
	for _, flag := range []string{"--agent-memory=x", "--agent-memory", "--no-agent-memory"} {
		var (
			o    options
			ref  string
			tail []string
			err  error
		)
		captureStderr(t, func() { o, ref, tail, err = parse("run", []string{"claude", flag, "hello"}) })
		if err != nil {
			t.Fatalf("%s: %v", flag, err)
		}
		if ref != "claude" || !slices.Equal(tail, []string{flag, "hello"}) {
			t.Errorf("%s: ref %q, tail %q; want both words handed to the agent", flag, ref, tail)
		}
		if o.load.Project != "" {
			t.Errorf("%s: the project is %q", flag, o.load.Project)
		}
		if o.load.Memory != "" || o.load.NoMemory {
			t.Errorf("%s after the ref set memory %q, no-memory %v", flag, o.load.Memory, o.load.NoMemory)
		}
	}
	// After a project word too.
	o, _, tail, err := parse("run", []string{"claude", "dir", "--agent-memory"})
	if err != nil || o.load.Project != "dir" || o.load.Memory != "" || !slices.Equal(tail, []string{"--agent-memory"}) {
		t.Errorf("after the project: project %q, memory %q, tail %q, %v", o.load.Project, o.load.Memory, tail, err)
	}
}

// plan previews the run line, so a memory flag that run hands to the agent
// is not read by plan either: plan refuses it as a stray argument.
func TestPlanRefusesAMemoryFlagAfterTheRef(t *testing.T) {
	for _, args := range [][]string{
		{"claude", "--agent-memory=team"},
		{"claude", "dir", "--agent-memory=team"},
		{"claude", "dir", "--no-agent-memory"},
	} {
		o, _, tail, err := parse("plan", args)
		if err == nil {
			err = rejectTail("plan", tail)
		}
		if err == nil {
			t.Errorf("plan %q: accepted, memory %q; want it refused as run hands it to the agent", args, o.load.Memory)
		}
	}
}

// --memory is the older spelling of --mem and keeps meaning guest RAM. It
// does not turn agent memory on.
func TestMemoryFlagStaysGuestRAM(t *testing.T) {
	o, ref, _, err := parse("run", []string{"--memory", "2048", "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if ref != "claude" || o.load.Mem != 2048 {
		t.Errorf("ref = %q, mem = %d; want claude with 2048", ref, o.load.Mem)
	}
	if o.load.Memory != "" || o.load.NoMemory {
		t.Errorf("--memory 2048 touched agent memory: %q, %v", o.load.Memory, o.load.NoMemory)
	}
}

// loadAgentMemory loads claude-code with memory asked for as memory, or off
// when it is "", in state of its own.
func loadAgentMemory(t *testing.T, mem, rawName string) (*wrap.Config, profile.Profile) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BRIG_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("BRIG_WORKSPACE", filepath.Join(dir, "ws"))
	t.Setenv("BRIG_POLICY_DIR", filepath.Join(dir, "policies"))
	for _, key := range []string{"BRIG_MEMORY", "BRIG_CLAUDE_CODE_MEMORY"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("no claude-code profile")
	}
	cfg, err := wrap.Load(p, wrap.Options{Memory: mem, Name: rawName}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, p
}

// Claude Code with memory is started with the path of the MCP configuration
// that points it at its store, before its own name flag and the user's
// words. The path is all: the configuration and the token are not on the
// command line.
func TestAgentArgsAddTheMemoryMCPConfig(t *testing.T) {
	cfg, p := loadAgentMemory(t, "team", "refactor")
	got := agentArgs(cfg, p, []string{"-p", "hi"})
	want := []string{"--mcp-config=/brig/mcp/mcp.json", "--name", "refactor", "-p", "hi"}
	if !slices.Equal(got, want) {
		t.Errorf("agentArgs = %q, want %q", got, want)
	}
	for _, a := range got {
		if strings.Contains(a, "{") || strings.Contains(a, "token") {
			t.Errorf("agentArgs carries %q, not a path", a)
		}
	}
	// Another agent does not take Claude Code's flag.
	other := p
	other.Name = "codex"
	if got := agentArgs(cfg, other, []string{"x"}); !slices.Equal(got, []string{"x"}) {
		t.Errorf("agentArgs for codex = %q", got)
	}
}

// Without memory, Claude Code gets no --mcp-config.
func TestAgentArgsWithoutMemoryHaveNoMCPConfig(t *testing.T) {
	cfg, p := loadAgentMemory(t, "", "")
	if got := agentArgs(cfg, p, []string{"-p", "hi"}); slices.ContainsFunc(got, func(a string) bool {
		return strings.HasPrefix(a, "--mcp-config")
	}) {
		t.Errorf("agentArgs = %q for a run without memory", got)
	}
}

// --mcp-config takes every word after it as a config file until the next
// flag. An unnamed run has no --name after it, so the flag is one argument
// with =, and a prompt that follows stays the prompt.
func TestAgentArgsKeepAPromptAfterTheMCPConfig(t *testing.T) {
	cfg, p := loadAgentMemory(t, "team", "")
	if cfg.RawName != "" {
		t.Fatalf("RawName = %q, want an unnamed run", cfg.RawName)
	}
	got := agentArgs(cfg, p, []string{"fix it"})
	if want := []string{"--mcp-config=/brig/mcp/mcp.json", "fix it"}; !slices.Equal(got, want) {
		t.Errorf("agentArgs = %q, want %q", got, want)
	}
}

// Every off word is off and every on word is the project's store on the
// flag, as in BRIG_MEMORY and the profile.
func TestAgentMemoryFlagSwitchWords(t *testing.T) {
	for _, v := range []string{"off", "no", "0", "none", "OFF", "False"} {
		o, _, _, err := parse("run", []string{"--agent-memory=" + v, "claude"})
		if err != nil || !o.load.NoMemory || o.load.Memory != "" {
			t.Errorf("--agent-memory=%s: memory %q, no-memory %v, %v; want off", v, o.load.Memory, o.load.NoMemory, err)
		}
	}
	for _, v := range []string{"on", "yes", "1", "TRUE"} {
		o, _, _, err := parse("run", []string{"--agent-memory=" + v, "claude"})
		if err != nil || o.load.NoMemory || o.load.Memory != "project" {
			t.Errorf("--agent-memory=%s: memory %q, no-memory %v, %v; want project", v, o.load.Memory, o.load.NoMemory, err)
		}
	}
}

// The last --agent-memory on the line wins, whichever way it points, and
// --no-agent-memory beats them all wherever it stands.
func TestAgentMemoryFlagLastWins(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		memory string
		off    bool
	}{
		{[]string{"--agent-memory=off", "--agent-memory=team"}, "team", false},
		{[]string{"--agent-memory=team", "--agent-memory=off"}, "", true},
		{[]string{"--no-agent-memory", "--agent-memory=team"}, "", true},
		{[]string{"--agent-memory=team", "--no-agent-memory", "--agent-memory=other"}, "", true},
	} {
		o, _, _, err := parse("run", append(tc.args, "claude"))
		if err != nil || o.load.Memory != tc.memory || o.load.NoMemory != tc.off {
			t.Errorf("%v: memory %q, no-memory %v, %v; want %q, %v", tc.args, o.load.Memory, o.load.NoMemory, err, tc.memory, tc.off)
		}
	}
}

// Memory and an MCP server share one configuration file, so Claude Code is
// started with --mcp-config once, naming it.
func TestAgentArgsPassOneMCPConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("BRIG_WORKSPACE", filepath.Join(dir, "ws"))
	t.Setenv("BRIG_POLICY_DIR", filepath.Join(dir, "policies"))
	for _, key := range []string{"BRIG_MEMORY", "BRIG_CLAUDE_CODE_MEMORY"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	doc := "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: nofire}\nspec:\n  mcp:\n" +
		"    - name: nofire\n      preset: nofire\n      credential: {secretRef: {name: nofire-token}}\n" +
		"      tools: {allow: [search]}\n"
	if err := os.MkdirAll(filepath.Join(dir, "policies"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "policies", "nofire.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("no claude-code profile")
	}
	p.Policy = []string{"nofire"}
	for _, mem := range []string{"", "team"} {
		cfg, err := wrap.Load(p, wrap.Options{Memory: mem}, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := agentArgs(cfg, p, []string{"fix it"})
		if want := []string{"--mcp-config=/brig/mcp/mcp.json", "fix it"}; !slices.Equal(got, want) {
			t.Errorf("memory %q: agentArgs = %q, want %q", mem, got, want)
		}
	}
}
