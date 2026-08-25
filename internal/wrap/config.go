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
	"github.com/brig-sh/brig/internal/secret"
	"github.com/brig-sh/brig/internal/session"
	"github.com/brig-sh/brig/internal/verify"
)

// NamePrefix is what every brig sandbox name begins with. It is how brig picks
// its own sandboxes out of a runtime that may be running other things, so it is
// the one part of the name that is not the caller's to drop -- see the BRIG_NAME
// check in Load. cmd/brig reads the same prefix back when it lists and resets.
const NamePrefix = "brig-"

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
	// HostConfig are host directories seeded into the workspace, empty unless
	// the run opted in. See hostProjections.
	HostConfig []hostSeed
	Image      string
	Pull       string
	Mem        int
	CPUs       int

	// ReadyTimeout is how long to wait for the guest agent after the runtime
	// reports the sandbox running. The two are not the same moment: the VMM
	// process is up several seconds before the in-guest agent binds its
	// listener.
	ReadyTimeout time.Duration

	// Env is what the guest sees, with BRIG_FORWARD_ENV already applied. See
	// envOverride for what that override does and does not reach. The
	// requirement list it may read from stays on Profile: one list, one place.
	Env []profile.EnvBinding
	// OpenStore returns the secret store, and is called only when the profile
	// declares secrets -- opening it unconditionally would raise a keychain
	// prompt for runs that read nothing. Replaced in tests.
	OpenStore func() (creds.SecretReader, error)
	// ReadKeychain reads the blob behind a profile's deprecated
	// hostCredential:, and is nil outside tests, where nil means the host's own
	// keychain. It exists because a test must not read the login keychain of
	// whoever runs the suite, and because the blob a test wants is one it wrote
	// itself. BRIG_CREDENTIALS_CMD used to serve that purpose by accident.
	ReadKeychain creds.KeychainRead

	// MacOSVersion reports the host's macOS version like "14.5", or "" off
	// macOS. A field so a test can pin the version the hypervisor preflight
	// sees without running on the OS in question; Load sets it to the real
	// host reader. See preflightHypervisor.
	MacOSVersion func() string
	// envWarnings are what building Env decided to drop, held until BuildEnv
	// has somewhere to print them: Load has no writer yet.
	envWarnings []string
	// secrets is what the store gave this run, kept so file delivery does not
	// read it twice -- and cleared the moment delivery is done, because a
	// plaintext refresh token has no business outliving its use.
	secrets creds.Resolution

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
	Verify       verify.Mode
	VerifyPolicy verify.Policy
	// BootDigest is the registry digest verifyDigest resolved and checked, set
	// only on a runtime that boots by digest. EnsureRunning hands it to the
	// runtime so the object that boots is the object that verified; empty means
	// boot the tag as given, which is every hull run and any path that could not
	// resolve a digest.
	BootDigest  string
	AllowRefs   bool
	AllowDenied bool
	// AllowExpired forwards the host credential even when its own expiry says it
	// is dead. Read here rather than where it is used so a typo in it refuses the
	// run before boot, like the other security switches, instead of at the moment
	// an expired credential happens to turn up.
	AllowExpired bool

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

	// BRIG_CREDENTIALS_CMD is gone, and a run that still sets it fails here
	// rather than proceeding. The variable named a command brig ran on every
	// boot to read the host credential, so a run that ignored it would boot a
	// sandbox with no login and no explanation -- the failure would surface
	// inside the guest, as a prompt to authenticate, which is the last place
	// anyone would connect back to a setting on the host.
	//
	// The replacement is two steps, and naming only the second sends the reader
	// into an error: --from-command fills one secret so it needs a name, and a
	// profile still on hostCredential: has no secrets: list for that name to be
	// in. Say both, in order.
	if cmd, ok := env.Get("CREDENTIALS_CMD"); ok && cmd != "" {
		return nil, fmt.Errorf("BRIG_CREDENTIALS_CMD has been removed. Declare the "+
			"credential under secrets: in %s, then store it once: brig secret import "+
			"%s <name> --from-command '<command>'", t.Name, t.Name)
	}

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

	// The sandbox name is settled first because the workspace may be read back
	// from an index keyed by it. See index.go.
	vmName := env.String("NAME", NamePrefix+t.Name)
	// BRIG_NAME replaces the whole sandbox name, and brig finds its own
	// sandboxes by the brig- prefix: ls lists what carries it and reset removes
	// what carries it. A name set through BRIG_NAME without the prefix boots and
	// runs fine and is then invisible to both -- a sandbox brig started that it
	// can no longer see or clean up. Refuse it at creation and say what it must
	// carry, rather than track brig's own sandboxes through some second channel:
	// the runtime hands back only the name `ps` prints, so the prefix is the one
	// mark that survives a stop, a reboot and a brig that has forgotten it ever
	// ran. See sandboxPrefix in cmd/brig, which reads the same mark back.
	if !strings.HasPrefix(vmName, NamePrefix) {
		return nil, fmt.Errorf("BRIG_NAME is %q, but a brig sandbox name must begin with %q so that "+
			"`brig ls` and `brig reset` can find it; use %q instead", vmName, NamePrefix, NamePrefix+vmName)
	}
	if slug != "" {
		vmName += "-" + slug
	}

	// Whether this invocation named a directory at all, tracked apart from the
	// value: the default is a directory too, and telling a chosen one from a
	// derived one is what lets the remembered path beat the second and not the
	// first.
	base, given := defaultWorkspace(t), false
	if v, ok := env.Get("WORKSPACE"); ok && v != "" {
		base, given = v, true
	}
	if o.Workspace != "" {
		base, given = o.Workspace, true
	}
	// Made absolute whichever of the two supplied it. A relative
	// --workspace was already resolved here and a relative BRIG_WORKSPACE was
	// not, which meant one exported variable named a different host directory
	// from every directory you ran brig in: same sandbox name, same profile,
	// and a workspace that moved with your shell. The sandbox's home is a host
	// path, so it is resolved once, here, against the directory the command was
	// actually invoked from.
	abs, err := filepath.Abs(base)
	if err != nil {
		return nil, err
	}
	base = abs
	// A named session suffixes the slug onto whatever the base already is, so
	// an exported BRIG_WORKSPACE keeps working alongside --name instead of
	// fighting it. An unnamed run adds nothing.
	workspace := base
	if slug != "" {
		workspace += "-" + slug
	}
	// Nothing on this invocation named a directory, so the one the sandbox was
	// started with beats the default just computed. Without this, a session
	// created with --workspace matches its own default on no later command, and
	// every flagless verb takes that for a stale share and restarts it -- see
	// index.go. The suffix is not reapplied: what was recorded is the workspace
	// as it was finally resolved, slug and all.
	if !given {
		if remembered := RememberedWorkspace(vmName); remembered != "" {
			workspace = remembered
		}
	}

	bindings, envWarnings := envOverride(t.Env, env.Fields("FORWARD_ENV", nil))

	// The security switches are read strictly: an unrecognised value on any of
	// them refuses the run rather than being guessed either way. strict marks
	// them at the call site so it is clear which knobs fail open (env.Bool) and
	// which fail closed (env.StrictBool). The first refusal wins; nothing built
	// below is used until strictErr is checked, so reading the rest is harmless.
	var strictErr error
	strict := func(key string, fallback bool) bool {
		b, err := env.StrictBool(key, fallback)
		if err != nil && strictErr == nil {
			strictErr = err
		}
		return b
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
		Env:            bindings,
		envWarnings:    envWarnings,
		OpenStore:      openStore,
		MacOSVersion:   macOSVersion,
		GitConfig:      strict("GIT_CONFIG", false),
		HostConfig:     hostProjections(t, o.Skills || env.Bool("SKILLS", false)),
		GitHosts:       env.Fields("GIT_HOSTS", []string{"github.com"}),
		GitIdentity:    env.Bool("GIT_IDENTITY", true),
		TrustWorkspace: strict("TRUST_WORKSPACE", true),
		Verify:         verify.ParseMode(env.String("VERIFY", string(verify.Warn))),
		VerifyPolicy:   verifyPolicy(env),
		AllowRefs:      strict("ALLOW_REFS", false),
		AllowDenied:    strict("ALLOW_DENIED", false),
		AllowExpired:   strict("ALLOW_EXPIRED", false),
		Cwd:            cwd,
		Out:            os.Stdout,
		Err:            os.Stderr,
		env:            env,
	}
	if strictErr != nil {
		return nil, strictErr
	}
	c.GuestCwd = GuestCwd(cwd, c.Workspace, t.GuestHome)
	c.resolveGitIdentity()
	return c, nil
}

// envOverride applies BRIG_FORWARD_ENV, which replaces the env-sourced set,
// and returns what it decided to drop.
//
// Only that set. Before bindings existed the variable replaced the whole of
// forward:, which was the whole of the mechanism -- so replacing the
// env-namespace bindings is what preserves its meaning rather than quietly
// widening it. A `ref: secrets.<name>` binding is the profile's own declaration
// of what the workload needs, and a list of bare variable names in the ambient
// environment was never able to speak about it.
//
// A name the profile already binds from somewhere else is dropped from the
// override rather than appended after it: the runtime merges last-wins, so
// appending would let whatever the shell happens to carry replace the keychain
// secret of the same name -- silently, for exactly the credential the profile
// exists to supply. The profile wins because the user wrote it; the ambient
// environment is what the terminal happened to be holding. Dropping something
// the user asked for is worth saying, so each one comes back as a warning.
//
// An empty list is not an override: env.Fields already falls back when the
// variable is unset or blank.
func envOverride(bindings []profile.EnvBinding, names []string) ([]profile.EnvBinding, []string) {
	if len(names) == 0 {
		return bindings, nil
	}
	kept := make([]profile.EnvBinding, 0, len(bindings)+len(names))
	bound := make(map[string]bool, len(bindings))
	for _, b := range bindings {
		if r, ok, err := b.Resolved(); err == nil && ok && r.Namespace == profile.NamespaceEnv {
			continue
		}
		kept = append(kept, b)
		bound[b.Name] = true
	}
	var warnings []string
	added := make(map[string]bool, len(names))
	for _, name := range names {
		if bound[name] {
			warnings = append(warnings, fmt.Sprintf(
				"ignoring %s from BRIG_FORWARD_ENV: the profile binds it to a source of "+
					"its own, and the profile's binding wins", name))
			continue
		}
		// A name listed twice is the same variable read from the same place,
		// so the repeat is dropped without a warning: there is nothing to
		// choose between and nothing the user would do about it.
		if added[name] {
			continue
		}
		added[name] = true
		// The name is taken as written, where a profile's own binding is held
		// to profile.Validate's rules (no =, space, tab or newline). The
		// asymmetry is deliberate: this is not a file someone else authored and
		// shipped, it is one word of one user's own variable, so a malformed
		// one is a typo rather than something to defend against. It costs
		// nothing to let through -- BRIG_FORWARD_ENV="A=B" binds the variable
		// named "A=B", whose lookup misses, and it is dropped like any unset
		// name.
		kept = append(kept, profile.EnvBinding{
			Name: name,
			Ref:  profile.NamespaceEnv + "." + name,
		})
	}
	return kept, warnings
}

// bindingNames is what a run could hand the guest, for reporting. Names only,
// never a value and never a ref's target.
func bindingNames(bindings []profile.EnvBinding) []string {
	names := make([]string, 0, len(bindings))
	for _, b := range bindings {
		names = append(names, b.Name)
	}
	return names
}

// openStore is the default OpenStore: the system keyring, opened on demand.
func openStore() (creds.SecretReader, error) { return secret.Open() }

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
//
// These are copied into the workspace rather than mounted read-only from the
// host. Read-only was the safer-looking choice and the wrong one: agents write
// inside these directories -- installing a plugin, populating a cache -- and a
// read-only mount turns that into an I/O error rather than a refusal it can
// handle. Copying gives the guest its own writable copy, and the host's stays
// untouched, which was the actual point of read-only.
func hostProjections(t profile.Profile, enabled bool) []hostSeed {
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
	var out []hostSeed
	for _, rel := range t.ProjectPaths {
		hostPath := filepath.Join(root, rel)
		info, err := os.Stat(hostPath)
		if err != nil || !info.IsDir() {
			continue
		}
		out = append(out, hostSeed{
			Host: hostPath,
			// Mirror the host layout: ~/.claude/skills becomes
			// <workspace>/.claude/skills, which the guest sees at
			// <guest home>/.claude/skills because the workspace is the home.
			Rel: filepath.Join(filepath.Base(root), rel),
		})
	}
	return out
}

// hostSeed is one host directory copied into the workspace.
//
// Rel is relative to the workspace, which is the guest's home, so the agent
// finds the result at the path it already looks in.
type hostSeed struct {
	Host string
	Rel  string
}
