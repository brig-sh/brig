package brigfile

import (
	"strings"
	"testing"
	"time"
	"unicode"
)

// example is the document the format was decided on, with an inline policy
// and an inline profile added so every declaration form is in one file.
const example = `apiVersion: brig.sh/v1alpha1
name: brain-go
defaultTarget: dev
profiles:
  claude: {from: builtin:claude-code}
  tool:
    inline:
      name: tool
      binary: tool
      image: example.com/tool:1
      guestHome: /root
      mem: 1024
      cpus: 1
policies:
  agent: {file: .brig/policies/claude-api.yaml}
  github: {file: .brig/policies/github-read.yaml}
  quiet:
    inline:
      apiVersion: brig.sh/v1alpha1
      kind: Policy
      metadata: {name: quiet}
      spec:
        egress: {default: deny}
targets:
  dev:
    profile: claude
    policies: [agent, github]
    overrides: {cpus: 4, mem: 8192}
    args: ["--print", "hello"]
  review:
    profile: claude
    policies: [agent]
    overrides: {cpus: 2, mem: 4096, image: "example.com/claude:2"}
  tooling:
    profile: tool
`

func TestParseTheDecidedFormat(t *testing.T) {
	p, err := Parse([]byte(example))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.APIVersion != APIVersion || p.Name != "brain-go" || p.DefaultTarget != "dev" {
		t.Errorf("header = %q %q %q", p.APIVersion, p.Name, p.DefaultTarget)
	}
	if got := p.Profiles["claude"].From; got != "builtin:claude-code" {
		t.Errorf("profiles.claude.from = %q", got)
	}
	if len(p.Profiles["tool"].Inline) == 0 {
		t.Error("profiles.tool.inline is empty")
	}
	if got := p.Policies["github"].File; got != ".brig/policies/github-read.yaml" {
		t.Errorf("policies.github.file = %q", got)
	}
	dev := p.Targets["dev"]
	if dev.Profile != "claude" || strings.Join(dev.Policies, ",") != "agent,github" {
		t.Errorf("targets.dev = %+v", dev)
	}
	if dev.Overrides == nil || *dev.Overrides.CPUs != 4 || *dev.Overrides.Mem != 8192 || dev.Overrides.Image != nil {
		t.Errorf("targets.dev.overrides = %+v", dev.Overrides)
	}
	if strings.Join(dev.Args, " ") != "--print hello" {
		t.Errorf("targets.dev.args = %q", dev.Args)
	}
	if img := p.Targets["review"].Overrides.Image; img == nil || *img != "example.com/claude:2" {
		t.Errorf("targets.review.overrides.image = %v", img)
	}
}

// JSON is a subset of YAML, so a Brigfile written as JSON reads the same.
func TestParseJSON(t *testing.T) {
	doc := `{"apiVersion": "brig.sh/v1alpha1",
		"profiles": {"c": {"from": "builtin:codex"}},
		"targets": {"t": {"profile": "c"}}}`
	if _, err := Parse([]byte(doc)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

// minimal is the smallest valid Brigfile; the refusal cases below each break
// one thing in it, so the error they get is about that thing alone.
const minimal = `apiVersion: brig.sh/v1alpha1
profiles:
  claude: {from: builtin:claude-code}
policies:
  agent: {file: agent.yaml}
targets:
  dev:
    profile: claude
    policies: [agent]
`

func TestParseMinimal(t *testing.T) {
	if _, err := Parse([]byte(minimal)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

// Every refusal names where in the file the problem is, so a reader can find
// the line without reading the parser.
func TestParseRefuses(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want []string
	}{
		{"an unknown top-level field", minimal + "extra: 1\n", []string{`"extra"`}},
		{"an unknown target field", strings.Replace(minimal, "profile: claude", "profile: claude\n    network: offline", 1),
			[]string{`targets.dev.network`}},
		{"an override brig does not take", strings.Replace(minimal, "profile: claude", "profile: claude\n    overrides: {network: offline}", 1),
			[]string{`targets.dev.overrides.network`}},
		{"an unknown alias field", strings.Replace(minimal, "{from: builtin:claude-code}", "{from: builtin:claude-code, image: x}", 1),
			[]string{`profiles.claude.image`}},
		{"a key in the wrong case", strings.Replace(minimal, "targets:", "Targets:", 1), []string{`"Targets"`}},
		{"a nested key in the wrong case", strings.Replace(minimal, "profile: claude", "Profile: claude", 1),
			[]string{`targets.dev.Profile`}},
		{"a key given twice", minimal + "apiVersion: brig.sh/v1alpha1\n", []string{`apiVersion`, `already set`}},
		{"an alias given twice", strings.Replace(minimal, "  claude: {from: builtin:claude-code}", "  claude: {from: builtin:claude-code}\n  claude: {from: builtin:codex}", 1),
			[]string{`"claude"`, `already set`}},
		{"a target given twice", minimal + "  dev:\n    profile: claude\n", []string{`"dev"`, `already set`}},
		{"a second document", minimal + "---\napiVersion: brig.sh/v1alpha1\n", []string{"more than one YAML document"}},
		{"another apiVersion", strings.Replace(minimal, "brig.sh/v1alpha1", "brig.sh/v1", 1), []string{`apiVersion`, `"brig.sh/v1"`}},
		{"no apiVersion", strings.Replace(minimal, "apiVersion: brig.sh/v1alpha1\n", "", 1), []string{"apiVersion is required"}},
		{"no targets", "apiVersion: brig.sh/v1alpha1\nprofiles:\n  claude: {from: builtin:claude-code}\n", []string{"targets", "at least one"}},
		{"an empty targets block", strings.Replace(minimal, "targets:\n  dev:\n    profile: claude\n    policies: [agent]\n", "targets: {}\n", 1),
			[]string{"targets", "at least one"}},
		{"a defaultTarget that names no target", minimal + "defaultTarget: prod\n", []string{"defaultTarget", `"prod"`}},
		{"a profile alias with two forms", strings.Replace(minimal, "{from: builtin:claude-code}", "{from: builtin:claude-code, file: c.yaml}", 1),
			[]string{"profiles.claude", "from and file"}},
		{"a profile alias with every form", strings.Replace(minimal, "{from: builtin:claude-code}", "{from: builtin:claude-code, file: c.yaml, inline: {name: x}}", 1),
			[]string{"profiles.claude", "from and file and inline"}},
		{"a profile alias with no form", strings.Replace(minimal, "{from: builtin:claude-code}", "{}", 1),
			[]string{"profiles.claude", "one of from, file or inline"}},
		{"a profile alias with no value", strings.Replace(minimal, "{from: builtin:claude-code}", "", 1),
			[]string{"profiles.claude", "one of from, file or inline"}},
		{"a from in another scheme", strings.Replace(minimal, "builtin:claude-code", "user:claude-code", 1),
			[]string{"profiles.claude.from", `"user:claude-code"`, "builtin:"}},
		{"a bare from", strings.Replace(minimal, "builtin:claude-code", "claude-code", 1),
			[]string{"profiles.claude.from", `"claude-code"`, "builtin:"}},
		{"a from with no name", strings.Replace(minimal, "builtin:claude-code", "\"builtin:\"", 1),
			[]string{"profiles.claude.from"}},
		{"a policy alias with two forms", strings.Replace(minimal, "{file: agent.yaml}", "{file: agent.yaml, inline: {name: x}}", 1),
			[]string{"policies.agent", "file and inline"}},
		{"a policy alias with no form", strings.Replace(minimal, "{file: agent.yaml}", "{}", 1),
			[]string{"policies.agent", "one of file or inline"}},
		{"a policy alias with from", strings.Replace(minimal, "{file: agent.yaml}", "{from: builtin:x}", 1),
			[]string{"policies.agent.from"}},
		{"an inline profile that is not a mapping", strings.Replace(minimal, "{from: builtin:claude-code}", "{inline: claude-code}", 1),
			[]string{"profiles.claude.inline", "mapping"}},
		{"an inline policy that is a list", strings.Replace(minimal, "{file: agent.yaml}", "{inline: [a]}", 1),
			[]string{"policies.agent.inline", "mapping"}},
		{"a target with no profile", strings.Replace(minimal, "    profile: claude\n", "", 1),
			[]string{"targets.dev.profile", "required"}},
		{"a target naming a missing profile", strings.Replace(minimal, "profile: claude", "profile: codex", 1),
			[]string{"targets.dev.profile", `"codex"`}},
		{"a target naming a missing policy", strings.Replace(minimal, "[agent]", "[agent, github]", 1),
			[]string{"targets.dev.policies", `"github"`}},
		{"a target naming one policy twice", strings.Replace(minimal, "[agent]", "[agent, agent]", 1),
			[]string{"targets.dev.policies", `"agent"`, "twice"}},
		{"policies with no value", strings.Replace(minimal, "[agent]", "", 1), []string{"targets.dev.policies", "no value"}},
		{"overrides with no value", strings.Replace(minimal, "profile: claude", "profile: claude\n    overrides:", 1),
			[]string{"targets.dev.overrides", "no value"}},
		{"zero cpus", strings.Replace(minimal, "profile: claude", "profile: claude\n    overrides: {cpus: 0}", 1),
			[]string{"targets.dev.overrides.cpus"}},
		{"negative mem", strings.Replace(minimal, "profile: claude", "profile: claude\n    overrides: {mem: -1}", 1),
			[]string{"targets.dev.overrides.mem"}},
		{"an empty image", strings.Replace(minimal, "profile: claude", "profile: claude\n    overrides: {image: \"\"}", 1),
			[]string{"targets.dev.overrides.image"}},
		{"an image the runtime would read as a flag", strings.Replace(minimal, "profile: claude", "profile: claude\n    overrides: {image: \"--privileged\"}", 1),
			[]string{"targets.dev.overrides.image"}},
		{"an image with a space", strings.Replace(minimal, "profile: claude", "profile: claude\n    overrides: {image: \"a b\"}", 1),
			[]string{"targets.dev.overrides.image"}},
		// YAML reads an unquoted 0600 as the number 384, and the decoder would
		// hand a string field "384". In a guest argument that silently changes
		// the command, so a scalar that is not a string is refused.
		{"an unquoted number in args", strings.Replace(minimal, "profile: claude", "profile: claude\n    args: [--mode, 0600]", 1),
			[]string{"targets.dev.args[1]", "quote"}},
		{"an unquoted boolean in args", strings.Replace(minimal, "profile: claude", "profile: claude\n    args: [yes]", 1),
			[]string{"targets.dev.args[0]", "quote"}},
		{"an unquoted number as a file", strings.Replace(minimal, "{file: agent.yaml}", "{file: 0644}", 1),
			[]string{"policies.agent.file", "quote"}},
		{"an alias name in capitals", strings.Replace(minimal, "  claude: {from", "  Claude: {from", 1),
			[]string{`"Claude"`}},
		// on: is the boolean true in YAML 1.1, so the alias would be named
		// "true" and nothing written as on could reach it.
		{"an alias name YAML reads as a boolean", strings.Replace(minimal, "  agent: {file", "  on: {file", 1),
			[]string{`"true"`}},
		{"a target name with a slash", strings.Replace(minimal, "  dev:\n", "  dev/x:\n", 1), []string{`"dev/x"`}},
		{"a name with a newline", minimal + "name: \"a\\nb\"\n", []string{"name"}},
		// A merge key that repeats an alias is still a key given twice.
		{"a merge key that repeats an alias", strings.Replace(minimal, "targets:\n", "targets:\n  <<: {dev: {profile: claude}}\n", 1),
			[]string{`"dev"`, "already set"}},
		{"a list at the top", "- apiVersion: brig.sh/v1alpha1\n", nil},
		{"an empty file", "", []string{"apiVersion is required"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.doc))
			if err == nil {
				t.Fatalf("Parse accepted:\n%s", c.doc)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

// A file reference is a path under the project root, written one way. Each
// of these is either outside the root, or a second spelling of a path that
// would give one file two keys in the snapshot.
func TestParseRefusesAFilePathThatIsNotPlain(t *testing.T) {
	for _, path := range []string{
		`""`, "/etc/passwd", "../outside.yaml", "a/../../outside.yaml", "a/../b.yaml",
		"./agent.yaml", "a//b.yaml", "a/", ".", `'a\b.yaml'`, `'..\outside.yaml'`, "~/agent.yaml",
	} {
		for _, form := range []string{"profiles", "policies"} {
			doc := strings.Replace(minimal, "{file: agent.yaml}", "{file: "+path+"}", 1)
			if form == "profiles" {
				doc = strings.Replace(minimal, "{from: builtin:claude-code}", "{file: "+path+"}", 1)
			}
			_, err := Parse([]byte(doc))
			if err == nil {
				t.Errorf("%s: file %s was accepted", form, path)
				continue
			}
			// An empty file: reads as no file: at all, which is refused as an
			// alias with no form; any other path is refused by name.
			if !strings.Contains(err.Error(), form+".") || (path != `""` && !strings.Contains(err.Error(), ".file")) {
				t.Errorf("%s: file %s: error %q does not name the field", form, path, err)
			}
		}
	}
}

// A path with a control character is refused, and the error quotes it. The
// error reaches the reader's terminal, and a raw escape sequence there could
// clear the screen or rewrite what it says.
func TestParseRefusesAFilePathWithAControlCharacter(t *testing.T) {
	for _, path := range []string{`"\e[2Jagent.yaml"`, `"a\tb.yaml"`, `"a\x7fb.yaml"`} {
		doc := strings.Replace(minimal, "{file: agent.yaml}", "{file: "+path+"}", 1)
		_, err := Parse([]byte(doc))
		if err == nil {
			t.Errorf("file %q was accepted", path)
			continue
		}
		if strings.IndexFunc(err.Error(), unicode.IsControl) >= 0 || !strings.Contains(err.Error(), "policies.agent.file") {
			t.Errorf("file %q: error %q", path, err)
		}
	}
}

// A Brigfile larger than the cap is refused before any decode.
func TestParseRefusesAnOversizeDocument(t *testing.T) {
	doc := minimal + "# " + strings.Repeat("x", MaxFileSize) + "\n"
	_, err := Parse([]byte(doc))
	if err == nil || !strings.Contains(err.Error(), "MiB") {
		t.Fatalf("Parse of %d bytes: %v", len(doc), err)
	}
}

// The YAML library bounds alias expansion: a document whose decode is almost
// all alias is refused as "excessive aliasing" (go.yaml.in/yaml/v2,
// decode.go). This is a billion-laughs document of nine levels of nine; were
// the bound gone it would expand to 9^9 strings, and the test would run out
// of memory rather than finish. The time limit is loose on purpose: it is
// there to fail loudly if the refusal ever comes after the expansion.
func TestParseRefusesAnAliasBomb(t *testing.T) {
	var b strings.Builder
	b.WriteString(minimal)
	b.WriteString("x0: &x0 [lol, lol, lol, lol, lol, lol, lol, lol, lol]\n")
	for i := 1; i <= 8; i++ {
		prev := "*x" + string(rune('0'+i-1))
		b.WriteString("x" + string(rune('0'+i)) + ": &x" + string(rune('0'+i)) + " [")
		b.WriteString(strings.TrimSuffix(strings.Repeat(prev+", ", 9), ", "))
		b.WriteString("]\n")
	}
	start := time.Now()
	_, err := Parse([]byte(b.String()))
	if err == nil {
		t.Fatal("the alias bomb was accepted")
	}
	if !strings.Contains(err.Error(), "excessive aliasing") {
		t.Errorf("error %q is not the library's alias bound", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("refusing the bomb took %v", d)
	}
}

// Anchors and aliases that stay small are ordinary YAML and still load.
func TestParseAcceptsASmallAlias(t *testing.T) {
	doc := strings.Replace(minimal, "    policies: [agent]\n", "    policies: &pol [agent]\n  review:\n    profile: claude\n    policies: *pol\n", 1)
	p, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := p.Targets["review"].Policies; len(got) != 1 || got[0] != "agent" {
		t.Errorf("targets.review.policies = %v", got)
	}
}
