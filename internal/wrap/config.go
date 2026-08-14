package wrap

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/session"
	"github.com/brig-sh/brig/internal/verify"
)

// Config is everything a run needs, resolved once.
type Config struct {
	Profile profile.Profile
	Runtime runtime.Runtime

	// RawName is the session name as typed; Slug is its path-safe form. The
	// raw name reaches the agent as its display name, so what you typed is
	// what you see; only the paths use the slug.
	RawName string
	Slug    string

	Workspace string
	VMName    string
	// HostConfig are host directories projected read-only into the guest,
	// empty unless the run opted in. See hostProjections.
	HostConfig []runtime.Share
	Image      string
	Pull       string
	Mem        int
	CPUs       int

	// ReadyTimeout is how long to wait for the guest agent after the runtime
	// reports the sandbox running. The two are not the same moment: the VMM
	// process is up several seconds before the in-guest agent binds its
	// listener.
	ReadyTimeout time.Duration

	// Forward is the list of environment variables to carry in.
	Forward []string

	GitConfig       bool
	GitHosts        []string
	GitIdentity     bool
	GitUser         string
	GitUserFromHost bool
	GitName         string
	GitEmail        string

	TrustWorkspace bool

	// Verify is how strictly the guest image is checked before boot, and
	// VerifyPolicy is what counts as ours.
	Verify         verify.Mode
	VerifyPolicy   verify.Policy
	AllowRefs      bool
	AllowDenied    bool
	CredentialsCmd string

	// Cwd is the host directory the command was invoked from, and GuestCwd is
	// where that lands inside the guest.
	Cwd      string
	GuestCwd string

	// HostCred is the credential read from the host during BuildEnv, kept so
	// the status report can say where the guest login comes from without
	// paying for a second keychain read.
	HostCred *creds.HostCredential

	Out io.Writer
	Err io.Writer

	env Env
}

// Options are the per-invocation overrides a command line can supply. Each
// one outranks the corresponding setting, which outranks the profile.
type Options struct {
	Name      string // session name, as typed
	Image     string
	Workspace string
	Mem       int
	CPUs      int
	// Skills opts in to projecting the host's own agent config (skills,
	// plugins) read-only into the guest. Off unless asked for: it is the
	// user's real config, and handing it to a sandbox should be a decision.
	Skills bool
}

// Load resolves the configuration for one invocation.
func Load(t profile.Profile, o Options, rt runtime.Runtime) (*Config, error) {
	env := NewEnv(t.Name, os.LookupEnv)
	rawName := o.Name

	slug := ""
	if rawName != "" {
		var err error
		if slug, err = session.Resolve(rawName); err != nil {
			return nil, err
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("cannot determine the current directory: %w", err)
	}

	base := env.String("WORKSPACE", defaultWorkspace(t))
	if o.Workspace != "" {
		abs, err := filepath.Abs(o.Workspace)
		if err != nil {
			return nil, err
		}
		base = abs
	}
	// A named session suffixes the slug onto whatever the base already is, so
	// an exported BRIG_WORKSPACE keeps working alongside --name instead of
	// fighting it. An unnamed run adds nothing.
	workspace := base
	vmName := env.String("NAME", "brig-"+t.Name)
	if slug != "" {
		workspace += "-" + slug
		vmName += "-" + slug
	}

	c := &Config{
		Profile:        t,
		Runtime:        rt,
		RawName:        rawName,
		Slug:           slug,
		Workspace:      workspace,
		VMName:         vmName,
		Image:          firstNonEmpty(o.Image, env.String("IMAGE", t.Image)),
		Pull:           env.String("PULL", "missing"),
		Mem:            firstPositive(o.Mem, env.Int("MEM", t.Mem)),
		CPUs:           firstPositive(o.CPUs, env.Int("CPUS", t.CPUs)),
		ReadyTimeout:   time.Duration(env.Int("READY_TIMEOUT", 30)) * time.Second,
		Forward:        env.Fields("FORWARD_ENV", t.Forward),
		GitConfig:      env.Bool("GIT_CONFIG", false),
		HostConfig:     hostProjections(t, o.Skills || env.Bool("SKILLS", false)),
		GitHosts:       env.Fields("GIT_HOSTS", []string{"github.com"}),
		GitIdentity:    env.Bool("GIT_IDENTITY", true),
		TrustWorkspace: env.Bool("TRUST_WORKSPACE", true),
		Verify:         verify.ParseMode(env.String("VERIFY", string(verify.Warn))),
		VerifyPolicy:   verifyPolicy(env),
		AllowRefs:      env.Bool("ALLOW_REFS", false),
		AllowDenied:    env.Bool("ALLOW_DENIED", false),
		CredentialsCmd: env.String("CREDENTIALS_CMD", ""),
		Cwd:            cwd,
		Out:            os.Stdout,
		Err:            os.Stderr,
		env:            env,
	}
	c.GuestCwd = GuestCwd(cwd, c.Workspace, t.GuestHome)
	c.resolveGitIdentity()
	return c, nil
}

// defaultWorkspace is ~/brig/<profile>.
func defaultWorkspace(t profile.Profile) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, "brig", t.Name)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func firstPositive(a, b int) int {
	if a > 0 {
		return a
	}
	return b
}

// verifyPolicy lets a user who publishes their own signed images point the
// check at their registry and their workflow, rather than having to turn it
// off because it only knows about ours.
func verifyPolicy(env Env) verify.Policy {
	p := verify.DefaultPolicy()
	p.Registry = env.String("VERIFY_REGISTRY", p.Registry)
	p.Identity = env.String("VERIFY_IDENTITY", p.Identity)
	p.Issuer = env.String("VERIFY_ISSUER", p.Issuer)
	p.Cosign = env.String("COSIGN_BIN", p.Cosign)
	return p
}

// GuestCwd maps the host directory a command was invoked from into the guest,
// so running brig from a project inside the workspace starts the agent in the
// matching guest directory.
//
// Outside the workspace there is nothing mounted to land in, so it falls back
// to the guest home. That includes a cwd inside the *unnamed* workspace while
// running a named session: this run mounts only its own workspace, so there is
// no guest directory matching the other path to start in.
func GuestCwd(cwd, workspace, guestHome string) string {
	rel, err := filepath.Rel(workspace, cwd)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return guestHome
	}
	if rel == "." {
		return guestHome
	}
	return path(guestHome, rel)
}

// path joins guest-side paths, which are always slash-separated whatever the
// host uses.
func path(base, rel string) string {
	return strings.TrimSuffix(base, "/") + "/" + filepath.ToSlash(rel)
}

// hostProjections resolves the host directories a profile wants to hand the
// guest read-only, or nothing at all when the run did not ask for them.
//
// How read-only looks from inside the guest is worth knowing before debugging
// it: Virtualization.framework enforces it on the virtiofs device, not through
// mount flags, so `mount` reports these as rw and a write fails with EPERM
// rather than EROFS. Trying to write is the only way to tell. Verified in a
// booted guest, with the shares nested inside the guest home mount.
//
// A path the user does not have is skipped rather than refused: somebody with
// skills but no plugins should not have to care about the difference. The
// guest path mirrors the host layout under GuestHome, so the agent finds them
// where it already looks.
func hostProjections(t profile.Profile, enabled bool) []runtime.Share {
	if !enabled || t.HostConfigDir == "" || len(t.ProjectPaths) == 0 {
		return nil
	}
	root := t.HostConfigDir
	if strings.HasPrefix(root, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		root = filepath.Join(home, root[2:])
	}
	var out []runtime.Share
	for _, rel := range t.ProjectPaths {
		hostPath := filepath.Join(root, rel)
		info, err := os.Stat(hostPath)
		if err != nil || !info.IsDir() {
			continue
		}
		out = append(out, runtime.Share{
			Host: hostPath,
			// Mirror the host layout: ~/.claude/skills becomes
			// <guest home>/.claude/skills.
			Guest:    filepath.Join(t.GuestHome, filepath.Base(root), rel),
			ReadOnly: true,
		})
	}
	return out
}
