package runtime

import (
	"strings"
	"testing"
)

// mustSplitEnv is splitEnv for a set it must accept.
func mustSplitEnv(t *testing.T, flag string, vars []Var) (args, env []string) {
	t.Helper()
	args, env, err := splitEnv(flag, vars)
	if err != nil {
		t.Fatal(err)
	}
	return args, env
}

// The whole point of the split: a value goes into the child's environment and
// only the bare name goes on the command line, so `ps` on a shared host
// cannot read a forwarded credential. This is the limitation the Homebrew
// wrapper documented (NOFireAI/urunc-macos#50, now brig-sh/hull) and the reason brig builds the
// command line itself.
func TestSplitEnvKeepsValuesOutOfArgv(t *testing.T) {
	t.Setenv("BRIG_ENV_ARGV", "")
	args, env := mustSplitEnv(t, "--env", []Var{
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
	args, env := mustSplitEnv(t, "-e", []Var{{Name: "GH_TOKEN", Value: "ghp_secret"}})
	if len(env) != 0 {
		t.Errorf("env = %v, want empty", env)
	}
	if strings.Join(args, " ") != "-e GH_TOKEN=ghp_secret" {
		t.Errorf("args = %v", args)
	}
}

// A store secret must stay out of argv even under the escape hatch: the guest
// agent prints every exec's argv to the guest console, which on some backends
// is a host file, and that is a change in severity class from the ambient
// shell values the hatch was built for.
func TestSplitEnvArgvEscapeHatchNeverExposesSecrets(t *testing.T) {
	t.Setenv("BRIG_ENV_ARGV", "1")
	args, env := mustSplitEnv(t, "-e", []Var{
		{Name: "GH_TOKEN", Value: "ghp_secret"},
		{Name: "OAUTH", Value: "sk-fromkeychain", Secret: true},
	})
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "sk-fromkeychain") {
		t.Fatalf("a secret value reached argv even under BRIG_ENV_ARGV: %q", joined)
	}
	if joined != "-e GH_TOKEN=ghp_secret -e OAUTH" {
		t.Errorf("args = %q", joined)
	}
	want := []string{"OAUTH=sk-fromkeychain"}
	if len(env) != len(want) || env[0] != want[0] {
		t.Errorf("env = %v, want %v", env, want)
	}
}

// The exposure has to be reportable before anything is spawned, and reported
// from the same rule the command line is built from: a warning that names one
// set of variables while argv carries another is wrong in exactly the case it
// exists for. So the check is against the argv splitEnv actually produces,
// rather than against a second list that merely looks right.
func TestArgvExposedNamesWhatReachesArgv(t *testing.T) {
	t.Setenv("BRIG_ENV_ARGV", "1")
	vars := []Var{
		{Name: "HOME", Value: "/root"},
		{Name: "GH_TOKEN", Value: "ghp_secret"},
		{Name: "OAUTH", Value: "sk-fromkeychain", Secret: true},
		{Name: "GIT_TERMINAL_PROMPT", Value: "0"},
	}

	got := ArgvExposed(vars)
	want := []string{"GH_TOKEN", "GIT_TERMINAL_PROMPT"}
	if len(got) != len(want) {
		t.Fatalf("ArgvExposed = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ArgvExposed = %v, want %v", got, want)
		}
	}

	args, _ := mustSplitEnv(t, "--env", vars)
	joined := strings.Join(args, " ")
	for _, name := range got {
		if !strings.Contains(joined, "--env "+name+"=") {
			t.Errorf("%s was reported as reaching argv but did not: %q", name, joined)
		}
	}
	// The other half of the claim: of what the report left out, only HOME
	// carries a value on the command line. It is there with the hatch off too,
	// so it is not the hatch's to report.
	if !strings.Contains(joined, "--env HOME=/root") {
		t.Errorf("HOME is not on the command line: %q", joined)
	}
	if strings.Contains(joined, "sk-fromkeychain") {
		t.Errorf("a value not named by the report reached argv: %q", joined)
	}
}

// Off, there is nothing to warn about: every value the report could name
// travels in the child's environment, which is what the report has to say by
// saying nothing. HOME is on the command line, but no hatch put it there.
func TestArgvExposedIsSilentWithTheHatchOff(t *testing.T) {
	t.Setenv("BRIG_ENV_ARGV", "")
	vars := []Var{{Name: "HOME", Value: "/root"}, {Name: "GH_TOKEN", Value: "ghp_secret"}}
	if got := ArgvExposed(vars); len(got) != 0 {
		t.Errorf("ArgvExposed = %v with the hatch off, want nothing", got)
	}
}

// A guest value for a name the runtime reads for itself goes on the command
// line, hatch or not, and stays out of the runtime's own environment. HOMEDIR
// and MY_XDG_DIR check that the rule matches names, not substrings.
func TestSplitEnvPutsWhatTheRuntimeReadsInArgv(t *testing.T) {
	t.Setenv("BRIG_ENV_ARGV", "")
	args, env := mustSplitEnv(t, "--env", []Var{
		{Name: "HOME", Value: "/root"},
		{Name: "PATH", Value: "/opt/tool/bin:/usr/bin"},
		{Name: "TMPDIR", Value: "/scratch"},
		{Name: "XDG_RUNTIME_DIR", Value: "/run/user/0"},
		{Name: "XDG_CONFIG_HOME", Value: "/root/.config"},
		{Name: "HOMEDIR", Value: "/h"},
		{Name: "MY_XDG_DIR", Value: "/x"},
		{Name: "GH_TOKEN", Value: "ghp_secret"},
	})
	want := "--env HOME=/root --env PATH=/opt/tool/bin:/usr/bin --env TMPDIR=/scratch " +
		"--env XDG_RUNTIME_DIR=/run/user/0 --env XDG_CONFIG_HOME=/root/.config " +
		"--env HOMEDIR --env MY_XDG_DIR --env GH_TOKEN"
	if got := strings.Join(args, " "); got != want {
		t.Errorf("args = %q\nwant   %q", got, want)
	}
	wantEnv := "HOMEDIR=/h MY_XDG_DIR=/x GH_TOKEN=ghp_secret"
	if got := strings.Join(env, " "); got != wantEnv {
		t.Errorf("env = %q, want %q", got, wantEnv)
	}
}

// A stored secret has nowhere to go under a name the runtime reads: argv is
// logged on the host, and the runtime's environment is the bug in #337. The
// run is refused, with the hatch on or off, and the error names the variable.
func TestSplitEnvRefusesAStoredSecretTheRuntimeReads(t *testing.T) {
	for _, hatch := range []string{"", "1"} {
		t.Setenv("BRIG_ENV_ARGV", hatch)
		for _, name := range []string{"HOME", "PATH", "TMPDIR", "XDG_CONFIG_HOME"} {
			args, env, err := splitEnv("-e", []Var{{Name: name, Value: "sk-fromkeychain", Secret: true}})
			if err == nil {
				t.Fatalf("BRIG_ENV_ARGV=%q: a stored secret named %s was accepted: args %v, env %v",
					hatch, name, args, env)
			}
			if !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), "sk-fromkeychain") {
				t.Errorf("the refusal must name %s and never its value: %v", name, err)
			}
		}
	}
}

// hull reads HULL_TELEMETRY_*. Getting this wrong does not break a run, it
// just misattributes or double-counts, which is exactly the kind of thing
// nobody notices for months.
func TestTelemetrySuppressesPlumbing(t *testing.T) {
	counting(t, true)
	plumbing := strings.Join(telemetryEnv(false), " ")
	if !strings.Contains(plumbing, "HULL_TELEMETRY_SUPPRESS=1") {
		t.Errorf("plumbing call is counted: %v", plumbing)
	}
	if !strings.Contains(plumbing, "HULL_TELEMETRY_PRODUCT=brig") {
		t.Errorf("attribution missing: %v", plumbing)
	}
	// A user action reports what only hull sees, and never hull's own command
	// event: brig sends that one.
	counted := strings.Join(telemetryEnv(true), " ")
	if !strings.Contains(counted, "HULL_TELEMETRY_SUPPRESS=command") {
		t.Errorf("user action is suppressed, or counted twice: %v", counted)
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
