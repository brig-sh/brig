package wrap

import (
	"strings"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/verify"
)

// InfoData is `brig info` as data: the same facts (*Config).envelope and
// (*Config).Status assemble for the text report, gathered once here so the two
// forms cannot come to disagree about what a sandbox would receive. It is the
// payload behind the Info kind of the shared JSON envelope.
//
// It is read from the same Config, creds.Set and resolved secrets the text
// report reads -- not re-derived -- so a row added there and a field added here
// stay in step. The one promise this shares with every other --json shape is
// the one jsonout.go states: a credential is named and sourced, never valued,
// not here and not in an error string. listSecrets below reads provenance
// without decrypting, the same no-value read the text report makes.
//
// A method beside envelope and Status rather than a second gather, so the JSON
// and the text say the same thing for the same reason the block and the report
// already do.
func (c *Config) InfoData(set creds.Set) InfoDocument {
	d := InfoDocument{
		Session:     c.RawName,
		Profile:     c.Profile.Name,
		Sandbox:     c.VMName,
		Runtime:     c.infoRuntime(),
		Isolation:   c.isolationLine(),
		Workspace:   c.Workspace,
		Image:       InfoImage{Ref: c.Image, Pull: c.Pull},
		Verify:      c.infoVerify(),
		Network:     c.Network.Line(),
		Credentials: c.infoCredentials(set),
		Git:         c.infoGit(),
		ArgvExposed: runtime.ArgvExposed(set.Vars),
	}
	if c.Project != "" {
		d.Project = &InfoProject{Host: c.Project, Guest: c.GuestProject}
	}
	if len(c.Profile.Deny) > 0 {
		d.Deny = c.infoDeny(set)
	}
	d.GuestLogin = c.infoGuestLogin(set)
	if c.GitIdentity {
		d.Identity = &InfoIdentity{Cwd: c.Cwd, Name: c.GitName, Email: c.GitEmail}
	}
	return d
}

// InfoDocument is the Info payload. Every field is a fact the text report
// already prints; none carries a credential value.
type InfoDocument struct {
	Session     string           `json:"session,omitempty"`
	Profile     string           `json:"profile"`
	Sandbox     string           `json:"sandbox"`
	Runtime     InfoRuntime      `json:"runtime"`
	Isolation   string           `json:"isolation"`
	Workspace   string           `json:"workspace"`
	Project     *InfoProject     `json:"project,omitempty"`
	Image       InfoImage        `json:"image"`
	Verify      InfoVerify       `json:"verify"`
	Network     string           `json:"network"`
	Credentials []InfoCredential `json:"credentials"`
	Deny        *InfoDeny        `json:"deny,omitempty"`
	Git         InfoGit          `json:"git"`
	// ArgvExposed is the values BRIG_ENV_ARGV would put on the runtime's command
	// line, by name. Empty -- and omitted -- when the hatch is off, which is the
	// default. The names are variable names, never values.
	ArgvExposed []string         `json:"argvExposed,omitempty"`
	GuestLogin  []InfoGuestLogin `json:"guestLogin,omitempty"`
	Identity    *InfoIdentity    `json:"identity,omitempty"`
}

// InfoRuntime is the runtime this run resolved to, and whether it is there at
// all. Available is false with Kind and Bin empty when no runtime is on PATH:
// `brig info` answers without one and marks the line unavailable rather than
// failing, so --json must carry the same complete object.
type InfoRuntime struct {
	Kind      string `json:"kind,omitempty"`
	Bin       string `json:"bin,omitempty"`
	Available bool   `json:"available"`
}

// InfoImage is the guest image and the pull policy, the pair the IMAGE row
// prints.
type InfoImage struct {
	Ref  string `json:"ref"`
	Pull string `json:"pull"`
}

// InfoVerify is whether the image is checked before boot and whose policy says
// so: the mode (off, warn, require) and "brig" for brig's own policy or
// "replaced" for one the BRIG_VERIFY_* variables overrode. The outcome is not
// here -- `brig info` reports before any boot, so there is nothing verified yet
// to report, the same reason the VERIFY row prints the mode and not the result.
type InfoVerify struct {
	Mode   string `json:"mode"`
	Policy string `json:"policy"`
}

// InfoProject is the second host directory a run mounts beside the guest home,
// and where it lands in the guest. Absent -- and omitted -- on a run that named
// no project.
type InfoProject struct {
	Host  string `json:"host"`
	Guest string `json:"guest"`
}

// InfoCredential is one credential this run hands the guest: its name, and
// where the value came from ("environment", "host", "secret" or "file"). Never
// the value.
type InfoCredential struct {
	Name   string `json:"name"`
	Source string `json:"source,omitempty"`
}

// InfoDeny is what the denylist did to this run, rather than the list recited:
// which of the denied names are being forwarded anyway (with the guard off),
// which are still withheld, and whether the guard is off at all. The same
// answer reportDeny gives, because the list on its own is a claim about the
// future that BRIG_ALLOW_DENIED can make false.
type InfoDeny struct {
	Off       bool     `json:"off"`
	Forwarded []string `json:"forwarded,omitempty"`
	Withheld  []string `json:"withheld,omitempty"`
}

// InfoGit is the guest git-over-HTTPS setup: on or off, and when on, the user
// and the hosts it is written for.
type InfoGit struct {
	Enabled bool     `json:"enabled"`
	User    string   `json:"user,omitempty"`
	Hosts   []string `json:"hosts,omitempty"`
}

// InfoIdentity is the guest commit identity as resolved in the invoking
// directory, which per-directory includeIf rules make a property of where the
// command ran. Names and an email, never a signing key.
type InfoIdentity struct {
	Cwd   string `json:"cwd"`
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

// InfoGuestLogin is where the guest's login comes from and whether it is still
// good: a name and a source, and Expired when its own expiry has passed. Built
// from provenance the store carries without decrypting, so it is a fact about
// where a credential came from and never the credential.
type InfoGuestLogin struct {
	Name    string `json:"name,omitempty"`
	Source  string `json:"source,omitempty"`
	Expired bool   `json:"expired,omitempty"`
}

// infoRuntime is the InfoRuntime for this run, unavailable when no runtime is
// on PATH -- the case ErrNoRuntime is swallowed for info, which --json must not
// undo.
func (c *Config) infoRuntime() InfoRuntime {
	if c.Runtime == nil {
		return InfoRuntime{Available: false}
	}
	return InfoRuntime{Kind: c.Runtime.Kind(), Bin: c.Runtime.Bin(), Available: true}
}

// infoVerify is the InfoVerify for this run. The policy word is the same
// distinction verifyLine draws in prose: off, a replaced trust policy, or
// brig's own.
func (c *Config) infoVerify() InfoVerify {
	policy := "brig"
	switch {
	case c.Verify == verify.Off:
		policy = "off"
	case c.VerifyPolicy.Replaced():
		policy = "replaced"
	}
	return InfoVerify{Mode: string(c.Verify), Policy: policy}
}

// infoCredentials is every credential this run hands the guest, by name and
// source, the two ways credentialLine names -- an environment variable and a
// file written into a tmpfs -- gathered from the same structures. Only the
// presence of a value is read, never the value.
func (c *Config) infoCredentials(set creds.Set) []InfoCredential {
	var out []InfoCredential
	for _, n := range credentialNames(set) {
		name, source := splitCredentialName(n)
		out = append(out, InfoCredential{Name: name, Source: source})
	}
	for _, b := range c.Profile.Files {
		// An unreadable ref names no credential, so it is left out rather than
		// reported -- the same under-report credentialLine makes, and safe for the
		// same reason: writeSecretFiles would fail the run on it.
		r, err := profile.ParseRef(b.Ref)
		if err != nil {
			continue
		}
		if _, ok := c.secrets.Values[r.Name]; ok {
			out = append(out, InfoCredential{Name: r.Name, Source: "file"})
		}
	}
	return out
}

// splitCredentialName reads "NAME(source)" into its two parts, the annotation
// Set carries. A bare name has no annotation, which is the ambient environment,
// named as such so the source is a word rather than a gap.
func splitCredentialName(s string) (name, source string) {
	if i := strings.LastIndex(s, "("); i > 0 && strings.HasSuffix(s, ")") {
		return strings.TrimRight(s[:i], " "), s[i+1 : len(s)-1]
	}
	return s, "environment"
}

// infoDeny is the InfoDeny for this run, computed from the resolved set the way
// reportDeny is: what was admitted, what is withheld, and whether the guard is
// off.
func (c *Config) infoDeny(set creds.Set) *InfoDeny {
	d := &InfoDeny{Off: c.AllowDenied}
	for _, name := range c.Profile.Deny {
		if set.Has(name) {
			d.Forwarded = append(d.Forwarded, name)
			continue
		}
		d.Withheld = append(d.Withheld, name)
	}
	return d
}

// infoGit is the InfoGit for this run, the same on/off and, when on, the user
// and hosts the GitConfig block prints.
func (c *Config) infoGit() InfoGit {
	if !c.GitConfig {
		return InfoGit{Enabled: false}
	}
	return InfoGit{Enabled: true, User: c.GitUser, Hosts: c.GitHosts}
}

// infoGuestLogin is where the guest's login comes from, from the same two
// sources status.go reads: the deprecated Profile.HostCredential, and imported
// secrets' provenance. Both read without decrypting a value.
func (c *Config) infoGuestLogin(set creds.Set) []InfoGuestLogin {
	var out []InfoGuestLogin
	if hc := c.Profile.HostCredential; hc != nil {
		source, forwarded := sourceOf(credentialNames(set), hc.TargetVar)
		switch {
		case forwarded && source == "secret":
			out = append(out, InfoGuestLogin{Name: hc.TargetVar, Source: "secret"})
		case forwarded && source == "":
			out = append(out, InfoGuestLogin{Name: hc.TargetVar, Source: "environment"})
		case c.HostCred != nil && c.HostCred.Expired(nowMilli()):
			out = append(out, InfoGuestLogin{Name: hc.TargetVar, Source: c.HostCred.Source, Expired: true})
		case c.HostCred != nil:
			out = append(out, InfoGuestLogin{Name: hc.TargetVar, Source: c.HostCred.Source})
		}
	}
	if secrets, ok := c.listSecrets(); ok {
		for _, decl := range c.Profile.Secrets {
			s, found := secrets[decl.Name]
			if !found || s.Provenance.From == "" {
				continue
			}
			expired := s.Provenance.ExpiresAt != 0 && s.Provenance.ExpiresAt < nowMilli()
			out = append(out, InfoGuestLogin{Name: s.Name, Source: s.Provenance.From, Expired: expired})
		}
	}
	return out
}
