package wrap

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
)

// seedsFor is what hostProjections resolves for a run that asked, dropping the
// notice beside it: the cases below are about what gets copied, and the notice
// has cases of its own.
func seedsFor(tm profile.Profile) []hostSeed {
	seeds, _ := hostProjections(tm, "--skills")
	return seeds
}

func TestHostProjections(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude", "skills", "ananos"), 0o755); err != nil {
		t.Fatal(err)
	}
	// plugins deliberately absent: a path the user does not have must be
	// skipped, not fail the run.
	tm, _ := profile.Lookup("claude-code")

	if got, notice := hostProjections(tm, ""); got != nil || notice != "" {
		t.Errorf("off by default, got %v and %q", got, notice)
	}
	got, notice := hostProjections(tm, "--skills")
	if notice != "" {
		t.Errorf("a projection that happened was reported as one that did not: %s", notice)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 projection (skills only), got %v", got)
	}
	if got[0].Host != filepath.Join(home, ".claude", "skills") {
		t.Errorf("host = %s", got[0].Host)
	}
	// Relative to the workspace, which is the guest's home, so the agent finds
	// it at /home/claude/.claude/skills.
	if got[0].Rel != filepath.Join(".claude", "skills") {
		t.Errorf("rel = %s, want .claude/skills", got[0].Rel)
	}
}

// The guest needs to write inside these directories -- installing a plugin,
// populating a cache -- which is the whole reason they are copied rather than
// mounted read-only.
func TestSeedHostConfigCopiesAndStaysWritable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	skill := filepath.Join(home, ".claude", "skills", "ananos")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	tm, _ := profile.Lookup("claude-code")
	c := &Config{Workspace: t.TempDir(), Profile: tm, HostConfig: seedsFor(tm)}

	if err := c.seedHostConfig(mustRoot(t, c)); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(c.Workspace, ".claude", "skills", "ananos", "SKILL.md")
	if b, err := os.ReadFile(copied); err != nil {
		t.Fatalf("skill was not copied: %v", err)
	} else if string(b) != "hello" {
		t.Fatalf("copied content = %q", b)
	}
	// Writable, which a read-only mount was not.
	if err := os.WriteFile(filepath.Join(c.Workspace, ".claude", "skills", "new"), []byte("x"), 0o644); err != nil {
		t.Fatalf("the seeded directory is not writable: %v", err)
	}
}

// What the guest has done since belongs to the guest. Re-seeding must not
// overwrite an edited skill or drop a plugin it installed.
func TestSeedHostConfigDoesNotClobberTheGuest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	hostSkills := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(filepath.Join(hostSkills, "ananos"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostSkills, "ananos", "SKILL.md"), []byte("host"), 0o644); err != nil {
		t.Fatal(err)
	}
	tm, _ := profile.Lookup("claude-code")
	c := &Config{Workspace: t.TempDir(), Profile: tm, HostConfig: seedsFor(tm)}
	if err := c.seedHostConfig(mustRoot(t, c)); err != nil {
		t.Fatal(err)
	}

	// The guest edits the skill and installs one of its own.
	edited := filepath.Join(c.Workspace, ".claude", "skills", "ananos", "SKILL.md")
	if err := os.WriteFile(edited, []byte("guest"), 0o644); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(c.Workspace, ".claude", "skills", "guest-installed")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}

	// A second boot.
	if err := c.seedHostConfig(mustRoot(t, c)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(edited); string(b) != "guest" {
		t.Errorf("the guest's edit was overwritten: %q", b)
	}
	if _, err := os.Stat(own); err != nil {
		t.Errorf("the guest's own skill was removed: %v", err)
	}
}

// A skill added on the host after the workspace exists should still arrive,
// which is why this seeds entry by entry rather than once per directory.
func TestSeedHostConfigPicksUpNewHostEntries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	hostSkills := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(filepath.Join(hostSkills, "first"), 0o755); err != nil {
		t.Fatal(err)
	}
	tm, _ := profile.Lookup("claude-code")
	c := &Config{Workspace: t.TempDir(), Profile: tm, HostConfig: seedsFor(tm)}
	if err := c.seedHostConfig(mustRoot(t, c)); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(hostSkills, "second"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.seedHostConfig(mustRoot(t, c)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.Workspace, ".claude", "skills", "second")); err != nil {
		t.Errorf("a skill added on the host later did not arrive: %v", err)
	}
}

// The host's own directory is what read-only was protecting. Copying must
// leave it exactly as it was, whatever the guest does to its copy.
func TestSeedHostConfigLeavesTheHostAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	hostSkill := filepath.Join(home, ".claude", "skills", "ananos")
	if err := os.MkdirAll(hostSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	hostFile := filepath.Join(hostSkill, "SKILL.md")
	if err := os.WriteFile(hostFile, []byte("host"), 0o644); err != nil {
		t.Fatal(err)
	}
	tm, _ := profile.Lookup("claude-code")
	c := &Config{Workspace: t.TempDir(), Profile: tm, HostConfig: seedsFor(tm)}
	if err := c.seedHostConfig(mustRoot(t, c)); err != nil {
		t.Fatal(err)
	}

	// The guest rewrites and deletes inside its copy.
	if err := os.WriteFile(filepath.Join(c.Workspace, ".claude", "skills", "ananos", "SKILL.md"), []byte("guest"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(c.Workspace, ".claude", "skills", "ananos")); err != nil {
		t.Fatal(err)
	}

	if b, err := os.ReadFile(hostFile); err != nil {
		t.Fatalf("the host's skill was removed: %v", err)
	} else if string(b) != "host" {
		t.Fatalf("the host's skill was modified: %q", b)
	}
}

// --skills is accepted on every profile and copies from the two that declare
// somewhere to copy from. On the other seven the run says so, since an agent
// that starts without the host's skills looks like one that starts with them.
//
// The profile is built here rather than looked up: a shipped one that later
// declares a projection would turn this case into a skip, and a negative test
// that stops running fails nobody.
func TestSkillsSaysSoWhenTheProfileHasNothingToSeed(t *testing.T) {
	tm := profile.Profile{Name: "bare"}

	for _, asked := range []string{"--skills", "BRIG_BARE_SKILLS"} {
		seeds, notice := hostProjections(tm, asked)
		if seeds != nil {
			t.Fatalf("%s: a profile that declares nothing projected %v", asked, seeds)
		}
		if !strings.Contains(notice, asked) {
			t.Errorf("%s: the notice does not name what was asked for: %q", asked, notice)
		}
		if !strings.Contains(notice, "bare") {
			t.Errorf("%s: the notice does not name the profile that has nothing to seed: %q", asked, notice)
		}
	}

	// The global setting is "every agent that can", set once and left on, so
	// a profile that cannot is not news on every run of it.
	if seeds, notice := hostProjections(tm, "BRIG_SKILLS"); seeds != nil || notice != "" {
		t.Errorf("BRIG_SKILLS on a profile with nothing to seed: got %v and %q", seeds, notice)
	}
}

// The same silence on the profile that does declare a projection: the paths
// resolve and there is nothing behind them, so nothing is copied and nothing
// fails.
func TestSkillsSaysSoWhenThereIsNothingToCopy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tm, _ := profile.Lookup("claude-code")

	seeds, notice := hostProjections(tm, "--skills")
	if seeds != nil {
		t.Fatalf("an empty host projected %v", seeds)
	}
	// Both paths, in full: a notice that will not say where brig looked
	// leaves the reader guessing.
	for _, want := range []string{
		filepath.Join(home, ".claude", "skills"),
		filepath.Join(home, ".claude", "plugins"),
	} {
		if !strings.Contains(notice, want) {
			t.Errorf("the notice does not name %s: %q", want, notice)
		}
	}

	// A directory that is there and empty is the same outcome, and the one a
	// host reaches by ordinary use: an agent creates ~/.claude/plugins for
	// itself, so the path exists long before anything is installed in it.
	skills := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, notice := hostProjections(tm, "--skills"); notice == "" {
		t.Error("an empty directory passed as a copy")
	}
	// The global setting is quiet only about a profile that cannot copy. This
	// one can and found nothing, which is news whichever spelling asked.
	if _, notice := hostProjections(tm, "BRIG_SKILLS"); notice == "" {
		t.Error("BRIG_SKILLS with nothing to copy on a profile that can said nothing")
	}

	// And one path with something in it is not a silence: the run copied
	// something, so it says nothing. Skills but no plugins is ordinary.
	if err := os.WriteFile(filepath.Join(skills, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if seeds, notice := hostProjections(tm, "--skills"); len(seeds) != 1 || notice != "" {
		t.Errorf("a partial projection was reported as none: %v, %q", seeds, notice)
	}
}

// The third way to copy nothing: a profile that names its configuration under
// ~ on a host where brig cannot find home. Nothing is copied, from anywhere
// else either, and the run is told why rather than starting as if it had.
func TestSkillsSaysSoWhenHomeCannotBeFound(t *testing.T) {
	t.Setenv("HOME", "")
	tm, _ := profile.Lookup("claude-code")

	seeds, notice := hostProjections(tm, "--skills")
	if seeds != nil {
		t.Fatalf("a host with no home projected %v", seeds)
	}
	if !strings.Contains(notice, "--skills") || !strings.Contains(notice, tm.HostConfigDir) {
		t.Errorf("the notice does not say what was asked and where brig looked: %q", notice)
	}
}

// Load is what decides this, so the wiring is pinned there as well as on the
// function: a notice nothing reaches is the same defect one layer up.
func TestLoadCarriesTheSkillsNotice(t *testing.T) {
	isolateState(t)
	t.Setenv("HOME", t.TempDir())

	if c := mustLoadAs(t, "codex", Options{}); c.skillsNotice != "" {
		t.Errorf("a run that never asked for skills was told about them: %q", c.skillsNotice)
	}
	if c := mustLoadAs(t, "codex", Options{Skills: true}); c.skillsNotice == "" {
		t.Error("--skills on a profile with nothing to seed said nothing")
	}

	// The setting is the same request, and the notice quotes the spelling the
	// user reached for: naming --skills to somebody who set the per-agent
	// variable points them at a flag they never typed.
	t.Setenv("BRIG_CODEX_SKILLS", "1")
	c := mustLoadAs(t, "codex", Options{})
	if !strings.Contains(c.skillsNotice, "BRIG_CODEX_SKILLS") {
		t.Errorf("the notice does not name the setting that asked: %q", c.skillsNotice)
	}

	// The global setting, left on in a shell profile for the agents that can
	// use it, does not follow the user into every codex run.
	t.Setenv("BRIG_CODEX_SKILLS", "")
	os.Unsetenv("BRIG_CODEX_SKILLS")
	t.Setenv("BRIG_SKILLS", "1")
	if c := mustLoadAs(t, "codex", Options{}); c.skillsNotice != "" {
		t.Errorf("BRIG_SKILLS was reported on a profile with nothing to seed: %q", c.skillsNotice)
	}
	// The flag on the same run still names the profile: it asked about this
	// one.
	if c := mustLoadAs(t, "codex", Options{Skills: true}); c.skillsNotice == "" {
		t.Error("--skills was not reported on a profile with nothing to seed under BRIG_SKILLS")
	}
}

// It reaches the user on the run that does the copying: every verb resolves an
// environment, and brig info previews the envelope without preparing a
// workspace, so a notice there would answer a question nobody asked.
func TestTheSkillsNoticeIsSaidOnTheRunThatWouldCopy(t *testing.T) {
	c := livenessConfig(t, &livenessRuntime{running: false})
	c.skillsNotice = "--skills copied nothing"
	warned := &bytes.Buffer{}
	c.Err = warned

	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warned.String(), c.skillsNotice) {
		t.Errorf("the notice did not reach stderr: %q", warned.String())
	}

	// -q drops it, like every other warning. This is not verification, so it
	// does not outrank a caller that asked for identifiers and errors alone.
	quiet := livenessConfig(t, &livenessRuntime{running: false})
	quiet.skillsNotice = c.skillsNotice
	silent := &bytes.Buffer{}
	quiet.Err = silent
	quiet.Verbosity = Quiet
	if err := quiet.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(silent.String(), quiet.skillsNotice) {
		t.Errorf("-q printed the notice: %q", silent.String())
	}
}
