package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/brig-sh/brig/internal/buildinfo"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/redact"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/session"
)

// The collectors behind `brig doctor bundle`. Each reads one source, registers
// identifying values with the redactor, and returns raw text; bundle.go runs
// and scrubs them.
//
// None reads a credential value. The agent section uses the profile's declared
// names, not `brig info`: BuildEnv opens the secret store and resolves values.

// bundleHostname is a seam: a test cannot change the machine's hostname.
var bundleHostname = os.Hostname

// hostFacts is what the redactor replaces before collection. $USER wins, so a
// test can set it; the account name is the fallback.
func hostFacts() redact.Host {
	h := redact.Host{User: os.Getenv("USER")}
	h.Home, _ = os.UserHomeDir()
	if h.User == "" {
		if u, err := user.Current(); err == nil {
			h.User = u.Username
		}
	}
	h.Hostname, _ = bundleHostname()
	return h
}

// environmentJSON is the bug report template's Environment section.
type environmentJSON struct {
	OS             string `json:"os"`
	OSVersion      string `json:"osVersion,omitempty"`
	Kernel         string `json:"kernel,omitempty"`
	WSL            bool   `json:"wsl"`
	Arch           string `json:"arch"`
	Brig           string `json:"brig"`
	Commit         string `json:"commit,omitempty"`
	Modified       bool   `json:"modified,omitempty"`
	Go             string `json:"go"`
	Installed      string `json:"installed"`
	Runtime        string `json:"runtime,omitempty"`
	RuntimeBin     string `json:"runtimeBin,omitempty"`
	RuntimeVersion string `json:"runtimeVersion,omitempty"`
	RuntimeError   string `json:"runtimeError,omitempty"`
}

// commandTimeout bounds each external command a collector runs itself.
const commandTimeout = 5 * time.Second

func environmentCollector(r *redact.Redactor) ([]rawEntry, error) {
	info := buildinfo.Read()
	env := environmentJSON{
		OS: goruntime.GOOS, Arch: goruntime.GOARCH,
		Brig: info.Version, Commit: info.Commit, Modified: info.Modified, Go: info.GoVersion,
	}
	switch goruntime.GOOS {
	case "darwin":
		env.OSVersion = commandLine("sw_vers", "-productVersion")
	case "linux":
		if b, err := os.ReadFile("/etc/os-release"); err == nil {
			env.OSVersion = osReleasePretty(string(b))
		}
		if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
			env.Kernel = strings.TrimSpace(string(b))
			env.WSL = isWSL(env.Kernel)
		}
	}
	exe, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	env.Installed = installMethod(exe, info)
	if rt, err := detectRuntime(); err != nil {
		env.RuntimeError = oneLine(err.Error())
	} else {
		env.Runtime, env.RuntimeBin = rt.Kind(), r.Path(rt.Bin())
		env.RuntimeVersion = commandLine(rt.Bin(), "--version")
	}
	body, err := bundleJSON(env)
	return []rawEntry{{"environment.json", body}}, err
}

// commandLine is the first line a command prints, or "" when it fails.
func commandLine(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line
}

// isWSL reports a Windows Subsystem for Linux kernel. Both WSL1 and WSL2 put
// "microsoft" in the release string.
func isWSL(kernel string) bool {
	return strings.Contains(strings.ToLower(kernel), "microsoft")
}

// osReleasePretty is PRETTY_NAME from /etc/os-release.
func osReleasePretty(content string) string {
	s := bufio.NewScanner(strings.NewReader(content))
	for s.Scan() {
		if v, ok := strings.CutPrefix(s.Text(), "PRETTY_NAME="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

var pseudoVersion = regexp.MustCompile(`[0-9]{14}-[0-9a-f]{12}`)

// installMethod guesses the install method from the binary's path and build
// info, labelled "(inferred)": install.sh and a copied release binary look the
// same.
func installMethod(exe string, info buildinfo.Info) string {
	switch {
	case strings.Contains(exe, "/Caskroom/") || strings.Contains(exe, "/Cellar/"):
		return "homebrew (inferred)"
	case info.Modified || info.Version == "" || info.Version == "(devel)" || pseudoVersion.MatchString(info.Version):
		return "source (inferred)"
	default:
		return "install.sh (inferred)"
	}
}

func brigdCollector(*redact.Redactor) ([]rawEntry, error) {
	return nil, skipped("brigd writes no log yet")
}

// doctorCollector is `brig doctor`'s report, whole.
//
// The report is not redacted field by field, so its values are registered
// here: imageCheck's findings embed agent.Image and agent.Name, and the
// profiles collector may have timed out. Each check's paths are registered
// after it runs; a directory outside $HOME matches no other rule. Every file
// name in the profile and policy directories is registered first, since a
// file that failed to load is not a profile and a load error quotes its name.
func doctorCollector(agent *profile.Profile, loadErr error) func(*redact.Redactor) ([]rawEntry, error) {
	return func(r *redact.Redactor) ([]rawEntry, error) {
		if agent != nil {
			registerImage(r, agent.Image)
			r.Name(redact.KindProfile, agent.Name)
		}
		registerDirNames(r, profile.Dir(), redact.KindProfile, profile.Embedded)
		registerDirNames(r, policy.Dir(), redact.KindPolicy, func(string) bool { return false })
		for _, p := range profile.All() {
			if profile.IsCustom(p.Name) {
				r.Name(redact.KindProfile, p.Name)
			}
		}
		checks := runDoctor(agent, loadErr)
		for i, c := range checks {
			for _, p := range c.paths {
				r.Path(p)
			}
			if c.Name == "profiles" && loadErr != nil {
				checks[i] = bundleProfilesCheck(c, loadErr)
			}
		}
		body, err := bundleJSON(checks)
		return []rawEntry{{"doctor.json", body}}, err
	}
}

// profileProblemCount matches profile.Load's own counts, the way
// policyProblemCount matches LoadAll's.
var profileProblemCount = regexp.MustCompile(`(\d+) (?:unusable profile|duplicate profile name)`)

// bundleProfilesCheck is the profiles check with profile.Load's error reduced
// to a count. The error quotes file content, such as a YAML line, and a
// profile can hold a literal env value. Plain `brig doctor` prints it whole.
func bundleProfilesCheck(c check, loadErr error) check {
	names := profile.Names()
	custom := 0
	for _, n := range names {
		if profile.IsCustom(n) {
			custom++
		}
	}
	c.Finding = fmt.Sprintf("%d built in, %d in %s; %d profile file(s) could not be read; `brig doctor` names them",
		len(names)-custom, custom, profile.Dir(), problemCount(profileProblemCount, loadErr))
	c.Fix = "run `brig doctor` on this host to see which file(s), then fix or remove them"
	return c
}

// problemCount sums the counts re finds in a loader's error; 1 when it finds
// none, since an error still means a problem.
func problemCount(re *regexp.Regexp, err error) int {
	n := 0
	for _, m := range re.FindAllStringSubmatch(err.Error(), -1) {
		if v, convErr := strconv.Atoi(m[1]); convErr == nil {
			n += v
		}
	}
	return max(n, 1)
}

// configFileExts are the extensions the profile and policy loaders read.
var configFileExts = map[string]bool{".yaml": true, ".yml": true, ".json": true}

// reservedConfigFiles are brig's settings files, skipped in both directories.
var reservedConfigFiles = map[string]bool{"config.yaml": true, "config.yml": true, "config.json": true}

// registerDirNames registers the name of every profile or policy file in dir,
// with and without its extension. It reads directory entries only, never file
// contents. Names of shipped profiles are skipped.
func registerDirNames(r *redact.Redactor, dir string, kind redact.Kind, shipped func(string) bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		ext := filepath.Ext(name)
		if e.IsDir() || !configFileExts[ext] || reservedConfigFiles[name] {
			continue
		}
		stem := strings.TrimSuffix(name, ext)
		if shipped(stem) {
			continue
		}
		r.Name(kind, name)
		r.Name(kind, stem)
	}
}

// bundleSandbox is one `brig ls` row, redacted, with the last boot.
type bundleSandbox struct {
	Ref        string     `json:"ref,omitempty"`
	Sandbox    string     `json:"sandbox"`
	State      string     `json:"state"`
	Workspace  string     `json:"workspace,omitempty"`
	LastBoot   string     `json:"lastBoot,omitempty"`
	LastBootAt *time.Time `json:"lastBootAt,omitempty"`
	// Image and Digest are what the last boot ran.
	Image  string `json:"image,omitempty"`
	Digest string `json:"digest,omitempty"`
}

func sandboxesCollector(r *redact.Redactor) ([]rawEntry, error) {
	rt, err := detectRuntime()
	if err != nil {
		return nil, skipped("no runtime: " + oneLine(err.Error()))
	}
	list, err := rt.List()
	if err != nil {
		return nil, err
	}
	out := []bundleSandbox{}
	for _, row := range sandboxRows(list, rt) {
		s := bundleSandbox{
			Ref:     redactRef(r, row.ref),
			Sandbox: redactSandbox(r, row.name),
			State:   row.state,
		}
		if filepath.IsAbs(row.workspace) {
			s.Workspace = r.Workspace(row.workspace)
		}
		if rec, ok, err := runtime.LastBoot(row.name); err == nil && ok {
			s.LastBoot = rec.Result
			s.Image = registerImage(r, rec.Image)
			s.Digest = rec.Digest
			at := rec.At
			s.LastBootAt = &at
		}
		out = append(out, s)
	}
	body, err := bundleJSON(out)
	return []rawEntry{{"sandboxes.json", body}}, err
}

// redactRef keeps a built-in agent's name and replaces the label.
func redactRef(r *redact.Redactor, ref string) string {
	if ref == "" {
		return ""
	}
	parsed, err := session.ParseRef(ref)
	if err != nil {
		return r.Name(redact.KindLabel, ref)
	}
	out := r.Name(redact.KindProfile, parsed.Agent)
	if parsed.Label != "" {
		out += "@" + r.Name(redact.KindLabel, parsed.Label)
	}
	return out
}

// redactSandbox keeps a built-in's default sandbox name, brig-<profile>, and
// replaces any other.
func redactSandbox(r *redact.Redactor, name string) string {
	if agent, ok := strings.CutPrefix(name, "brig-"); ok && profile.Embedded(agent) {
		return name
	}
	return r.Name(redact.KindSandbox, name)
}

// allowlistDomains are agent and forge hosts, kept readable. A leading dot
// covers the domain and its subdomains.
var allowlistDomains = []string{
	".anthropic.com", ".claude.ai", ".openai.com", ".github.com",
	".githubusercontent.com", ".ghcr.io", ".docker.io", ".brig.sh",
}

// allowlistFiles are names inside brig's own directories.
var allowlistFiles = []string{
	"sessions.json", "networks.json", "boots.json", "published.json", "gateway-ips.json",
	"homes", "assets", "policies", "templates", "brigd.sock", "gateway.log", "config.yaml",
}

// gatewayRange is the guest range of brig's gateways, the 198.18.0.0/15
// benchmarking range.
var gatewayRange = netip.MustParsePrefix("198.18.0.0/15")

// allowlist is what stays readable. It is built from the shipped specs, not
// the registry: an override of a built-in is the user's file.
func allowlist() redact.Allow {
	a := redact.Allow{
		Domains:  allowlistDomains,
		Files:    allowlistFiles,
		Prefixes: []netip.Prefix{gatewayRange},
	}
	for _, name := range profile.BuiltInNames() {
		p, ok := profile.BuiltIn(name)
		if !ok {
			continue
		}
		a.Names = append(a.Names, name)
		a.Names = append(a.Names, profile.Aliases(name)...)
		if p.Image != "" {
			a.Images = append(a.Images, imageRepo(p.Image))
		}
		// Forward is not read: Parse folds it into Env as `ref: env.<name>`
		// bindings and zeroes it, and BuiltIn goes through Parse.
		a.Vars = append(a.Vars, p.Deny...)
		for _, s := range p.Secrets {
			a.Vars = append(a.Vars, s.Name)
		}
		for _, e := range p.Env {
			a.Vars = append(a.Vars, e.Name)
		}
	}
	return a
}

// imageRepo is an image reference without its tag or digest.
func imageRepo(ref string) string {
	name, _, _ := strings.Cut(ref, "@")
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name = name[:i]
	}
	return name
}

// registerImage is r.Image, plus the registry host alone when the reference
// names a port: a pull error names the host without the port. Only a host
// with a dot, or an address, is registered; a dotless one such as localhost
// would replace that word everywhere.
func registerImage(r *redact.Redactor, ref string) string {
	out := r.Image(ref)
	if first, _, ok := strings.Cut(ref, "/"); ok {
		if host, port, err := net.SplitHostPort(first); err == nil && port != "" && isHostName(host) {
			r.Domain(host)
		}
	}
	return out
}

// isHostName reports a host with a dot, or an address.
func isHostName(h string) bool {
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	return strings.Contains(h, ".")
}

// pathSettings are the variables whose value is a host path. Their errors
// quote the value into free text, and a path outside $HOME matches no other
// rule.
var pathSettings = []string{
	"BRIG_RUNTIME_BIN", "BRIG_COSIGN_BIN", "BRIG_BOOT_ASSETS", "BRIG_PROFILE_DIR",
	"BRIG_POLICY_DIR", "BRIG_STATE_DIR", "BRIG_GATEWAY_DIR",
}

// registerSettingPaths registers every absolute path a setting names, and the
// named agent's runtimeBin, before any collector runs. A bare name such as
// hull is a PATH lookup and is skipped, as is a path one segment deep or less
// (/usr): registering it would rewrite every install path under it.
func registerSettingPaths(r *redact.Redactor, agent *profile.Profile) {
	for _, name := range pathSettings {
		if v := os.Getenv(name); deepPath(v) {
			r.Path(v)
		}
	}
	if agent == nil || agent.RuntimeBin == "" {
		return
	}
	// Expanded as runtime.DetectFor expands it.
	bin := agent.RuntimeBin
	if rest, ok := strings.CutPrefix(bin, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			bin = filepath.Join(home, rest)
		}
	}
	if deepPath(bin) {
		r.Path(bin)
	}
}

// deepPath is an absolute path at least two segments deep.
func deepPath(p string) bool {
	return filepath.IsAbs(p) && strings.Contains(strings.Trim(filepath.Clean(p), "/"), "/")
}

// bundleProfile is the profile fields the bundle carries, copied one by one.
// A field added to profile.Profile stays out until it is added here.
type bundleProfile struct {
	Name             string   `json:"name"`
	OverridesBuiltIn bool     `json:"overridesBuiltIn,omitempty"`
	Kind             string   `json:"kind,omitempty"`
	Image            string   `json:"image"`
	Secrets          []string `json:"secrets,omitempty"`
	Env              []string `json:"env,omitempty"`
	Deny             []string `json:"deny,omitempty"`
	Volumes          int      `json:"volumes,omitempty"`
	Files            int      `json:"files,omitempty"`
	Network          string   `json:"network,omitempty"`
	// Policies are the placeholder names of profile.Profile.Policy, never
	// the documents.
	Policies    []string `json:"policies,omitempty"`
	Hypervisor  string   `json:"hypervisor,omitempty"`
	RuntimeBin  string   `json:"runtimeBin,omitempty"`
	RootfsType  string   `json:"rootfsType,omitempty"`
	GenericBoot bool     `json:"genericBoot,omitempty"`
	GUI         bool     `json:"gui,omitempty"`
	Headless    bool     `json:"headless,omitempty"`
	Mem         int      `json:"mem"`
	CPUs        int      `json:"cpus"`
}

// postures are brig's own network values.
var postures = map[string]bool{"": true, "shared": true, "isolated": true, "offline": true, "none": true}

func projectProfile(r *redact.Redactor, p profile.Profile) bundleProfile {
	bp := bundleProfile{
		Name:             r.Name(redact.KindProfile, p.Name),
		OverridesBuiltIn: profile.OverridesBuiltIn(p.Name),
		Kind:             string(p.Kind),
		Image:            registerImage(r, p.Image),
		Volumes:          len(p.Volumes),
		Files:            len(p.Files),
		Network:          p.Network,
		Hypervisor:       p.Hypervisor,
		RuntimeBin:       r.Path(p.RuntimeBin),
		RootfsType:       p.RootfsType,
		GenericBoot:      p.GenericBoot,
		GUI:              p.GUI,
		Headless:         p.Headless,
		Mem:              p.Mem,
		CPUs:             p.CPUs,
	}
	if !postures[p.Network] {
		bp.Network = r.Name(redact.KindPolicy, p.Network)
	}
	for _, name := range p.Policy {
		bp.Policies = append(bp.Policies, r.Name(redact.KindPolicy, name))
	}
	// Forward is not read: Parse folds it into Env and zeroes it.
	for _, s := range p.Secrets {
		bp.Secrets = append(bp.Secrets, r.Var(s.Name))
	}
	for _, e := range p.Env {
		bp.Env = append(bp.Env, r.Var(e.Name))
	}
	for _, d := range p.Deny {
		bp.Deny = append(bp.Deny, r.Var(d))
	}
	return bp
}

// bundlePolicy is an egress policy with hosts and ranges redacted.
type bundlePolicy struct {
	Name    string   `json:"name"`
	Default string   `json:"default"`
	Allow   []string `json:"allow,omitempty"`
	Deny    []string `json:"deny,omitempty"`
}

// projectPolicies projects every policy LoadAll could read, even when it also
// returns an error.
func projectPolicies(r *redact.Redactor) ([]bundlePolicy, error) {
	all, loadErr := policy.LoadAll(policy.Dir())
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)
	out := []bundlePolicy{}
	for _, n := range names {
		e := all[n].Policy.Egress
		bp := bundlePolicy{Name: r.Name(redact.KindPolicy, n), Default: e.Default}
		for _, rule := range e.Allow {
			bp.Allow = append(bp.Allow, redactRule(r, rule))
		}
		for _, rule := range e.Deny {
			bp.Deny = append(bp.Deny, redactRule(r, rule))
		}
		out = append(out, bp)
	}
	if loadErr != nil {
		return out, errors.New(policyLoadProblem(loadErr))
	}
	return out, nil
}

// policyProblemCount matches only LoadAll's counts. The rest of its error
// quotes basenames verbatim, which the redactor does not have.
var policyProblemCount = regexp.MustCompile(`(\d+) (?:unusable policy file|duplicate policy name)`)

// policyLoadProblem reduces LoadAll's error to a count. `brig policy ls`
// names the files.
func policyLoadProblem(err error) string {
	return strconv.Itoa(problemCount(policyProblemCount, err)) + " policy file(s) could not be read; `brig policy ls` names them"
}

// redactRule redacts one egress rule. r.IP takes a CIDR whole and fails
// closed on anything that does not parse.
func redactRule(r *redact.Redactor, rule policy.Rule) string {
	switch {
	case rule.Host != "":
		// Free text names the domain under a glob, not the glob, so the
		// bare domain is registered too. The scrub matches whole values,
		// so a subdomain is replaced only if registered itself.
		if base, ok := strings.CutPrefix(rule.Host, "*."); ok {
			r.Domain(base)
		}
		return r.Domain(rule.Host)
	case rule.CIDR != "":
		return r.IP(rule.CIDR)
	}
	return ""
}

// profilesCollector carries the user's profiles, overrides of built-ins
// included, and the policies. Unmodified built-ins are left out.
func profilesCollector(r *redact.Redactor) ([]rawEntry, error) {
	profiles := []bundleProfile{}
	for _, p := range profile.All() {
		if profile.IsCustom(p.Name) {
			profiles = append(profiles, projectProfile(r, p))
		}
	}
	body, err := bundleJSON(profiles)
	if err != nil {
		return nil, err
	}
	entries := []rawEntry{{"profiles/profiles.json", body}}
	// Written even when projectPolicies errors: it returns what it loaded.
	policies, perr := projectPolicies(r)
	pbody, jerr := bundleJSON(policies)
	if jerr != nil {
		return entries, jerr
	}
	entries = append(entries, rawEntry{"profiles/policies.json", pbody})
	return entries, perr
}

// agentCollector is the named agent's profile, built-in or not.
func agentCollector(agent profile.Profile) func(*redact.Redactor) ([]rawEntry, error) {
	return func(r *redact.Redactor) ([]rawEntry, error) {
		// The local store's digest; hull answers "". The registry digest is
		// in doctor.json, the booted one in sandboxes.json.
		out := struct {
			bundleProfile
			LocalDigest string `json:"localDigest,omitempty"`
		}{bundleProfile: projectProfile(r, agent)}
		if rt, err := detectRuntime(); err == nil {
			if d, err := rt.LocalDigest(agent.Image); err == nil {
				out.LocalDigest = d
			}
		}
		body, err := bundleJSON(out)
		name := "agent/" + r.Name(redact.KindProfile, agent.Name) + "/profile.json"
		return []rawEntry{{name, body}}, err
	}
}

// bundleLogTail caps each log at its last lines.
var bundleLogTail = 2000

// logsTimeout exceeds the default: it reads one log per sandbox.
const logsTimeout = 30 * time.Second

// bundleLogsTimeout is a test seam for logsTimeout.
var bundleLogsTimeout = logsTimeout

// logsCollector reads the console log of each sandbox whose last boot failed.
// With --include-logs it reads every sandbox's log and the gateway logs.
//
// A failed boot's log is boot output only; the agent never started. A booted
// sandbox's log holds agent output, and the gateway logs record where the
// agent connected, so both need the flag.
func logsCollector(o bundleOptions) func(*redact.Redactor) ([]rawEntry, error) {
	return func(r *redact.Redactor) ([]rawEntry, error) {
		rt, err := detectRuntime()
		if err != nil {
			return nil, skipped("no runtime: " + oneLine(err.Error()))
		}
		list, err := rt.List()
		if err != nil {
			return nil, err
		}
		var entries []rawEntry
		for _, inst := range list {
			if !strings.HasPrefix(inst.Name, sandboxPrefix) {
				continue
			}
			rec, ok, _ := runtime.LastBoot(inst.Name)
			if !o.includeLogs && !(ok && rec.Result == runtime.BootFailed) {
				continue
			}
			// Registered here too, as the sandboxes collector may have timed
			// out: a boot log names the workspace, label and image.
			if ws := workspaceOf(inst.Name, rt); filepath.IsAbs(ws) {
				r.Workspace(ws)
			}
			redactRef(r, refOf(inst.Name))
			if ok {
				registerImage(r, rec.Image)
			}
			name := redactSandbox(r, inst.Name)
			var out, stderr bytes.Buffer
			err := rt.Logs(runtime.LogsSpec{
				Name: inst.Name, Tail: bundleLogTail,
				Out: filtered(&out, false), Err: filtered(&stderr, false),
			})
			body := out.String()
			if stderr.Len() > 0 {
				body += "\n--- runtime stderr ---\n" + stderr.String()
			}
			if err != nil {
				body += "\n--- brig: reading this log failed: " + oneLine(err.Error()) + "\n"
			}
			entries = append(entries, rawEntry{"logs/" + name + ".log", body})
			if o.includeLogs {
				if path, err := runtime.IsolatedGatewayLogPath(inst.Name); err == nil {
					if b, err := os.ReadFile(path); err == nil {
						entries = append(entries, rawEntry{"logs/" + name + ".gateway.log", string(lastLines(b, bundleLogTail))})
					}
				}
			}
		}
		if o.includeLogs {
			if path, err := runtime.GatewayLogPath(); err == nil {
				if b, err := os.ReadFile(path); err == nil {
					entries = append(entries, rawEntry{"logs/gateway.log", string(lastLines(b, bundleLogTail))})
				}
			}
		}
		return entries, nil
	}
}

// defaultBundleCollectors is the collector list, in manifest order.
func defaultBundleCollectors(o bundleOptions, agent *profile.Profile, loadErr error) []collector {
	cs := []collector{
		{name: "profiles", run: profilesCollector},
		{name: "environment", run: environmentCollector},
		{name: "doctor", run: doctorCollector(agent, loadErr)},
		{name: "sandboxes", run: sandboxesCollector},
	}
	if agent != nil {
		cs = append(cs, collector{name: "agent", run: agentCollector(*agent)})
	}
	cs = append(cs, collector{name: "logs", timeout: bundleLogsTimeout, run: logsCollector(o)})
	return append(cs, collector{name: "brigd", run: brigdCollector})
}
