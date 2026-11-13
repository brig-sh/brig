package wrap

import (
	"crypto/sha256"
	"encoding/hex"
	"os"

	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/runtime"
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

// projectKeyPrefix starts the session index key of a Brigfile target. No ref
// starts with it: a ref starts with its agent's name.
const projectKeyPrefix = "@p/"

// projectLabelMax bounds the readable part of a project sandbox name, so the
// whole name stays at 47 bytes: the prefix, the label, a dash and the id.
//
// The limit that binds is a socket path, not a name rule. hull keeps a
// sandbox's agent socket at <store>/instances/<name>/agent.sock, and macOS
// takes a socket path of at most 103 bytes. With the store in a home
// directory that leaves a name a little over 50 bytes, and a longer one
// would plan and then fail at boot. The id carries the identity; the label
// is only there for a person to read, so it is what gives way.
const projectLabelMax = 20

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
	return Identity{Sandbox: projectPrefix + label + "-" + id, Key: projectKeyPrefix + id, EnvAgent: envAgent}
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
	Name string
	// Session is the session's name, or "" for the target's default session.
	Session       string
	Brigfile      string
	Root          string
	ProfileAlias  string
	ProfileSource string
	// BuiltIn says the profile is one brig ships, by the Brigfile's builtin:
	// source. A profile's name cannot say so: a repository can call its own
	// profile claude-code.
	BuiltIn bool
	// Base names where each of the image, the memory, the vCPUs and the
	// network came from before any flag or setting: the target's overrides,
	// or its profile. NetworkAsked is not read.
	Base Origins
	// Notes are what the plan should say about the target beside its rows.
	Notes []string
	// Args are the target's default guest arguments. Only the configuration
	// digest reads them.
	Args []string
}

// ProjectConfig is the Config of a recorded session of a Brigfile target, for
// the verbs that stop or remove its sandbox. It names the sandbox, the
// runtime, the guest home and the project, and nothing a boot needs: those
// verbs must reach a session whose Brigfile is gone or has lost the target.
func ProjectConfig(ps ProjectSession, rt runtime.Runtime, v Verbosity) *Config {
	id := ProjectIdentity(ps.Root, ps.Target, ps.Session, "")
	id.Sandbox = ps.Sandbox
	// The home is the sandbox's own, whether or not an entry names it.
	home, err := defaultWorkspace(ps.Sandbox)
	if err != nil {
		home = ps.Home
	}
	return &Config{
		Runtime:       rt,
		Slug:          ps.Session,
		VMName:        ps.Sandbox,
		Workspace:     home,
		EphemeralHome: true,
		Project:       ps.Root,
		ProjectReal:   ps.Root,
		identity:      &id,
		target:        &Target{Name: ps.Target, Session: ps.Session, Root: ps.Root, Brigfile: ps.Brigfile},
		Out:           os.Stdout,
		Err:           os.Stderr,
		Progress:      os.Stderr,
		Verbosity:     v,
	}
}

// sessionRef is the key this session's entry in the session index is filed
// under: its identity's, or its profile and slug.
func (c *Config) sessionRef() string {
	if c.identity != nil {
		return c.identity.Key
	}
	return sessionKey(c.Profile.Name, c.Slug)
}

// subject names what this run is of in a refusal: the profile, or the
// Brigfile target. A target's profile name is the repository's word, and a
// refusal that said "claude-code" would point you at your own profile.
func (c *Config) subject() string {
	if c.target != nil {
		return "target " + c.target.Name
	}
	return c.Profile.Name
}

// profileNoun is the profile this run reads, as a refusal names it.
func (c *Config) profileNoun() string {
	if c.target != nil {
		return "the profile of target " + c.target.Name
	}
	return "the " + c.Profile.Name + " profile"
}
