package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/brigfile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/secret"
)

// projectBrigfile has two targets: dev, the default, on the built-in
// claude-code with one policy, a cpus override and default arguments; and
// ci, on a profile the repository wrote, with nothing overridden.
const projectBrigfile = `apiVersion: brig.sh/v1alpha1
name: demo
defaultTarget: dev
profiles:
  claude: {from: builtin:claude-code}
  tool: {file: .brig/tool.yaml}
policies:
  agent: {file: .brig/agent.yaml}
targets:
  dev:
    profile: claude
    policies: [agent]
    overrides: {cpus: 2}
    args: ["--model", "opus"]
  ci:
    profile: tool
`

const projectTool = `name: tool
binary: tool
image: example.com/tool:1
guestHome: /root
mem: 1024
cpus: 1
`

const projectAgentPolicy = `apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: {name: claude-api}
spec:
  egress:
    default: deny
    allow: [{host: api.anthropic.com}]
`

// brigHost is a host on which nothing but the Brigfile is there to be read:
// scratch profile, state and guest home directories, no runtime on PATH, and
// a project that is the root of a Git worktree holding files. The working
// directory is that project. It returns the project, with its links resolved.
func brigHost(t *testing.T, files map[string]string) string {
	t.Helper()
	scratchHost(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A .git of its own, so that discovery does not depend on whether the
	// temporary directory sits inside a checkout on this machine.
	files[".git/HEAD"] = "ref: refs/heads/main\n"
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	return dir
}

func demoProject(t *testing.T) string {
	t.Helper()
	return brigHost(t, map[string]string{
		brigfile.FileName:  projectBrigfile,
		".brig/tool.yaml":  projectTool,
		".brig/agent.yaml": projectAgentPolicy,
	})
}

func exitOf(t *testing.T, err error, want int) {
	t.Helper()
	if got := exitCode(err); got != want {
		t.Errorf("exit %d, want %d (err: %v)", got, want, err)
	}
}

func TestTargetLsListsTheTargets(t *testing.T) {
	demoProject(t)
	out, err := captureStdout(t, func() error { return run([]string{"target", "ls"}) })
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out, "\n")
	var dev, ci string
	for _, l := range lines {
		switch {
		case strings.Contains(l, " dev "):
			dev = l
		case strings.Contains(l, " ci "):
			ci = l
		}
	}
	if !strings.HasPrefix(dev, "* dev ") || !strings.Contains(dev, "claude") ||
		!strings.Contains(dev, "builtin:claude-code") || !strings.Contains(dev, "agent") {
		t.Errorf("the dev row is not marked default with its profile and policy:\n%s", out)
	}
	if !strings.HasPrefix(ci, "  ci ") || !strings.Contains(ci, "file:.brig/tool.yaml") {
		t.Errorf("the ci row is unmarked with its profile source:\n%s", out)
	}
	if !strings.Contains(out, "* the default target") {
		t.Errorf("the marker is not explained:\n%s", out)
	}
}

// Without a defaultTarget no row is marked and the marker is not explained.
func TestTargetLsMarksNoDefaultWhenThereIsNone(t *testing.T) {
	brigHost(t, map[string]string{brigfile.FileName: strings.Replace(projectBrigfile, "defaultTarget: dev\n", "", 1),
		".brig/tool.yaml": projectTool, ".brig/agent.yaml": projectAgentPolicy})
	out, err := captureStdout(t, func() error { return run([]string{"target", "ls"}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "*") {
		t.Errorf("a target is marked default with no defaultTarget:\n%s", out)
	}
}

type targetRowDoc struct {
	Name    string `json:"name"`
	Default bool   `json:"default"`
	Profile struct {
		Alias  string `json:"alias"`
		Source string `json:"source"`
	} `json:"profile"`
	Policies []struct {
		Alias  string `json:"alias"`
		Source string `json:"source"`
	} `json:"policies"`
}

func TestTargetLsJSON(t *testing.T) {
	demoProject(t)
	for _, args := range [][]string{{"--json", "target", "ls"}, {"target", "ls", "--json"}} {
		out, err := captureStdout(t, func() error { return run(args) })
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		var doc struct {
			APIVersion string         `json:"apiVersion"`
			Kind       string         `json:"kind"`
			Data       []targetRowDoc `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		if doc.APIVersion != jsonAPIVersion || doc.Kind != "TargetList" || len(doc.Data) != 2 {
			t.Fatalf("%v: envelope %s %s with %d rows", args, doc.APIVersion, doc.Kind, len(doc.Data))
		}
		ci, dev := doc.Data[0], doc.Data[1]
		if ci.Name != "ci" || ci.Default || ci.Profile.Alias != "tool" || ci.Profile.Source != "file:.brig/tool.yaml" ||
			len(ci.Policies) != 0 {
			t.Errorf("%v: ci row %+v", args, ci)
		}
		if dev.Name != "dev" || !dev.Default || dev.Profile.Source != "builtin:claude-code" ||
			len(dev.Policies) != 1 || dev.Policies[0].Alias != "agent" || dev.Policies[0].Source != "file:.brig/agent.yaml" {
			t.Errorf("%v: dev row %+v", args, dev)
		}
	}
}

func TestConfigShowsTheProject(t *testing.T) {
	dir := demoProject(t)
	out, err := captureStdout(t, func() error { return run([]string{"config"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"demo",
		filepath.Join(dir, brigfile.FileName),
		"default target  dev",
		"claude  builtin:claude-code",
		"tool    file:.brig/tool.yaml",
		"agent  file:.brig/agent.yaml",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("config does not show %q:\n%s", want, out)
		}
	}
}

func TestConfigTargetShowsWhereEachValueComesFrom(t *testing.T) {
	demoProject(t)
	out, err := captureStdout(t, func() error { return run([]string{"config", "--target", "dev"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"target    dev (default)",
		"profile   claude  builtin:claude-code",
		"cpus      2",
		"from target dev",
		"mem       4096",
		"from profile claude",
		"agent  file:.brig/agent.yaml",
		`"--model" "opus"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("config --target dev does not show %q:\n%s", want, out)
		}
	}
	// The overridden field names the target, and the untouched ones the
	// profile, each on its own line.
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "cpus") && !strings.Contains(l, "from target dev"):
			t.Errorf("cpus does not come from the target: %q", l)
		case strings.HasPrefix(l, "mem") && !strings.Contains(l, "from profile claude"):
			t.Errorf("mem does not come from the profile: %q", l)
		case strings.HasPrefix(l, "image") && !strings.Contains(l, "from profile claude"):
			t.Errorf("image does not come from the profile: %q", l)
		}
	}
}

type configDoc struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Data       struct {
		Name          string `json:"name"`
		Path          string `json:"path"`
		Root          string `json:"root"`
		DefaultTarget string `json:"defaultTarget"`
		Profiles      []struct {
			Alias           string `json:"alias"`
			Source          string `json:"source"`
			IgnoredUserFile string `json:"ignoredUserFile"`
		} `json:"profiles"`
		Policies []struct {
			Alias  string `json:"alias"`
			Source string `json:"source"`
		} `json:"policies"`
		Targets []targetRowDoc `json:"targets"`
		Target  *struct {
			Name    string `json:"name"`
			Default bool   `json:"default"`
			Profile struct {
				Alias           string `json:"alias"`
				Source          string `json:"source"`
				IgnoredUserFile string `json:"ignoredUserFile"`
			} `json:"profile"`
			CPUs struct {
				Value int    `json:"value"`
				From  string `json:"from"`
			} `json:"cpus"`
			Mem struct {
				Value int    `json:"value"`
				From  string `json:"from"`
			} `json:"mem"`
			Image struct {
				Value string `json:"value"`
				From  string `json:"from"`
			} `json:"image"`
			Policies []struct {
				Alias  string `json:"alias"`
				Source string `json:"source"`
			} `json:"policies"`
			Args []string `json:"args"`
		} `json:"target"`
	} `json:"data"`
}

func TestConfigJSON(t *testing.T) {
	dir := demoProject(t)
	out, err := captureStdout(t, func() error { return run([]string{"config", "--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var doc configDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	d := doc.Data
	if doc.Kind != "Config" || d.Name != "demo" || d.Root != dir || d.Path != filepath.Join(dir, brigfile.FileName) ||
		d.DefaultTarget != "dev" || len(d.Profiles) != 2 || len(d.Policies) != 1 || len(d.Targets) != 2 || d.Target != nil {
		t.Errorf("config --json: %s", out)
	}

	out, err = captureStdout(t, func() error { return run([]string{"--json", "config", "--target", "dev"}) })
	if err != nil {
		t.Fatal(err)
	}
	doc = configDoc{}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	tg := doc.Data.Target
	if tg == nil {
		t.Fatalf("config --target dev --json has no target:\n%s", out)
	}
	if tg.Name != "dev" || !tg.Default || tg.Profile.Alias != "claude" || tg.Profile.Source != "builtin:claude-code" {
		t.Errorf("target head: %+v", tg)
	}
	if tg.CPUs.Value != 2 || tg.CPUs.From != "target" || tg.Mem.Value != 4096 || tg.Mem.From != "profile" ||
		tg.Image.From != "profile" || !strings.HasPrefix(tg.Image.Value, "ghcr.io/brig-sh/claude-code") {
		t.Errorf("target values: %+v %+v %+v", tg.CPUs, tg.Mem, tg.Image)
	}
	if len(tg.Policies) != 1 || tg.Policies[0].Source != "file:.brig/agent.yaml" ||
		strings.Join(tg.Args, " ") != "--model opus" {
		t.Errorf("target policies and args: %+v %v", tg.Policies, tg.Args)
	}
	if tg.Profile.IgnoredUserFile != "" {
		t.Errorf("an ignored user file is named with none in the profile directory: %q", tg.Profile.IgnoredUserFile)
	}
}

// A file of the reader's own that overrides claude-code changes what `brig
// run claude-code` boots. A Brigfile that says builtin:claude-code does not
// read it, and config says so, but only when there is such a file.
func TestConfigNotesAnIgnoredUserOverride(t *testing.T) {
	demoProject(t)
	for _, args := range [][]string{{"config"}, {"config", "--target", "dev"}} {
		out, err := captureStdout(t, func() error { return run(args) })
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "ignore") {
			t.Errorf("%v notes an override that is not there:\n%s", args, out)
		}
	}

	userFile := filepath.Join(os.Getenv("BRIG_PROFILE_DIR"), "claude-code.yaml")
	if err := os.WriteFile(userFile, []byte("name: claude-code\nbinary: claude\nimage: example.com/mine:1\n"+
		"guestHome: /root\nmem: 1024\ncpus: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"config"}, {"config", "--target", "dev"}} {
		out, err := captureStdout(t, func() error { return run(args) })
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, userFile) || !strings.Contains(out, "builtin:claude-code") ||
			!strings.Contains(out, "ignore") {
			t.Errorf("%v does not note that %s is ignored:\n%s", args, userFile, out)
		}
		// The Brigfile's builtin: is what config resolves, not the file.
		if strings.Contains(out, "example.com/mine:1") {
			t.Errorf("%v resolved the user's file:\n%s", args, out)
		}
	}
	out, err := captureStdout(t, func() error { return run([]string{"config", "--target", "dev", "--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var doc configDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Data.Target.Profile.IgnoredUserFile != userFile || doc.Data.Profiles[0].IgnoredUserFile != userFile {
		t.Errorf("the JSON does not name the ignored file:\n%s", out)
	}
}

// With no Brigfile the three read verbs say where brig looked and what to do,
// and exit with the no-such-thing code.
func TestNoBrigfile(t *testing.T) {
	dir := brigHost(t, map[string]string{"sub/keep": ""})
	t.Chdir(filepath.Join(dir, "sub"))
	for _, args := range [][]string{{"target", "ls"}, {"config"}, {"config", "--target", "dev"}} {
		_, err := captureStdout(t, func() error { return run(args) })
		if err == nil {
			t.Fatalf("%v succeeded with no Brigfile", args)
		}
		exitOf(t, err, exitNotFound)
		for _, want := range []string{dir, "brig init", "-f"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%v: %q does not name %q", args, err, want)
			}
		}
	}
}

// -f names a file that is not there: the same class, naming the path.
func TestNoBrigfileAtTheNamedPath(t *testing.T) {
	brigHost(t, map[string]string{})
	_, err := captureStdout(t, func() error { return run([]string{"target", "ls", "-f", "nope/Brigfile"}) })
	exitOf(t, err, exitNotFound)
	if err == nil || !strings.Contains(err.Error(), "nope/Brigfile") {
		t.Errorf("err = %v", err)
	}
}

// A Brigfile that does not load is reported as the loader words it, with its
// path and the field, and is a general failure rather than a missing one.
func TestBrokenBrigfile(t *testing.T) {
	dir := brigHost(t, map[string]string{brigfile.FileName: "apiVersion: brig.sh/v1alpha1\ntargets:\n  dev: {profile: nope}\n"})
	for _, args := range [][]string{{"target", "ls"}, {"config"}, {"config", "--target", "dev"}} {
		_, err := captureStdout(t, func() error { return run(args) })
		exitOf(t, err, exitFailure)
		if err == nil || !strings.Contains(err.Error(), filepath.Join(dir, brigfile.FileName)) ||
			!strings.Contains(err.Error(), "targets.dev.profile") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
	// A file the Brigfile names that is missing is a broken Brigfile, not a
	// missing one.
	brigHost(t, map[string]string{brigfile.FileName: projectBrigfile, ".brig/tool.yaml": projectTool})
	_, err := captureStdout(t, func() error { return run([]string{"target", "ls"}) })
	exitOf(t, err, exitFailure)
}

func TestConfigUnknownTargetListsTheTargets(t *testing.T) {
	demoProject(t)
	_, err := captureStdout(t, func() error { return run([]string{"config", "--target", "prod"}) })
	exitOf(t, err, exitNotFound)
	if err == nil || !strings.Contains(err.Error(), `"prod"`) || !strings.Contains(err.Error(), "ci, dev") {
		t.Errorf("err = %v", err)
	}
}

// -f selects a file by path, and its directory is the project root: the files
// it names resolve beside it, not beside the working directory.
func TestDashFSelectsABrigfile(t *testing.T) {
	dir := brigHost(t, map[string]string{
		"ci/Brigfile.ci":      projectBrigfile,
		"ci/.brig/tool.yaml":  projectTool,
		"ci/.brig/agent.yaml": projectAgentPolicy,
	})
	for _, args := range [][]string{
		{"target", "ls", "-f", "ci/Brigfile.ci"},
		{"target", "ls", "--file", filepath.Join(dir, "ci", "Brigfile.ci")},
		{"config", "-f", "ci/Brigfile.ci", "--target", "ci"},
		{"config", "--target", "ci", "--file=ci/Brigfile.ci"},
	} {
		out, err := captureStdout(t, func() error { return run(args) })
		if err != nil {
			t.Errorf("%v: %v", args, err)
			continue
		}
		if !strings.Contains(out, "tool") {
			t.Errorf("%v:\n%s", args, out)
		}
	}
	out, err := captureStdout(t, func() error { return run([]string{"config", "-f", "ci/Brigfile.ci", "--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var doc configDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Data.Root != filepath.Join(dir, "ci") || doc.Data.Path != filepath.Join(dir, "ci", "Brigfile.ci") {
		t.Errorf("root %q, path %q", doc.Data.Root, doc.Data.Path)
	}
}

// From a subdirectory of a worktree the Brigfile at the root is the one read,
// and one nearer the working directory is not.
func TestDiscoveryFromASubdirectoryReadsTheRoot(t *testing.T) {
	dir := brigHost(t, map[string]string{
		brigfile.FileName:          projectBrigfile,
		".brig/tool.yaml":          projectTool,
		".brig/agent.yaml":         projectAgentPolicy,
		"a/" + brigfile.FileName:   "apiVersion: brig.sh/v1alpha1\nprofiles: {x: {from: builtin:claude-code}}\ntargets: {planted: {profile: x}}\n",
		"a/b/" + brigfile.FileName: "not even YAML: [",
	})
	t.Chdir(filepath.Join(dir, "a", "b"))
	out, err := captureStdout(t, func() error { return run([]string{"target", "ls"}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "planted") || !strings.Contains(out, "dev") ||
		!strings.Contains(out, filepath.Join(dir, brigfile.FileName)) {
		t.Errorf("target ls from a/b:\n%s", out)
	}
}

func TestInitWritesAStarterThatLoads(t *testing.T) {
	dir := brigHost(t, map[string]string{"sub/keep": ""})
	t.Chdir(filepath.Join(dir, "sub"))
	out, err := captureStdout(t, func() error { return run([]string{"init"}) })
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, brigfile.FileName)
	if !strings.Contains(out, path) || !strings.Contains(out, "brig target ls") {
		t.Errorf("init does not name the file and the next command:\n%s", out)
	}
	l, err := brigfile.Load(path)
	if err != nil {
		t.Fatalf("the starter does not load: %v", err)
	}
	tg, err := l.Target("dev")
	if err != nil || l.Project.DefaultTarget != "dev" || tg.Profile.Source.String() != "builtin:claude-code" {
		t.Errorf("the starter: default %q, dev %+v, %v", l.Project.DefaultTarget, tg.Profile.Source, err)
	}
	// And the verbs read it back from where init put it.
	if _, err := captureStdout(t, func() error { return run([]string{"config", "--target", "dev"}) }); err != nil {
		t.Errorf("config of the starter: %v", err)
	}
}

// The starter carries the directory's name. A name YAML would read as a
// number or a boolean, or that needs quoting, still loads as written.
func TestInitStarterLoadsWhateverTheDirectoryIsCalled(t *testing.T) {
	for _, name := range []string{"2026", "yes", "on", "a b", `quo"te`, "x: y", "#hash", "ünï"} {
		t.Run(name, func(t *testing.T) {
			blob := starterBrigfile(name)
			p, err := brigfile.Parse(blob)
			if err != nil {
				t.Fatalf("%v\n%s", err, blob)
			}
			if p.Name != name {
				t.Errorf("name %q, want %q", p.Name, name)
			}
		})
	}
	// A control character cannot be in the name at all, so the line is left
	// out rather than written to fail.
	if _, err := brigfile.Parse(starterBrigfile("a\x1bb")); err != nil {
		t.Errorf("a name with a control character: %v", err)
	}
}

func TestInitWithDashFWritesThatFile(t *testing.T) {
	dir := brigHost(t, map[string]string{"ci/keep": ""})
	out, err := captureStdout(t, func() error { return run([]string{"init", "-f", "ci/Brigfile.ci"}) })
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ci", "Brigfile.ci")
	if _, err := brigfile.Load(path); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "-f ci/Brigfile.ci") {
		t.Errorf("the next command does not carry -f:\n%s", out)
	}
	if _, err := os.Lstat(filepath.Join(dir, brigfile.FileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("init -f also wrote the default Brigfile: %v", err)
	}
}

// init never replaces a file, and never writes through a link: not a link to
// a file, and not a dangling link, which an existence check followed by a
// create would write through to wherever it points.
func TestInitRefusesWhatIsThere(t *testing.T) {
	t.Run("a file", func(t *testing.T) {
		dir := brigHost(t, map[string]string{brigfile.FileName: "mine\n"})
		_, err := captureStdout(t, func() error { return run([]string{"init"}) })
		if err == nil || !strings.Contains(err.Error(), "already") {
			t.Fatalf("err = %v", err)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, brigfile.FileName)); string(b) != "mine\n" {
			t.Errorf("the file was changed: %q", b)
		}
	})
	t.Run("a link to a file", func(t *testing.T) {
		dir := brigHost(t, map[string]string{})
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte("theirs\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, brigfile.FileName)); err != nil {
			t.Fatal(err)
		}
		_, err := captureStdout(t, func() error { return run([]string{"init"}) })
		if err == nil || !strings.Contains(err.Error(), "already") {
			t.Fatalf("err = %v", err)
		}
		if b, _ := os.ReadFile(outside); string(b) != "theirs\n" {
			t.Errorf("the link's target was written: %q", b)
		}
	})
	t.Run("a dangling link", func(t *testing.T) {
		dir := brigHost(t, map[string]string{})
		outside := filepath.Join(t.TempDir(), "not-yet")
		if err := os.Symlink(outside, filepath.Join(dir, brigfile.FileName)); err != nil {
			t.Fatal(err)
		}
		_, err := captureStdout(t, func() error { return run([]string{"init"}) })
		if err == nil || !strings.Contains(err.Error(), "already") {
			t.Fatalf("err = %v", err)
		}
		if _, err := os.Lstat(outside); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("init created the link's target: %v", err)
		}
	})
}

// listing is every entry under dir with its size and modification time, so
// that a write, a create or a delete anywhere under it shows as a difference.
func listing(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, rel+" "+fi.Mode().String()+" "+fi.ModTime().String()+" "+
			strings.Repeat("#", int(min(fi.Size(), 64))))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// The Brigfile verbs read the Brigfile and the files it names, and nothing
// else is touched: no runtime is looked for, no store is opened, telemetry is
// not started, and no directory brig keeps gains or loses a file. The runtime
// setting would fail any detection, so the verbs also work with no runtime.
func TestBrigfileVerbsTouchNothing(t *testing.T) {
	dir := demoProject(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BRIG_RUNTIME", "no-such-runtime")
	t.Setenv("BRIG_RUNTIME_BIN", filepath.Join(home, "no-such-binary"))
	if err := os.WriteFile(filepath.Join(os.Getenv("BRIG_PROFILE_DIR"), "claude-code.yaml"),
		[]byte("name: claude-code\nbinary: claude\nimage: example.com/mine:1\nguestHome: /root\nmem: 1024\ncpus: 1\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	swap(t, &detectRuntime, func() (runtime.Runtime, error) {
		t.Fatal("a Brigfile verb looked for a runtime")
		return nil, nil
	})
	swap(t, &detectRuntimeFor, func(runtime.Preference) (runtime.Runtime, error) {
		t.Fatal("a Brigfile verb looked for a runtime")
		return nil, nil
	})
	swap(t, &openStore, func() (secret.Store, error) {
		t.Fatal("a Brigfile verb opened the secret store")
		return nil, nil
	})
	swap(t, &startTelemetry, func(command string, _ bool) {
		t.Fatalf("a Brigfile verb started telemetry as %q", command)
	})

	dirs := []string{dir, home, os.Getenv("BRIG_PROFILE_DIR"), os.Getenv("BRIG_STATE_DIR"),
		os.Getenv("BRIG_WORKSPACE"), os.Getenv("BRIG_GATEWAY_DIR")}
	before := map[string][]string{}
	for _, d := range dirs {
		before[d] = listing(t, d)
	}
	for _, args := range [][]string{
		{"target", "ls"}, {"--json", "target", "ls"},
		{"config"}, {"config", "--json"},
		{"config", "--target", "dev"}, {"config", "--target", "ci", "--json"},
	} {
		if _, err := captureStdout(t, func() error { return run(args) }); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	for _, d := range dirs {
		if got := listing(t, d); strings.Join(got, "\n") != strings.Join(before[d], "\n") {
			t.Errorf("%s changed:\nbefore:\n%s\nafter:\n%s", d, strings.Join(before[d], "\n"), strings.Join(got, "\n"))
		}
	}

	// init writes the one file and nothing else.
	if err := os.Remove(filepath.Join(dir, brigfile.FileName)); err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		before[d] = listing(t, d)
	}
	if _, err := captureStdout(t, func() error { return run([]string{"init"}) }); err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		want := before[d]
		got := listing(t, d)
		if d == dir {
			var kept []string
			for _, l := range got {
				if !strings.HasPrefix(l, brigfile.FileName+" ") && !strings.HasPrefix(l, ". ") {
					kept = append(kept, l)
				}
			}
			got = kept
			var wantKept []string
			for _, l := range want {
				if !strings.HasPrefix(l, ". ") {
					wantKept = append(wantKept, l)
				}
			}
			want = wantKept
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("init changed %s beyond the Brigfile:\nbefore:\n%s\nafter:\n%s", d,
				strings.Join(want, "\n"), strings.Join(got, "\n"))
		}
	}
}

// The verbs this change adds are the only ones that read a Brigfile. A run
// or a plan in a worktree with a broken one is not affected by it.
func TestPlanDoesNotReadABrigfile(t *testing.T) {
	planHost(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string]string{".git/HEAD": "", brigfile.FileName: "not even YAML: ["} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	if _, err := captureStdout(t, func() error { return run([]string{"plan", "planned"}) }); err != nil {
		t.Errorf("plan in a worktree with a broken Brigfile: %v", err)
	}
}

// What the verbs refuse on their own lines is a usage error.
func TestBrigfileVerbsRefuseStrayWords(t *testing.T) {
	demoProject(t)
	for _, args := range [][]string{
		{"target"},
		{"target", "rm"},
		{"target", "ls", "dev"},
		{"target", "ls", "--target", "dev"},
		{"config", "dev"},
		{"config", "--nope"},
		{"init", "--json"},
		{"init", "extra"},
	} {
		_, err := captureStdout(t, func() error { return run(args) })
		exitOf(t, err, exitUsage)
	}
	// --json in the global position: init has none.
	_, err := captureStdout(t, func() error { return run([]string{"--json", "init"}) })
	exitOf(t, err, exitUsage)
}

func TestBrigfileVerbsAnswerHelp(t *testing.T) {
	demoProject(t)
	for _, args := range [][]string{
		{"target", "--help"}, {"target", "ls", "--help"}, {"help", "target"},
		{"config", "--target", "dev", "-h"}, {"init", "--help"},
	} {
		out, err := captureStdout(t, func() error { return run(args) })
		if err != nil || !strings.Contains(out, "usage:") {
			t.Errorf("%v: %v\n%s", args, err, out)
		}
	}
}

// A string the repository wrote reaches the terminal of whoever runs config.
// One holding a control character is printed quoted, so it cannot move the
// cursor or recolour the screen.
func TestConfigQuotesControlCharacters(t *testing.T) {
	brigHost(t, map[string]string{brigfile.FileName: `apiVersion: brig.sh/v1alpha1
profiles:
  claude: {from: builtin:claude-code}
targets:
  dev:
    profile: claude
    args: ["\e[2Jgone"]
`})
	for _, args := range [][]string{{"config", "--target", "dev"}, {"config", "--target", "dev", "--json"}} {
		out, err := captureStdout(t, func() error { return run(args) })
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if strings.ContainsRune(out, '\x1b') {
			t.Errorf("%v: an escape character reached the output: %q", args, out)
		}
	}

	// A file path is printed too, in the source column and in the error for
	// a file that is missing. The loader refuses one with a control character,
	// and its error quotes the path.
	brigHost(t, map[string]string{brigfile.FileName: `apiVersion: brig.sh/v1alpha1
profiles:
  tool: {file: "\e[2Jtool.yaml"}
targets:
  dev: {profile: tool}
`})
	for _, args := range [][]string{{"config"}, {"target", "ls"}} {
		out, err := captureStdout(t, func() error { return run(args) })
		exitOf(t, err, exitFailure)
		if err == nil || strings.ContainsRune(err.Error()+out, '\x1b') || !strings.Contains(err.Error(), "control character") {
			t.Errorf("%v: err %q, out %q", args, err, out)
		}
	}
}

func TestCompletionOffersTheBrigfileVerbs(t *testing.T) {
	demoProject(t)
	cases := []struct {
		words     []string
		directive string
		exactly   []string
	}{
		{[]string{"tar"}, dirNames, []string{"target"}},
		{[]string{"con"}, dirNames, []string{"config"}},
		{[]string{"in"}, dirNames, []string{"info", "init"}},
		{[]string{"target", ""}, dirNames, []string{"ls"}},
		{[]string{"target", "ls", "-"}, dirNames, []string{"--file", "--json", "-f"}},
		{[]string{"target", "ls", "-f", ""}, dirFiles, nil},
		{[]string{"config", "-"}, dirNames, []string{"--file", "--json", "--target", "-f"}},
		{[]string{"config", "--target", ""}, dirNames, []string{"ci", "dev"}},
		{[]string{"config", "--json", "--target", "d"}, dirNames, []string{"dev"}},
		{[]string{"config", "--target=d"}, dirNames, []string{"--target=dev"}},
		{[]string{"config", "-f", ""}, dirFiles, nil},
		{[]string{"config", "--target", "dev", ""}, dirNone, nil},
		{[]string{"init", "-"}, dirNames, []string{"--file", "-f"}},
		{[]string{"init", "--file", ""}, dirFiles, nil},
		{[]string{"init", ""}, dirNone, nil},
	}
	for _, tc := range cases {
		directive, got := complete(tc.words)
		if directive != tc.directive || strings.Join(got, " ") != strings.Join(tc.exactly, " ") {
			t.Errorf("complete(%q) = %s %v, want %s %v", tc.words, directive, got, tc.directive, tc.exactly)
		}
	}
}

// Target names come from the Brigfile -f names when the line has one, and a
// Brigfile that does not load completes nothing and says nothing.
func TestTargetCompletionReadsOnlyTheBrigfile(t *testing.T) {
	dir := brigHost(t, map[string]string{
		brigfile.FileName: "not even YAML: [",
		"ci/Brigfile.ci":  "apiVersion: brig.sh/v1alpha1\nprofiles: {x: {from: builtin:claude-code}}\ntargets: {nightly: {profile: x}}\n",
	})
	if directive, got := complete([]string{"config", "--target", ""}); directive != dirNone || got != nil {
		t.Errorf("a broken Brigfile completed %s %v", directive, got)
	}
	directive, got := complete([]string{"config", "-f", filepath.Join(dir, "ci", "Brigfile.ci"), "--target", ""})
	if directive != dirNames || strings.Join(got, " ") != "nightly" {
		t.Errorf("with -f: %s %v", directive, got)
	}
	directive, got = complete([]string{"config", "--file=ci/Brigfile.ci", "--target", "n"})
	if directive != dirNames || strings.Join(got, " ") != "nightly" {
		t.Errorf("with --file=: %s %v", directive, got)
	}
}
