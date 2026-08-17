// Package profile holds the per-profile specs as data.
//
// A profile is not code. It is the handful of facts that differ between one
// sandboxed workload and the next -- which binary to exec, which environment
// variables carry its credentials, which of those must never be forwarded,
// where its state lives in the home directory, and which guest image to boot.
// Everything else -- workspace-as-home, credential resolution, git config,
// the VM lifecycle -- is the same for every profile and lives in internal/wrap.
//
// An LLM agent is expected to become a special handling of a profile rather
// than the general case, which is why this package is named for the general
// thing.
//
// Profile names are load-bearing strings: a name here is also the image name
// in brig-sh/community-images and the taxonomer signature.AgentRuntime value.
// Renaming one means renaming all three.
package profile

import (
	"fmt"
	"maps"
	"slices"
)

// Kind is what sort of workload a profile is, and decides what brig does with
// the guest once it is up. It is also where an LLM agent's own special
// handling can eventually hang.
type Kind string

const (
	// KindAgent is a CLI agent: brig execs Binary and passes your arguments
	// through to it. The default, so a profile need not say so.
	KindAgent Kind = "agent"
	// KindShell is a profile whose "agent" is the guest shell itself. `brig
	// run` opens a login shell, and any trailing words run as one command.
	// There is no CLI to pass arguments to.
	KindShell Kind = "shell"
	// KindGUI is a windowed workload: the VM boots with a graphical console
	// and there is no CLI pass-through.
	KindGUI Kind = "gui"
)

// Onboarding describes an agent's first-run state file.
//
// Some agents stop on a first-run screen that is not authentication and that
// the guest cannot complete -- picking a login method there opens a browser
// the microVM does not have. Seeding a couple of non-secret flags into the
// agent's own state file settles it. Never a credential: credentials are
// forwarded as environment and are never written to the workspace.
type Onboarding struct {
	// File is relative to the guest home, e.g. ".claude.json".
	File string `json:"file"`
	// Seed is written as JSON when File does not exist. An existing file
	// belongs to the agent and is never overwritten.
	Seed map[string]any `json:"seed,omitempty"`
	// TrustKey, when set, is the per-directory boolean the agent reads to
	// decide whether it trusts the files in the directory it starts in. brig
	// sets it for the directory each run starts in, resolved to the git root
	// as the guest sees it.
	//
	// Expressed as the two JSON levels around the directory name:
	// projects[<dir>].hasTrustDialogAccepted -> {"projects", "hasTrustDialogAccepted"}.
	TrustKey [2]string `json:"trustKey,omitempty"`
}

// HostCredential describes a credential brig can resolve from the host when
// the environment carries none, so a fresh sandbox works without anyone
// minting a token by hand.
//
// The value is read fresh on every invocation and forwarded as environment
// only. Nothing is written to the workspace, and a rotated credential is
// picked up without restarting the VM.
type HostCredential struct {
	// KeychainService is the macOS keychain generic-password service name.
	KeychainService string `json:"keychainService,omitempty"`
	// TokenField and ExpiryField are keys in the credential JSON. The blob is
	// searched recursively, so a nested envelope needs no path.
	TokenField  string `json:"tokenField,omitempty"`
	ExpiryField string `json:"expiryField,omitempty"`
	// TargetVar is the guest environment variable the token is forwarded as.
	TargetVar string `json:"targetVar,omitempty"`
	// RenewHint is shown when the host credential has expired.
	RenewHint string `json:"renewHint,omitempty"`
}

// Profile is one sandboxed workload, as data.
type Profile struct {
	// Name is the profile name, the community-images image name, and the
	// taxonomer AgentRuntime value: one string, three uses.
	Name string `json:"name"`
	// Desc is one line for `brig agents`.
	Desc string `json:"desc,omitempty"`
	// Binary is the agent CLI inside the guest.
	Binary string `json:"binary,omitempty"`
	// Image is the default guest image.
	Image string `json:"image"`
	// GuestHome is where the workspace is mounted. The agent's state lands
	// here, which is what makes the workspace the unit of persistence.
	GuestHome string `json:"guestHome"`
	// Forward names the environment variables carried into the guest when set
	// and non-empty in brig's own environment.
	//
	// Deprecated: use Env with `ref: env.<name>`, which says the same thing and
	// leaves room for a value from somewhere other than the environment. Parse
	// translates each entry and zeroes this, the same way it folds the shell
	// and gui booleans into Kind -- so this field is empty on any profile that
	// came through Parse, and reading it is a bug.
	Forward []string `json:"forward,omitempty"`
	// Secrets is the requirement list: the names this profile needs out of the
	// store brig owns, and for each one whether a run without it should stop
	// and where `brig secret import` may find it. A bare string in YAML is
	// {name: <it>, required: true}, so every profile written before this
	// schema parses unchanged.
	//
	// Kept separate from the bindings below so a missing secret names every
	// one of them at once rather than dying on the first ref -- the difference
	// between one error you can act on and one failed run per secret.
	Secrets []SecretDecl `json:"secrets,omitempty"`
	// Env is what the guest actually sees. Each entry has exactly one of
	// value: (a literal, for non-secret configuration) or ref: (resolved --
	// secrets.<name> from the store, env.<name> from brig's own environment).
	//
	// Parse folds Forward into this, so nothing below the parser ever reads
	// two mechanisms for the same thing.
	Env []EnvBinding `json:"env,omitempty"`
	// Deny names variables that are never forwarded, even if a caller puts
	// them in the Forward override. These are the ones that flip billing: an
	// API key that outranks a subscription token would move the sandbox onto
	// metered usage without saying so. Overriding a deny is deliberate and
	// needs BRIG_ALLOW_DENIED=1.
	Deny []string `json:"deny,omitempty"`
	// StatePaths are the paths under GuestHome holding the agent's persistent
	// state. Documentation today; the unit that #1251's clone/overlay and
	// explicit-apply will move.
	StatePaths []string `json:"statePaths,omitempty"`
	// Headless is whether the agent supports a non-interactive run.
	Headless bool `json:"headless,omitempty"`
	// Kind is what sort of workload this is: agent (the default), shell or
	// gui. See Kind.
	Kind Kind `json:"kind,omitempty"`
	// Shell and GUI are the older spellings of Kind, still decoded so that a
	// profile using them keeps working -- the decoder is strict, so an unknown
	// field is an error rather than something ignored.
	// Parse folds them into Kind and zeroes them, so nothing below this line
	// ever reads two sources of truth.
	Shell bool `json:"shell,omitempty"`
	GUI   bool `json:"gui,omitempty"`
	// GUITitle is the window title for a GUI profile.
	GUITitle string `json:"guiTitle,omitempty"`
	// Mem is the guest memory in MB, CPUs the vCPU count.
	Mem  int `json:"mem"`
	CPUs int `json:"cpus"`
	// Hypervisor is the macOS backend this workload wants: vz, hvi or qemu.
	// Empty means vz, the one with a graphical console and the notarised
	// runner. A profile states it when it needs a particular backend rather
	// than whichever the host defaults to -- hvi for its introspection, say.
	// Ignored on Linux, where the shim decides.
	Hypervisor string `json:"hypervisor,omitempty"`
	// RuntimeBin is the runtime binary to drive instead of the one on PATH.
	//
	// Unlike every other field this one is about your machine rather than the
	// workload, so it does not travel usefully to anyone else. It is here so a
	// profile can be pinned to a build you are working on without exporting a
	// variable for every shell.
	RuntimeBin string `json:"runtimeBin,omitempty"`
	// RootfsType is how the guest root reaches the VM: block, virtiofs or
	// 9pfs. Empty leaves the runtime's default, which suits a profile that
	// only runs an agent. A profile that installs packages wants block: an
	// ext4 disk with room to write, rather than a share sized to the image.
	RootfsType string `json:"rootfsType,omitempty"`
	// GenericBoot marks a profile whose image was never built to be a guest
	// -- a plain OCI image with no kernel and no urunc metadata. The runtime
	// supplies the kernel and initrd and boots the image unmodified. Both
	// operating systems support this: the artifacts travel as OCI annotations
	// either way, on hull's command line or through nerdctl to urunc.
	GenericBoot bool `json:"genericBoot,omitempty"`
	// StaleCredentialFiles are paths under GuestHome that an older wrapper
	// used to write a credential into. brig never does, so finding one means
	// a real token is sitting on disk that nothing needs any more.
	StaleCredentialFiles []string `json:"staleCredentialFiles,omitempty"`
	// Onboarding is the first-run state file, if the agent has one.
	Onboarding *Onboarding `json:"onboarding,omitempty"`
	// HostCredential is the host-side fallback, if the agent has one.
	HostCredential *HostCredential `json:"hostCredential,omitempty"`
	// HostConfigDir is where the user's own agent configuration lives on the
	// host, and ProjectPaths are the subdirectories of it worth handing to
	// the guest. They are projected read-only under GuestHome at the same
	// relative location, so the agent finds them exactly where it looks for
	// its own, and only when the user opts in.
	//
	// Read-only is the point. These are the user's real skills, shared rather
	// than copied, so the sandbox cannot edit them.
	HostConfigDir string   `json:"hostConfigDir,omitempty"`
	ProjectPaths  []string `json:"projectPaths,omitempty"`
	// Unpublished marks a profile whose image we do not publish, so brig
	// can say why up front instead of letting the pull fail with a registry
	// 404. Build it yourself and pass --image, or import your own profile.
	Unpublished bool `json:"unpublished,omitempty"`
	// Reserved marks a profile that owns the workspace a session name could
	// otherwise slug onto. See the Reserved function. Exported so a file can
	// set it, and so it survives export and import.
	Reserved bool `json:"reserved,omitempty"`
}

// clone returns a copy that shares nothing mutable with the original.
//
// The registry hands profiles out by value, which copies the struct but not
// what its slices and pointers point at. Without this, a caller that appended
// to Forward, sorted StatePaths or cleared Deny would be editing the
// registry's own profile for every later caller in the process -- and brigd
// loads the registry once and serves many requests from it. Deny is the
// billing guard: an entry dropped there moves a sandbox onto metered API
// billing, which is the kind of change nobody notices until the invoice.
//
// Onboarding.Seed is cloned one level deep, which is as deep as a seed goes:
// it holds the handful of non-secret first-run flags an agent's state file
// needs, never a nested document.
func (p Profile) clone() Profile {
	p.Forward = slices.Clone(p.Forward)
	// A SecretDecl holds a []Source and a *bool, so a shallow element copy is
	// not a full one: a caller flipping Required through a clone would be
	// editing the registry's own answer for every later caller.
	p.Secrets = slices.Clone(p.Secrets)
	for i, d := range p.Secrets {
		p.Secrets[i].Sources = slices.Clone(d.Sources)
		if d.Required != nil {
			required := *d.Required
			p.Secrets[i].Required = &required
		}
	}
	// An EnvBinding carries a Refs chain, so the elements need cloning too:
	// the shallow copy that sufficed while a binding held only strings would
	// hand two profiles the same chain.
	p.Env = slices.Clone(p.Env)
	for i, b := range p.Env {
		p.Env[i].Refs = slices.Clone(b.Refs)
	}
	p.Deny = slices.Clone(p.Deny)
	p.StatePaths = slices.Clone(p.StatePaths)
	p.ProjectPaths = slices.Clone(p.ProjectPaths)
	p.StaleCredentialFiles = slices.Clone(p.StaleCredentialFiles)
	if p.Onboarding != nil {
		o := *p.Onboarding
		o.Seed = maps.Clone(o.Seed)
		p.Onboarding = &o
	}
	if p.HostCredential != nil {
		h := *p.HostCredential
		p.HostCredential = &h
	}
	return p
}

// Denied reports whether a variable is on the profile's denylist.
func (p Profile) Denied(name string) bool {
	for _, d := range p.Deny {
		if d == name {
			return true
		}
	}
	return false
}

// GuestUser is the guest account the home directory belongs to, derived from
// GuestHome so a profile states it once.
//
// Only meaningful for a profile whose workspace IS the home directory. The
// ubuntu profile mounts at /root/work, so this reads "work" for it; nothing
// consumes it there, and anything that starts to should take the account from
// the image instead.
func (p Profile) GuestUser() string {
	h := p.GuestHome
	for i := len(h) - 1; i >= 0; i-- {
		if h[i] == '/' {
			return h[i+1:]
		}
	}
	return h
}

// IsShell and IsGUI are how the rest of brig asks what a profile is, so that
// no caller has to know that an absent kind means agent.
func (p Profile) IsShell() bool { return p.kind() == KindShell }
func (p Profile) IsGUI() bool   { return p.kind() == KindGUI }

// kind is Kind with the default applied. Parse normalises, so a profile read
// from a file always carries a concrete kind; this covers the one that was
// built in Go without going through it.
func (p Profile) kind() Kind {
	if p.Kind == "" {
		return KindAgent
	}
	return p.Kind
}

// normaliseKind folds the deprecated shell and gui booleans into Kind and
// applies the default, so that an in-memory profile read from a file always
// carries a concrete kind.
//
// A conflict is an error rather than a silent precedence rule: either answer
// would surprise whoever wrote both spellings.
func (p *Profile) normaliseKind() error {
	if p.Shell && p.GUI {
		return fmt.Errorf("shell and gui are both set; a profile is one kind")
	}
	deprecated := Kind("")
	switch {
	case p.Shell:
		deprecated = KindShell
	case p.GUI:
		deprecated = KindGUI
	}
	if deprecated != "" {
		if p.Kind != "" && p.Kind != deprecated {
			return fmt.Errorf("kind is %q but %s: true says %q; "+
				"drop the deprecated field", p.Kind, deprecated, deprecated)
		}
		p.Kind = deprecated
	}
	p.Shell, p.GUI = false, false
	if p.Kind == "" {
		p.Kind = KindAgent
	}
	switch p.Kind {
	case KindAgent, KindShell, KindGUI:
		return nil
	default:
		return fmt.Errorf("unknown kind %q (agent, shell or gui)", p.Kind)
	}
}
