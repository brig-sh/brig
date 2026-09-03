package policy

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
)

// loadTestProfiles points the profile registry at a fresh, empty custom
// directory plus whatever files were written into it, so a test sees only
// the built-ins and its own profile, nothing left over from another test.
func loadTestProfiles(t *testing.T, profileDir string) {
	t.Helper()
	t.Setenv("BRIG_PROFILE_DIR", profileDir)
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
}

func writeTestProfile(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "profile.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestBindingsCollectsInlineProfileAndSessionAttachments(t *testing.T) {
	policyDir := t.TempDir()
	loadTestProfiles(t, writeTestProfile(t, t.TempDir(), `
name: mytool
image: ghcr.io/brig-sh/mytool:latest
guestHome: /home/mytool
binary: mytool
mem: 1024
cpus: 1
policy: [inline-only]
`))
	var a Attachments
	a.AttachToProfile("no-net", "claude-code")
	a.AttachToSession("staging", "claude-code", "work")
	if err := a.Save(policyDir); err != nil {
		t.Fatal(err)
	}

	got, err := Bindings(policyDir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"mytool (inline)"}; !reflect.DeepEqual(got["inline-only"], want) {
		t.Errorf(`Bindings()["inline-only"] = %v, want %v`, got["inline-only"], want)
	}
	if want := []string{"claude-code"}; !reflect.DeepEqual(got["no-net"], want) {
		t.Errorf(`Bindings()["no-net"] = %v, want %v`, got["no-net"], want)
	}
	if want := []string{"claude-code -n work"}; !reflect.DeepEqual(got["staging"], want) {
		t.Errorf(`Bindings()["staging"] = %v, want %v`, got["staging"], want)
	}
}

// internal/profile.Validate does not reject a policy: list with the same
// name twice, so Bindings has to collapse it itself, or the same profile
// would be listed twice as a binder of one policy.
func TestBindingsDeduplicatesARepeatedInlineName(t *testing.T) {
	loadTestProfiles(t, writeTestProfile(t, t.TempDir(), `
name: mytool
image: ghcr.io/brig-sh/mytool:latest
guestHome: /home/mytool
binary: mytool
mem: 1024
cpus: 1
policy: [no-net, no-net]
`))

	got, err := Bindings(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"mytool (inline)"}; !reflect.DeepEqual(got["no-net"], want) {
		t.Errorf(`Bindings()["no-net"] = %v, want %v (deduplicated)`, got["no-net"], want)
	}
}

// A policy nothing binds is simply absent from the map -- not a zero-length
// slice a caller has to check for either way.
func TestBindingsOmitsAnUnboundPolicy(t *testing.T) {
	loadTestProfiles(t, t.TempDir())
	got, err := Bindings(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["no-net"]; ok {
		t.Errorf(`Bindings()["no-net"] = %v, want absent`, got["no-net"])
	}
}

// The profiles are a parameter, not the registry, so a caller that reads
// inline bindings gets exactly the profiles it passed. Passing none is a
// real answer -- no inline bindings -- rather than whatever the registry
// happened to hold, which is what makes removePolicy's refusal depend on
// its own argument instead of on init order somewhere else.
func TestOrphanedFindsANameNothingDeclares(t *testing.T) {
	bound := map[string][]string{"no-net": {"claude-code"}}
	got := Orphaned(bound, map[string]Entry{})
	if want := []string{"no-net"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Orphaned = %v, want %v", got, want)
	}
}

func TestOrphanedIgnoresANameStillDeclared(t *testing.T) {
	bound := map[string][]string{"no-net": {"claude-code"}}
	if got := Orphaned(bound, map[string]Entry{"no-net": {}}); len(got) != 0 {
		t.Errorf("Orphaned = %v, want none: no-net is still declared", got)
	}
}

// Sorted, so the line a listing prints does not reorder itself between
// runs just because the map iterated differently.
func TestOrphanedIsSortedByName(t *testing.T) {
	bound := map[string][]string{
		"zzz": {"claude-code"},
		"aaa": {"codex"},
	}
	got := Orphaned(bound, map[string]Entry{})
	if want := []string{"aaa", "zzz"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Orphaned = %v, want %v", got, want)
	}
}

// A malformed attachments.yaml is a real failure, not silently ignored:
// the caller decides how to degrade (see cmd/brig's listPolicies, which
// still lists policies without the bound-to lines).
func TestBindingsReturnsAnErrorForAMalformedAttachmentsFile(t *testing.T) {
	loadTestProfiles(t, t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "attachments.yaml"),
		[]byte("profiles: [this is not valid: yaml structure"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Bindings(dir); err == nil {
		t.Error("a malformed attachments.yaml was not reported as an error")
	}
}
