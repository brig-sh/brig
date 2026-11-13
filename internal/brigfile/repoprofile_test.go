package brigfile

import (
	"path/filepath"
	"strings"
	"testing"
)

// repoProfileBase is a repository profile with nothing a repository may not
// set. Each case below adds one line to it.
const repoProfileBase = `name: tool
binary: tool
image: example.com/tool:1
guestHome: /root
mem: 1024
cpus: 1
`

const repoProfileBrigfile = `apiVersion: brig.sh/v1alpha1
profiles:
  tool: {file: tool.yaml}
targets:
  dev: {profile: tool}
`

func loadRepoProfile(t *testing.T, body string) (*Loaded, error) {
	t.Helper()
	dir := project(t, map[string]string{FileName: repoProfileBrigfile, "tool.yaml": body})
	return Load(filepath.Join(dir, FileName))
}

// What a repository profile may set. It may name a credential and bind it,
// because naming is what a later approval step shows the reader; it may not
// say where on the host the value comes from.
func TestARepositoryProfileMayNameCredentials(t *testing.T) {
	for name, extra := range map[string]string{
		"a bare secret name": "secrets: [tool-token]\n",
		"a secret object":    "secrets: [{name: tool-token, required: false}]\n",
		"an env ref to a secret": "secrets: [tool-token]\n" +
			"env: [{name: TOOL_TOKEN, ref: secrets.tool-token}]\n",
		"a literal env value": "env: [{name: MODE, value: ci}]\n",
		"a tmpfs volume":      "volumes: [{kind: tmpfs, path: .tool}]\n",
		"a file binding": "secrets: [tool-token]\nvolumes: [{kind: tmpfs, path: .tool}]\n" +
			"files: [{ref: secrets.tool-token, path: .tool/token}]\n",
		"a deny list": "deny: [ANTHROPIC_API_KEY]\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadRepoProfile(t, repoProfileBase+extra); err != nil {
				t.Errorf("Load: %v", err)
			}
		})
	}
}

// Each field that lets a repository name a host path, a host executable or a
// host lookup is refused, by name, in both declaration forms.
func TestARepositoryProfileMayNotReachTheHost(t *testing.T) {
	cases := map[string]struct{ extra, want string }{
		"runtimeBin":    {"runtimeBin: /tmp/evil\n", "runtimeBin"},
		"hostConfigDir": {"hostConfigDir: ~/.ssh\n", "hostConfigDir"},
		"projectPaths":  {"projectPaths: [../.ssh]\n", "projectPaths"},
		"sources": {"secrets:\n  - name: tok\n    sources: [{from: file, path: ~/.ssh/id_ed25519}]\n",
			"secrets[0].sources"},
		"shorthand from file": {"secrets: [{name: tok, from: file, path: ~/.aws/credentials}]\n", "secrets[0].from"},
		"shorthand from keychain": {"secrets: [{name: tok, from: keychain, service: Claude Code-credentials}]\n",
			"secrets[0].from"},
		"shorthand from env":     {"secrets: [{name: tok, from: env, var: GITHUB_TOKEN}]\n", "secrets[0].from"},
		"shorthand from aws":     {"secrets: [{name: tok, from: aws}]\n", "secrets[0].from"},
		"service alone":          {"secrets: [{name: tok, service: x}]\n", "secrets[0].service"},
		"path alone":             {"secrets: [{name: tok, path: x}]\n", "secrets[0].path"},
		"var alone":              {"secrets: [{name: tok, var: X}]\n", "secrets[0].var"},
		"hint alone":             {"secrets: [{name: tok, hint: x}]\n", "secrets[0].hint"},
		"field":                  {"secrets: [{name: tok, field: a.b}]\n", "secrets[0].field"},
		"expiryField":            {"secrets: [{name: tok, expiryField: a.b}]\n", "secrets[0].expiryField"},
		"an env ref to the host": {"env: [{name: GH_TOKEN, ref: env.GITHUB_TOKEN}]\n", "env[0]"},
		"an env refs chain to the host": {"secrets: [tok]\nenv: [{name: GH_TOKEN, refs: [secrets.tok, env.GITHUB_TOKEN]}]\n",
			"env[0]"},
		"a hostmount": {"volumes: [{kind: tmpfs, path: .tool}, {kind: hostmount, path: .tool/state}]\n", "volumes[1]"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			l, err := loadRepoProfile(t, repoProfileBase+c.extra)
			if err == nil {
				t.Fatalf("Load accepted %q", c.extra)
			}
			if l != nil {
				t.Error("a refused load returned a result")
			}
			if !strings.Contains(err.Error(), "profiles.tool") || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not name profiles.tool and %s", err, c.want)
			}
		})
	}
}

// The same refusal holds for an inline profile.
func TestAnInlineProfileMayNotReachTheHost(t *testing.T) {
	doc := `apiVersion: brig.sh/v1alpha1
profiles:
  tool:
    inline:
      name: tool
      binary: tool
      image: example.com/tool:1
      guestHome: /root
      mem: 1024
      cpus: 1
      runtimeBin: /tmp/evil
targets:
  dev: {profile: tool}
`
	dir := project(t, map[string]string{FileName: doc})
	_, err := Load(filepath.Join(dir, FileName))
	if err == nil || !strings.Contains(err.Error(), "profiles.tool.inline") || !strings.Contains(err.Error(), "runtimeBin") {
		t.Fatalf("Load: %v", err)
	}
}

// profile.Parse matches keys without regard to case, so RuntimeBin: would
// set the field. A repository profile is held to exact keys, and the field
// check reads the decoded struct, so neither path lets it through.
func TestARepositoryProfileKeyInTheWrongCaseIsRefused(t *testing.T) {
	for _, extra := range []string{"RuntimeBin: /tmp/evil\n", "secrets: [{name: tok, SERVICE: x}]\n"} {
		if _, err := loadRepoProfile(t, repoProfileBase+extra); err == nil {
			t.Errorf("Load accepted %q", extra)
		}
	}
}

// A repository profile file holds one document, as a Brigfile does.
func TestARepositoryProfileWithTwoDocumentsIsRefused(t *testing.T) {
	_, err := loadRepoProfile(t, repoProfileBase+"---\nruntimeBin: /tmp/evil\n")
	if err == nil || !strings.Contains(err.Error(), "more than one YAML document") {
		t.Fatalf("Load: %v", err)
	}
}

// An inline profile is decoded no more loosely than the same profile in a
// file: an unquoted octal mode is refused in both.
func TestAnInlineProfileIsNoLooserThanAFile(t *testing.T) {
	extra := "secrets: [tok]\nvolumes: [{kind: tmpfs, path: .tool}]\nfiles: [{ref: secrets.tok, path: .tool/t, mode: 0600}]\n"
	if _, err := loadRepoProfile(t, repoProfileBase+extra); err == nil {
		t.Fatal("the file form accepted an unquoted mode")
	}
	var inline strings.Builder
	inline.WriteString("apiVersion: brig.sh/v1alpha1\nprofiles:\n  tool:\n    inline:\n")
	for _, line := range strings.Split(strings.TrimSuffix(repoProfileBase+extra, "\n"), "\n") {
		inline.WriteString("      " + line + "\n")
	}
	inline.WriteString("targets:\n  dev: {profile: tool}\n")
	dir := project(t, map[string]string{FileName: inline.String()})
	if _, err := Load(filepath.Join(dir, FileName)); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("the inline form: %v", err)
	}
}

// A built-in profile keeps every declaration it ships with, including the
// ones a repository profile may not make.
func TestABuiltinProfileKeepsItsHostFields(t *testing.T) {
	dir := project(t, map[string]string{FileName: minimal, "agent.yaml": agentPolicy})
	l, err := Load(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	p := l.Profiles["claude"].Profile
	if p.HostConfigDir == "" {
		t.Error("builtin:claude-code lost hostConfigDir")
	}
	sources := 0
	for _, s := range p.Secrets {
		sources += len(s.SourceList())
	}
	if sources == 0 {
		t.Error("builtin:claude-code lost its secret sources")
	}
}

// The runtime takes the image as a word after its flags, so one that starts
// with a dash would reach it as an option the repository chose.
func TestARepositoryProfileImageMayNotBeAFlag(t *testing.T) {
	doc := strings.Replace(repoProfileBase, "image: example.com/tool:1", `image: "--privileged"`, 1)
	if doc == repoProfileBase {
		t.Fatal("the base profile no longer has the image line this test replaces")
	}
	_, err := loadRepoProfile(t, doc)
	if err == nil || !strings.Contains(err.Error(), "profiles.tool") || !strings.Contains(err.Error(), "image") {
		t.Fatalf("Load: %v", err)
	}
}

// A repository may ask for more isolation than the default and never for
// less. shared puts the guest on a network with other sandboxes, and a
// hypervisor other than the default falls back to a shared network.
func TestARepositoryProfileMayNotWeakenTheNetwork(t *testing.T) {
	for _, extra := range []string{"network: shared\n", "hypervisor: vz\n", "hypervisor: qemu\n", "hypervisor: hvi\n"} {
		_, err := loadRepoProfile(t, repoProfileBase+extra)
		field := strings.SplitN(extra, ":", 2)[0]
		if err == nil || !strings.Contains(err.Error(), "profiles.tool") || !strings.Contains(err.Error(), field) {
			t.Errorf("%q: Load: %v", strings.TrimSpace(extra), err)
		}
	}
	for _, extra := range []string{"network: isolated\n", "network: offline\n"} {
		if _, err := loadRepoProfile(t, repoProfileBase+extra); err != nil {
			t.Errorf("%q is refused: %v", strings.TrimSpace(extra), err)
		}
	}
}
