package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/brigfile"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/wrap"
)

// plantedValue is a secret value the store holds. It must reach the guest's
// environment and nothing a person or a script reads: not the approval
// store, not the prompt, not --json.
const plantedValue = "PLANTED-SECRET-VALUE-7f3a"

const runBrigfile = `apiVersion: brig.sh/v1alpha1
defaultTarget: dev
profiles:
  agent: {file: .brig/agent.yaml}
policies:
  egress: {file: .brig/egress.yaml}
targets:
  dev:
    profile: agent
    policies: [egress]
    args: ["--default"]
  review:
    profile: agent
    policies: [egress]
`

const runAgentProfile = "name: agent\nbinary: agent\nimage: example.com/agent:1\nguestHome: /root\n" +
	"mem: 1024\ncpus: 1\nsecrets:\n  - name: tok\n    required: false\nenv:\n  - name: TOKEN\n    ref: secrets.tok\n"

func egressPolicy(host string) string {
	return "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: egress}\nspec:\n  egress:\n" +
		"    default: deny\n    allow: [{host: " + host + "}]\n"
}

// targetRuntime is recordingRuntime that remembers which sandboxes it holds,
// so a second run finds the first one's sandbox.
type targetRuntime struct {
	recordingRuntime
	have map[string]bool
}

func (r *targetRuntime) Run(spec runtime.RunSpec) error {
	r.have[spec.Name] = true
	return r.recordingRuntime.Run(spec)
}
func (r *targetRuntime) Isolation(runtime.RunSpec) runtime.Isolation {
	return runtime.Isolation{Boundary: runtime.BoundaryVM, Detail: "hull, test"}
}
func (r *targetRuntime) Running(name string) (bool, error) { return r.have[name], nil }
func (r *targetRuntime) List() ([]runtime.Instance, error) {
	var list []runtime.Instance
	for name := range r.have {
		list = append(list, runtime.Instance{Name: name, State: "running"})
	}
	return list, nil
}
func (r *targetRuntime) Exists(name string) (bool, error) { return r.have[name], nil }
func (r *targetRuntime) Stop(string) error                { return nil }
func (r *targetRuntime) Remove(name string) error {
	delete(r.have, name)
	return nil
}

// runHost is a project with the Brigfile, the profile and the policy above,
// a runtime that records what it is handed, a store that holds the planted
// value, and no terminal. It returns the runtime.
func runHost(t *testing.T, files map[string]string) *targetRuntime {
	t.Helper()
	if files == nil {
		files = map[string]string{}
	}
	for rel, body := range map[string]string{brigfile.FileName: runBrigfile, ".brig/agent.yaml": runAgentProfile,
		".brig/egress.yaml": egressPolicy("api.example.com"), "src/main.go": "package main\n"} {
		if _, ok := files[rel]; !ok {
			files[rel] = body
		}
	}
	brigHost(t, files)
	rt := &targetRuntime{have: map[string]bool{}}
	swap(t, &detectRuntimeFor, func(runtime.Preference) (runtime.Runtime, error) { return rt, nil })
	withRuntime(t, rt)
	swap(t, &revokeRoutes, func(string) error { return nil })
	swap(t, &revokeAllRoutes, func() ([]string, error) { return nil, nil })
	t.Setenv("BRIG_VERIFY", "off")
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	t.Setenv("BRIG_GIT_IDENTITY", "0")
	for _, name := range []string{"BRIG_IMAGE", "BRIG_FORWARD_ENV", "BRIG_NETWORK", "BRIG_MEM", "BRIG_CPUS"} {
		t.Setenv(name, "")
		_ = os.Unsetenv(name)
	}
	f := newFake(t)
	f.seed("tok", plantedValue)
	noTerminal(t)
	return rt
}

// noTerminal takes the terminal away, and fails the test if anything reads
// an answer.
func noTerminal(t *testing.T) {
	t.Helper()
	swap(t, &approvalTerminal, func() bool { return false })
	swap(t, &approvalStdin, func() io.Reader {
		t.Error("the approval read an answer with no terminal to ask on")
		return strings.NewReader("")
	})
}

// answer puts a terminal in front of the approval, with r as what is typed.
// It returns a count of the reads of an answer.
func answer(t *testing.T, r io.Reader) *int {
	t.Helper()
	asked := 0
	swap(t, &approvalTerminal, func() bool { return true })
	swap(t, &approvalStdin, func() io.Reader { asked++; return r })
	return &asked
}

func approvalsPath() string { return filepath.Join(os.Getenv("BRIG_STATE_DIR"), "approvals.json") }

var digestPattern = regexp.MustCompile(`sha256:[0-9a-f]{64}`)

// digestIn is the approval digest a refusal names.
func digestIn(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("the run was not refused")
	}
	d := digestPattern.FindString(err.Error())
	if d == "" {
		t.Fatalf("the refusal names no digest: %v", err)
	}
	return d
}

// With no terminal and no approval, nothing boots: the runtime is not asked
// to start anything, nothing is stored, the exit is 8, and the refusal
// carries the digest that would approve the run. The plan is on stderr for
// whoever reads the log.
func TestRunTargetWithoutApprovalBootsNothing(t *testing.T) {
	rt := runHost(t, nil)
	out, stderr, err := runQuiet(t, "run", "--target", "dev")
	exitOf(t, err, 8)
	digestIn(t, err)
	if len(rt.runs) != 0 || len(rt.execs) != 0 {
		t.Errorf("a run with no approval reached the runtime: %d boots, %d execs", len(rt.runs), len(rt.execs))
	}
	if there(approvalsPath()) {
		t.Error("a refused run stored an approval")
	}
	if out != "" {
		t.Errorf("stdout: %q", out)
	}
	for _, want := range []string{"TARGET", "EGRESS", "api.example.com", "ARGS", `"--default"`,
		"APPROVAL", "--approve sha256:"} {
		if !strings.Contains(stderr+err.Error(), want) {
			t.Errorf("the refusal does not show %q:\n%s\n%v", want, stderr, err)
		}
	}
}

// No, an empty answer and a closed stdin are not yes. Each refuses with 8,
// and boots and stores nothing.
func TestRunTargetAnswersOtherThanYesRefuse(t *testing.T) {
	for name, typed := range map[string]string{"no": "n\n", "empty": "\n", "EOF": "", "word": "sure\n"} {
		t.Run(name, func(t *testing.T) {
			rt := runHost(t, nil)
			asked := answer(t, strings.NewReader(typed))
			_, _, err := runQuiet(t, "run", "--target", "dev")
			exitOf(t, err, 8)
			if *asked != 1 {
				t.Errorf("asked %d times", *asked)
			}
			if len(rt.runs) != 0 || len(rt.execs) != 0 || there(approvalsPath()) {
				t.Errorf("%q booted %d, ran %d, stored %v", typed, len(rt.runs), len(rt.execs), there(approvalsPath()))
			}
		})
	}
}

// Yes boots, stores the approval in the state directory and the session in
// projects.json, and the next run boots with no question.
func TestRunTargetYesBootsAndTheNextRunAsksNothing(t *testing.T) {
	rt := runHost(t, nil)
	root, _ := os.Getwd()
	asked := answer(t, strings.NewReader("y\n"))
	if _, stderr, err := runQuiet(t, "run", "--target", "dev"); err != nil {
		t.Fatalf("run: %v\n%s", err, stderr)
	}
	if *asked != 1 || len(rt.runs) != 1 || len(rt.execs) != 1 {
		t.Fatalf("asked %d, booted %d, ran %d", *asked, len(rt.runs), len(rt.execs))
	}
	if got := rt.execs[0].Cmd; !slices.Equal(got, []string{"agent", "--default"}) {
		t.Errorf("the agent ran %q, want the target's args", got)
	}
	if strings.HasPrefix(approvalsPath(), root) || !there(approvalsPath()) {
		t.Errorf("approvals.json at %s, project %s", approvalsPath(), root)
	}
	id := wrap.ProjectIdentity(root, "dev", "", "")
	if _, ok := wrap.LookupProject(id.Sandbox); !ok {
		t.Error("projects.json has no entry for the session")
	}
	if _, ok := wrap.Approved(id.Sandbox); !ok {
		t.Error("no approval is stored for the session")
	}

	noTerminal(t)
	if _, stderr, err := runQuiet(t, "run", "--target", "dev"); err != nil {
		t.Fatalf("second run: %v\n%s", err, stderr)
	} else if strings.Contains(stderr, "APPROVAL") {
		t.Errorf("the second run showed the approval again:\n%s", stderr)
	}
	if len(rt.execs) != 2 {
		t.Errorf("the second run ran %d times", len(rt.execs)-1)
	}
}

// approveDev approves `brig run --target dev` with the given extra words,
// through a yes at the terminal, and takes the terminal away again.
func approveDev(t *testing.T, line ...string) {
	t.Helper()
	answer(t, strings.NewReader("y\n"))
	args := append([]string{"run", "--target", "dev"}, line...)
	if _, stderr, err := runQuiet(t, args...); err != nil {
		t.Fatalf("brig %s: %v\n%s", strings.Join(args, " "), err, stderr)
	}
	noTerminal(t)
}

// changedAfter runs the line with no terminal, and returns what the refusal
// says changed. The line carries --recreate, so a change that the sandbox
// would have to be booted again for reaches the approval.
func changedAfter(t *testing.T, line ...string) string {
	t.Helper()
	_, stderr, err := runQuiet(t, append([]string{"run", "--target", "dev", "--recreate"}, line...)...)
	exitOf(t, err, 8)
	_, changes, ok := strings.Cut(stderr, "this differs from what you approved")
	if !ok {
		t.Fatalf("the refusal names no change:\n%s", stderr)
	}
	return changes
}

func writeProjectFile(t *testing.T, rel, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rel, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Each of these changes what the run may do, so each invalidates the
// approval, and the refusal names it.
func TestRunTargetApprovalIsInvalidatedByEachChange(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(t *testing.T) []string
		want   string
	}{
		{"one byte of a policy file", func(t *testing.T) []string {
			writeProjectFile(t, ".brig/egress.yaml", egressPolicy("api.example.org"))
			return nil
		}, "the file .brig/egress.yaml (policy egress) changed"},
		{"the Brigfile's overrides", func(t *testing.T) []string {
			writeProjectFile(t, brigfile.FileName, strings.Replace(runBrigfile, `args: ["--default"]`,
				"args: [\"--default\"]\n    overrides: {cpus: 4}", 1))
			return nil
		}, "the vCPUs 4, and were 1"},
		{"BRIG_FORWARD_ENV", func(t *testing.T) []string {
			t.Setenv("BRIG_FORWARD_ENV", "MY_VAR")
			return nil
		}, "BRIG_FORWARD_ENV is set"},
		{"BRIG_IMAGE", func(t *testing.T) []string {
			t.Setenv("BRIG_IMAGE", "example.com/other:2")
			return nil
		}, "the image example.com/other:2 (a tag), and was example.com/agent:1 (a tag)"},
		{"the args after --", func(t *testing.T) []string {
			return []string{"--", "--resume"}
		}, `the arguments: "--resume", and were "--default"`},
		{"--network", func(t *testing.T) []string {
			return []string{"--network", "offline"}
		}, "the network asked for: offline by --network, and was nothing"},
		{"a published port", func(t *testing.T) []string {
			return []string{"--publish", "3000"}
		}, "the published ports: 127.0.0.1:3000"},
	} {
		t.Run(c.name, func(t *testing.T) {
			runHost(t, nil)
			approveDev(t)
			line := c.change(t)
			if got := changedAfter(t, line...); !strings.Contains(got, c.want) {
				t.Errorf("the change list does not name %q:\n%s", c.want, got)
			}
		})
	}
}

// Editing a source file of the project is the agent's work, not a change to
// what the run may do. The approval stands.
func TestRunTargetApprovalSurvivesASourceEdit(t *testing.T) {
	rt := runHost(t, nil)
	approveDev(t)
	writeProjectFile(t, "src/main.go", "package main\n\nfunc main() {}\n")
	writeProjectFile(t, "README.md", "new file\n")
	if _, stderr, err := runQuiet(t, "run", "--target", "dev"); err != nil {
		t.Fatalf("run after a source edit: %v\n%s", err, stderr)
	}
	if len(rt.execs) != 2 {
		t.Errorf("ran %d times, want 2", len(rt.execs))
	}
}

// --approve with a digest that is not this run's refuses and says which
// digest is; with this run's digest it boots with no terminal, and stores
// the approval.
func TestRunTargetApproveTakesOnlyTheCurrentDigest(t *testing.T) {
	rt := runHost(t, nil)
	_, _, err := runQuiet(t, "run", "--target", "dev")
	current := digestIn(t, err)

	stale := "sha256:" + strings.Repeat("0", 64)
	_, stderr, err := runQuiet(t, "run", "--target", "dev", "--approve", stale)
	exitOf(t, err, 8)
	if !strings.Contains(err.Error(), stale) || !strings.Contains(err.Error(), current) {
		t.Errorf("the refusal does not name both digests: %v", err)
	}
	if len(rt.runs) != 0 || !strings.Contains(stderr, "APPROVAL") {
		t.Errorf("a stale digest booted %d, or showed no plan:\n%s", len(rt.runs), stderr)
	}

	// The digest covers the line: the same digest on a line with other args
	// is not this run's.
	_, _, err = runQuiet(t, "run", "--target", "dev", "--approve", current, "--", "--other")
	exitOf(t, err, 8)

	if _, stderr, err := runQuiet(t, "run", "--target", "dev", "--approve", current); err != nil {
		t.Fatalf("--approve with the current digest: %v\n%s", err, stderr)
	}
	if len(rt.runs) != 1 || !there(approvalsPath()) {
		t.Errorf("booted %d, stored %v", len(rt.runs), there(approvalsPath()))
	}
}

// A run brig refuses cannot be approved: two policies whose routes share a
// name cannot apply together. Nothing is asked, stored or booted, and the
// plan of the target still names what each policy asked for.
func TestRunTargetARefusedPlanCannotBeApproved(t *testing.T) {
	route := func(policy, upstream string) string {
		return "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: " + policy + "}\nspec:\n  routes:\n" +
			"    - {name: api, preset: github-token, upstream: \"" + upstream + "\", " +
			"credential: {secretRef: {name: tok}}}\n"
	}
	rt := runHost(t, map[string]string{
		brigfile.FileName: "apiVersion: brig.sh/v1alpha1\nprofiles:\n  agent: {file: .brig/agent.yaml}\n" +
			"policies:\n  one: {file: .brig/one.yaml}\n  two: {file: .brig/two.yaml}\n" +
			"targets:\n  dev: {profile: agent, policies: [one, two]}\n",
		".brig/one.yaml": route("one", "https://one.example"),
		".brig/two.yaml": route("two", "https://two.example"),
	})
	asked := answer(t, strings.NewReader("y\n"))
	_, _, err := runQuiet(t, "run", "--target", "dev")
	if err == nil || exitCode(err) == 0 || exitCode(err) == 8 {
		t.Fatalf("a refused plan: exit %d, %v", exitCode(err), err)
	}
	if *asked != 0 || len(rt.runs) != 0 || there(approvalsPath()) {
		t.Errorf("asked %d, booted %d, stored %v", *asked, len(rt.runs), there(approvalsPath()))
	}
	_, _, err = runQuiet(t, "run", "--target", "dev", "--approve", "sha256:"+strings.Repeat("1", 64))
	if exitCode(err) == 8 || len(rt.runs) != 0 {
		t.Errorf("--approve on a refused plan: exit %d, booted %d", exitCode(err), len(rt.runs))
	}

	plan := planTargetText(t, "--target", "dev")
	for _, want := range []string{"REFUSED", "route api to https://one.example with your secret tok",
		"route api to https://two.example with your secret tok"} {
		if !strings.Contains(plan, want) {
			t.Errorf("the plan of a route conflict does not show %q:\n%s", want, plan)
		}
	}
}

// What a run boots is what was approved, read once. Between the yes and the
// boot the Brigfile and the policy are rewritten on disk, as an agent in a
// running sandbox could, and the runtime is still handed the approved egress
// and image.
func TestRunTargetBootsTheApprovedSnapshot(t *testing.T) {
	rt := runHost(t, nil)
	answer(t, rewriteOnRead{t: t, then: strings.NewReader("y\n")})
	if _, stderr, err := runQuiet(t, "run", "--target", "dev"); err != nil {
		t.Fatalf("run: %v\n%s", err, stderr)
	}
	if len(rt.runs) != 1 {
		t.Fatalf("booted %d times", len(rt.runs))
	}
	spec := rt.runs[0]
	if spec.Image != "example.com/agent:1" {
		t.Errorf("booted image %s, want the approved one", spec.Image)
	}
	if spec.Egress.Default != "deny" || len(spec.Egress.Allow) != 1 || spec.Egress.Allow[0].Host != "api.example.com" {
		t.Errorf("booted egress %+v, want the approved rules", spec.Egress)
	}
	if spec.Mem != 1024 {
		t.Errorf("booted mem %d", spec.Mem)
	}
	if got := rt.execs[0].Cmd; !slices.Equal(got, []string{"agent", "--default"}) {
		t.Errorf("the agent ran %q, want the approved args", got)
	}
	// And the next run reads the rewritten files, which are not what was
	// approved.
	noTerminal(t)
	_, _, err := runQuiet(t, "run", "--target", "dev", "--recreate")
	exitOf(t, err, 8)
}

// rewriteOnRead rewrites the Brigfile and the policy when the answer is
// read: after the approval was shown and before anything boots.
type rewriteOnRead struct {
	t    *testing.T
	then io.Reader
}

func (r rewriteOnRead) Read(p []byte) (int, error) {
	writeProjectFile(r.t, ".brig/egress.yaml", "apiVersion: brig.sh/v1alpha1\nkind: Policy\n"+
		"metadata: {name: egress}\nspec:\n  egress:\n    default: allow\n")
	writeProjectFile(r.t, brigfile.FileName, strings.Replace(runBrigfile, `args: ["--default"]`,
		"args: [\"--evil\"]\n    overrides: {image: evil.example/x:1, mem: 8192}", 1))
	return r.then.Read(p)
}

// No secret value reaches what a person or a script reads: the approval
// store, the prompt, the refusal and the --json output. The value does
// reach the guest, which is how the test knows it was read.
func TestRunTargetShowsNoSecretValue(t *testing.T) {
	rt := runHost(t, nil)
	_, refusal, err := runQuiet(t, "--json", "run", "--target", "dev")
	exitOf(t, err, 8)
	asked := answer(t, strings.NewReader("y\n"))
	out, prompt, err := runQuiet(t, "run", "--target", "dev")
	if err != nil || *asked != 1 {
		t.Fatalf("run: %v, asked %d", err, *asked)
	}
	noTerminal(t)
	jsonOut, jsonErr, err := runQuiet(t, "--json", "run", "--target", "dev")
	if exitCode(err) != 0 {
		t.Fatalf("--json run: exit %d, %v", exitCode(err), err)
	}
	stored, err := os.ReadFile(approvalsPath())
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"approvals.json": string(stored), "the refusal": refusal,
		"the prompt": prompt + out, "--json": jsonOut + jsonErr} {
		if strings.Contains(text, plantedValue) {
			t.Errorf("%s holds the secret value", name)
		}
	}
	var guest bool
	for _, v := range rt.execs[0].Env {
		guest = guest || v.Value == plantedValue
	}
	if !guest {
		t.Error("the guest did not get the secret, so this test proves nothing")
	}
}

// An approval is for one session: target dev's does not approve target
// review, another session of dev, or dev in another checkout of the same
// repository.
func TestRunTargetApprovalIsForOneSession(t *testing.T) {
	runHost(t, nil)
	root, _ := os.Getwd()
	approveDev(t)
	for _, line := range [][]string{{"--target", "review"}, {"--target", "dev", "--session", "fix"}} {
		_, _, err := runQuiet(t, append([]string{"run"}, line...)...)
		exitOf(t, err, 8)
	}
	other := filepath.Join(t.TempDir(), "clone")
	for _, rel := range []string{brigfile.FileName, ".brig/agent.yaml", ".brig/egress.yaml"} {
		blob, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(other, filepath.Dir(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(other, rel), blob, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(other, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(other)
	_, _, err := runQuiet(t, "run", "--target", "dev")
	exitOf(t, err, 8)
}

// An approvals.json brig cannot read approves nothing, and is not written
// over, even after a yes.
func TestRunTargetCorruptApprovalsApproveNothing(t *testing.T) {
	rt := runHost(t, nil)
	approveDev(t)
	corrupt := []byte(`{"version": 99, "approvals": {}}`)
	if err := os.WriteFile(approvalsPath(), corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := runQuiet(t, "run", "--target", "dev")
	exitOf(t, err, 8)
	answer(t, strings.NewReader("y\n"))
	_, stderr, err := runQuiet(t, "run", "--target", "dev")
	if err != nil {
		t.Fatalf("a yes over an unreadable store: %v", err)
	}
	if !strings.Contains(stderr, "could not record the approval") {
		t.Errorf("no word about the store:\n%s", stderr)
	}
	if got, _ := os.ReadFile(approvalsPath()); string(got) != string(corrupt) {
		t.Errorf("approvals.json was written over: %s", got)
	}
	if len(rt.runs) != 2 {
		t.Errorf("booted %d times", len(rt.runs))
	}
}

// A state directory inside the project is refused before anything is
// asked: the guest could write its own approval there.
func TestRunTargetRefusesTheStateDirectoryInTheProject(t *testing.T) {
	rt := runHost(t, nil)
	root, _ := os.Getwd()
	t.Setenv("BRIG_STATE_DIR", filepath.Join(root, ".brig-state"))
	asked := answer(t, strings.NewReader("y\n"))
	_, _, err := runQuiet(t, "run", "--target", "dev")
	if err == nil || !strings.Contains(err.Error(), "state directory") {
		t.Fatalf("err %v", err)
	}
	if *asked != 0 || len(rt.runs) != 0 || there(filepath.Join(root, ".brig-state", "approvals.json")) {
		t.Errorf("asked %d, booted %d", *asked, len(rt.runs))
	}
}

// A sandbox booted with another configuration is not started again with
// this one. --recreate boots it again on the same guest home, and the entry
// stands.
func TestRunTargetRecreatesOnlyWhenAsked(t *testing.T) {
	rt := runHost(t, nil)
	root, _ := os.Getwd()
	approveDev(t, "-d")
	id := wrap.ProjectIdentity(root, "dev", "", "")
	before, ok := wrap.LookupProject(id.Sandbox)
	if !ok {
		t.Fatal("no entry after the first run")
	}
	writeProjectFile(t, ".brig/egress.yaml", egressPolicy("api.example.org"))

	answer(t, strings.NewReader("y\n"))
	_, _, err := runQuiet(t, "run", "--target", "dev", "-d")
	if err == nil || !strings.Contains(err.Error(), "brig run --target dev --recreate") ||
		!strings.Contains(err.Error(), "brig rm --target dev") {
		t.Fatalf("a changed configuration: %v", err)
	}
	if len(rt.runs) != 1 {
		t.Errorf("booted %d times", len(rt.runs))
	}

	answer(t, strings.NewReader("y\n"))
	if _, stderr, err := runQuiet(t, "run", "--target", "dev", "-d", "--recreate"); err != nil {
		t.Fatalf("--recreate: %v\n%s", err, stderr)
	}
	after, ok := wrap.LookupProject(id.Sandbox)
	if !ok || after.Home != before.Home || after.ConfigDigest == before.ConfigDigest {
		t.Errorf("after --recreate the entry is %+v (ok %v), was %+v", after, ok, before)
	}
	if len(rt.runs) != 2 || rt.runs[1].Shares[0].Host != before.Home {
		t.Errorf("booted %d times, home %v", len(rt.runs), rt.runs[len(rt.runs)-1].Shares)
	}
}

// rm --target removes the approval with the session.
func TestRmTargetRemovesTheApproval(t *testing.T) {
	runHost(t, nil)
	root, _ := os.Getwd()
	approveDev(t, "-d")
	id := wrap.ProjectIdentity(root, "dev", "", "")
	if _, ok := wrap.Approved(id.Sandbox); !ok {
		t.Fatal("no approval to remove")
	}
	if _, stderr, err := runQuiet(t, "rm", "--target", "dev"); err != nil {
		t.Fatalf("rm --target: %v\n%s", err, stderr)
	}
	if _, ok := wrap.Approved(id.Sandbox); ok {
		t.Error("rm --target left the approval")
	}
}

// A repository profile named claude-code is not your claude-code: the run
// boots in the target's own sandbox and home, and the agent gets no --name,
// whatever the session is called.
func TestRunTargetOnARepositoryClaudeCode(t *testing.T) {
	rt := runHost(t, map[string]string{
		brigfile.FileName: "apiVersion: brig.sh/v1alpha1\nprofiles:\n  mine: {file: .brig/claude.yaml}\n" +
			"targets:\n  dev: {profile: mine}\n",
		".brig/claude.yaml": repoClaudeProfile,
	})
	answer(t, strings.NewReader("y\n"))
	if _, stderr, err := runQuiet(t, "run", "--target", "dev", "--session", "fix"); err != nil {
		t.Fatalf("run: %v\n%s", err, stderr)
	}
	spec := rt.runs[0]
	if !strings.HasPrefix(spec.Name, "brig--dev.fix-") || strings.Contains(spec.Shares[0].Host, "brig-claude-code") {
		t.Errorf("sandbox %s, home %s", spec.Name, spec.Shares[0].Host)
	}
	if slices.Contains(rt.execs[0].Cmd, "--name") {
		t.Errorf("the agent got --name: %q", rt.execs[0].Cmd)
	}
}

// A bare `brig run` in a checkout whose Brigfile names a default target runs
// it, gated the same way. A Brigfile with no default target lists the
// targets and asks for one.
func TestBareRunUsesTheDefaultTarget(t *testing.T) {
	rt := runHost(t, nil)
	_, _, err := runQuiet(t, "run")
	exitOf(t, err, 8)
	answer(t, strings.NewReader("y\n"))
	if _, stderr, err := runQuiet(t, "run", "--", "one", "two"); err != nil {
		t.Fatalf("bare run: %v\n%s", err, stderr)
	}
	if got := rt.execs[0].Cmd; !slices.Equal(got, []string{"agent", "one", "two"}) {
		t.Errorf("the agent ran %q", got)
	}

	writeProjectFile(t, brigfile.FileName, strings.Replace(runBrigfile, "defaultTarget: dev\n", "", 1))
	_, _, err = runQuiet(t, "run")
	exitOf(t, err, 2)
	if err == nil || !strings.Contains(err.Error(), "dev, review") {
		t.Errorf("no default target: %v", err)
	}
}

// A line that names a target takes no profile, no project and no flag of
// the agent's before --.
func TestRunTargetLineTakesNoProfileOrStrayWord(t *testing.T) {
	runHost(t, nil)
	for _, line := range [][]string{
		{"run", "--target", "dev", "claude"},
		{"run", "--target", "dev", "--resume"},
		{"run", "--target", "dev", "--home", "/tmp"},
		{"run", "--target", "dev", "--no-project"},
		{"plan", "--target", "dev", "--approve", "x"},
		{"rm", "--target", "dev", "--recreate"},
	} {
		_, _, err := runQuiet(t, line...)
		exitOf(t, err, 2)
	}
}

// A run of a profile in a Brigfile checkout asks for no approval and boots
// as before.
func TestARunOfAProfileIsNeverAskedForApproval(t *testing.T) {
	rt := runHost(t, nil)
	dir := os.Getenv("BRIG_PROFILE_DIR")
	body := "name: faker\nimage: img\nguestHome: /home/faker\nbinary: agent\nmem: 1024\ncpus: 1\n"
	if err := os.WriteFile(filepath.Join(dir, "faker.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := profile.Load(dir); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := runQuiet(t, "run", "faker"); err != nil {
		t.Fatalf("run faker: %v\n%s", err, stderr)
	} else if strings.Contains(stderr, "APPROVAL") || strings.Contains(stderr, "approve") {
		t.Errorf("a run of a profile mentioned an approval:\n%s", stderr)
	}
	if len(rt.runs) != 1 || there(approvalsPath()) {
		t.Errorf("booted %d, stored %v", len(rt.runs), there(approvalsPath()))
	}
}

// The stored subject can be read back, and its digest is its own.
func TestTheStoredApprovalIsItsSubject(t *testing.T) {
	runHost(t, nil)
	approveDev(t)
	blob, err := os.ReadFile(approvalsPath())
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Version   int                            `json:"version"`
		Approvals map[string]wrap.ApprovalRecord `json:"approvals"`
	}
	if err := json.Unmarshal(blob, &f); err != nil || f.Version != 1 || len(f.Approvals) != 1 {
		t.Fatalf("approvals.json: %v %s", err, blob)
	}
	for key, r := range f.Approvals {
		if r.Subject.Sandbox != key || r.Digest != wrap.ApprovalDigest(r.Subject) || r.Target != "dev" {
			t.Errorf("record %s: %+v", key, r)
		}
	}
}

// A required secret the store does not hold refuses the run before it asks,
// with the credential exit, and the advice creates the secret by its name.
func TestRunTargetAMissingSecretIsRefusedBeforeAsking(t *testing.T) {
	rt := runHost(t, nil)
	writeProjectFile(t, ".brig/agent.yaml", strings.Replace(strings.Replace(runAgentProfile,
		"  - name: tok\n    required: false\n", "  - absent\n", 1), "secrets.tok", "secrets.absent", 1))
	asked := answer(t, strings.NewReader("y\n"))
	_, _, err := runQuiet(t, "run", "--target", "dev")
	exitOf(t, err, 6)
	if err == nil || !strings.Contains(err.Error(), "brig secret create absent") || strings.Contains(err.Error(), "import") {
		t.Errorf("the refusal: %v", err)
	}
	if *asked != 0 || len(rt.runs) != 0 || there(approvalsPath()) {
		t.Errorf("asked %d, booted %d, stored %v", *asked, len(rt.runs), there(approvalsPath()))
	}
}
