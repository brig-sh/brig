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

// Verbosity is how much a run says about itself.
//
// The rule it encodes: by default, print what the user has to act on. A
// warning is an action and stays; brig's own narration and the
// runtime's own output are not, and wait to be asked for. Below the default is
// the form a script reads, where the output is identifiers and the errors that
// stopped them, and nothing in between.
//
// Ordered rather than a set of modes, so every print site asks the same
// question -- is this worth saying at the level we are at -- with one
// comparison instead of a switch that has to be kept exhaustive.
type Verbosity int

const (
	// Quiet is identifiers and errors only, for a script. An error is never
	// printed at this level; it is returned, and the caller prints it.
	Quiet Verbosity = iota - 1
	// Normal is the default: warnings, the execution envelope, and one line
	// each end of a long operation. It is the zero value, so a Config built by
	// hand is at the level a person reads rather than silent.
	Normal
	// Verbose adds brig's own progress and the runtime's own output.
	Verbose
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
	// slugMigration is the notice for a session whose home has moved since it
	// was created, held until BuildEnv for the same reason. See
	// slugMigrationNotice.
	slugMigration []string
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
	// Network is the posture this run was resolved to. Held here so the
	// envelope, the report and the spec handed to the runtime cannot disagree
	// about what a reader was told.
	Network Network
	// BootDigest is the registry digest verifyDigest resolved and checked, set
	// only on a runtime that boots by digest. EnsureRunning hands it to the
	// runtime so the object that boots is the object that verified; empty means
	// boot the tag as given, which is every hull run and any path that could not
	// resolve a digest.
	BootDigest string
	// envelopeShown records that the execution envelope has been printed for
	// this run, so the VERIFY row's fact is not then repeated as a standalone
	// line. Set by PrintPreRunEnvelope, read by verifyImage.
	envelopeShown bool
	// verified is what actually checked out during this run's verification
	// step, in the order it was checked: "image", "boot assets". Empty when
	// nothing was positively verified -- BRIG_VERIFY=off, an image nobody
	// claimed to publish, a machine with no cosign -- each of which says so on
	// its own, at warning level, where it belongs.
	//
	// Collected rather than reported as it happens because the run says it in
	// one line for the whole step. See sayVerified.
	verified    []string
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

	// Project is the host directory this run was told to mount beside the
	// guest home, and GuestProject is where it lands in the guest. Both are
	// empty on a run that named none, which is what every path below reads to
	// tell the two shapes apart.
	//
	// A second mount rather than a redirection of the first, because the guest
	// home IS the agent's home: its dotfiles, its onboarding state, its
	// caches. Pointing that mount at a repository would put all of it in the
	// repository. So the home stays brig's own and the project arrives
	// alongside it, outside the home -- see GuestProject.
	//
	// Per run, not per session: the session's identity is its ref, which is
	// the agent and the label, which is the home. Two runs of one session on
	// two projects are the same session, so nothing here reaches the sandbox
	// name or the slug. See mountProject and sessionEntry.
	Project      string
	GuestProject string

	// HostCred is the credential read from the host during BuildEnv, kept so
	// the status report can say where the guest login comes from without
	// paying for a second keychain read.
	HostCred *creds.HostCredential

	// NoTerminal declares that this run has no terminal to put a question to,
	// whatever this process's own stdin happens to be. brigd sets it: started
	// from a shell its stdin is a terminal, but it is the daemon's terminal and
	// not the client's, so a question asked there is asked of nobody while the
	// caller waits for an answer that cannot arrive. See confirm.
	NoTerminal bool

	Out io.Writer
	Err io.Writer

	// Progress is where the run narrates what it is doing -- the line that says
	// a boot has started, and nothing that anybody has to act on.
	//
	// Separate from Err because a caller that collects what a run said and
	// hands it to somebody else has to be able to tell the two apart. brigd is
	// that caller: it returns Err to the client as the request's warnings, and
	// with the narration in there a boot that went perfectly came back carrying
	// "starting sandbox ..." as a warning about itself. On a terminal the
	// distinction does not arise, both being lines on the same stderr, which is
	// why it went unnoticed for as long as the CLI was the only caller.
	Progress io.Writer

	// Verbosity decides how much of the above is written. See the type: Err
	// carries warnings and goes quiet only under -q, and Progress carries
	// narration and stays empty until --verbose asks for it. Out is not
	// levelled -- it is the report `brig info` was asked for by name.
	Verbosity Verbosity

	env Env
}

// Options are the per-invocation overrides a command line can supply. Each
// one outranks the corresponding setting, which outranks the profile.
type Options struct {
	Name      string // session name, as typed
	Image     string
	Workspace string
	// Project is the directory named as run's positional argument, to be
	// mounted beside the guest home. Relative is fine: it is resolved against
	// the directory the command was invoked from, because `brig run claude .`
	// is the line the positional exists for.
	Project string
	// NoProject asks for this run to have no project at all, and is the only
	// way to say so once a project is remembered: a positional names a
	// directory, and no directory names absence. Without it a session handed a
	// project keeps it for the rest of its life, since every later flagless
	// verb inherits it.
	NoProject bool
	Mem       int
	CPUs      int
	// Skills opts in to projecting the host's own agent config (skills,
	// plugins) read-only into the guest. Off unless asked for: it is the
	// user's real config, and handing it to a sandbox should be a decision.
	Skills bool
	// Network is the posture asked for on the command line, and beats both the
	// setting and the profile. Empty means nothing was asked for.
	Network string
	// Verbosity is how much this invocation was asked to say: --verbose and
	// -q, which are read left of the verb. The zero value is the default
	// level, so a caller with no opinion -- brigd, and every test that builds
	// Options by hand -- gets what a person reads.
	Verbosity Verbosity
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
		if slug, err = session.Resolve(t.Name, rawName); err != nil {
			return nil, err
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("cannot determine the current directory: %w", err)
	}

	// The sandbox name is settled first because the entry read back below is
	// only good for the sandbox it was recorded against. See index.go.
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
	// Kept before the suffix goes on, for the migration notice below: it has
	// to name the sandbox an older release derived from this same profile.
	vmBase := vmName
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
	//
	// Looked up by ref, which is the profile as it resolved and the slug, so
	// the entry a run under one spelling of the profile wrote is the entry a
	// run under its alias reads. Both halves of the key are in hand here.
	if !given {
		if remembered := rememberedWorkspace(sessionKey(t.Name, slug), vmName); remembered != "" {
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

	verifyMode, err := verify.ParseModeStrict(env.String("VERIFY", ""))
	if err != nil && strictErr == nil {
		strictErr = err
	}

	// Flag beats setting beats profile, the order every other setting follows.
	// Resolved once here so the envelope row and the spec cannot disagree. The
	// source travels with the value so a refusal names what was actually typed.
	netValue, netSource := o.Network, "--network"
	if netValue == "" {
		netValue, netSource = env.String("NETWORK", ""), env.settingName("NETWORK")
	}
	if netValue == "" {
		netValue, netSource = t.Network, "the profile's network:"
	}
	network, err := ParseNetworkStrict(netValue, netSource)
	if err != nil && strictErr == nil {
		strictErr = err
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
		HostConfig:     hostProjections(t, o.Skills || strict("SKILLS", false)),
		GitHosts:       env.Fields("GIT_HOSTS", []string{"github.com"}),
		GitIdentity:    env.Bool("GIT_IDENTITY", true),
		TrustWorkspace: strict("TRUST_WORKSPACE", true),
		Network:        network,
		Verify:         verifyMode,
		VerifyPolicy:   verifyPolicy(env),
		AllowRefs:      strict("ALLOW_REFS", false),
		AllowDenied:    strict("ALLOW_DENIED", false),
		AllowExpired:   strict("ALLOW_EXPIRED", false),
		Cwd:            cwd,
		Out:            os.Stdout,
		Err:            os.Stderr,
		Progress:       os.Stderr,
		Verbosity:      o.Verbosity,
		env:            env,
	}
	if strictErr != nil {
		return nil, strictErr
	}
	c.GuestCwd = GuestCwd(cwd, c.Workspace, t.GuestHome)
	// And then the project, which replaces that derivation: it is a directory
	// of its own, mounted beside the home, so the cwd-under-home rule has
	// nothing to say about it. Resolved after the Config exists because it
	// reads the workspace and the cwd this run resolved and writes three of its
	// fields.
	//
	// A positional on this invocation wins, and when there is none the project
	// the sandbox was started with stands -- the same two-step the workspace
	// above follows, for the same reason and by the same read. An invocation
	// that names no directory is not asking for a different one: `brig sh
	// claude@x` said nothing about a project, and taking that for "no project"
	// destroyed the mount, the guest's memory-only state and the index entry
	// recording it. Looked up by ref and sandbox, so it has to happen here,
	// after both are resolved. See rememberedProject and projectShareStale.
	//
	// --no-project is the third answer, and the reason it exists: inheriting
	// left no way to say "not this time". It is read here rather than as an
	// empty Project, because empty is what an invocation that said nothing
	// looks like and those two have to stay apart.
	switch {
	case o.NoProject:
		// Nothing to mount, and nothing remembered is consulted. The sandbox
		// recreates without the share, which is projectShareStale's
		// c.Project == "" branch reached deliberately for once.
	case o.Project != "":
		if err := c.mountProject(o.Project); err != nil {
			return nil, err
		}
	default:
		remembered := rememberedProject(sessionKey(t.Name, slug), vmName)
		if remembered == "" {
			break
		}
		// The error is dropped rather than returned, which is the one place the
		// two steps differ. A directory this line never named is not the user's
		// to fix, so a remembered project that has since been renamed or
		// deleted leaves the run with no project instead of refusing it -- and
		// EnsureRunning then names it, in the warning about the restart that
		// takes the dead mount away.
		_ = c.mountProject(remembered)
	}
	// After the Config is built, because the notice reads the pair this run
	// resolved to and reports it against the pair the old one had.
	c.slugMigration = c.slugMigrationNotice(base, vmBase)
	if err := c.resolveGitIdentity(); err != nil {
		return nil, err
	}
	return c, nil
}

// slugMigrationNotice is what this run has to be told about a session created
// before a slug stopped being cut to ten characters, or nothing when this run
// is not one.
//
// A longer --name now keeps its name in full, so it derives a different slug
// than it used to -- and both the sandbox name and the guest home come off the
// slug on every run. The same command that worked yesterday therefore boots a
// new sandbox on a new home directory, and the old pair is orphaned: the work
// in the old workspace is on the host and is still there, but state inside the
// guest is not, and a login made in the sandbox is the half of that people
// notice.
//
// Both conditions have to hold before this says anything, because a notice
// that reaches people it does not apply to is one they learn to skip. The slug
// has to have actually moved -- a name of ten characters or fewer slugs today
// exactly as it always did -- and the directory it moved from has to be on
// disk, since a long name that never ran has nothing orphaned to name.
//
// The old home is read back from the session index first and derived only as a
// fallback, so a session started under --workspace or BRIG_WORKSPACE is named
// where it actually is rather than where this invocation's base would have put
// it.
func (c *Config) slugMigrationNotice(base, vmBase string) []string {
	if c.RawName == "" {
		return nil
	}
	old := session.LegacySlug(c.RawName)
	if old == "" || old == c.Slug {
		return nil
	}
	oldVM := vmBase + "-" + old
	oldWorkspace := rememberedWorkspace(sessionKey(c.Profile.Name, old), oldVM)
	if oldWorkspace == "" {
		oldWorkspace = base + "-" + old
	}
	// Only a stat that says the directory is there. One that fails any other
	// way has not established that there is anything to move, and guessing
	// from it would put the notice in front of the run that cannot act on it.
	if _, err := os.Stat(oldWorkspace); err != nil {
		return nil
	}
	return []string{
		fmt.Sprintf("session %q used to be shortened to %q and now keeps its name in full, "+
			"so it has a new home and a new sandbox: %s (%s) instead of %s (%s).",
			c.RawName, old, c.Workspace, c.VMName, oldWorkspace, oldVM),
		fmt.Sprintf("Nothing reads %s now. The work in it is on the host, so move or delete "+
			"it at your leisure -- but state inside the old sandbox does not come across, "+
			"so this session may ask you to log in again.", oldWorkspace),
	}
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

// guestProjectRoot is where a named project is mounted in the guest, and the
// one thing about it that matters is that it is not under any profile's
// GuestHome.
//
// That is the mechanical guarantee behind mounting a project at all: a
// directory under /work cannot be mistaken for home state and cannot collide
// with the agent's dotfiles, by layout rather than by a convention someone has
// to remember. A constant rather than a profile field because it is a fact
// about brig's mount layout, not about any one agent.
const guestProjectRoot = "/work"

// GuestProject is where the host directory dir is mounted in the guest:
// /work/<basename>.
//
// The basename rather than the whole path, so the agent's prompt and every
// path it prints name the project the way its owner does. Two projects with
// one basename cannot be mounted at once, and nothing tries to: one run mounts
// one project.
func GuestProject(dir string) string {
	return guestProjectRoot + "/" + filepath.Base(dir)
}

// mountProject resolves the project this run named: the host directory, the
// guest path it is mounted at, and the working directory the agent starts in.
//
// The directory has to be there. Under the old grammar this word reached the
// agent, so a line that passed one through is a line this release changes the
// meaning of -- and failing here, naming both the directory and the way past
// it, is what makes that legible. The alternative considered on #6 was to read
// the word as a project only when it names an existing directory, which was
// dropped for good reason: the meaning of an argument would then depend on the
// filesystem, which is hard to explain and harder to script. The meaning is
// fixed; a directory that is not there is simply an error.
func (c *Config) mountProject(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err == nil && !info.IsDir() {
		err = fmt.Errorf("it is not a directory")
	}
	if err != nil {
		return fmt.Errorf("cannot mount %s as this run's project: %w. brig run reads the "+
			"word after the ref as a directory to mount; if it is an argument for the "+
			"agent, put it after -- instead", abs, err)
	}
	// Resolved before it is judged, because filepath.Abs cleans a path
	// lexically and stops there: `/Users/..` collapses to `/`, but `~/mnt ->
	// /` does not, and the guard below was reading the spelling rather than
	// the directory. A link is a way around every rule made about a path
	// unless the rule is made about what the path resolves to -- and the
	// person who follows a link is not necessarily the person who made it.
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// It stats, so this is a path brig cannot establish the truth about
		// rather than one that is not there. Refused rather than checked
		// lexically and let through, because the lexical check is exactly the
		// one that does not hold here.
		return fmt.Errorf("cannot mount %s as this run's project: its real path could not be "+
			"resolved: %w", abs, err)
	}
	// A filesystem root has no basename to mount it under -- filepath.Base
	// gives back the separator -- and /work// is not a guest path. Nobody means
	// to hand an agent the whole machine, so say what to name instead.
	if filepath.Base(real) == string(filepath.Separator) {
		// The resolution is named only when it is the thing the reader cannot
		// see. `brig run claude /` needs no explaining; a link's own name
		// plainly has a basename, so there "it has no name to mount it under"
		// would read as nonsense against the word on the line.
		subject := "it has"
		if real != abs {
			subject = fmt.Sprintf("it resolves to %s, which has", real)
		}
		return fmt.Errorf("cannot mount %s as this run's project: %s no name to "+
			"mount it under; name a project directory rather than a filesystem root",
			abs, subject)
	}
	// Resolved to judge it, and the path as typed is what gets mounted. A link
	// is the name its owner uses for the project, so /work/<link> is the path
	// the agent should see and the host path it came from is the one to record
	// and print -- resolving those too would rewrite `/tmp/x` to `/private/tmp/x`
	// on every macOS host, for a directory the VMM resolves at mount time
	// anyway.
	c.Project = abs
	c.GuestProject = GuestProject(abs)
	// The point of naming a directory: the agent starts in it. The cwd-under-
	// home derivation above still decides this for a run that names none.
	c.GuestCwd = c.GuestProject
	return nil
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
