package policy

import (
	"reflect"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
)

func TestEffectivePoliciesWithNothingBoundIsJustInline(t *testing.T) {
	p := profile.Profile{Name: "claude-code", Policy: []string{"no-net"}}
	got, err := EffectivePolicies(p, "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"no-net"}; !reflect.DeepEqual(got, want) {
		t.Errorf("EffectivePolicies = %v, want %v", got, want)
	}
}

func TestEffectivePoliciesUnionsInlineAndProfileAttachments(t *testing.T) {
	dir := t.TempDir()
	var a Attachments
	a.AttachToProfile("staging", "claude-code")
	if err := a.Save(dir); err != nil {
		t.Fatal(err)
	}
	p := profile.Profile{Name: "claude-code", Policy: []string{"no-net"}}

	got, err := EffectivePolicies(p, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"no-net", "staging"}; !reflect.DeepEqual(got, want) {
		t.Errorf("EffectivePolicies = %v, want %v", got, want)
	}
}

func TestEffectivePoliciesAddsTheSessionOnlyWhenNamed(t *testing.T) {
	dir := t.TempDir()
	var a Attachments
	a.AttachToSession("work-only", "claude-code", "work")
	if err := a.Save(dir); err != nil {
		t.Fatal(err)
	}
	p := profile.Profile{Name: "claude-code"}

	withoutSession, err := EffectivePolicies(p, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(withoutSession) != 0 {
		t.Errorf("EffectivePolicies with no session = %v, want none: a different session's "+
			"binding must not leak in", withoutSession)
	}

	withSession, err := EffectivePolicies(p, "work", dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"work-only"}; !reflect.DeepEqual(withSession, want) {
		t.Errorf("EffectivePolicies(..., \"work\", ...) = %v, want %v", withSession, want)
	}
}

// The same name can reach a run through more than one source at once -- an
// inline policy: entry that was also attached, say. It appears once.
func TestEffectivePoliciesDeduplicatesByName(t *testing.T) {
	dir := t.TempDir()
	var a Attachments
	a.AttachToProfile("no-net", "claude-code")
	a.AttachToSession("no-net", "claude-code", "work")
	if err := a.Save(dir); err != nil {
		t.Fatal(err)
	}
	p := profile.Profile{Name: "claude-code", Policy: []string{"no-net"}}

	got, err := EffectivePolicies(p, "work", dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"no-net"}; !reflect.DeepEqual(got, want) {
		t.Errorf("EffectivePolicies = %v, want %v (deduplicated)", got, want)
	}
}

// A profile's own name never collides with another profile's attachments:
// EffectivePolicies must key strictly on p.Name, not on every entry in
// Attachments.Profiles.
func TestEffectivePoliciesIgnoresOtherProfilesAttachments(t *testing.T) {
	dir := t.TempDir()
	var a Attachments
	a.AttachToProfile("other-teams-policy", "some-other-profile")
	if err := a.Save(dir); err != nil {
		t.Fatal(err)
	}
	p := profile.Profile{Name: "claude-code"}

	got, err := EffectivePolicies(p, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("EffectivePolicies = %v, want none: a different profile's attachment leaked in", got)
	}
}

// claude-code -n work and codex -n work are different sandboxes
// (brig-claude-code-work vs brig-codex-work), so a session name is only
// unique within its profile. EffectivePolicies must not let one profile's
// "work" session see what is bound to another profile's session of the
// same name.
func TestEffectivePoliciesSessionDoesNotCrossProfiles(t *testing.T) {
	dir := t.TempDir()
	var a Attachments
	a.AttachToSession("no-net", "claude-code", "work")
	a.AttachToSession("staging", "codex", "work")
	if err := a.Save(dir); err != nil {
		t.Fatal(err)
	}

	got, err := EffectivePolicies(profile.Profile{Name: "codex"}, "work", dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"staging"}; !reflect.DeepEqual(got, want) {
		t.Errorf("EffectivePolicies(codex, work) = %v, want %v: claude-code's session "+
			"binding must not apply to codex's own \"work\" session", got, want)
	}
}
