// Package agent holds the per-agent templates as data.
//
// A template is not code. It is the handful of facts that differ between one
// coding agent and the next -- which binary to exec, which environment
// variables carry its credentials, which of those must never be forwarded,
// where its state lives in the home directory, and which guest image to boot.
// Everything else -- workspace-as-home, credential resolution, git config,
// the VM lifecycle -- is the same for every agent and lives in internal/wrap.
//
// Template names are load-bearing strings: a name here is also the image name
// in brig-sh/community-images and the taxonomer signature.AgentRuntime value.
// Renaming one means renaming all three.
package agent

import "sort"

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

// Template is one agent, as data.
type Template struct {
	// Name is the template name, the community-images image name, and the
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
	// and non-empty in brig's own environment. brig resolves nothing itself,
	// so any secret backend works: whatever populates the environment is
	// enough.
	Forward []string `json:"forward,omitempty"`
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
	// Shell marks a template whose "agent" is the guest shell itself: `brig
	// run` opens a login shell, and any trailing words are run as one
	// command. There is no CLI to pass arguments to.
	Shell bool `json:"shell,omitempty"`
	// GUI marks a windowed agent: the VM boots with a graphical console and
	// there is no CLI pass-through.
	GUI bool `json:"gui,omitempty"`
	// GUITitle is the window title for a GUI agent.
	GUITitle string `json:"guiTitle,omitempty"`
	// Mem is the guest memory in MB, CPUs the vCPU count.
	Mem  int `json:"mem"`
	CPUs int `json:"cpus"`
	// StaleCredentialFiles are paths under GuestHome that an older wrapper
	// used to write a credential into. brig never does, so finding one means
	// a real token is sitting on disk that nothing needs any more.
	StaleCredentialFiles []string `json:"staleCredentialFiles,omitempty"`
	// Onboarding is the first-run state file, if the agent has one.
	Onboarding *Onboarding `json:"onboarding,omitempty"`
	// HostCredential is the host-side fallback, if the agent has one.
	HostCredential *HostCredential `json:"hostCredential,omitempty"`
	// reserved marks a template owning a workspace that a session name could
	// otherwise slug onto. See Reserved.
	reserved bool
}

// templates is the registry. Wave 1 ships claude-code and codex as the proven
// core; gemini, grok and opencode are example templates that build and run
// but have had less mileage. cursor is unpublished pending a terms check.
var templates = []Template{
	{
		Name:      "claude-code",
		Desc:      "Claude Code (Anthropic)",
		Binary:    "claude",
		Image:     "ghcr.io/brig-sh/claude-code:arm64",
		GuestHome: "/home/claude",
		// CLAUDE_CODE_OAUTH_TOKEN authenticates Claude Code outright. The
		// refresh-token pair is the documented way to provision auth without
		// a browser: with both set, `claude auth login` exchanges the refresh
		// token instead of opening one, which the guest cannot do. GH_TOKEN
		// is for git over HTTPS, not for the agent.
		Forward: []string{
			"CLAUDE_CODE_OAUTH_TOKEN",
			"CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
			"CLAUDE_CODE_OAUTH_SCOPES",
			"GH_TOKEN",
		},
		// Both outrank CLAUDE_CODE_OAUTH_TOKEN in Claude Code's auth
		// precedence, so forwarding either would move the sandbox off the
		// subscription and onto metered API billing without saying so.
		Deny:                 []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"},
		StatePaths:           []string{".claude", ".claude.json"},
		StaleCredentialFiles: []string{".claude/.credentials.json"},
		Headless:             true,
		Mem:                  4096,
		CPUs:                 4,
		Onboarding: &Onboarding{
			File: ".claude.json",
			Seed: map[string]any{
				"hasCompletedOnboarding": true,
				"hasTrustDialogAccepted": true,
			},
			TrustKey: [2]string{"projects", "hasTrustDialogAccepted"},
		},
		HostCredential: &HostCredential{
			KeychainService: "Claude Code-credentials",
			TokenField:      "accessToken",
			ExpiryField:     "expiresAt",
			TargetVar:       "CLAUDE_CODE_OAUTH_TOKEN",
			RenewHint:       "run claude on the host once to renew it",
		},
	},
	{
		Name:      "claude-desktop",
		Desc:      "Claude Desktop (Anthropic), in a graphical window",
		Binary:    "", // GUI: nothing to exec, the app owns the console
		Image:     "ghcr.io/nofireai/urunc-claude-desktop:aarch64",
		GuestHome: "/home/claude",
		Forward: []string{
			"CLAUDE_CODE_OAUTH_TOKEN",
			"CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
			"CLAUDE_CODE_OAUTH_SCOPES",
			"GH_TOKEN",
		},
		Deny:       []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"},
		StatePaths: []string{".config/Claude", ".claude"},
		GUI:        true,
		GUITitle:   "Claude Desktop",
		Mem:        6144,
		CPUs:       4,
		reserved:   true,
	},
	{
		Name:      "codex",
		Desc:      "Codex (OpenAI)",
		Binary:    "codex",
		Image:     "ghcr.io/brig-sh/codex:arm64",
		GuestHome: "/home/codex",
		Forward:   []string{"GH_TOKEN"},
		// Codex signs in with `codex login --device-auth` and keeps the
		// result in ~/.codex/auth.json, inside the persisted home. A
		// forwarded OPENAI_API_KEY is the metered path and would take that
		// decision away from you, so it is denied for the same reason
		// ANTHROPIC_API_KEY is: put it in BRIG_FORWARD_ENV together with
		// BRIG_ALLOW_DENIED=1 if metered billing is what you want.
		Deny:       []string{"OPENAI_API_KEY"},
		StatePaths: []string{".codex"},
		Headless:   true,
		Mem:        4096,
		CPUs:       4,
	},
	{
		Name:   "ubuntu",
		Desc:   "A plain Ubuntu shell, running as root",
		Binary: "bash",
		Image:  "ghcr.io/nofireai/urunc-ubuntu:aarch64",
		// The workspace is mounted at /root/work rather than over the home
		// directory: this guest runs as root and the point of it is the
		// machine, not an agent's state. GuestHome names where the workspace
		// lands, which is what every path calculation needs.
		GuestHome: "/root/work",
		Forward:   []string{"GH_TOKEN"},
		Shell:     true,
		Mem:       2048,
		CPUs:      2,
	},
	{
		Name:       "gemini",
		Desc:       "Gemini CLI (Google) -- example template",
		Binary:     "gemini",
		Image:      "ghcr.io/brig-sh/gemini:arm64",
		GuestHome:  "/home/gemini",
		Forward:    []string{"GEMINI_API_KEY", "GH_TOKEN"},
		StatePaths: []string{".gemini"},
		Headless:   true,
		Mem:        4096,
		CPUs:       4,
	},
	{
		Name:       "grok",
		Desc:       "Grok CLI (xAI) -- example template",
		Binary:     "grok",
		Image:      "ghcr.io/brig-sh/grok:arm64",
		GuestHome:  "/home/grok",
		Forward:    []string{"XAI_API_KEY", "GH_TOKEN"},
		StatePaths: []string{".grok"},
		Headless:   true,
		Mem:        4096,
		CPUs:       4,
	},
	{
		Name:       "opencode",
		Desc:       "opencode (OSS, provider-agnostic) -- example template",
		Binary:     "opencode",
		Image:      "ghcr.io/brig-sh/opencode:arm64",
		GuestHome:  "/home/opencode",
		Forward:    []string{"OPENROUTER_API_KEY", "GH_TOKEN"},
		StatePaths: []string{".local/share/opencode", ".config/opencode"},
		Headless:   true,
		Mem:        4096,
		CPUs:       4,
	},
	{
		Name:       "cursor",
		Desc:       "Cursor Agent -- example template, image unpublished pending a terms check",
		Binary:     "cursor-agent",
		Image:      "ghcr.io/brig-sh/cursor:arm64",
		GuestHome:  "/home/cursor",
		Forward:    []string{"CURSOR_API_KEY", "GH_TOKEN"},
		StatePaths: []string{".cursor", ".local/share/cursor-agent"},
		Headless:   true,
		Mem:        4096,
		CPUs:       4,
	},
}

// aliases are the short spellings people actually type.
var aliases = map[string]string{
	"claude":  "claude-code",
	"desktop": "claude-desktop",
}

// Lookup returns the template for a name or alias. A custom template wins:
// overriding a built-in is how you pin your own image for an agent brig
// already knows about.
func Lookup(name string) (Template, bool) {
	if t, ok := custom[name]; ok {
		return t, true
	}
	if canonical, ok := aliases[name]; ok {
		name = canonical
	}
	if t, ok := custom[name]; ok {
		return t, true
	}
	for _, t := range templates {
		if t.Name == name {
			return t, true
		}
	}
	return Template{}, false
}

// All returns every template, built-in and custom, in name order. A custom
// template replaces the built-in of the same name.
func All() []Template {
	seen := map[string]bool{}
	var out []Template
	for _, t := range customTemplates() {
		seen[t.Name] = true
		out = append(out, t)
	}
	for _, t := range templates {
		if !seen[t.Name] {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names returns every template name, in order.
func Names() []string {
	out := make([]string, 0, len(templates))
	for _, t := range All() {
		out = append(out, t.Name)
	}
	return out
}

// Reserved reports whether a session slug collides with a template that owns
// the workspace it would land on. Without this, `brig run claude --name
// desktop` puts a Claude Code session on the Desktop app's workspace.
func Reserved(slug string) (string, bool) {
	for _, t := range templates {
		if !t.reserved {
			continue
		}
		if slug == t.Name {
			return t.Name, true
		}
		// The trailing word is what a slug of the template name reads as:
		// claude-desktop is reserved as "desktop" too.
		if i := lastDash(t.Name); i >= 0 && slug == t.Name[i+1:] {
			return t.Name, true
		}
	}
	return "", false
}

func lastDash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '-' {
			return i
		}
	}
	return -1
}

// Denied reports whether a variable is on the template's denylist.
func (t Template) Denied(name string) bool {
	for _, d := range t.Deny {
		if d == name {
			return true
		}
	}
	return false
}

// GuestUser is the guest account the home directory belongs to, derived from
// GuestHome so a template states it once.
//
// Only meaningful for a template whose workspace IS the home directory. The
// ubuntu template mounts at /root/work, so this reads "work" for it; nothing
// consumes it there, and anything that starts to should take the account from
// the image instead.
func (t Template) GuestUser() string {
	h := t.GuestHome
	for i := len(h) - 1; i >= 0; i-- {
		if h[i] == '/' {
			return h[i+1:]
		}
	}
	return h
}
