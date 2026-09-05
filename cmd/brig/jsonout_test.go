package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/secret"
)

// Every --json shape is the shared envelope: it parses, and it carries the
// apiVersion that pins it and the kind that names it. doctor already asserts
// this for its own; these are the four #7 adds.

// ls is SandboxList: the runtime once beside the list, and each sandbox with the
// ref split into agent and label. A sandbox with no ref is still in the list,
// ref-less, with its sandbox name -- the way the table shows it exists too.
func TestLsJSONShape(t *testing.T) {
	rows := []sandboxRow{
		{ref: "claude-code@refactor", name: "brig-claude-code-refactor", state: "stopped", workspace: "/ws/refactor"},
		{ref: "", name: "brig-mystery", state: "running", workspace: ""},
	}

	var buf bytes.Buffer
	if err := writeJSONDocument(&buf, "SandboxList", sandboxListData(rows, doctorRuntime{bin: "/opt/hull"})); err != nil {
		t.Fatal(err)
	}

	var doc struct {
		APIVersion string          `json:"apiVersion"`
		Kind       string          `json:"kind"`
		Data       sandboxListJSON `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("ls --json did not parse: %v\n%s", err, buf.String())
	}
	if doc.APIVersion != jsonAPIVersion || doc.Kind != "SandboxList" {
		t.Errorf("envelope is %q/%q, want %q/SandboxList", doc.APIVersion, doc.Kind, jsonAPIVersion)
	}
	if !doc.Data.Runtime.Available || doc.Data.Runtime.Kind != "hull" || doc.Data.Runtime.Bin != "/opt/hull" {
		t.Errorf("the runtime is not named beside the list: %+v", doc.Data.Runtime)
	}
	if len(doc.Data.Sandboxes) != 2 {
		t.Fatalf("the list has %d sandboxes, want 2: %+v", len(doc.Data.Sandboxes), doc.Data.Sandboxes)
	}
	first := doc.Data.Sandboxes[0]
	if first.Ref != "claude-code@refactor" || first.Agent != "claude-code" || first.Label != "refactor" {
		t.Errorf("the ref is not split into agent and label: %+v", first)
	}
	if first.Sandbox == "" || first.State == "" || first.Workspace == "" {
		t.Errorf("a listed sandbox is missing a field: %+v", first)
	}
	// The ref-less sandbox is present, named, and carries no invented ref.
	mystery := doc.Data.Sandboxes[1]
	if mystery.Ref != "" || mystery.Agent != "" || mystery.Sandbox != "brig-mystery" {
		t.Errorf("a sandbox with no ref is not listed the way the table shows it: %+v", mystery)
	}

	// No runtime is a complete document too: an empty list, the runtime marked
	// unavailable, no kind or bin claimed.
	empty := sandboxListData(nil, nil)
	if empty.Runtime.Available || empty.Runtime.Kind != "" {
		t.Errorf("no runtime is not marked unavailable: %+v", empty.Runtime)
	}
	if empty.Sandboxes == nil {
		t.Error("an empty list marshals to null rather than []")
	}
}

// agent ls is AgentList: each agent with its aliases, kind, image, origin, and
// -- for a file-backed one -- the file behind it.
func TestAgentLsJSONShape(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	own := []byte("name: mine\nimage: docker.io/me/mine:latest\n" +
		"guestHome: /home/mine\nbinary: m\nmem: 1\ncpus: 1\n")
	if err := os.WriteFile(filepath.Join(dir, "mine.yaml"), own, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error { return agentCmd([]string{"ls", "--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		APIVersion string      `json:"apiVersion"`
		Kind       string      `json:"kind"`
		Data       []agentJSON `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("agent ls --json did not parse: %v\n%s", err, out)
	}
	if doc.APIVersion != jsonAPIVersion || doc.Kind != "AgentList" {
		t.Errorf("envelope is %q/%q, want %q/AgentList", doc.APIVersion, doc.Kind, jsonAPIVersion)
	}

	byName := map[string]agentJSON{}
	for _, a := range doc.Data {
		byName[a.Name] = a
	}
	// A built-in, addressed by its alias: brig ships it, no file backs it, and it
	// carries its kind and image.
	cc, ok := byName["claude-code"]
	if !ok {
		t.Fatalf("claude-code is not in the listing: %+v", doc.Data)
	}
	if !cc.BuiltIn || cc.File != "" {
		t.Errorf("a built-in names a file or is not marked built in: %+v", cc)
	}
	if cc.Kind == "" || cc.Image == "" {
		t.Errorf("a listed agent is missing its kind or image: %+v", cc)
	}
	if !containsStr(cc.Aliases, "claude") {
		t.Errorf("claude-code does not list its alias: %+v", cc.Aliases)
	}
	// A profile of your own: file-backed, so it names its file and is not built
	// in.
	mine, ok := byName["mine"]
	if !ok {
		t.Fatalf("the file-backed profile is not in the listing: %+v", doc.Data)
	}
	if mine.BuiltIn || mine.File == "" {
		t.Errorf("a file-backed profile is marked built in or names no file: %+v", mine)
	}
}

// secret ls is SecretList: names, dates and provenance -- and never a value,
// which List does not decrypt and neither does the JSON. This is the assertion
// #7 calls a defect if it fails.
func TestSecretLsJSONShape(t *testing.T) {
	f := newFake(t)
	const planted = "super-secret-token-value-99"
	f.seedWithProvenance("gh-token", planted, secret.Provenance{From: "keychain:GitHub"})
	f.seed("hand-made", planted) // no provenance, the dash case

	var buf bytes.Buffer
	if err := secretCmd(&buf, []string{"ls", "--json"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), planted) {
		t.Fatalf("secret ls --json leaked a value:\n%s", buf.String())
	}

	var doc struct {
		APIVersion string       `json:"apiVersion"`
		Kind       string       `json:"kind"`
		Data       []secretJSON `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("secret ls --json did not parse: %v\n%s", err, buf.String())
	}
	if doc.APIVersion != jsonAPIVersion || doc.Kind != "SecretList" {
		t.Errorf("envelope is %q/%q, want %q/SecretList", doc.APIVersion, doc.Kind, jsonAPIVersion)
	}
	if len(doc.Data) != 2 {
		t.Fatalf("the list has %d secrets, want 2: %+v", len(doc.Data), doc.Data)
	}
	byName := map[string]secretJSON{}
	for _, s := range doc.Data {
		byName[s.Name] = s
	}
	if got := byName["gh-token"]; got.Provenance != "keychain:GitHub" || got.Modified == "" {
		t.Errorf("gh-token lost its provenance or date: %+v", got)
	}
	// A hand-created secret has no provenance, and the field is absent rather
	// than an invented word -- the same rule the text FROM column follows.
	if got := byName["hand-made"]; got.Provenance != "" {
		t.Errorf("a hand-created secret invented a provenance: %+v", got)
	}

	// An empty store is data: [] and exit 0, not the prose hint.
	g := newFake(t)
	_ = g
	var empty bytes.Buffer
	if err := secretCmd(&empty, []string{"ls", "--json"}); err != nil {
		t.Fatal(err)
	}
	var emptyDoc struct {
		Data []secretJSON `json:"data"`
	}
	if err := json.Unmarshal(empty.Bytes(), &emptyDoc); err != nil {
		t.Fatalf("empty secret ls --json did not parse: %v\n%s", err, empty.String())
	}
	if len(emptyDoc.Data) != 0 {
		t.Errorf("an empty store is not an empty list: %+v", emptyDoc.Data)
	}
	if strings.Contains(empty.String(), "no secrets yet") {
		t.Errorf("--json printed the prose hint:\n%s", empty.String())
	}
}

// info is Info, driven end to end through run() with no runtime on PATH: it
// exits 0 -- ErrNoRuntime is swallowed for info -- and prints a complete,
// parseable document with the runtime marked unavailable. Both spellings of the
// flag reach it.
func TestInfoJSONThroughRun(t *testing.T) {
	for _, args := range [][]string{
		{"--json", "info", "claude"}, // the global position, canonical
		{"info", "claude", "--json"}, // and the local one people type
	} {
		scratchHost(t)
		out, err := captureStdout(t, func() error { return run(args) })
		if err != nil {
			t.Fatalf("brig %s: %v", strings.Join(args, " "), err)
		}
		var doc struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Data       struct {
				Profile string `json:"profile"`
				Runtime struct {
					Available bool `json:"available"`
				} `json:"runtime"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("brig %s did not print parseable JSON: %v\n%s", strings.Join(args, " "), err, out)
		}
		if doc.APIVersion != jsonAPIVersion || doc.Kind != "Info" {
			t.Errorf("brig %s envelope is %q/%q, want %q/Info", strings.Join(args, " "), doc.APIVersion, doc.Kind, jsonAPIVersion)
		}
		if doc.Data.Profile == "" {
			t.Errorf("brig %s printed an incomplete object:\n%s", strings.Join(args, " "), out)
		}
		if doc.Data.Runtime.Available {
			t.Errorf("brig %s marked the runtime available with none on PATH", strings.Join(args, " "))
		}
	}
}

// The read verbs accept --json in both positions; the verbs with no JSON form
// refuse it in both, with a usage error (exit 2) that names the verbs that do.
func TestJSONPositionsAndRefusals(t *testing.T) {
	// Accepted, global and local. secret ls needs a store, so it gets a fake one;
	// the rest answer on a bare host.
	accept := [][]string{
		{"--json", "ls"}, {"ls", "--json"},
		{"--json", "agent", "ls"}, {"agent", "ls", "--json"},
		{"--json", "info", "claude"}, {"info", "claude", "--json"},
	}
	for _, args := range accept {
		scratchHost(t)
		if _, err := captureStdout(t, func() error { return run(args) }); err != nil {
			t.Errorf("brig %s was refused: %v", strings.Join(args, " "), err)
		}
	}
	// secret ls, both positions, against a fake store.
	for _, args := range [][]string{{"--json", "secret", "ls"}, {"secret", "ls", "--json"}} {
		scratchHost(t)
		newFake(t)
		if _, err := captureStdout(t, func() error { return run(args) }); err != nil {
			t.Errorf("brig %s was refused: %v", strings.Join(args, " "), err)
		}
	}

	// Refused, both positions, on the verbs with no JSON form. Exit 2, and the
	// message names a verb that does have one so the reader can move the flag.
	refuse := [][]string{
		{"--json", "stop", "claude"}, {"stop", "claude", "--json"},
		{"--json", "rm", "claude"}, {"rm", "claude", "--json"},
		{"--json", "run", "claude"}, {"run", "claude", "--json"},
		{"--json", "sh", "claude"}, {"sh", "claude", "--json"},
	}
	for _, args := range refuse {
		scratchHost(t)
		_, err := captureStdout(t, func() error { return run(args) })
		if err == nil {
			t.Errorf("brig %s was accepted", strings.Join(args, " "))
			continue
		}
		if code := exitCode(err); code != exitUsage {
			t.Errorf("brig %s exits %d, want %d: %v", strings.Join(args, " "), code, exitUsage, err)
		}
		if !strings.Contains(err.Error(), "ls") {
			t.Errorf("brig %s does not name a verb that takes --json: %v", strings.Join(args, " "), err)
		}
	}
}

// -q and --json together, and --verbose and --json together, are both fine: the
// notices go to stderr and the JSON to stdout, so a script gets clean JSON on
// stdout whichever of the other two it also asked for.
func TestJSONWithQuietAndVerbose(t *testing.T) {
	for _, args := range [][]string{
		{"-q", "--json", "ls"},
		{"--verbose", "--json", "ls"},
	} {
		scratchHost(t)
		out, err := captureStdout(t, func() error { return run(args) })
		if err != nil {
			t.Fatalf("brig %s: %v", strings.Join(args, " "), err)
		}
		var doc struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("brig %s did not put clean JSON on stdout: %v\n%s", strings.Join(args, " "), err, out)
		}
		if doc.Kind != "SandboxList" {
			t.Errorf("brig %s stdout is not the listing: %q", strings.Join(args, " "), out)
		}
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
