package session

import (
	"fmt"
	"strings"

	"github.com/brig-sh/brig/internal/profile"
)

// Ref names one session on a command line: which agent, and which of that
// agent's sessions. An empty Label is the agent's default session, so `claude`
// and `claude@refactor` are two sessions of one agent rather than two agents.
type Ref struct{ Agent, Label string }

// refSep is the one character between the agent and its label. Exactly one:
// see ParseRef.
const refSep = "@"

// ParseRef reads a session ref, refusing a label it would otherwise have to
// rewrite.
//
// The label is not cleaned up for you, and that is the point. It reaches two
// places that have to agree: the sandbox name, which is the profile's name
// plus "-" plus the slug (see internal/wrap/config.go), and the workspace
// directory the guest gets as its home. Slug is what turns a name into both,
// so a label Slug would rewrite is a label that addresses a directory nobody
// typed. Refusing it is what keeps a ref an address, instead of quietly
// mapping one spelling onto a session started under another.
//
// So the rule is: a label must already be what Slug would make of it. Slug
// changes characters and nothing else -- it does not shorten -- so the single
// `label != Slug(label)` is an exact test for "not slug-clean" at any length,
// and there is no length for a label to be refused for. The character rules
// stay in Slug and are not restated here, so the two cannot drift.
//
// --name is deliberately not held to this. It keeps its older, lenient
// behaviour, sanitising the name and reporting the directory it actually got;
// the '@' form is the strict one.
func ParseRef(s string) (Ref, error) {
	agent, label, hasLabel := strings.Cut(s, refSep)
	if strings.Contains(label, refSep) {
		// Two '@' is a typo, and there is no reading of it worth guessing:
		// taking either side would make a@b@c mean whichever half the parser
		// happened to read first.
		return Ref{}, fmt.Errorf("session ref %q has more than one %q. "+
			"A ref is an agent, or an agent and one label: agent%slabel", s, refSep, refSep)
	}
	if agent == "" {
		return Ref{}, fmt.Errorf("session ref %q names no agent. "+
			"A ref begins with the agent, for example `claude%srefactor`", s, refSep)
	}
	if !hasLabel {
		return Ref{Agent: agent}, nil
	}
	// A trailing '@' is a label the typing stopped short of, not a way to ask
	// for the default session -- `claude` already asks for that, and reading
	// the two the same way would hide the slip.
	if label == "" {
		return Ref{}, fmt.Errorf("session ref %q ends with %q and names no session. "+
			"Drop the %q for the default session, or name one: `%s%srefactor`",
			s, refSep, refSep, agent, refSep)
	}
	if slug := Slug(label); slug != label {
		if slug == "" {
			return Ref{}, fmt.Errorf("session label %q has no usable characters. "+
				"Labels use lowercase letters, digits, dot, dash and underscore", label)
		}
		return Ref{}, fmt.Errorf("session label %q is not usable as it stands: it would have to "+
			"become %q. Labels use lowercase letters, digits, dot, dash and underscore, and "+
			"start and end with one of those. Type %q if that is the session you want",
			label, slug, slug)
	}
	// The same refusal Resolve makes, on the label. claude-desktop already
	// owns ~/brig/claude-desktop, so claude@desktop would put a Claude Code
	// session on the Desktop app's workspace.
	if owner, ok := profile.Reserved(label); ok {
		return Ref{}, fmt.Errorf("session label %q is the workspace the %s profile "+
			"already uses. Pick another label", label, owner)
	}
	return Ref{Agent: agent, Label: label}, nil
}

// IsRefShaped reports whether a token carries the separator, which is the one
// thing that tells a mistyped ref from a mistyped verb.
//
// It is here rather than a strings.Contains at the call site so that refSep
// keeps one definition. The question it answers is only worth asking because
// the separator is brig's: no verb has one, and ParseRef cuts on the first, so
// an agent name cannot have one either. A token with an '@' in it therefore
// has a single possible reading, however badly it is spelled.
func IsRefShaped(s string) bool {
	return strings.Contains(s, refSep)
}

// String writes a ref the way it is typed, so ParseRef reads back what String
// prints. A ref with no label is the agent on its own rather than an agent
// with a bare '@' after it, which ParseRef refuses.
func (r Ref) String() string {
	if r.Label == "" {
		return r.Agent
	}
	return r.Agent + refSep + r.Label
}
