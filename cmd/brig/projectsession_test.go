package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/brigfile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/wrap"
)

// projectRuntime holds a set of sandboxes by name and state, answers for one
// name as hull inspect does, and records what it was asked to stop and
// remove. Everything else it embeds is nil and panics: stop and rm have no
// business booting anything.
type projectRuntime struct {
	runtime.Runtime
	have    map[string]string
	stopped []string
	removed []string
	// unlisted are sandboxes the listing leaves out though they exist, as
	// hull's plain ps leaves out a stopped one.
	unlisted map[string]bool
}

func (r *projectRuntime) Kind() string { return "hull" }
func (r *projectRuntime) Bin() string  { return "hull" }
func (r *projectRuntime) List() ([]runtime.Instance, error) {
	var list []runtime.Instance
	for name, state := range r.have {
		if !r.unlisted[name] {
			list = append(list, runtime.Instance{Name: name, State: state})
		}
	}
	return list, nil
}
func (r *projectRuntime) Exists(name string) (bool, error) { _, ok := r.have[name]; return ok, nil }
func (r *projectRuntime) Stop(name string) error {
	r.stopped = append(r.stopped, name)
	if _, ok := r.have[name]; ok {
		r.have[name] = "stopped"
	}
	return nil
}
func (r *projectRuntime) Running(name string) (bool, error) { return r.have[name] == "running", nil }
func (r *projectRuntime) Remove(name string) error {
	r.removed = append(r.removed, name)
	delete(r.have, name)
	return nil
}
func (r *projectRuntime) LogsHint(name string) string { return "hull logs " + name }

// projectsHost is the demo project with the given runtime wired into both
// detection seams, and scratch gateway and policy directories. It returns
// the project root.
func projectsHost(t *testing.T, rt runtime.Runtime) string {
	t.Helper()
	root := demoProject(t)
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	swap(t, &detectRuntimeFor, func(runtime.Preference) (runtime.Runtime, error) { return rt, nil })
	withRuntime(t, rt)
	swap(t, &revokeRoutes, func(string) error { return nil })
	swap(t, &revokeAllRoutes, func() ([]string, error) { return nil, nil })
	return root
}

// seedProject records a session of target in root as a boot of it would, with
// a home holding guest state, and gives the runtime its sandbox. It returns
// the entry.
func seedProject(t *testing.T, rt *projectRuntime, root, target, session string) wrap.ProjectSession {
	t.Helper()
	id := wrap.ProjectIdentity(root, target, session, "")
	home := filepath.Join(os.Getenv("BRIG_STATE_DIR"), "homes", id.Sandbox)
	writeGuestState(t, home)
	ps := wrap.ProjectSession{Sandbox: id.Sandbox, Root: root, Target: target, Session: session,
		Home: home, Brigfile: filepath.Join(root, brigfile.FileName)}
	if err := wrap.RecordProject(ps); err != nil {
		t.Fatal(err)
	}
	rt.have[id.Sandbox] = "running"
	return ps
}

// seedProfile gives the runtime your own claude-code session, with an
// ephemeral home and its entry in sessions.json. It returns the home.
func seedProfile(t *testing.T, rt *projectRuntime) string {
	t.Helper()
	state := os.Getenv("BRIG_STATE_DIR")
	home := filepath.Join(state, "homes", "brig-claude-code")
	writeGuestState(t, home)
	index := `{"claude-code": {"home": "` + home + `", "sandbox": "brig-claude-code", "ephemeral": true}}`
	if err := os.WriteFile(filepath.Join(state, "sessions.json"), []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
	rt.have["brig-claude-code"] = "running"
	return home
}

func writeGuestState(t *testing.T, home string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "agent-state"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func there(path string) bool { _, err := os.Lstat(path); return err == nil }

// runQuiet runs a brig line and returns its stdout, its stderr and its error.
func runQuiet(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = captureStdout(t, func() error { return run(args) })
	})
	return out, stderr, err
}

// stop and rm --target reach a session from the project root, the target
// and the session name alone. The Brigfile can be gone, or no longer have the
// target, and the session is still found, stopped and removed with its home
// and its entry.
func TestStopAndRemoveATargetWithoutItsBrigfile(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, root string){
		"the Brigfile is deleted": func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, brigfile.FileName)); err != nil {
				t.Fatal(err)
			}
		},
		"the target is dropped": func(t *testing.T, root string) {
			body := "apiVersion: brig.sh/v1alpha1\nprofiles:\n  tool: {file: .brig/tool.yaml}\n" +
				"targets:\n  other: {profile: tool}\n"
			if err := os.WriteFile(filepath.Join(root, brigfile.FileName), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			rt := &projectRuntime{have: map[string]string{}}
			root := projectsHost(t, rt)
			dev := seedProject(t, rt, root, "dev", "")
			fix := seedProject(t, rt, root, "dev", "fix")
			change(t, root)
			if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Chdir(filepath.Join(root, "sub"))

			if _, _, err := runQuiet(t, "stop", "--target", "dev"); err != nil {
				t.Fatalf("stop --target dev: %v", err)
			}
			if !slices.Equal(rt.stopped, []string{dev.Sandbox}) {
				t.Errorf("stopped %q, want %s", rt.stopped, dev.Sandbox)
			}
			_, stderr, err := runQuiet(t, "rm", "--target", "dev", "--session", "fix")
			if err != nil {
				t.Fatalf("rm --target dev --session fix: %v", err)
			}
			if !slices.Equal(rt.removed, []string{fix.Sandbox}) {
				t.Errorf("removed %q, want %s", rt.removed, fix.Sandbox)
			}
			if !strings.Contains(stderr, "removed session fix of target dev and its guest home "+fix.Home) {
				t.Errorf("rm said:\n%s", stderr)
			}
			if there(fix.Home) {
				t.Error("the home of dev.fix is still there")
			}
			if _, ok := wrap.LookupProject(fix.Sandbox); ok {
				t.Error("the entry of dev.fix is still there")
			}
			if !there(filepath.Join(dev.Home, "agent-state")) {
				t.Error("rm of dev.fix took dev's home")
			}
			if _, ok := wrap.LookupProject(dev.Sandbox); !ok {
				t.Error("rm of dev.fix took dev's entry")
			}
		})
	}
}

// rm --target removes one session and nothing else: not the other target's
// sandbox, home or entry, and not your profile session.
func TestRemoveATargetLeavesEveryOtherSession(t *testing.T) {
	rt := &projectRuntime{have: map[string]string{}}
	root := projectsHost(t, rt)
	dev := seedProject(t, rt, root, "dev", "")
	ci := seedProject(t, rt, root, "ci", "")
	yours := seedProfile(t, rt)

	if _, _, err := runQuiet(t, "rm", "--target", "dev"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rt.removed, []string{dev.Sandbox}) {
		t.Errorf("removed %q, want only %s", rt.removed, dev.Sandbox)
	}
	if there(dev.Home) {
		t.Error("dev's home is still there")
	}
	if got, ok := wrap.LookupProject(ci.Sandbox); !ok || got != ci || !there(filepath.Join(ci.Home, "agent-state")) {
		t.Errorf("ci's entry %+v (%v) or home went with dev", got, ok)
	}
	if !there(filepath.Join(yours, "agent-state")) || wrap.WorkspaceOfSandbox("brig-claude-code") != yours {
		t.Error("rm --target dev touched your claude-code session")
	}
	if rt.have["brig-claude-code"] == "" || rt.have[ci.Sandbox] == "" {
		t.Errorf("the runtime lost a sandbox: %v", rt.have)
	}
}

// A repository profile called claude-code, under a target called claude-code,
// is a session of its own. Before it ever ran, stop and rm of that target
// find nothing, say which project and target they looked for, and leave your
// claude-code session alone. A profile-mode rm leaves the target's session
// alone in turn.
func TestARepositoryClaudeCodeNeverReachesYourSession(t *testing.T) {
	rt := &projectRuntime{have: map[string]string{}}
	root := projectsHost(t, rt)
	body := "apiVersion: brig.sh/v1alpha1\nprofiles:\n  repo: {file: .brig/claude.yaml}\n" +
		"targets:\n  claude-code: {profile: repo}\n"
	for rel, b := range map[string]string{brigfile.FileName: body, ".brig/claude.yaml": repoClaudeProfile} {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	yours := seedProfile(t, rt)

	for _, verb := range []string{"stop", "rm"} {
		_, _, err := runQuiet(t, verb, "--target", "claude-code")
		exitOf(t, err, exitNotFound)
		if err == nil || !strings.Contains(err.Error(), "target claude-code") || !strings.Contains(err.Error(), root) {
			t.Errorf("%s --target claude-code: %v", verb, err)
		}
	}
	if len(rt.stopped) != 0 || len(rt.removed) != 0 {
		t.Errorf("stopped %q and removed %q", rt.stopped, rt.removed)
	}
	if !there(filepath.Join(yours, "agent-state")) {
		t.Error("your home is gone")
	}

	target := seedProject(t, rt, root, "claude-code", "")
	if _, _, err := runQuiet(t, "rm", "claude-code"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rt.removed, []string{"brig-claude-code"}) {
		t.Errorf("rm claude-code removed %q", rt.removed)
	}
	if got, ok := wrap.LookupProject(target.Sandbox); !ok || got != target || !there(filepath.Join(target.Home, "agent-state")) {
		t.Error("rm claude-code touched the target's session")
	}
	rows := sandboxRows(mustList(t, rt), rt)
	for _, r := range rows {
		if r.name == target.Sandbox && (r.ref != "" || r.project == nil || r.project.Target != "claude-code") {
			t.Errorf("the target's sandbox is listed as %+v", r)
		}
	}
}

func mustList(t *testing.T, rt runtime.Runtime) []runtime.Instance {
	t.Helper()
	list, err := rt.List()
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// rm --target of a session the runtime reports missing forgets the session
// and keeps the home, as rm of a ref does: hull reports a sandbox whose
// metadata it cannot read as missing too.
func TestRemoveATargetTheRuntimeLost(t *testing.T) {
	rt := &projectRuntime{have: map[string]string{}}
	root := projectsHost(t, rt)
	dev := seedProject(t, rt, root, "dev", "")
	delete(rt.have, dev.Sandbox)

	_, stderr, err := runQuiet(t, "rm", "--target", "dev", "--dry-run")
	exitOf(t, err, exitNotFound)
	if _, ok := wrap.LookupProject(dev.Sandbox); !ok {
		t.Fatal("the dry run forgot the session")
	}
	if !strings.Contains(err.Error(), "`brig rm --target dev` would forget its session") ||
		!strings.Contains(stderr, "the guest home "+dev.Home+" stays") {
		t.Errorf("dry run: %v\n%s", err, stderr)
	}

	_, _, err = runQuiet(t, "rm", "--target", "dev")
	exitOf(t, err, exitNotFound)
	if _, ok := wrap.LookupProject(dev.Sandbox); ok {
		t.Error("the session is still recorded")
	}
	if !there(filepath.Join(dev.Home, "agent-state")) {
		t.Error("rm deleted the home of a sandbox the runtime may still hold")
	}
	if len(rt.removed) != 0 {
		t.Errorf("removed %q", rt.removed)
	}
	// Now nothing knows the session.
	_, _, err = runQuiet(t, "rm", "--target", "dev")
	exitOf(t, err, exitNotFound)
	if err == nil || !strings.Contains(err.Error(), "no target dev in the project "+root) {
		t.Errorf("the second rm: %v", err)
	}
}

// A stopped sandbox the listing leaves out is still found, through the
// runtime's answer for the one name.
func TestStopAndRemoveFindAStoppedSandboxTheListingLeftOut(t *testing.T) {
	rt := &projectRuntime{have: map[string]string{}, unlisted: map[string]bool{}}
	root := projectsHost(t, rt)
	id := wrap.ProjectIdentity(root, "dev", "", "")
	rt.have[id.Sandbox], rt.unlisted[id.Sandbox] = "stopped", true
	if _, _, err := runQuiet(t, "rm", "--target", "dev"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rt.removed, []string{id.Sandbox}) {
		t.Errorf("removed %q", rt.removed)
	}
}

// rm --all removes the sandbox of a target like any other of brig's, and its
// home and entry with it, so no entry is left that names a home that is gone.
// A session whose sandbox the runtime did not list keeps its entry and home:
// nothing confirmed that sandbox gone.
func TestRemoveAllTakesProjectSessionsWhole(t *testing.T) {
	rt := &projectRuntime{have: map[string]string{}, unlisted: map[string]bool{}}
	root := projectsHost(t, rt)
	dev := seedProject(t, rt, root, "dev", "")
	ci := seedProject(t, rt, root, "ci", "")
	rt.unlisted[ci.Sandbox] = true
	yours := seedProfile(t, rt)

	out, err := captureStdout(t, func() error { return run([]string{"rm", "--all", "--dry-run"}) })
	if err != nil || !strings.Contains(out, dev.Sandbox) || !strings.Contains(out, "target dev in "+root) {
		t.Errorf("the preview: %v\n%s", err, out)
	}
	if _, _, err := runQuiet(t, "rm", "--all", "-y"); err != nil {
		t.Fatal(err)
	}
	if there(dev.Home) || there(yours) {
		t.Errorf("a home stayed: dev %v, yours %v", there(dev.Home), there(yours))
	}
	if _, ok := wrap.LookupProject(dev.Sandbox); ok {
		t.Error("dev's entry names a home that is gone")
	}
	for _, ps := range wrap.ProjectSessions() {
		if !there(ps.Home) {
			t.Errorf("%s's entry names a home that is gone", ps.Sandbox)
		}
	}
	if _, ok := wrap.LookupProject(ci.Sandbox); !ok || !there(filepath.Join(ci.Home, "agent-state")) {
		t.Error("rm --all took the entry or home of a sandbox it did not remove")
	}
}

// --target, --session and -f stand where a ref would on stop and rm, so a
// profile beside them, or one of them without --target, is a usage error.
func TestTheTargetLineOfStopAndRemove(t *testing.T) {
	rt := &projectRuntime{have: map[string]string{}}
	projectsHost(t, rt)
	for _, args := range [][]string{
		{"stop", "--target", "dev", "claude"},
		{"rm", "claude", "--target", "dev"},
		{"stop", "--session", "fix"},
		{"rm", "claude", "--session", "fix"},
		{"rm", "-f", "Brigfile", "claude"},
		{"stop", "--target"},
		{"stop", "--target", ""},
		{"rm", "--target", "dev", "--session", "Fix!"},
		{"rm", "--target", "dev", "--session", ""},
		{"rm", "--all", "--target", "dev"},
		{"rm", "--target", "dev", "-y"},
		{"stop", "--target", "dev", "--json"},
		{"stop", "--target", "dev", "--home", t.TempDir()},
	} {
		_, _, err := runQuiet(t, args...)
		exitOf(t, err, exitUsage)
	}
	if len(rt.stopped) != 0 || len(rt.removed) != 0 {
		t.Errorf("a refused line acted: stopped %q, removed %q", rt.stopped, rt.removed)
	}
}

// The verbs that take a ref alone refuse --target by name, and none of them
// misreads the key or the sandbox name of a target's session as a ref.
func TestRefVerbsRefuseATargetAndItsNames(t *testing.T) {
	rt := &projectRuntime{have: map[string]string{}}
	root := projectsHost(t, rt)
	dev := seedProject(t, rt, root, "dev", "")
	state := os.Getenv("BRIG_STATE_DIR")
	index := `{"@p/` + strings.TrimPrefix(dev.Sandbox, "brig--dev-") + `": {"home": "` + dev.Home +
		`", "sandbox": "` + dev.Sandbox + `", "ephemeral": true}}`
	if err := os.WriteFile(filepath.Join(state, "sessions.json"), []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"info", "--target", "dev"}, {"sh", "--target=dev"}, {"logs", "--target", "dev"},
		{"info", "--session", "x", "claude"}, {"network", "ls", "--target", "dev"},
	} {
		_, _, err := runQuiet(t, args...)
		exitOf(t, err, exitUsage)
		if err == nil || !strings.Contains(err.Error(), "Brigfile target") {
			t.Errorf("%v: %v", args, err)
		}
	}
	key := "@p/" + strings.TrimPrefix(dev.Sandbox, "brig--dev-")
	for _, args := range [][]string{{"info", key}, {"logs", key}, {"rm", key}, {"stop", key}} {
		_, _, err := runQuiet(t, args...)
		exitOf(t, err, exitUsage)
	}
	for _, args := range [][]string{{"rm", dev.Sandbox}, {"stop", dev.Sandbox}, {"logs", dev.Sandbox}} {
		_, _, err := runQuiet(t, args...)
		exitOf(t, err, exitNotFound)
	}
	if len(rt.stopped) != 0 || len(rt.removed) != 0 {
		t.Errorf("stopped %q, removed %q", rt.stopped, rt.removed)
	}

	for _, words := range [][]string{{"stop", ""}, {"rm", ""}, {"run", ""}, {"logs", ""}} {
		if _, got := complete(words); slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "@p/") }) {
			t.Errorf("completion after %v offers %v", words, got)
		}
	}
	if _, got := complete([]string{"rm", "--t"}); !contains(got, "--target") {
		t.Errorf("rm does not offer --target: %v", got)
	}
	if _, got := complete([]string{"stop", "--target", ""}); strings.Join(got, " ") != "ci dev" {
		t.Errorf("stop --target completes %v", got)
	}
	if refOf(dev.Sandbox) != "" || hasSession("") {
		t.Errorf("refOf(%s) = %q", dev.Sandbox, refOf(dev.Sandbox))
	}
}

// brig ls lists a session of a target with its target, session and project,
// below the sessions of profiles and apart from them. A sandbox of a target
// the index has no record of is listed as unknown, and one the index records
// and the runtime did not list is listed as unlisted. -q prints no line for
// any of them: none has a ref.
func TestTheListingShowsProjectSessions(t *testing.T) {
	rt := &projectRuntime{have: map[string]string{}, unlisted: map[string]bool{}}
	root := projectsHost(t, rt)
	dev := seedProject(t, rt, root, "dev", "fix")
	ci := seedProject(t, rt, root, "ci", "")
	rt.unlisted[ci.Sandbox] = true
	seedProfile(t, rt)
	stray := "brig--old-0123456789abcdef0123"
	rt.have[stray] = "stopped"

	rows := sandboxRows(mustList(t, rt), rt)
	var table bytes.Buffer
	printSandboxes(&table, rows)
	printProjects(&table, rows)
	text := table.String()
	main, projects, ok := strings.Cut(text, "\n\n")
	if !ok || strings.Contains(main, "brig--") || !strings.Contains(main, "brig-claude-code") {
		t.Fatalf("the listing does not keep the two apart:\n%s", text)
	}
	for _, want := range []string{
		"TARGET", "SESSION", "PROJECT",
		"dev    fix     " + dev.Sandbox,
		ci.Sandbox, "unlisted", root,
		stray, "unknown: brig has no record of this session",
	} {
		if !strings.Contains(projects, want) {
			t.Errorf("the project block has no %q:\n%s", want, projects)
		}
	}
	var refs bytes.Buffer
	printRefs(&refs, rows)
	if refs.String() != "claude-code\n" {
		t.Errorf("ls -q printed %q", refs.String())
	}

	var doc bytes.Buffer
	if err := writeJSONDocument(&doc, "SandboxList", sandboxListData(rows, rt)); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Data sandboxListJSON `json:"data"`
	}
	if err := json.Unmarshal(doc.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	byName := map[string]sandboxJSON{}
	for _, s := range got.Data.Sandboxes {
		byName[s.Sandbox] = s
	}
	if s := byName[dev.Sandbox]; s.Ref != "" || s.Project == nil || !s.Project.Recorded ||
		s.Project.Target != "dev" || s.Project.Session != "fix" || s.Project.Root != root || s.Workspace != dev.Home {
		t.Errorf("dev.fix as JSON: %+v %+v", s, s.Project)
	}
	if s := byName[stray]; s.Project == nil || s.Project.Recorded || s.Project.Target != "" {
		t.Errorf("the stray sandbox as JSON: %+v %+v", s, s.Project)
	}
	if s := byName[ci.Sandbox]; s.State != stateUnlisted || s.Project == nil || !s.Project.Recorded {
		t.Errorf("the unlisted session as JSON: %+v", s)
	}
	if s := byName["brig-claude-code"]; s.Project != nil {
		t.Errorf("your session carries a project: %+v", s.Project)
	}
}

// ls prunes sessions.json against a listing that leaves out a stopped
// sandbox. That costs a project session neither its entry nor its home.
func TestPruningTheListingKeepsProjectSessions(t *testing.T) {
	rt := &projectRuntime{have: map[string]string{}, unlisted: map[string]bool{}}
	root := projectsHost(t, rt)
	dev := seedProject(t, rt, root, "dev", "")
	rt.unlisted[dev.Sandbox] = true
	pruneSessionIndex(mustList(t, rt))
	if got, ok := wrap.LookupProject(dev.Sandbox); !ok || got != dev {
		t.Errorf("the prune dropped the entry: %+v %v", got, ok)
	}
	if !there(filepath.Join(dev.Home, "agent-state")) {
		t.Error("the prune took the home")
	}
}

// The root stop and rm compute is the root the loader gives the Brigfile, so
// the sandbox name matches the one a plan names, through a link to the
// project and through -f.
func TestTheProjectRootIsTheLoadersRoot(t *testing.T) {
	root := demoProject(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(link, "sub"))
	l, err := loadBrigfile("")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"", filepath.Join(link, brigfile.FileName), "../" + brigfile.FileName} {
		got, err := projectRoot(file)
		if err != nil || got != l.Root {
			t.Errorf("projectRoot(%q) = %q, %v, want %q", file, got, err, l.Root)
		}
	}
}

// The project is mounted read-write in the guest. With brig's state inside
// it, stop and rm --target act on nothing the guest could have planted.
func TestStopAndRemoveRefuseAStateDirectoryInsideTheProject(t *testing.T) {
	rt := &projectRuntime{have: map[string]string{}}
	root := projectsHost(t, rt)
	t.Setenv("BRIG_STATE_DIR", filepath.Join(root, ".brig-state"))
	id := wrap.ProjectIdentity(root, "dev", "", "")
	rt.have[id.Sandbox] = "running"
	for _, verb := range []string{"stop", "rm"} {
		_, _, err := runQuiet(t, verb, "--target", "dev")
		if err == nil || !strings.Contains(err.Error(), "inside the project") {
			t.Errorf("%s: %v", verb, err)
		}
	}
	if len(rt.stopped) != 0 || len(rt.removed) != 0 {
		t.Errorf("stopped %q, removed %q", rt.stopped, rt.removed)
	}
	_, err := captureStdout(t, func() error { return run([]string{"plan", "--target", "dev"}) })
	if err == nil || !strings.Contains(err.Error(), "inside the project") {
		t.Errorf("plan: %v", err)
	}
}

// --session names a session of the target with its own sandbox and home, and
// the plan shows it.
func TestPlanTargetSession(t *testing.T) {
	root, _ := targetHost(t, map[string]string{brigfile.FileName: projectBrigfile,
		".brig/tool.yaml": projectTool, ".brig/agent.yaml": projectAgentPolicy})
	plain := planTargetJSON(t, "--target", "ci").Data
	named := planTargetJSON(t, "--target", "ci", "--session", "fix").Data
	want := wrap.ProjectIdentity(root, "ci", "fix", "")
	if named.Sandbox != want.Sandbox || named.Sandbox == plain.Sandbox || named.Home.Host == plain.Home.Host {
		t.Errorf("ci.fix plans %s %s, ci %s %s", named.Sandbox, named.Home.Host, plain.Sandbox, plain.Home.Host)
	}
	if !strings.HasPrefix(named.Sandbox, "brig--ci.fix-") {
		t.Errorf("the session is not in the sandbox name: %s", named.Sandbox)
	}
	_, err := captureStdout(t, func() error { return run([]string{"plan", "--target", "ci", "--session", "../x"}) })
	exitOf(t, err, exitUsage)
}
