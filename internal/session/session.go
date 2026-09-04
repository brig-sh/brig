// Package session turns a session name into the identifiers a run needs.
//
// `brig run claude --name foo` runs a session of its own: its own workspace
// directory and its own microVM, so two names keep two guest homes and two
// virtual machines. The name also reaches the agent as its display name --
// only the paths use the slug.
package session

import (
	"fmt"
	"strings"

	"github.com/brig-sh/brig/internal/profile"
)

// Slug turns a session name into something safe to put in a path. It returns
// the empty string when the name holds no usable characters; the caller
// decides what to do about that.
//
// Nothing is shortened. A slug used to be cut to ten characters to keep a
// directory name short, and that cut is what made two long names one
// directory: the same guest home and the same sandbox for two sessions, with
// whichever credentials arrived last. A slug is now as long as the name makes
// it, and the only ceiling left is the filesystem's own limit on a path
// component.
//
// Sanitising still collapses names, which is a smaller class and not one a
// budget was ever protecting against: Foo, foo! and foo are all foo. That is
// what claimSlug refuses in internal/wrap -- see slugclaim.go.
//
// Replacing everything outside [A-Za-z0-9._-] is what makes this safe: it
// takes out every '/', so a slug holds no path separator and cannot escape
// its parent directory. Stripping leading dots and dashes then covers the two
// cosmetic hazards, a hidden directory and a name that reads as a flag.
//
// Lowercasing matters more than it looks: macOS filesystems ignore case by
// default, so Foo and foo name one directory. Without this they would also
// name two different microVMs, and both would write to that one directory.
func Slug(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			// One dash per rune, collapsed below: a multi-byte rune becomes
			// one dash rather than one per byte, so ünï reads as a name and
			// not as a row of dashes.
			b.WriteByte('-')
		}
	}
	s := collapseDashes(b.String())
	s = strings.TrimLeft(s, "-.")
	return strings.TrimRight(s, "-.")
}

func collapseDashes(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if r == '-' {
			if prevDash {
				continue
			}
			prevDash = true
		} else {
			prevDash = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// legacyBudget is the ten characters a slug used to be cut to. It is not a
// limit any more -- see Slug -- and it survives only so that a session named
// under it can be recognised. See LegacySlug.
const legacyBudget = 10

// LegacySlug is the slug an older release would have derived from this name:
// today's slug cut to the old budget, with the trailing-separator trim that
// release reapplied after the cut. It equals Slug for every name short enough
// never to have been cut, which is how a caller tells a session whose
// directory has moved from one whose has not.
//
// It exists for the migration notice and for nothing else. Nothing in a run
// derives a path from it: the point is to name the directory the old release
// left behind, so the reader can move or delete it.
func LegacySlug(name string) string {
	s := Slug(name)
	if len(s) <= legacyBudget {
		return s
	}
	// Counted in bytes, because bytes are the budget that release spent.
	return strings.TrimRight(s[:legacyBudget], "-.")
}

// Resolve validates a session name for an agent and returns its slug.
//
// A name that would slug the agent's session onto a reserved profile's own
// workspace is refused rather than silently shared. The workspace is the
// agent's name and the slug, so the collision is the agent's to have: `brig run
// claude --name desktop` becomes claude-desktop, the workspace the Desktop app
// owns, and is refused, where the same name under another agent lands somewhere
// no profile has and is not. The check reads the slug, so Desktop and Desktop!
// are the same name here.
func Resolve(agent, name string) (string, error) {
	slug := Slug(name)
	if slug == "" {
		return "", fmt.Errorf("session name %q has no usable characters. "+
			"Names use letters, digits, dot, dash and underscore", name)
	}
	if owner, ok := profile.Reserved(slug, agent); ok {
		return "", fmt.Errorf("session name %q becomes %q, which the %s profile "+
			"already uses. Pick another name", name, slug, owner)
	}
	return slug, nil
}
