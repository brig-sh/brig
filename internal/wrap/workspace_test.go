package wrap

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
)

// The registry is built at run time now rather than being a package-level
// literal, so a test that looks a profile up has to load the built-ins the way
// main does. No test here writes to it, so once for the package is enough.
func TestMain(m *testing.M) {
	if err := profile.Load(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// testConfig builds a Config the way Load would, for the claude-code profile
// or for one the caller passes instead -- the binding cases need a profile of
// their own, and a second builder alongside this one is how the two drift
// apart from the code they are testing.
func testConfig(t *testing.T, workspace, cwd string, override ...profile.Profile) *Config {
	t.Helper()
	tmpl, _ := profile.Lookup("claude-code")
	if len(override) > 0 {
		tmpl = override[0]
	}
	return &Config{
		Profile:        tmpl,
		VMName:         "brig-" + tmpl.Name,
		Env:            tmpl.Env,
		Workspace:      workspace,
		Cwd:            cwd,
		GuestCwd:       GuestCwd(cwd, workspace, tmpl.GuestHome),
		TrustWorkspace: true,
		GitHosts:       []string{"github.com"},
		Out:            &bytes.Buffer{},
		Err:            &bytes.Buffer{},
		env:            NewEnv(tmpl.Name, func(string) (string, bool) { return "", false }),
	}
}

// mustRoot opens the workspace the way PrepareWorkspace does, for tests that
// exercise one of its steps on its own.
func mustRoot(t *testing.T, c *Config) *workspaceRoot {
	t.Helper()
	r, err := c.openWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// The state file is created only when it is absent: an existing one belongs
// to the agent, and the single key brig changes in a file it did not create
// is the per-directory trust key.
func TestSeedOnboardingDoesNotOverwrite(t *testing.T) {
	ws := t.TempDir()
	c := testConfig(t, ws, ws)
	if err := c.seedOnboarding(mustRoot(t, c)); err != nil {
		t.Fatal(err)
	}
	seeded, err := os.ReadFile(filepath.Join(ws, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(seeded), "hasCompletedOnboarding") {
		t.Errorf("seed = %s", seeded)
	}
	// No credential may ever be written into the workspace: the token is
	// forwarded as environment only.
	if strings.Contains(string(seeded), "Token") || strings.Contains(string(seeded), "token") {
		t.Errorf("the seeded state file mentions a token: %s", seeded)
	}

	existing := `{"mine":true}`
	if err := os.WriteFile(filepath.Join(ws, ".claude.json"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.seedOnboarding(mustRoot(t, c)); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(ws, ".claude.json"))
	if string(after) != existing {
		t.Errorf("an existing state file was rewritten: %s", after)
	}
}

func TestTrustGuestCwdSetsOneKeyAndKeepsTheRest(t *testing.T) {
	ws := t.TempDir()
	proj := filepath.Join(ws, "myrepo")
	mustMkdir(t, filepath.Join(proj, ".git"))

	// A number big enough that decoding through float64 would rewrite it as
	// 1.7e+12, quietly reformatting a file that is not ours.
	original := `{"numAccounts":2,"lastCost":1700000000000,"projects":{"/home/claude":{"hasTrustDialogAccepted":true}}}`
	path := filepath.Join(ws, ".claude.json")
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	c := testConfig(t, ws, proj)
	if err := c.trustGuestCwd(mustRoot(t, c)); err != nil {
		t.Fatal(err)
	}

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte("e+12")) {
		t.Errorf("a number was reformatted: %s", blob)
	}
	var doc map[string]any
	if err := json.Unmarshal(blob, &doc); err != nil {
		t.Fatalf("the file is no longer valid JSON: %v", err)
	}
	if doc["numAccounts"] == nil {
		t.Errorf("an unrelated key was dropped: %s", blob)
	}
	projects := doc["projects"].(map[string]any)
	// The key is the repository root as the guest sees it, not the cwd.
	entry, ok := projects["/home/claude/myrepo"].(map[string]any)
	if !ok || entry["hasTrustDialogAccepted"] != true {
		t.Errorf("trust key not set for the repository root: %s", blob)
	}
	if projects["/home/claude"] == nil {
		t.Errorf("an existing trust entry was dropped: %s", blob)
	}
	// The file stays on one line, the way the agent writes it.
	if bytes.Contains(bytes.TrimSpace(blob), []byte("\n")) {
		t.Errorf("the state file was reformatted onto several lines: %s", blob)
	}
}

// The agent owns this file, so it owns a parse failure too: say what happened
// and leave it alone rather than truncating someone's state.
func TestTrustGuestCwdLeavesInvalidJSONAlone(t *testing.T) {
	ws := t.TempDir()
	path := filepath.Join(ws, ".claude.json")
	broken := "{not json"
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	c := testConfig(t, ws, ws)
	if err := c.trustGuestCwd(mustRoot(t, c)); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != broken {
		t.Errorf("the file was rewritten: %s", after)
	}
	if !strings.Contains(c.Err.(*bytes.Buffer).String(), "invalid JSON") {
		t.Errorf("nothing was said about it: %q", c.Err)
	}
}

func TestTrustWorkspaceOffKeepsTheDialog(t *testing.T) {
	ws := t.TempDir()
	path := filepath.Join(ws, ".claude.json")
	original := `{"projects":{}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	c := testConfig(t, ws, ws)
	c.TrustWorkspace = false
	if err := c.trustGuestCwd(mustRoot(t, c)); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != original {
		t.Errorf("BRIG_TRUST_WORKSPACE=0 still edited the file: %s", after)
	}
}

// A renamed and recreated directory reuses the path with a different inode,
// which is why the marker carries both.
func TestMarkerCoversPathAndInode(t *testing.T) {
	dir := t.TempDir()
	first, err := Marker(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, ":") {
		t.Fatalf("marker = %q, want path:inode", first)
	}
	again, _ := Marker(dir)
	if again != first {
		t.Errorf("marker is not stable: %q then %q", first, again)
	}

	replaced := filepath.Join(filepath.Dir(dir), "replaced")
	mustMkdir(t, replaced)
	other, _ := Marker(replaced)
	if other == first {
		t.Errorf("two different directories share a marker: %q", other)
	}
}

func TestWriteIfChangedLeavesAnIdenticalFileAlone(t *testing.T) {
	ws := t.TempDir()
	r := mustRoot(t, testConfig(t, ws, ws))
	path := filepath.Join(ws, "f")
	if err := r.writeIfChanged("f", "content\n", 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.writeIfChanged("f", "content\n", 0o644); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("an unchanged file was rewritten")
	}
	if err := r.writeIfChanged("f", "new\n", 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new\n" {
		t.Errorf("content = %q", got)
	}
}

// Adopting a Homebrew-era workspace is exactly how a real token ends up
// sitting on disk that nothing reads any more. Say so; do not delete it.
func TestWarnsAboutACredentialAnOlderWrapperLeftBehind(t *testing.T) {
	ws := t.TempDir()
	stale := filepath.Join(ws, ".claude", ".credentials.json")
	mustMkdir(t, filepath.Dir(stale))
	if err := os.WriteFile(stale, []byte(`{"accessToken":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := testConfig(t, ws, ws)
	c.warnStaleCredentials(mustRoot(t, c))

	said := c.Err.(*bytes.Buffer).String()
	if !strings.Contains(said, ".credentials.json") {
		t.Errorf("nothing was said about it: %q", said)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Error("the file was deleted rather than reported")
	}

	// A workspace without one stays quiet.
	clean := testConfig(t, t.TempDir(), ws)
	clean.warnStaleCredentials(mustRoot(t, clean))
	if got := clean.Err.(*bytes.Buffer).String(); got != "" {
		t.Errorf("a clean workspace produced a warning: %q", got)
	}
}
