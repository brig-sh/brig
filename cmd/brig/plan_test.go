package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/secret"
)

// readCountingFake is the CLI's fake store with every Read counted, because a
// Read is the decrypt `brig plan` must never make.
type readCountingFake struct {
	*fakeStore
	reads int
}

func (f *readCountingFake) Read(name string) ([]byte, error) {
	f.reads++
	return f.fakeStore.Read(name)
}

// planHost is a bare host with one profile that needs a stored secret, and a
// fake store in place of the keychain whose reads are counted.
func planHost(t *testing.T) *readCountingFake {
	t.Helper()
	scratchHost(t)
	t.Setenv("BRIG_PROFILE_DIR", writeProfile(t, "name: planned\nimage: i\nguestHome: /home/p\n"+
		"binary: p\nmem: 1024\ncpus: 2\n"+
		"secrets:\n  - gh\n  - tok\n"+
		"env:\n  - name: GH_TOKEN\n    ref: secrets.gh\n"+
		"volumes:\n  - kind: tmpfs\n    path: .config\n"+
		"files:\n  - ref: secrets.tok\n    path: .config/cred\n"))
	f := &readCountingFake{fakeStore: newFake(t)}
	old := openStore
	openStore = func() (secret.Store, error) { return f, nil }
	t.Cleanup(func() { openStore = old })
	return f
}

type planDoc struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Data       struct {
		Profile string `json:"profile"`
		Refused *struct {
			Reason string `json:"reason"`
		} `json:"refused"`
		Project *struct {
			Host string `json:"host"`
		} `json:"project"`
		Digest   string   `json:"digest"`
		Posture  string   `json:"posture"`
		Policies []string `json:"policies"`
		NoPolicy bool     `json:"noPolicy"`
		Egress   *struct {
			Default string `json:"default"`
		} `json:"egress"`
		Credentials []struct {
			Name     string `json:"name"`
			State    string `json:"state"`
			Required bool   `json:"required"`
			Reason   string `json:"reason"`
		} `json:"credentials"`
	} `json:"data"`
}

// A required secret missing from the store is where `brig info` exits 6. The
// plan exits 0, names the credential as unresolved, and never reads the store.
func TestPlanExitsZeroOnAMissingRequiredSecret(t *testing.T) {
	f := planHost(t)
	f.seed("tok", "PLANTED-FILE-VALUE")

	out, err := captureStdout(t, func() error { return run([]string{"--json", "plan", "planned"}) })
	if err != nil {
		t.Fatalf("brig plan refused a missing secret: %v (exit %d)", err, exitCode(err))
	}
	if f.reads != 0 {
		t.Errorf("brig plan read %d secret values", f.reads)
	}
	if strings.Contains(out, "PLANTED-FILE-VALUE") {
		t.Fatalf("brig plan printed a secret value:\n%s", out)
	}
	var doc planDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("brig plan --json did not print parseable JSON: %v\n%s", err, out)
	}
	if doc.APIVersion != jsonAPIVersion || doc.Kind != "Plan" {
		t.Errorf("envelope is %q/%q, want %q/Plan", doc.APIVersion, doc.Kind, jsonAPIVersion)
	}
	states := map[string]string{}
	for _, c := range doc.Data.Credentials {
		states[c.Name] = c.State
	}
	if states["GH_TOKEN"] != "unresolved" {
		t.Errorf("GH_TOKEN is %q, want unresolved: %s", states["GH_TOKEN"], out)
	}
	if states[".config/cred"] != "resolved" {
		t.Errorf(".config/cred is %q, want resolved: %s", states[".config/cred"], out)
	}
	if !strings.HasPrefix(doc.Data.Digest, "sha256:") {
		t.Errorf("the plan carries no digest: %s", out)
	}
}

// The text form is the same view, and it too never reads the store.
func TestPlanTextNeverReadsTheStore(t *testing.T) {
	f := planHost(t)
	f.seed("gh", "PLANTED-ENV-VALUE")
	f.seed("tok", "PLANTED-FILE-VALUE")

	out, err := captureStdout(t, func() error { return run([]string{"plan", "planned"}) })
	if err != nil {
		t.Fatalf("brig plan: %v", err)
	}
	if f.reads != 0 {
		t.Errorf("brig plan read %d secret values", f.reads)
	}
	if strings.Contains(out, "PLANTED") {
		t.Fatalf("brig plan printed a secret value:\n%s", out)
	}
	for _, want := range []string{"GH_TOKEN", ".config/cred", "DIGEST"} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan does not name %s:\n%s", want, out)
		}
	}
}

// --json reaches plan from both positions, as it reaches info.
func TestPlanTakesJSONInBothPositions(t *testing.T) {
	for _, args := range [][]string{{"--json", "plan", "planned"}, {"plan", "planned", "--json"}} {
		planHost(t)
		out, err := captureStdout(t, func() error { return run(args) })
		if err != nil {
			t.Fatalf("brig %s: %v", strings.Join(args, " "), err)
		}
		var doc planDoc
		if err := json.Unmarshal([]byte(out), &doc); err != nil || doc.Kind != "Plan" {
			t.Errorf("brig %s printed %q (%v)", strings.Join(args, " "), out, err)
		}
	}
	if !verbTakesGlobalJSON("plan", []string{"planned"}) {
		t.Error("verbTakesGlobalJSON does not know plan")
	}
	if !strings.Contains(jsonUnsupportedf("stop").Error(), "plan") {
		t.Error("the --json refusal does not name plan among the verbs that take it")
	}
}

// A policy attached to one session binds that session's plan and no other.
// The lookup runs through Load and the session's slug, the path #172 broke,
// so it is driven through the ref here and not set on a Config by hand.
func TestPlanListsThePolicyBoundToASession(t *testing.T) {
	planHost(t)
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	if _, err := captureStdout(t, func() error {
		return run([]string{"policy", "attach", "no-net", "planned", "-n", "x"})
	}); err != nil {
		t.Fatalf("policy attach: %v", err)
	}

	plan := func(ref string) planDoc {
		t.Helper()
		out, err := captureStdout(t, func() error { return run([]string{"--json", "plan", ref}) })
		if err != nil {
			t.Fatalf("brig plan %s: %v", ref, err)
		}
		var doc planDoc
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("brig plan %s: %v\n%s", ref, err, out)
		}
		return doc
	}

	bound := plan("planned@x").Data
	if bound.NoPolicy || strings.Join(bound.Policies, ",") != "no-net" {
		t.Errorf("planned@x: noPolicy %v, policies %v, want no-net", bound.NoPolicy, bound.Policies)
	}
	if bound.Egress == nil || bound.Egress.Default != "deny" {
		t.Errorf("planned@x: egress %+v, want default deny", bound.Egress)
	}
	if bound.Posture != "isolated" {
		t.Errorf("planned@x: posture %q, want isolated under a policy", bound.Posture)
	}

	free := plan("planned").Data
	if !free.NoPolicy || len(free.Policies) != 0 || free.Egress != nil {
		t.Errorf("planned: noPolicy %v, policies %v, egress %+v, want none bound",
			free.NoPolicy, free.Policies, free.Egress)
	}
}

// Completion offers the verb, a ref after it, and --json on its line.
func TestCompletionKnowsPlan(t *testing.T) {
	completionHost(t)
	_, got := complete([]string{""})
	if !contains(got, "plan") {
		t.Errorf("the verbs offered are %v, want plan among them", got)
	}
	if !refVerbs["plan"] {
		t.Error("plan does not complete a ref")
	}
	if _, got := complete([]string{"plan", "claude", "--j"}); !contains(got, "--json") {
		t.Errorf("after `brig plan claude --j` completion offers %v, want --json", got)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// bareHost is a host with one profile that declares no secret, so a run of it
// reaches the backend's check without opening a store. hull is the real
// adapter over a stub binary that holds no sandbox, so the plan and the run
// ask the same CanRun a Mac asks.
func bareHost(t *testing.T) {
	t.Helper()
	scratchHost(t)
	t.Setenv("BRIG_PROFILE_DIR", writeProfile(t, "name: bare\nimage: i\nguestHome: /home/b\n"+
		"binary: b\nmem: 1024\ncpus: 2\n"))
	bin := filepath.Join(t.TempDir(), "hull")
	stub := "#!/bin/sh\ncase \"$1\" in\n  inspect) echo \"instance not found: $2\" >&2; exit 1 ;;\nesac\nexit 0\n"
	if err := os.WriteFile(bin, []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIG_RUNTIME", "hull")
	t.Setenv("BRIG_RUNTIME_BIN", bin)
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	t.Setenv("BRIG_VERIFY", "off")
	t.Setenv("BRIG_NETWORK", "")
}

// A policy on vz is refused by `brig run`, because nothing on vz enforces it.
// The plan of that run says so, in the run's own words, and still exits 0:
// it printed what was asked for.
func TestPlanSaysARunIsRefusedAndStillExitsZero(t *testing.T) {
	bareHost(t)
	t.Setenv("BRIG_HYPERVISOR", "vz")
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	if _, err := captureStdout(t, func() error {
		return run([]string{"policy", "attach", "no-net", "bare", "-n", "x"})
	}); err != nil {
		t.Fatalf("policy attach: %v", err)
	}

	out, err := captureStdout(t, func() error { return run([]string{"--json", "plan", "bare@x"}) })
	if err != nil {
		t.Fatalf("brig plan of a refused run failed: %v (exit %d)", err, exitCode(err))
	}
	var doc planDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("brig plan --json: %v\n%s", err, out)
	}
	if doc.Data.Refused == nil || !strings.Contains(doc.Data.Refused.Reason, "cannot enforce the egress policy") {
		t.Fatalf("the plan does not say the run is refused:\n%s", out)
	}
	text, err := captureStdout(t, func() error { return run([]string{"plan", "bare@x"}) })
	if err != nil || !strings.HasPrefix(text, "REFUSED") {
		t.Errorf("the text plan does not open on the refusal (%v):\n%s", err, text)
	}

	_, err = captureStdout(t, func() error { return run([]string{"run", "bare@x", "-d"}) })
	if err == nil {
		t.Fatal("the run was not refused, so the case tests nothing")
	}
	if !strings.Contains(err.Error(), doc.Data.Refused.Reason) {
		t.Errorf("the plan says\n  %s\nand the run says\n  %v", doc.Data.Refused.Reason, err)
	}
}

// The plan previews a run, and the first `brig run claude ~/src/x` is the one
// worth previewing: no session remembers the directory yet. So plan reads the
// project and --no-project where run does, and refuses what run refuses.
func TestPlanTakesTheProjectARunWouldMount(t *testing.T) {
	planHost(t)
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return run([]string{"--json", "plan", "planned", dir}) })
	if err != nil {
		t.Fatalf("brig plan with a project: %v", err)
	}
	var doc planDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("brig plan --json: %v\n%s", err, out)
	}
	if doc.Data.Project == nil || doc.Data.Project.Host != real {
		t.Errorf("project = %+v, want %s", doc.Data.Project, real)
	}

	out, err = captureStdout(t, func() error { return run([]string{"--json", "plan", "planned", "--no-project"}) })
	if err != nil {
		t.Fatalf("brig plan --no-project: %v", err)
	}
	doc = planDoc{}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc.Data.Project != nil {
		t.Errorf("--no-project planned a project (%v):\n%s", err, out)
	}

	for _, args := range [][]string{
		{"plan", "planned", dir, "extra"},
		{"plan", "planned", dir, "--no-project"},
	} {
		_, err := captureStdout(t, func() error { return run(args) })
		var ue *usageError
		if !errors.As(err, &ue) {
			t.Errorf("brig %s: got %v, want a usage error", strings.Join(args, " "), err)
		}
	}

	if directive, _ := complete([]string{"plan", "claude", ""}); directive != dirDirs {
		t.Errorf("after `brig plan claude` completion offers %q, want directories", directive)
	}
	_, flags := complete([]string{"plan", "--"})
	if !contains(flags, "--no-project") || contains(flags, "--detach") {
		t.Errorf("plan's flags are %v, want --no-project and not --detach", flags)
	}
}

// The reason on a withheld credential quotes a secret-manager scheme, and
// never the start of a token that happens to have :// in it.
func TestPlanReasonDoesNotEchoAShellValue(t *testing.T) {
	scratchHost(t)
	t.Setenv("BRIG_PROFILE_DIR", writeProfile(t, "name: tokp\nimage: i\nguestHome: /home/p\n"+
		"binary: p\nmem: 1024\ncpus: 2\nenv:\n  - name: TOK\n    ref: env.TOK\n"))
	t.Setenv("TOK", "ghp_REALTOKENPREFIX://x")
	out, err := captureStdout(t, func() error { return run([]string{"--json", "plan", "tokp"}) })
	if err != nil {
		t.Fatalf("brig plan: %v", err)
	}
	if strings.Contains(out, "REALTOKENPREFIX") {
		t.Fatalf("the plan echoes part of a shell value:\n%s", out)
	}
	var doc planDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Data.Credentials) != 1 || doc.Data.Credentials[0].State != "withheld" {
		t.Errorf("TOK is %+v, want withheld", doc.Data.Credentials)
	}
}

// plan takes a project, so its refusals say so. A second word is refused
// as one more than plan takes, and a project that is not a directory names
// the run line as the place for an agent argument, since plan has no agent
// and refuses a word after -- as well.
func TestPlanRefusalsNameWhatPlanTakes(t *testing.T) {
	planHost(t)
	dir := t.TempDir()
	_, err := captureStdout(t, func() error { return run([]string{"plan", "planned", dir, "extra"}) })
	var ue *usageError
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), "`brig plan` takes a ref and a project directory") {
		t.Errorf("a second word: %v", err)
	}

	missing := filepath.Join(dir, "missing")
	_, err = captureStdout(t, func() error { return run([]string{"plan", "planned", missing}) })
	if err == nil {
		t.Fatal("a project that is not there was planned")
	}
	for _, want := range []string{"brig run and brig plan read the word after the ref", "on the brig run line"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	// After -- the word has no agent to go to, so plan refuses it too.
	_, err = captureStdout(t, func() error { return run([]string{"plan", "planned", "--", "missing"}) })
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), "`brig plan` takes a ref and a project directory") {
		t.Errorf("a word after -- on plan: %v, want the usage error that names what plan takes", err)
	}
	if !strings.Contains(completionUsage, "On run and plan the first word after the ref is the project") {
		t.Error("the completion help still says only run takes a project")
	}
}
