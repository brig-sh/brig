package wrap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// projectEntry is the entry a session of target in root would record.
func projectEntry(t *testing.T, root, target, session string) ProjectSession {
	t.Helper()
	id := ProjectIdentity(root, target, session, "")
	home, err := defaultWorkspace(id.Sandbox)
	if err != nil {
		t.Fatal(err)
	}
	return ProjectSession{Sandbox: id.Sandbox, Root: root, Target: target, Session: session,
		Home: home, ConfigDigest: "d", Brigfile: filepath.Join(root, "Brigfile")}
}

// Two checkouts, two targets and a named session are eight entries, each
// read back as it was filed, and forgetting one leaves the other seven.
func TestTheProjectIndexKeepsOneEntryPerSession(t *testing.T) {
	isolateState(t)
	var all []ProjectSession
	for _, root := range []string{"/src/a", "/src/b"} {
		for _, target := range []string{"dev", "ci"} {
			for _, session := range []string{"", "fix"} {
				ps := projectEntry(t, root, target, session)
				if err := RecordProject(ps); err != nil {
					t.Fatal(err)
				}
				all = append(all, ps)
			}
		}
	}
	got := ProjectSessions()
	if len(got) != len(all) {
		t.Fatalf("%d entries, want %d: %+v", len(got), len(all), got)
	}
	homes := map[string]bool{}
	for _, ps := range all {
		back, ok := LookupProject(ps.Sandbox)
		if !ok || back != ps {
			t.Errorf("%s read back as %+v (%v), want %+v", ps.Sandbox, back, ok, ps)
		}
		homes[back.Home] = true
	}
	if len(homes) != len(all) {
		t.Errorf("%d sessions share %d homes", len(all), len(homes))
	}

	gone := all[3]
	if forgot, err := ForgetProject(gone.Sandbox); err != nil || !forgot {
		t.Fatalf("forget: %v %v", forgot, err)
	}
	if _, ok := LookupProject(gone.Sandbox); ok {
		t.Error("a forgotten entry is still there")
	}
	if n := len(ProjectSessions()); n != len(all)-1 {
		t.Errorf("forgetting one entry left %d, want %d", n, len(all)-1)
	}
	if forgot, err := ForgetProject(gone.Sandbox); err != nil || forgot {
		t.Errorf("forgetting it again: %v %v", forgot, err)
	}
}

// A file brig cannot read lists nothing, and no writer replaces it: the
// entries in it are the record of which home belongs to which project.
func TestAProjectIndexBrigCannotReadIsNeverWrittenOver(t *testing.T) {
	for name, body := range map[string]string{
		"corrupt":     `{"version": 1, "sessions": {`,
		"newer":       `{"version": 2, "sessions": {}}`,
		"older shape": `{"brig--dev-0123456789abcdef0123": {"root": "/src/a"}}`,
		"null":        `null`,
	} {
		t.Run(name, func(t *testing.T) {
			state := isolateState(t)
			path := filepath.Join(state, "projects.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := ProjectSessions(); len(got) != 0 {
				t.Errorf("an unreadable index lists %+v", got)
			}
			ps := projectEntry(t, "/src/a", "dev", "")
			if _, ok := LookupProject(ps.Sandbox); ok {
				t.Error("an unreadable index has an entry")
			}
			err := RecordProject(ps)
			if !errors.Is(err, errProjectIndex) || !strings.Contains(err.Error(), path) {
				t.Errorf("record over an unreadable index: %v", err)
			}
			if _, err := ForgetProject(ps.Sandbox); !errors.Is(err, errProjectIndex) {
				t.Errorf("forget in an unreadable index: %v", err)
			}
			if blob, _ := os.ReadFile(path); string(blob) != body {
				t.Errorf("the file was rewritten:\n%s", blob)
			}
		})
	}
}

// rm deletes the home an entry names, so an entry that names any directory
// but the sandbox's own home is not read, and RecordProject does not write
// one.
func TestAProjectEntryNamesOnlyItsOwnHome(t *testing.T) {
	state := isolateState(t)
	ps := projectEntry(t, "/src/a", "dev", "")
	planted := `{"version": 1, "sessions": {` +
		`"` + ps.Sandbox + `": {"root": "/src/a", "target": "dev", "home": "` + t.TempDir() + `"},` +
		`"brig-claude-code": {"root": "/src/a", "target": "dev", "home": "` + filepath.Join(state, "homes", "brig-claude-code") + `"}}}`
	if err := os.WriteFile(filepath.Join(state, "projects.json"), []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ProjectSessions(); len(got) != 0 {
		t.Errorf("entries naming another home were read: %+v", got)
	}

	other := ps
	other.Home = t.TempDir()
	if err := RecordProject(other); err == nil {
		t.Error("an entry naming another home was recorded")
	}
	profileSession := ps
	profileSession.Sandbox = "brig-claude-code"
	profileSession.Home = filepath.Join(state, "homes", "brig-claude-code")
	if err := RecordProject(profileSession); err == nil {
		t.Error("an entry for a profile's sandbox was recorded")
	}
}

// The project is mounted read-write in the guest, so brig's state may not be
// inside it, and it may not be inside brig's state. Both directions are
// refused, through a link, and for a state directory not created yet.
func TestAProjectAndTheStateDirectoryMayNotContainEachOther(t *testing.T) {
	isolateState(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(project, link); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, state, root, want string
	}{
		{"state inside the project", filepath.Join(project, ".brig"), project, "inside the project"},
		{"state not yet created inside the project", filepath.Join(project, "a", "b"), project, "inside the project"},
		{"state inside the project through a link", filepath.Join(link, ".brig"), project, "inside the project"},
		{"the project is the state directory", project, project, "inside the project"},
		{"the project inside the state directory", base, project, "inside brig's state directory"},
		{"the project inside the state directory through a link", base, link, "inside brig's state directory"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("BRIG_STATE_DIR", c.state)
			err := CheckProjectState(c.root)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("CheckProjectState: %v, want %q", err, c.want)
			}
			ps := projectEntry(t, c.root, "dev", "")
			if err := RecordProject(ps); err == nil {
				t.Error("RecordProject recorded it")
			}
			if _, err := os.Lstat(filepath.Join(c.state, "projects.json")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a project index was written: %v", err)
			}
		})
	}

	// Siblings are fine, including a name that only shares a prefix.
	t.Setenv("BRIG_STATE_DIR", project+"-state")
	if err := CheckProjectState(project); err != nil {
		t.Errorf("a sibling state directory: %v", err)
	}
}
