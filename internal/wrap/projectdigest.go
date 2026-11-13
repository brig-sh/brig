package wrap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
)

// A session of a Brigfile target is booted from what the repository says,
// and the repository can change that between two runs. A running sandbox
// keeps what it was booted with: its image, its size, its network and the
// rules of its policies. So the run of a target records a digest of what it
// booted, and the next run compares it with what it would boot now, before it
// reuses the sandbox.

// configDigestVersion changes when what the digest covers changes, so that a
// digest recorded under the old rule reads as changed rather than as a match
// by chance.
const configDigestVersion = 1

// configDigestInput is everything the digest covers.
//
// It is what a boot is made of: the profile after the target's overrides,
// the policy documents, the image, the size, the network asked for, and the
// target's arguments, with the project root that names the session. It leaves
// out what a sandbox does while it runs, the prose a reader sees, and every
// host path but the root: the guest home is named after the sandbox, and the
// directory brig was started in changes nothing that boots.
type configDigestInput struct {
	Version  int             `json:"v"`
	Root     string          `json:"root"`
	Profile  profile.Profile `json:"profile"`
	Policies []policy.Policy `json:"policies"`
	Image    string          `json:"image"`
	Mem      int             `json:"mem"`
	CPUs     int             `json:"cpus"`
	Network  string          `json:"network"`
	Args     []string        `json:"args"`
}

// ConfigDigest is the digest of the configuration this run would boot. See
// configDigestInput for what it covers.
//
// The profile is hashed whole but for its prose: its description and the
// hints of its secrets. A field the profile gains later is then covered from
// the start, and the worst a new prose field costs is a session reported as
// changed when it is not.
func (c *Config) ConfigDigest() string {
	// Without its prose, and with the image and the size covered below as
	// resolved. See digestProfile.
	p := digestProfile(c.Profile)
	in := configDigestInput{
		Version:  configDigestVersion,
		Profile:  p,
		Policies: c.policyDocs,
		Image:    c.Image,
		Mem:      c.Mem,
		CPUs:     c.CPUs,
	}
	// A posture this line or a setting asked for. One read back from the
	// sandbox is the sandbox's own state, and the profile's is in the profile.
	if c.netExplicit {
		in.Network = string(c.askedNetwork)
	}
	if c.target != nil {
		in.Root, in.Args = c.target.Root, c.target.Args
	}
	blob, err := json.Marshal(in)
	if err != nil {
		// Every field above marshals. A digest that cannot be taken is one
		// that matches nothing, so a session is never reused on it.
		return "unhashable: " + err.Error()
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

// ConfigChanged reports whether the session of this target was recorded with
// a configuration digest other than this run's, and whether a digest is
// recorded at all. A session with no record, or a record with no digest, is
// not reported as changed: there is nothing to compare.
func (c *Config) ConfigChanged() (changed, recorded bool) {
	ps, ok := LookupProject(c.VMName)
	if !ok || ps.ConfigDigest == "" {
		return false, false
	}
	return ps.ConfigDigest != c.ConfigDigest(), true
}

// ProjectSession is the entry the project index keeps for this run's
// session, with this run's digest, or false when the run is not of a target.
func (c *Config) ProjectSession() (ProjectSession, bool) {
	if c.identity == nil || c.target == nil {
		return ProjectSession{}, false
	}
	return ProjectSession{
		Sandbox:      c.VMName,
		Root:         c.target.Root,
		Target:       c.target.Name,
		Session:      c.target.Session,
		Home:         c.Workspace,
		ConfigDigest: c.ConfigDigest(),
		Brigfile:     c.target.Brigfile,
	}, true
}

// Recreate removes the sandbox of a recorded session of a target, so that the
// next run boots the configuration it resolves, on the same guest home.
//
// The home and the project entry stay, and the entry is what keeps the next
// run from taking the home for an orphan. The session's entry in
// sessions.json goes, with the network the sandbox booted on, so the next
// boot resolves both afresh rather than from the sandbox that was removed.
// The look at the project's .git stays, so a change the guest made before the
// recreate is still named. The published ports stay: they belong to the
// session, as they survive any other recreate.
func (c *Config) Recreate() error {
	if !IsProjectSandbox(c.VMName) {
		return fmt.Errorf("%s is not the sandbox of a Brigfile target", c.VMName)
	}
	if _, ok := LookupProject(c.VMName); !ok {
		return fmt.Errorf("brig has no record of the session of %s, so a recreate would leave its "+
			"guest home to be deleted as an orphan", c.VMName)
	}
	c.forgetRoutes()
	_ = c.Runtime.Stop(c.VMName)
	if err := c.Runtime.Remove(c.VMName); err != nil {
		// The removal fails when there was no sandbox to remove. Only a
		// sandbox that is still there, or a runtime that cannot say, is a
		// failure.
		if held, herr := sandboxExists(c.Runtime, c.VMName); herr != nil || held {
			return fmt.Errorf("could not remove the sandbox %s to recreate it: %w", c.VMName, err)
		}
	}
	dropSessionEntries(c.VMName)
	runtime.ForgetBootedNet(c.VMName)
	return nil
}
