package runtime

import (
	"strings"
	"testing"
)

// The whole point of the split: a value goes into the child's environment and
// only the bare name goes on the command line, so `ps` on a shared host
// cannot read a forwarded credential. This is the limitation the Homebrew
// wrapper documented (NOFireAI/urunc-macos#50, now brig-sh/hull) and the reason brig builds the
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

// hull reads HULL_TELEMETRY_*. Getting this wrong does not break a run, it
// just misattributes or double-counts, which is exactly the kind of thing
// nobody notices for months.
func TestTelemetrySuppressesPlumbing(t *testing.T) {
	for _, prefix := range []string{"HULL"} {
		plumbing := strings.Join(telemetryEnv(false), " ")
		if !strings.Contains(plumbing, prefix+"_TELEMETRY_SUPPRESS=1") {
			t.Errorf("plumbing call is counted for %s: %v", prefix, plumbing)
		}
		if !strings.Contains(plumbing, prefix+"_TELEMETRY_PRODUCT=brig") {
			t.Errorf("%s attribution missing: %v", prefix, plumbing)
		}
		counted := strings.Join(telemetryEnv(true), " ")
		if !strings.Contains(counted, prefix+"_TELEMETRY_SUPPRESS= ") &&
			!strings.HasSuffix(counted, prefix+"_TELEMETRY_SUPPRESS=") {
			t.Errorf("user action is suppressed for %s: %v", prefix, counted)
		}
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

// The Linux sandbox is a microVM, not a namespace: the container goes to the
// urunc shim unless someone deliberately asks for something else.
func TestContainerdRuntimeDefaultsToUrunc(t *testing.T) {
	t.Setenv("BRIG_CONTAINERD_RUNTIME", "")
	if got := containerdRuntime(); got != "io.containerd.urunc.v2" {
		t.Errorf("containerdRuntime() = %q, want the urunc shim", got)
	}
	t.Setenv("BRIG_CONTAINERD_RUNTIME", "runc")
	if got := containerdRuntime(); got != "runc" {
		t.Errorf("override ignored: %q", got)
	}
}
