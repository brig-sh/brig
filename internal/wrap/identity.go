package wrap

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/brig-sh/brig/internal/policy"
)

// Identity names a session apart from its profile. Load derives the sandbox
// name, the guest home, the session index key and the settings prefix from
// the profile's name when Options.Identity is nil. A Brigfile target cannot
// use those: its profile's name is the repository's to choose, and a
// repository profile called claude-code would otherwise read your
// BRIG_CLAUDE_CODE_* settings and land in your claude-code sandbox and home.
type Identity struct {
	// Sandbox is the whole sandbox name. The guest home is named after it.
	Sandbox string
	// Key is what the session index files this session under.
	Key string
	// EnvAgent is the agent whose BRIG_<AGENT>_<KEY> settings apply, or ""
	// for the global BRIG_<KEY> settings alone.
	EnvAgent string
}

// projectPrefix starts every sandbox name of a Brigfile target. A profile
// name starts with a letter or a digit, so no sandbox of a profile run starts
// with it, and Load refuses a BRIG_NAME that does.
const projectPrefix = NamePrefix + "-"

// projectLabelMax bounds the readable part of a project sandbox name, so the
// whole name stays at 63 bytes: the prefix, the label, a dash and the id.
const projectLabelMax = 36

// ProjectIdentity is the identity of one target of the Brigfile at root,
// with slug the session's path-safe name or "". envAgent is passed through
// to Identity.EnvAgent.
//
// The id hashes the root, the target and the slug together, so two
// repositories with a target of the same name, and two targets of one
// repository, never share a sandbox or a home. The label in front of it is
// for a person reading `brig ls` and carries no identity of its own.
func ProjectIdentity(root, target, slug, envAgent string) Identity {
	sum := sha256.Sum256([]byte("brig-project-v1\x00" + root + "\x00" + target + "\x00" + slug))
	id := hex.EncodeToString(sum[:])[:20]
	label := target
	if slug != "" {
		label += "." + slug
	}
	if len(label) > projectLabelMax {
		label = label[:projectLabelMax]
	}
	return Identity{Sandbox: projectPrefix + label + "-" + id, Key: "@p/" + id, EnvAgent: envAgent}
}

// PolicySet is every policy bound to a run, resolved by the caller. When
// Options.Policies is set, Load reads no policy directory and applies no
// attachment: the caller has already chosen what applies, and resolving the
// profile's names again would bind a policy of yours that happens to share a
// name with one the repository declared.
type PolicySet struct {
	Docs []policy.Policy
	// Origins says where each document came from, at the same index, for the
	// plan: "brigfile <alias> (<source>)", "builtin profile <name>" or
	// "attachment to <name>".
	Origins []string
}

// Origins says where a run's image, memory, vCPUs and network came from.
type Origins struct {
	Image   string `json:"image"`
	Mem     string `json:"mem"`
	CPUs    string `json:"cpus"`
	Network string `json:"network"`
	// NetworkAsked is the posture before a policy narrowed it, and empty
	// when no policy did.
	NetworkAsked string `json:"networkAsked,omitempty"`
}

// Target is the Brigfile target a run was resolved from, as the plan shows
// it. Load reads nothing from it but Base.
type Target struct {
	Name          string
	Brigfile      string
	Root          string
	ProfileAlias  string
	ProfileSource string
	// Base names where each of the image, the memory, the vCPUs and the
	// network came from before any flag or setting: the target's overrides,
	// or its profile. NetworkAsked is not read.
	Base Origins
	// Notes are what the plan should say about the target beside its rows.
	Notes []string
}

// sessionRef is the key this session's entry in the session index is filed
// under: its identity's, or its profile and slug.
func (c *Config) sessionRef() string {
	if c.identity != nil {
		return c.identity.Key
	}
	return sessionKey(c.Profile.Name, c.Slug)
}
