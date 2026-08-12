package runtime

import (
	"strings"
	"testing"
)

// The whole point of the split: a value goes into the child's environment and
// only the bare name goes on the command line, so `ps` on a shared host
// cannot read a forwarded credential. This is the limitation the Homebrew
// wrapper documented (NOFireAI/urunc-macos#50) and the reason brig builds the
// command line itself.
func TestSplitEnvKeepsValuesOutOfArgv(t *testing.T) {
	t.Setenv("BRIG_ENV_ARGV", "")
	args, env := splitEnv("--env", []Var{
		{Name: "CLAUDE_CODE_OAUTH_TOKEN", Value: "sk-secret"},
		{Name: "GH_TOKEN", Value: "ghp_secret"},
	})
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "sk-secret") || strings.Contains(joined, "ghp_secret") {
		t.Fatalf("a credential value reached argv: %q", joined)
	}
	if joined != "--env CLAUDE_CODE_OAUTH_TOKEN --env GH_TOKEN" {
		t.Errorf("args = %q", joined)
	}
	want := []string{"CLAUDE_CODE_OAUTH_TOKEN=sk-secret", "GH_TOKEN=ghp_secret"}
	if len(env) != len(want) {
		t.Fatalf("env = %v, want %v", env, want)
	}
	for i := range want {
		if env[i] != want[i] {
			t.Errorf("env[%d] = %q, want %q", i, env[i], want[i])
		}
	}
}

// The escape hatch exists for a runtime build that does not take a bare KEY.
// It puts values back where ps can read them, so it must be explicit.
func TestSplitEnvArgvEscapeHatch(t *testing.T) {
	t.Setenv("BRIG_ENV_ARGV", "1")
	args, env := splitEnv("-e", []Var{{Name: "GH_TOKEN", Value: "ghp_secret"}})
	if len(env) != 0 {
		t.Errorf("env = %v, want empty", env)
	}
	if strings.Join(args, " ") != "-e GH_TOKEN=ghp_secret" {
		t.Errorf("args = %v", args)
	}
}

func TestTelemetrySuppressesPlumbing(t *testing.T) {
	if got := telemetryEnv(false); got[1] != "URUNC_TELEMETRY_SUPPRESS=1" {
		t.Errorf("plumbing call is counted: %v", got)
	}
	if got := telemetryEnv(true); got[1] != "URUNC_TELEMETRY_SUPPRESS=" {
		t.Errorf("user action is suppressed: %v", got)
	}
}

func TestMergeEnvLastOneWins(t *testing.T) {
	t.Setenv("BRIG_TEST_KEY", "original")
	out := mergeEnv([]string{"BRIG_TEST_KEY=replaced"}, []string{"BRIG_TEST_OTHER=added"})
	seen := map[string]int{}
	for _, kv := range out {
		k, v, _ := strings.Cut(kv, "=")
		if k == "BRIG_TEST_KEY" {
			seen[k]++
			if v != "replaced" {
				t.Errorf("BRIG_TEST_KEY = %q, want replaced", v)
			}
		}
	}
	if seen["BRIG_TEST_KEY"] != 1 {
		t.Errorf("BRIG_TEST_KEY appears %d times, want 1", seen["BRIG_TEST_KEY"])
	}
}
