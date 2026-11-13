package brigfile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
)

// project writes files under a fresh directory and returns its canonical
// path. Canonical because t.TempDir is under /var on macOS, which is a link
// to /private/var, and Load reports the root with links resolved.
func project(t *testing.T, files map[string]string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const agentPolicy = `apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: {name: claude-api}
spec:
  egress:
    default: deny
    allow: [{host: api.anthropic.com}]
`

const githubPolicy = `apiVersion: brig.sh/v1alpha1
name: github-read
egress:
  default: deny
  allow: [{host: api.github.com}]
`

const toolProfile = `name: tool
binary: tool
image: example.com/tool:1
guestHome: /root
mem: 1024
cpus: 1
secrets: [{name: tool-token}]
env:
  - {name: TOOL_TOKEN, ref: secrets.tool-token}
policy: [github]
`

func exampleProject(t *testing.T) string {
	t.Helper()
	return project(t, map[string]string{
		FileName:                          example,
		".brig/policies/claude-api.yaml":  agentPolicy,
		".brig/policies/github-read.yaml": githubPolicy,
	})
}

func TestLoadResolvesEveryAlias(t *testing.T) {
	dir := exampleProject(t)
	l, err := Load(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if l.Root != dir || l.Path != filepath.Join(dir, FileName) {
		t.Errorf("root %q path %q, want %q", l.Root, l.Path, dir)
	}
	if string(l.Raw) != example {
		t.Error("Raw is not the Brigfile's bytes")
	}
	if l.Project.Name != "brain-go" {
		t.Errorf("project name %q", l.Project.Name)
	}

	sources := map[string]string{}
	for alias, p := range l.Profiles {
		if p.Alias != alias {
			t.Errorf("profiles[%q].Alias = %q", alias, p.Alias)
		}
		sources["profiles."+alias] = p.Source.String()
	}
	for alias, p := range l.Policies {
		if p.Alias != alias {
			t.Errorf("policies[%q].Alias = %q", alias, p.Alias)
		}
		sources["policies."+alias] = p.Source.String()
	}
	want := map[string]string{
		"profiles.claude": "builtin:claude-code",
		"profiles.tool":   "inline",
		"policies.agent":  "file:.brig/policies/claude-api.yaml",
		"policies.github": "file:.brig/policies/github-read.yaml",
		"policies.quiet":  "inline",
	}
	for k, v := range want {
		if sources[k] != v {
			t.Errorf("%s source = %q, want %q", k, sources[k], v)
		}
	}
	if len(sources) != len(want) {
		t.Errorf("sources = %v", sources)
	}

	if got := l.Profiles["claude"].Profile.Name; got != "claude-code" {
		t.Errorf("profiles.claude resolved to %q", got)
	}
	if got := l.Policies["agent"].Policy.Metadata.Name; got != "claude-api" {
		t.Errorf("policies.agent resolved to %q", got)
	}
	// The flat form of a policy loads too: Load parses with policy.Parse.
	if got := l.Policies["github"].Policy.Metadata.Name; got != "github-read" {
		t.Errorf("policies.github resolved to %q", got)
	}

	// Each referenced file is kept, keyed by the path the Brigfile wrote, with
	// the bytes that were parsed.
	if len(l.Files) != 2 || string(l.Files[".brig/policies/claude-api.yaml"]) != agentPolicy ||
		string(l.Files[".brig/policies/github-read.yaml"]) != githubPolicy {
		t.Errorf("Files = %v", l.Files)
	}
	if !bytes.Equal(l.Policies["agent"].Doc, l.Files[".brig/policies/claude-api.yaml"]) {
		t.Error("policies.agent.Doc is not the file's bytes")
	}
	if _, blob, _ := profile.BuiltIn("claude-code"); !bytes.Equal(l.Profiles["claude"].Doc, blob) {
		t.Error("profiles.claude.Doc is not the embedded spec")
	}
}

// Two aliases that name one file read it once and keep one copy of it.
func TestLoadKeepsOneCopyOfAFileNamedTwice(t *testing.T) {
	doc := strings.Replace(minimal, "policies:\n  agent: {file: agent.yaml}\n",
		"policies:\n  agent: {file: agent.yaml}\n  again: {file: agent.yaml}\n", 1)
	dir := project(t, map[string]string{FileName: doc, "agent.yaml": agentPolicy})
	l, err := Load(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(l.Files) != 1 {
		t.Errorf("Files has %d entries, want 1", len(l.Files))
	}
}

func TestLoadRefusesAMissingFile(t *testing.T) {
	dir := project(t, map[string]string{FileName: minimal})
	_, err := Load(filepath.Join(dir, FileName))
	if err == nil || !strings.Contains(err.Error(), "policies.agent.file") || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load: %v", err)
	}
}

func TestLoadRefusesAMissingBrigfile(t *testing.T) {
	dir := project(t, nil)
	if _, err := Load(filepath.Join(dir, FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load: %v", err)
	}
}

// A symlink inside the project that leads out of it is refused, and the file
// it leads to is not read: its content is in neither the error nor the
// result. The repository is the attacker here, and the file outside is
// whatever on the host they would like brig to parse and hand to a sandbox.
func TestLoadRefusesASymlinkThatLeavesTheRoot(t *testing.T) {
	// The outside file is a valid policy, so a loader that followed the link
	// would load it and succeed: only the refusal makes this test pass.
	const marker = "outside-marker-4d1f"
	outside := project(t, map[string]string{"secret.yaml": strings.Replace(agentPolicy, "claude-api", marker, 1)})
	for _, target := range []string{
		filepath.Join(outside, "secret.yaml"), // absolute
		"",                                    // relative, set below
	} {
		dir := project(t, map[string]string{FileName: minimal})
		if target == "" {
			rel, err := filepath.Rel(dir, filepath.Join(outside, "secret.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			target = rel
		}
		if err := os.Symlink(target, filepath.Join(dir, "agent.yaml")); err != nil {
			t.Fatal(err)
		}
		l, err := Load(filepath.Join(dir, FileName))
		if err == nil {
			t.Fatalf("a link to %s was followed", target)
		}
		if l != nil {
			t.Errorf("a refused load returned a result")
		}
		if strings.Contains(err.Error(), marker) {
			t.Errorf("the outside file's content is in the error: %v", err)
		}
		if !strings.Contains(err.Error(), "policies.agent.file") || !strings.Contains(err.Error(), "escapes") {
			t.Errorf("error %q does not name the field and the escape", err)
		}
	}
}

// The Brigfile itself is read through the root too, so a Brigfile that is a
// link out of the project is refused the same way.
func TestLoadRefusesABrigfileThatLeavesTheRoot(t *testing.T) {
	// Declared inline only, so it would load cleanly if the link were followed.
	inline := "apiVersion: brig.sh/v1alpha1\nprofiles:\n  c: {from: builtin:codex}\ntargets:\n  t: {profile: c}\n"
	outside := project(t, map[string]string{FileName: inline})
	if _, err := Load(filepath.Join(outside, FileName)); err != nil {
		t.Fatalf("the outside Brigfile does not load on its own, so this test proves nothing: %v", err)
	}
	dir := project(t, nil)
	if err := os.Symlink(filepath.Join(outside, FileName), filepath.Join(dir, FileName)); err != nil {
		t.Fatal(err)
	}
	_, err := Load(filepath.Join(dir, FileName))
	if err == nil {
		t.Fatal("a Brigfile linked out of the project was loaded")
	}
	if !strings.Contains(err.Error(), "escapes") {
		t.Errorf("error %q is not the escape refusal", err)
	}
}

// A link that stays inside the root is accepted. os.Root resolves it without
// leaving the project, so it reaches nothing a plain path could not, and
// refusing it would only break a repository that keeps one policy under two
// names. internal/wrap makes the same choice for the workspace. The bytes are
// kept under the path the Brigfile wrote.
func TestLoadAcceptsASymlinkInsideTheRoot(t *testing.T) {
	dir := project(t, map[string]string{FileName: minimal, "real/agent.yaml": agentPolicy})
	if err := os.Symlink("real/agent.yaml", filepath.Join(dir, "agent.yaml")); err != nil {
		t.Fatal(err)
	}
	l, err := Load(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(l.Files["agent.yaml"]) != agentPolicy {
		t.Errorf("Files = %v", l.Files)
	}
}

func TestLoadRefusesWhatIsNotARegularFile(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		dir := project(t, map[string]string{FileName: minimal, "agent.yaml/x": ""})
		_, err := Load(filepath.Join(dir, FileName))
		if err == nil || !strings.Contains(err.Error(), "directory") || !strings.Contains(err.Error(), "policies.agent.file") {
			t.Fatalf("Load: %v", err)
		}
	})
	// A FIFO would park a plain read forever, so the test finishing is half
	// of the assertion.
	t.Run("fifo", func(t *testing.T) {
		dir := project(t, map[string]string{FileName: minimal})
		if err := syscall.Mkfifo(filepath.Join(dir, "agent.yaml"), 0o644); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
		_, err := Load(filepath.Join(dir, FileName))
		if err == nil || !strings.Contains(err.Error(), "FIFO") {
			t.Fatalf("Load: %v", err)
		}
	})
	t.Run("device", func(t *testing.T) {
		dir := project(t, map[string]string{FileName: minimal})
		if err := os.Symlink("/dev/null", filepath.Join(dir, "agent.yaml")); err != nil {
			t.Fatal(err)
		}
		// /dev/null is outside the root, so os.Root refuses it before the type
		// check; either refusal is correct, and neither reads it.
		if _, err := Load(filepath.Join(dir, FileName)); err == nil {
			t.Fatal("a link to /dev/null was loaded")
		}
	})
	t.Run("brigfile directory", func(t *testing.T) {
		dir := project(t, map[string]string{"Brigfile/x": ""})
		if _, err := Load(filepath.Join(dir, FileName)); err == nil || !strings.Contains(err.Error(), "directory") {
			t.Fatalf("Load: %v", err)
		}
	})
}

func TestLoadRefusesAnOversizeFile(t *testing.T) {
	big := agentPolicy + "# " + strings.Repeat("x", MaxFileSize) + "\n"
	dir := project(t, map[string]string{FileName: minimal, "agent.yaml": big})
	_, err := Load(filepath.Join(dir, FileName))
	if err == nil || !strings.Contains(err.Error(), "MiB") || !strings.Contains(err.Error(), "policies.agent.file") {
		t.Fatalf("Load: %v", err)
	}

	dir = project(t, map[string]string{FileName: minimal + "# " + strings.Repeat("x", MaxFileSize) + "\n"})
	if _, err := Load(filepath.Join(dir, FileName)); err == nil || !strings.Contains(err.Error(), "MiB") {
		t.Fatalf("Load of an oversize Brigfile: %v", err)
	}
}

// A file that does not parse names the alias and the file.
func TestLoadRefusesAFileThatDoesNotParse(t *testing.T) {
	dir := project(t, map[string]string{FileName: minimal, "agent.yaml": "apiVersion: brig.sh/v1alpha1\nkind: Policy\nnope: 1\n"})
	_, err := Load(filepath.Join(dir, FileName))
	if err == nil || !strings.Contains(err.Error(), "policies.agent") || !strings.Contains(err.Error(), "agent.yaml") {
		t.Fatalf("Load: %v", err)
	}
}

// builtin:claude-code is the profile brig ships, even when the reader has a
// claude-code of their own in the profile directory.
func TestLoadBuiltinIgnoresAUserOverride(t *testing.T) {
	user := project(t, map[string]string{"claude-code.yaml": "name: claude-code\nbinary: claude\n" +
		"image: example.com/override:1\nguestHome: /root\nmem: 512\ncpus: 1\n"})
	t.Cleanup(func() { _ = profile.Load() })
	if err := profile.Load(user); err != nil {
		t.Fatal(err)
	}
	if p, _ := profile.Lookup("claude-code"); p.Image != "example.com/override:1" {
		t.Fatalf("the override did not load, so this test proves nothing: %q", p.Image)
	}

	dir := project(t, map[string]string{FileName: minimal, "agent.yaml": agentPolicy})
	l, err := Load(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := l.Profiles["claude"].Profile.Image; got == "example.com/override:1" {
		t.Errorf("builtin:claude-code resolved to the user's override")
	}
	want, _, _ := profile.BuiltIn("claude-code")
	if got := l.Profiles["claude"].Profile.Image; got != want.Image {
		t.Errorf("builtin:claude-code image %q, want %q", got, want.Image)
	}
	rt, err := l.Target("dev")
	if err != nil {
		t.Fatal(err)
	}
	if rt.Profile.Profile.Image != want.Image {
		t.Errorf("the target resolved image %q, want %q", rt.Profile.Profile.Image, want.Image)
	}
}

func TestLoadRefusesABuiltinBrigDoesNotShip(t *testing.T) {
	for from, want := range map[string]string{
		"builtin:nope":   `"nope"`,
		"builtin:claude": "claude-code", // an alias is named, with the full name
	} {
		doc := strings.Replace(minimal, "builtin:claude-code", from, 1)
		dir := project(t, map[string]string{FileName: doc, "agent.yaml": agentPolicy})
		_, err := Load(filepath.Join(dir, FileName))
		if err == nil || !strings.Contains(err.Error(), "profiles.claude.from") || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", from, err)
		}
	}
}

func TestTargetResolvesProfilePoliciesOverridesAndArgs(t *testing.T) {
	dir := exampleProject(t)
	l, err := Load(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	rt, err := l.Target("dev")
	if err != nil {
		t.Fatal(err)
	}
	if rt.Name != "dev" || rt.Profile.Alias != "claude" || rt.Profile.Profile.Name != "claude-code" {
		t.Errorf("target = %+v", rt)
	}
	var aliases []string
	for _, p := range rt.Policies {
		aliases = append(aliases, p.Alias)
	}
	if strings.Join(aliases, ",") != "agent,github" {
		t.Errorf("policies = %v", aliases)
	}
	if *rt.Overrides.CPUs != 4 || *rt.Overrides.Mem != 8192 || rt.Overrides.Image != nil {
		t.Errorf("overrides = %+v", rt.Overrides)
	}
	if strings.Join(rt.Args, " ") != "--print hello" {
		t.Errorf("args = %q", rt.Args)
	}
	// The overrides are reported, not applied: the profile keeps its own size.
	builtin, _, _ := profile.BuiltIn("claude-code")
	if rt.Profile.Profile.CPUs != builtin.CPUs || rt.Profile.Profile.Mem != builtin.Mem {
		t.Errorf("the profile was resized: %d cpus, %d MB", rt.Profile.Profile.CPUs, rt.Profile.Profile.Mem)
	}

	if _, err := l.Target("prod"); err == nil || !strings.Contains(err.Error(), `"prod"`) {
		t.Errorf("Target(prod): %v", err)
	}
}

// A repository profile's own policy: list names policy aliases of the same
// Brigfile. The target gets its own list first and then the profile's, each
// alias once.
func TestTargetAddsARepositoryProfilesOwnPolicies(t *testing.T) {
	doc := `apiVersion: brig.sh/v1alpha1
profiles:
  tool: {file: tool.yaml}
policies:
  agent: {file: agent.yaml}
  github: {file: github.yaml}
targets:
  plain: {profile: tool}
  both: {profile: tool, policies: [agent, github]}
`
	dir := project(t, map[string]string{FileName: doc, "tool.yaml": toolProfile,
		"agent.yaml": agentPolicy, "github.yaml": githubPolicy})
	l, err := Load(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for target, want := range map[string]string{"plain": "github", "both": "agent,github"} {
		rt, err := l.Target(target)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, p := range rt.Policies {
			got = append(got, p.Alias)
		}
		if strings.Join(got, ",") != want {
			t.Errorf("%s: policies %v, want %s", target, got, want)
		}
	}
	// The names are Brigfile aliases, not installed policies, so they are
	// moved off the profile: a later step that resolves Profile.Policy against
	// the reader's installed policies must not find them there.
	tool := l.Profiles["tool"]
	if len(tool.Profile.Policy) != 0 || strings.Join(tool.Policies, ",") != "github" {
		t.Errorf("profile policy %v, aliases %v", tool.Profile.Policy, tool.Policies)
	}
}

// A built-in profile's policy: list is left for the run to resolve against
// the reader's installed policies, as it is today.
func TestTargetLeavesABuiltinProfilesPolicyListAlone(t *testing.T) {
	dir := project(t, map[string]string{FileName: minimal, "agent.yaml": agentPolicy})
	l, err := Load(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	builtin, _, _ := profile.BuiltIn("claude-code")
	rt, err := l.Target("dev")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rt.Profile.Profile.Policy, builtin.Policy) || len(rt.Profile.Policies) != 0 {
		t.Errorf("policy %v, aliases %v; the built-in lists %v",
			rt.Profile.Profile.Policy, rt.Profile.Policies, builtin.Policy)
	}
}

func TestLoadRefusesARepositoryProfileNamingAMissingPolicy(t *testing.T) {
	doc := strings.Replace(minimal, "{from: builtin:claude-code}", "{file: tool.yaml}", 1)
	dir := project(t, map[string]string{FileName: doc, "tool.yaml": toolProfile, "agent.yaml": agentPolicy})
	_, err := Load(filepath.Join(dir, FileName))
	if err == nil || !strings.Contains(err.Error(), "profiles.claude") || !strings.Contains(err.Error(), `"github"`) {
		t.Fatalf("Load: %v", err)
	}
}

// A shell or gui profile has no agent to hook an egress rule into, so a
// target that gives one policies would be a policy that is never enforced.
func TestLoadRefusesPoliciesOnAShellTarget(t *testing.T) {
	doc := strings.Replace(minimal, "builtin:claude-code", "builtin:ubuntu", 1)
	if p, _, _ := profile.BuiltIn("ubuntu"); !p.IsShell() {
		t.Fatal("ubuntu is not kind: shell, so this test proves nothing")
	}
	dir := project(t, map[string]string{FileName: doc, "agent.yaml": agentPolicy})
	_, err := Load(filepath.Join(dir, FileName))
	if err == nil || !strings.Contains(err.Error(), "targets.dev") || !strings.Contains(err.Error(), "shell") {
		t.Fatalf("Load: %v", err)
	}
}

// Target hands out values the caller may change without changing what the
// next call returns.
func TestTargetReturnsAFreshCopy(t *testing.T) {
	dir := exampleProject(t)
	l, err := Load(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := l.Target("dev")
	a.Args[0] = "CHANGED"
	*a.Overrides.CPUs = 99
	a.Profile.Profile.Deny = append(a.Profile.Profile.Deny[:0], "CHANGED")
	a.Policies[0].Policy.Spec.Egress.Default = "allow"
	for i := range a.Policies[0].Doc {
		a.Policies[0].Doc[i] = '#'
	}
	for i := range a.Profile.Doc {
		a.Profile.Doc[i] = '#'
	}
	b, err := l.Target("dev")
	if err != nil {
		t.Fatalf("an edit to one result's Doc broke the next Target: %v", err)
	}
	if len(b.Policies[0].Doc) == 0 || b.Policies[0].Doc[0] == '#' || len(b.Profile.Doc) == 0 || b.Profile.Doc[0] == '#' {
		t.Error("an edit to one result's Doc reached the next result")
	}
	if b.Args[0] == "CHANGED" || *b.Overrides.CPUs == 99 || b.Profile.Profile.Deny[0] == "CHANGED" ||
		b.Policies[0].Policy.Spec.Egress.Default == "allow" {
		t.Errorf("an edit to one Target result reached the next: %+v", b)
	}
	if l.Project.Targets["dev"].Args[0] == "CHANGED" || *l.Project.Targets["dev"].Overrides.CPUs == 99 {
		t.Error("an edit to a Target result reached the Project")
	}
}
