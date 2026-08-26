package policy

import (
	"path/filepath"
	"testing"
)

// A fresh install has no attachments.yaml. That is not an error: nothing
// has been attached yet.
func TestLoadAttachmentsIgnoresAMissingFile(t *testing.T) {
	a, err := LoadAttachments(t.TempDir())
	if err != nil {
		t.Fatalf("LoadAttachments: %v", err)
	}
	if len(a.Profiles) != 0 || len(a.Sessions) != 0 {
		t.Errorf("a.Profiles/Sessions = %v/%v, want both empty", a.Profiles, a.Sessions)
	}
}

// Attaching the same name twice changes nothing the second time.
func TestAttachToProfileIsIdempotent(t *testing.T) {
	var a Attachments
	a.AttachToProfile("no-net", "claude-code")
	a.AttachToProfile("no-net", "claude-code")
	if got := a.Profiles["claude-code"]; len(got) != 1 || got[0] != "no-net" {
		t.Errorf("Profiles[claude-code] = %v, want [no-net]", got)
	}
}

func TestDetachFromProfileRemovesTheEntry(t *testing.T) {
	var a Attachments
	a.AttachToProfile("no-net", "claude-code")
	a.AttachToProfile("other", "claude-code")
	a.DetachFromProfile("no-net", "claude-code")
	if got := a.Profiles["claude-code"]; len(got) != 1 || got[0] != "other" {
		t.Errorf("Profiles[claude-code] = %v, want [other]", got)
	}
}

// Detaching the last policy from a profile drops the profile's own entry
// entirely, rather than leaving an empty list behind.
func TestDetachFromProfileDropsAnEmptyEntry(t *testing.T) {
	var a Attachments
	a.AttachToProfile("no-net", "claude-code")
	a.DetachFromProfile("no-net", "claude-code")
	if _, ok := a.Profiles["claude-code"]; ok {
		t.Errorf("Profiles still has claude-code: %v", a.Profiles)
	}
}

// Detaching a name that was never attached is a no-op, not a panic -- the
// zero-value Attachments{} that LoadAttachments returns for a fresh
// directory has nil maps, and detach has to survive that.
func TestDetachFromProfileOnAZeroValueDoesNotPanic(t *testing.T) {
	var a Attachments
	a.DetachFromProfile("no-net", "claude-code")
}

func TestDetachFromSessionOnAZeroValueDoesNotPanic(t *testing.T) {
	var a Attachments
	a.DetachFromSession("no-net", "claude-code", "work")
}

func TestAttachToSessionIsIdempotent(t *testing.T) {
	var a Attachments
	a.AttachToSession("no-net", "claude-code", "work")
	a.AttachToSession("no-net", "claude-code", "work")
	if got := a.Sessions["claude-code"]["work"]; len(got) != 1 || got[0] != "no-net" {
		t.Errorf("Sessions[claude-code][work] = %v, want [no-net]", got)
	}
}

func TestDetachFromSessionDropsAnEmptyEntry(t *testing.T) {
	var a Attachments
	a.AttachToSession("no-net", "claude-code", "work")
	a.DetachFromSession("no-net", "claude-code", "work")
	if _, ok := a.Sessions["claude-code"]["work"]; ok {
		t.Errorf("Sessions still has claude-code/work: %v", a.Sessions)
	}
	if _, ok := a.Sessions["claude-code"]; ok {
		t.Errorf("Sessions still has claude-code, with no session left under it: %v", a.Sessions)
	}
}

// A session name is only unique within its profile -- claude-code -n work
// and codex -n work are different sandboxes (brig-claude-code-work and
// brig-codex-work). One profile's session binding must not reach the
// other's, in either direction.
func TestSessionAttachmentsDoNotCrossProfiles(t *testing.T) {
	var a Attachments
	a.AttachToSession("no-net", "claude-code", "work")
	a.AttachToSession("staging", "codex", "work")

	if got := a.Sessions["claude-code"]["work"]; len(got) != 1 || got[0] != "no-net" {
		t.Errorf("Sessions[claude-code][work] = %v, want [no-net]", got)
	}
	if got := a.Sessions["codex"]["work"]; len(got) != 1 || got[0] != "staging" {
		t.Errorf("Sessions[codex][work] = %v, want [staging]", got)
	}

	a.DetachFromSession("staging", "codex", "work")
	if got := a.Sessions["claude-code"]["work"]; len(got) != 1 || got[0] != "no-net" {
		t.Errorf("detaching codex's work session changed claude-code's: %v", got)
	}
}

// A save survives a round trip through disk: what LoadAttachments reads
// back matches what Save wrote, for both maps.
func TestAttachmentsSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	var a Attachments
	a.AttachToProfile("no-net", "claude-code")
	a.AttachToSession("no-net", "claude-code", "work")
	if err := a.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := LoadAttachments(dir)
	if err != nil {
		t.Fatalf("LoadAttachments: %v", err)
	}
	if p := got.Profiles["claude-code"]; len(p) != 1 || p[0] != "no-net" {
		t.Errorf("Profiles[claude-code] = %v, want [no-net]", p)
	}
	if s := got.Sessions["claude-code"]["work"]; len(s) != 1 || s[0] != "no-net" {
		t.Errorf("Sessions[claude-code][work] = %v, want [no-net]", s)
	}
}

// Save creates the policy directory if it does not exist yet, the same way
// a fresh install has no policies directory until something writes to it.
func TestAttachmentsSaveCreatesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist-yet")
	var a Attachments
	a.AttachToProfile("no-net", "claude-code")
	if err := a.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := LoadAttachments(dir); err != nil {
		t.Fatalf("LoadAttachments after Save: %v", err)
	}
}
