package wrap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/profile"
)

// A Brigfile is the repository's, and the agent in a sandbox can edit the
// repository: the Brigfile, the policies it names and the profile it runs.
// So reading a Brigfile authorizes nothing. A run of a target boots only
// what you approved, outside the repository, and the approval is of an
// ApprovalSubject: everything that decides what the run may do, and nothing
// that changes without changing what it may do.
//
// The subject holds names, paths, hashes and settings. It holds no secret
// value, no token and nothing read from the keychain, because it is stored in
// approvals.json and printed when it changes.

// approvalVersion changes when what the subject covers changes. A stored
// approval of another version is a subject of another shape, so it reads as
// changed and is asked for again.
const approvalVersion = 1

// ApprovalSubject is what an approval of a target's run covers.
type ApprovalSubject struct {
	Version int `json:"version"`
	// Root, Target, Session and Sandbox name the session: the checkout, the
	// target and the session name, and the sandbox they make.
	Root    string `json:"root"`
	Target  string `json:"target"`
	Session string `json:"session,omitempty"`
	Sandbox string `json:"sandbox"`
	// Brigfile is the file and the sha256 of its bytes. Documents are the
	// files and inline documents the target reads through it: its profile
	// and its policies from the Brigfile.
	Brigfile  ApprovalDocument   `json:"brigfile"`
	Documents []ApprovalDocument `json:"documents"`
	// Profile is the profile the run boots, after the target's overrides and
	// the settings, hashed as ConfigDigest hashes it.
	Profile ApprovalProfile `json:"profile"`
	// Policies is every policy the run is bound to, with where it came from.
	Policies []ApprovalPolicy `json:"policies"`
	// Egress is the merged rule set of those policies.
	Egress ApprovalEgress `json:"egress"`
	// Routes is every route, with where it sends requests and which of your
	// secrets it reads.
	Routes []PlanRoute `json:"routes"`
	// KeptOnHost are the secrets the routes keep on the host.
	KeptOnHost []string      `json:"keptOnHost"`
	Image      ApprovalImage `json:"image"`
	Limits     PlanLimits    `json:"limits"`
	Network    ApprovalNet   `json:"network"`
	// Mounts are the host directories the guest reaches: the guest home, the
	// project, and what --skills copies in.
	Mounts []PlanMount `json:"mounts"`
	// Settings are the BRIG_* settings that took effect, by the name that
	// won and its value. See approvalSettings.
	Settings []ApprovalSetting `json:"settings"`
	// Credentials are the bindings that deliver a credential to the guest:
	// the guest's name for it, how, and from which secret or variable.
	Credentials []ApprovalCredential `json:"credentials"`
	// Forwarded are the host variables the run reads into the guest, by name.
	Forwarded []string `json:"forwarded"`
	// Ports are the guest ports published on the host.
	Ports   []string        `json:"ports"`
	Runtime ApprovalRuntime `json:"runtime"`
	// Binary is what the run starts in the guest, and Args its arguments.
	Binary string   `json:"binary"`
	Args   []string `json:"args"`
}

// ApprovalDocument is one document a target reads, by its sha256.
type ApprovalDocument struct {
	// Kind is "brigfile", "profile" or "policy".
	Kind   string `json:"kind"`
	Alias  string `json:"alias,omitempty"`
	Source string `json:"source"`
	SHA256 string `json:"sha256"`
}

// ApprovalProfile is the profile a run boots.
type ApprovalProfile struct {
	Alias  string `json:"alias"`
	Source string `json:"source"`
	SHA256 string `json:"sha256"`
}

// ApprovalPolicy is one policy of the run, with where it came from and the
// sha256 of the document as brig read it.
type ApprovalPolicy struct {
	Name   string `json:"name"`
	From   string `json:"from"`
	SHA256 string `json:"sha256"`
}

// ApprovalEgress is the merged egress of the run's policies.
type ApprovalEgress struct {
	Default string        `json:"default"`
	Allow   []policy.Rule `json:"allow"`
	Deny    []policy.Rule `json:"deny"`
}

// ApprovalImage is the image a run boots. Pinned says the reference names a
// digest. A tag can move after the approval: the digest a boot resolves is
// known only once the boot verifies it, after the approval is given.
type ApprovalImage struct {
	Ref    string `json:"ref"`
	Pinned bool   `json:"pinned"`
	Pull   string `json:"pull"`
}

// ApprovalNet is the network of a run: the posture a flag or a setting asked
// for and which one, and the posture the next boot gets. Where the posture
// came from when nothing asked is left out: on a second run it is the
// sandbox's own record, and that is state, not a request.
type ApprovalNet struct {
	Asked     string `json:"asked,omitempty"`
	AskedBy   string `json:"askedBy,omitempty"`
	Effective string `json:"effective"`
}

// ApprovalSetting is one BRIG_* setting that took effect.
type ApprovalSetting struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ApprovalCredential is one binding that delivers a credential: the guest's
// name for it or the file it lands in, how it is delivered, and the secret or
// variable it is read from. Never a value.
type ApprovalCredential struct {
	Name     string `json:"name"`
	Delivery string `json:"delivery"`
	From     string `json:"from"`
	Required bool   `json:"required,omitempty"`
}

// ApprovalRuntime is the runtime and the backend a run boots on.
type ApprovalRuntime struct {
	Kind    string `json:"kind"`
	Backend string `json:"backend,omitempty"`
}

// ApprovalSource is one document of the Brigfile as the caller read it, for
// ApprovalSubject: wrap does not read a Brigfile.
type ApprovalSource struct {
	Kind   string
	Alias  string
	Source string
	Bytes  []byte
}

// ApprovalInput is what the subject needs that the Config does not hold:
// the Brigfile and the documents of the target, and the arguments the agent
// gets.
type ApprovalInput struct {
	Brigfile  []byte
	Documents []ApprovalSource
	Args      []string
}

// ApprovalSubject is the subject of this run of a target, or false when the
// run is not of a target.
func (c *Config) ApprovalSubject(in ApprovalInput) (ApprovalSubject, bool) {
	if c.identity == nil || c.target == nil {
		return ApprovalSubject{}, false
	}
	s := ApprovalSubject{
		Version: approvalVersion,
		Root:    c.target.Root,
		Target:  c.target.Name,
		Session: c.target.Session,
		Sandbox: c.VMName,
		Brigfile: ApprovalDocument{Kind: "brigfile", Source: c.target.Brigfile,
			SHA256: sha256Hex(in.Brigfile)},
		Documents: []ApprovalDocument{},
		Profile: ApprovalProfile{Alias: c.target.ProfileAlias, Source: c.target.ProfileSource,
			SHA256: jsonSHA256(digestProfile(c.Profile))},
		Policies:    []ApprovalPolicy{},
		Routes:      c.planRoutes(),
		KeptOnHost:  sortedCopy(c.routeSkipped),
		Image:       ApprovalImage{Ref: c.Image, Pinned: strings.Contains(c.Image, "@sha256:"), Pull: c.Pull},
		Limits:      PlanLimits{Mem: c.Mem, CPUs: c.CPUs},
		Network:     ApprovalNet{Effective: string(c.Network)},
		Settings:    c.approvalSettings(),
		Credentials: c.approvalCredentials(),
		Forwarded:   c.forwardedNames(),
		Ports:       []string{},
		Runtime:     ApprovalRuntime{Backend: c.hypervisor()},
		Binary:      c.Profile.Binary,
		Args:        append([]string{}, in.Args...),
	}
	for _, d := range in.Documents {
		s.Documents = append(s.Documents, ApprovalDocument{Kind: d.Kind, Alias: d.Alias, Source: d.Source,
			SHA256: sha256Hex(d.Bytes)})
	}
	sort.Slice(s.Documents, func(i, j int) bool {
		a, b := s.Documents[i], s.Documents[j]
		return a.Kind+"\x00"+a.Alias+"\x00"+a.Source < b.Kind+"\x00"+b.Alias+"\x00"+b.Source
	})
	for i, p := range c.policyDocs {
		from := ""
		if i < len(c.policyOrigins) {
			from = c.policyOrigins[i]
		}
		s.Policies = append(s.Policies, ApprovalPolicy{Name: p.Metadata.Name, From: from, SHA256: jsonSHA256(p)})
	}
	sort.Slice(s.Policies, func(i, j int) bool {
		a, b := s.Policies[i], s.Policies[j]
		return a.Name+"\x00"+a.From+"\x00"+a.SHA256 < b.Name+"\x00"+b.From+"\x00"+b.SHA256
	})
	s.Egress = ApprovalEgress{Default: string(c.Egress.Default), Allow: sortedRules(c.Egress.Allow),
		Deny: sortedRules(c.Egress.Deny)}
	if c.netExplicit {
		s.Network.Asked, s.Network.AskedBy = string(c.askedNetwork), c.networkSource
	}
	s.Mounts = append(s.Mounts, PlanMount{Host: c.Workspace, Guest: c.Profile.GuestHome, Mode: "read-write"})
	if c.Project != "" {
		s.Mounts = append(s.Mounts, PlanMount{Host: c.ProjectReal, Guest: c.GuestProject, Mode: "read-write"})
	}
	s.Mounts = append(s.Mounts, c.planSkills()...)
	for _, p := range c.Publish {
		s.Ports = append(s.Ports, p.String())
	}
	sort.Strings(s.Ports)
	if c.Runtime != nil {
		s.Runtime.Kind = c.Runtime.Kind()
	}
	return s, true
}

// ApprovalDigest is sha256 over the compact JSON encoding of s. Every list in
// the subject is sorted, or kept in an order that is part of what is
// approved, such as the arguments, so one subject has one encoding.
func ApprovalDigest(s ApprovalSubject) string {
	return "sha256:" + jsonSHA256(s)
}

// digestProfile is the profile as a digest covers it: without the prose a
// reader sees and without the values the run resolves on its own, which the
// digest covers as resolved. ConfigDigest and the approval share it, so the
// two agree about which change to a profile counts.
func digestProfile(p profile.Profile) profile.Profile {
	// The repository's word for its profile, and the line `brig agent ls`
	// prints. Neither reaches the sandbox.
	p.Name, p.Desc = "", ""
	// Covered as resolved.
	p.Image, p.Mem, p.CPUs = "", 0, 0
	p.Secrets = append([]profile.SecretDecl(nil), p.Secrets...)
	for i := range p.Secrets {
		p.Secrets[i].Hint = ""
	}
	return p
}

// approvalSettings are the BRIG_* settings that took effect on this run, by
// the name that won: a built-in's own BRIG_<AGENT>_<KEY> beats BRIG_<KEY>.
func (c *Config) approvalSettings() []ApprovalSetting {
	out := []ApprovalSetting{}
	for _, key := range approvalAgentSettings {
		if name, value, ok := c.env.getNamed(key); ok {
			out = append(out, ApprovalSetting{Name: name, Value: value})
		}
	}
	for _, key := range approvalGlobalSettings {
		if value, ok := os.LookupEnv("BRIG_" + key); ok {
			out = append(out, ApprovalSetting{Name: "BRIG_" + key, Value: value})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// approvalAgentSettings are the settings a run reads through Env, which a
// built-in's own name can set too. Each one changes what the run may do:
// what it boots, what it checks, what reaches the guest, or what reaches the
// command line.
//
// approvalsettings_test.go reads every setting key in brig's source, and fails
// on one that is in neither this list, approvalGlobalSettings nor
// approvalExempt. A new setting is then a decision about the approval, not
// one more value an approval ignores.
var approvalAgentSettings = []string{
	"ALLOW_DENIED", "ALLOW_REFS", "COSIGN_BIN", "CPUS", "FORWARD_ENV", "GIT_CONFIG", "GIT_EMAIL",
	"GIT_HOSTS", "GIT_IDENTITY", "GIT_NAME", "GIT_USER", "HYPERVISOR", "IMAGE", "MEM", "NETWORK", "PULL",
	"ROOTFS_MODE", "ROOTFS_TYPE", "SKILLS", "TRUST_WORKSPACE", "VERIFY", "VERIFY_IDENTITY",
	"VERIFY_ISSUER", "VERIFY_REGISTRY", "VERIFY_RUNTIME_IDENTITY", "VERIFY_RUNTIME_ISSUER",
}

// approvalGlobalSettings are the settings read under BRIG_<KEY> alone, by the
// runtime and the broker. Each names a program brig runs, the kernel it
// boots, the shim that decides the boundary, or what reaches a command line.
var approvalGlobalSettings = []string{
	"BOOT_ASSETS", "BOOT_ASSETS_REF", "BROKER_BIN", "CONTAINERD_RUNTIME", "ENV_ARGV", "GATEWAY_SOCK",
	"RUNTIME", "RUNTIME_BIN",
}

// approvalExempt are the settings an approval does not cover, each with why.
var approvalExempt = map[string]string{
	"NAME":            "a target's sandbox name is its own; Load refuses to let BRIG_NAME take it",
	"WORKSPACE":       "a target's guest home is its own; Load does not read BRIG_WORKSPACE for one",
	"CREDENTIALS_CMD": "removed; Load refuses a run that sets it",
	"READY_TIMEOUT":   "how long to wait for the guest, which grants nothing",
	"TITLE":           "the title of a graphical agent's window",
	"STATE_DIR":       "where approvals.json is; an approval is read from the state directory it names",
	"PROFILE_DIR":     "a target reads its profile from the Brigfile or the built-ins, not your directory",
	"TEMPLATE_DIR":    "profile templates for brig agent new, which a run does not read",
	"POLICY_DIR":      "the policies read from it are in the subject by their content",
	"GATEWAY_DIR":     "where the gateway keeps its own state",
}

// approvalCredentials are the run's credential bindings as the profile
// declares them, after BRIG_FORWARD_ENV and the routes: never what the store
// or the environment holds now, so storing a secret does not change an
// approval.
func (c *Config) approvalCredentials() []ApprovalCredential {
	required := map[string]bool{}
	for _, d := range c.Profile.Secrets {
		required[d.Name] = d.IsRequired()
	}
	out := []ApprovalCredential{}
	for _, b := range c.Env {
		from := "value"
		refs := b.RefList()
		if len(refs) > 0 {
			from = strings.Join(refs, ", ")
		}
		req := false
		for _, r := range refs {
			if ref, err := profile.ParseRef(r); err == nil && ref.Namespace == profile.NamespaceSecrets {
				req = req || required[ref.Name]
			}
		}
		out = append(out, ApprovalCredential{Name: b.Name, Delivery: "env", From: from, Required: req})
	}
	for _, f := range c.Profile.Files {
		req := false
		if ref, err := profile.ParseRef(f.Ref); err == nil && ref.Namespace == profile.NamespaceSecrets {
			req = required[ref.Name]
		}
		out = append(out, ApprovalCredential{Name: f.Path, Delivery: "file", From: f.Ref, Required: req})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Delivery+"\x00"+out[i].Name < out[j].Delivery+"\x00"+out[j].Name
	})
	return out
}

// forwardedNames are the host variables the run reads into the guest.
func (c *Config) forwardedNames() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, b := range c.Env {
		for _, r := range b.RefList() {
			ref, err := profile.ParseRef(r)
			if err != nil || ref.Namespace != profile.NamespaceEnv || seen[ref.Name] {
				continue
			}
			seen[ref.Name] = true
			out = append(out, ref.Name)
		}
	}
	sort.Strings(out)
	return out
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// jsonSHA256 is the sha256 of v's compact JSON encoding. Every value hashed
// here marshals: plain strings, numbers, bools and lists of them.
func jsonSHA256(v any) string {
	blob, err := json.Marshal(v)
	if err != nil {
		// A digest that cannot be taken matches nothing, so nothing is
		// approved on it.
		return "unhashable: " + err.Error()
	}
	return sha256Hex(blob)
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

// ApprovalChanges names, in words, each part of the subject that differs
// between what was approved and what this run would get. Each entry is one
// line for a reader.
func ApprovalChanges(was, now ApprovalSubject) []string {
	var out []string
	add := func(format string, a ...any) { out = append(out, fmt.Sprintf(format, a...)) }
	if was.Version != now.Version {
		add("the approval was given by a release of brig that covered other things")
	}
	if was.Brigfile.SHA256 != now.Brigfile.SHA256 {
		add("the Brigfile %s", now.Brigfile.Source)
	}
	// A document by where it is, since the policy rows below name it by its
	// metadata.name: the file the repository can edit, or the inline
	// document in the Brigfile.
	docKey := func(d ApprovalDocument) string {
		if file, ok := strings.CutPrefix(d.Source, "file:"); ok {
			return "the file " + file + " (" + d.Kind + " " + d.Alias + ")"
		}
		return "the " + d.Kind + " " + d.Alias + " (" + d.Source + ")"
	}
	diffKeyed(was.Documents, now.Documents, docKey, func(d ApprovalDocument) string { return d.SHA256 },
		func(k string) { add("%s is new", k) }, func(k string) { add("%s is gone", k) },
		func(k string, _, _ ApprovalDocument) { add("%s changed", k) })
	if was.Profile != now.Profile {
		add("the profile %s (%s)", now.Profile.Alias, now.Profile.Source)
	}
	polKey := func(p ApprovalPolicy) string { return "policy " + p.Name + ", from " + p.From }
	diffKeyed(was.Policies, now.Policies, polKey, func(p ApprovalPolicy) string { return p.SHA256 },
		func(k string) { add("%s is new", k) }, func(k string) { add("%s is gone", k) },
		func(k string, _, _ ApprovalPolicy) { add("%s changed", k) })
	if jsonSHA256(was.Egress) != jsonSHA256(now.Egress) {
		add("the egress rules")
	}
	routeKey := func(r PlanRoute) string { return "route " + r.Name }
	diffKeyed(was.Routes, now.Routes, routeKey, func(r PlanRoute) string { return jsonSHA256(r) },
		func(k string) { add("%s is new", k) }, func(k string) { add("%s is gone", k) },
		func(k string, a, b PlanRoute) {
			if a.Upstream != b.Upstream || !slices.Equal(a.Services, b.Services) {
				add("%s sends to %s, and sent to %s", k, routeDest(b), routeDest(a))
			}
			if !slices.Equal(a.Secrets, b.Secrets) {
				add("%s reads your secret %s, and read %s", k, listOr(b.Secrets), listOr(a.Secrets))
			}
			if a.Policy != b.Policy || a.Preset != b.Preset || a.Mode != b.Mode || a.Vends != b.Vends {
				add("%s changed its policy, preset or mode", k)
			}
		})
	if !slices.Equal(was.KeptOnHost, now.KeptOnHost) {
		add("the secrets kept on the host: %s, and were %s", listOr(now.KeptOnHost), listOr(was.KeptOnHost))
	}
	if was.Image != now.Image {
		add("the image %s, and was %s", imageWords(now.Image), imageWords(was.Image))
	}
	if was.Limits.Mem != now.Limits.Mem {
		add("the memory %d MB, and was %d MB", now.Limits.Mem, was.Limits.Mem)
	}
	if was.Limits.CPUs != now.Limits.CPUs {
		add("the vCPUs %d, and were %d", now.Limits.CPUs, was.Limits.CPUs)
	}
	if was.Network.Asked != now.Network.Asked || was.Network.AskedBy != now.Network.AskedBy {
		add("the network asked for: %s, and was %s", netAsked(now.Network), netAsked(was.Network))
	}
	if was.Network.Effective != now.Network.Effective {
		add("the network %s, and was %s", now.Network.Effective, was.Network.Effective)
	}
	if jsonSHA256(was.Mounts) != jsonSHA256(now.Mounts) {
		add("the directories the guest reaches")
	}
	setKey := func(s ApprovalSetting) string { return s.Name }
	diffKeyed(was.Settings, now.Settings, setKey, func(s ApprovalSetting) string { return s.Value },
		func(k string) { add("%s is set", k) }, func(k string) { add("%s is no longer set", k) },
		func(k string, a, b ApprovalSetting) { add("%s is %q, and was %q", k, b.Value, a.Value) })
	credKey := func(c ApprovalCredential) string { return c.Delivery + " " + c.Name }
	diffKeyed(was.Credentials, now.Credentials, credKey, func(c ApprovalCredential) string { return jsonSHA256(c) },
		func(k string) { add("the credential %s is new", k) }, func(k string) { add("the credential %s is gone", k) },
		func(k string, _, b ApprovalCredential) { add("the credential %s comes from %s", k, b.From) })
	if !slices.Equal(was.Forwarded, now.Forwarded) {
		add("the host variables read into the guest: %s, and were %s", listOr(now.Forwarded), listOr(was.Forwarded))
	}
	if !slices.Equal(was.Ports, now.Ports) {
		add("the published ports: %s, and were %s", listOr(now.Ports), listOr(was.Ports))
	}
	if was.Runtime != now.Runtime {
		add("the runtime %s, and was %s", runtimeWords(now.Runtime), runtimeWords(was.Runtime))
	}
	if was.Binary != now.Binary {
		add("the program the guest starts: %s, and was %s", now.Binary, was.Binary)
	}
	if !slices.Equal(was.Args, now.Args) {
		add("the arguments: %s, and were %s", argsWords(now.Args), argsWords(was.Args))
	}
	if len(out) == 0 && ApprovalDigest(was) != ApprovalDigest(now) {
		add("the session: %s", now.Sandbox)
	}
	return out
}

// diffKeyed compares two lists by key and calls added, removed or changed for
// each key that differs. The callbacks run in key order.
func diffKeyed[T any](was, now []T, key func(T) string, value func(T) string,
	added, removed func(string), changed func(string, T, T)) {
	old := map[string]T{}
	for _, v := range was {
		old[key(v)] = v
	}
	cur := map[string]T{}
	for _, v := range now {
		cur[key(v)] = v
	}
	keys := make([]string, 0, len(old)+len(cur))
	for k := range old {
		keys = append(keys, k)
	}
	for k := range cur {
		if _, ok := old[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		a, inOld := old[k]
		b, inNew := cur[k]
		switch {
		case !inOld:
			added(k)
		case !inNew:
			removed(k)
		case value(a) != value(b):
			changed(k, a, b)
		}
	}
}

func routeDest(r PlanRoute) string {
	if r.Upstream == "" && len(r.Services) > 0 {
		return "the Google APIs " + strings.Join(r.Services, ", ")
	}
	return r.Upstream
}

func listOr(list []string) string {
	if len(list) == 0 {
		return "none"
	}
	return strings.Join(list, ", ")
}

func imageWords(i ApprovalImage) string {
	if i.Pinned {
		return i.Ref
	}
	return i.Ref + " (a tag)"
}

func netAsked(n ApprovalNet) string {
	if n.Asked == "" {
		return "nothing"
	}
	return n.Asked + " by " + n.AskedBy
}

func runtimeWords(r ApprovalRuntime) string {
	if r.Backend == "" {
		return r.Kind
	}
	return r.Kind + " " + r.Backend
}

func argsWords(args []string) string {
	if len(args) == 0 {
		return "none"
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = fmt.Sprintf("%q", a)
	}
	return strings.Join(quoted, " ")
}
