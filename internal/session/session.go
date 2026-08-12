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

	"github.com/brig-sh/brig/internal/agent"
)

// maxSlug keeps directory names short. It is a tight budget, so two long
// names can slug the same and share one sandbox; callers report the
// directory in use whenever the slug differs from the name.
const maxSlug = 10

// Slug turns a session name into something safe to put in a path. It returns
// the empty string when the name holds no usable characters; the caller
// decides what to do about that.
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
			// One dash per rune, collapsed below. A multi-byte rune is one
			// character here, not one per byte, so a non-ASCII name does not
			// blow the length budget on dashes.
			b.WriteByte('-')
		}
	}
	s := collapseDashes(b.String())
	s = strings.TrimLeft(s, "-.")
	s = strings.TrimRight(s, "-.")
	if len(s) > maxSlug {
		s = s[:maxSlug]
	}
	// Truncation can leave a trailing separator behind.
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

// Resolve validates a session name and returns its slug.
//
// A name that slugs to a template's own workspace is refused rather than
// silently shared: claude-desktop already owns ~/brig/claude-desktop, so a
// session called desktop would land a Claude Code session on the Desktop
// app's workspace. The check reads the slug, so Desktop and Desktop! stop
// here as well.
func Resolve(name string) (string, error) {
	slug := Slug(name)
	if slug == "" {
		return "", fmt.Errorf("session name %q has no usable characters. "+
			"Names use letters, digits, dot, dash and underscore", name)
	}
	if owner, ok := agent.Reserved(slug); ok {
		return "", fmt.Errorf("session name %q becomes %q, which the %s template "+
			"already uses. Pick another name", name, slug, owner)
	}
	return slug, nil
}
