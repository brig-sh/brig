package wrap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/runtime"
)

// Plan is `brig plan`: the permissions of a run of this Config, read without
// opening a secret.
//
// It is built from the Config Load resolved, the same one `brig info` and a
// run read, so the three cannot disagree about the home, the network or the
// image. What it does not share with them is BuildEnv. BuildEnv reads every
// needed secret out of the store, fails on a missing one, and writes the git
// helper into the guest home. The plan lists the store instead, which reads
// names and no values, and it writes nothing. So a missing secret is a row
// marked unresolved and the command still answers.
//
// It asks the checks EnsureRunning makes before it prepares anything, and
// says when one refuses. Without them a plan would show an enforced policy,
// or a network of the sandbox's own, for a run `brig run` refuses.
func (c *Config) Plan() PlanDocument {
	listing, listed := c.listSecrets()
	stored := func(name string) (bool, bool) {
		if !listed {
			return false, false
		}
		_, ok := listing[name]
		return ok, true
	}
	d := PlanDocument{
		Session: c.RawName,
		Profile: c.Profile.Name,
		Sandbox: c.VMName,
		Runtime: PlanRuntime{
			InfoRuntime: c.infoRuntime(),
			Backend:     c.hypervisor(),
		},
		Isolation: c.isolationLine(),
		Home:      PlanMount{Host: c.Workspace, Guest: c.Profile.GuestHome, Mode: "read-write"},
		Skills:    c.planSkills(),
		Image:     InfoImage{Ref: c.Image, Pull: c.Pull},
		Verify:    c.infoVerify(),
		// The Info payload's own line, so the two agree on what the running
		// sandbox has, what its next boot gets, and why a backend refuses it.
		Network:  c.networkLine(),
		Posture:  string(c.Network),
		Ports:    []string{},
		Policies: []string{},
		NoPolicy: len(c.Policies) == 0,
		Limits:   PlanLimits{Mem: c.Mem, CPUs: c.CPUs},
	}
	if c.netRecovery == networkUnknown && !c.netExplicit {
		// The run refuses this sandbox (see sessionRefusal). The posture its
		// next boot would get is not one brig can read off it.
		d.Posture = "unknown"
	}
	// The order EnsureRunning asks in, so the reason is the one the run
	// prints, less one check. EnsureRunning asks claimSlug between these
	// two, and claimSlug writes the claim index, which a plan must not. So a
	// session name another session owns is not in the plan, and when it
	// collides and backendRefusal refuses too, the run names the collision
	// and the plan the backend. A runtime that is not there has nothing to
	// answer for the backend, and the RUNTIME row already says it is missing.
	refusal := c.sessionRefusal()
	if refusal == nil && c.Runtime != nil {
		refusal = c.backendRefusal(c.hypervisor())
	}
	if refusal != nil {
		d.Refused = &PlanRefusal{Reason: refusal.Error()}
	}
	if c.Project != "" {
		d.Project = &PlanMount{Host: c.ProjectReal, Guest: c.GuestProject, Mode: "read-write"}
	}
	if c.projectRefused != nil {
		d.ProjectRefused = c.projectRefused.Error()
	}
	for _, p := range c.Publish {
		d.Ports = append(d.Ports, p.String())
	}
	sort.Strings(d.Ports)
	d.Policies = append(d.Policies, c.Policies...)
	sort.Strings(d.Policies)
	if c.Egress.Default != "" {
		d.Egress = &PlanEgress{
			Default: c.Egress.Default,
			Allow:   sortedRules(c.Egress.Allow),
			Deny:    sortedRules(c.Egress.Deny),
		}
	}
	d.Credentials = []PlanCredential{}
	for _, p := range creds.Preview(c.Profile, c.Env, stored, os.LookupEnv, creds.Options{
		AllowRefs:   c.AllowRefs,
		AllowDenied: c.AllowDenied,
	}) {
		d.Credentials = append(d.Credentials, PlanCredential(p))
	}
	d.ArgvExposed = c.planArgvExposed(d.Credentials)
	d.Digest = planDigest(d)
	return d
}

// planSkills is every host directory --skills or BRIG_SKILLS copies into the
// guest home, sorted, never nil. It is host data the guest receives, so it
// belongs in the plan and its digest beside the mounts.
func (c *Config) planSkills() []PlanMount {
	out := []PlanMount{}
	for _, s := range c.HostConfig {
		out = append(out, PlanMount{
			Host:  s.Host,
			Guest: path(c.Profile.GuestHome, s.Rel),
			Mode:  "copy",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// planArgvExposed is the Info payload's argvExposed for the run this plan
// describes: the variables whose values BRIG_ENV_ARGV puts on the runtime's
// command line, by name, sorted.
//
// The run reads them off the set BuildEnv builds, and that set holds values.
// The plan builds the same list of names from what it has: the literals Bind
// forwards, the env credentials Preview resolved, and the plumbing BuildEnv
// and SetupGit add. runtime.ArgvExposed then applies the setting, and leaves
// out a value from the store as it does on the run.
// TestPlanArgvExposedAgreesWithBuildEnv holds the two lists together.
func (c *Config) planArgvExposed(planned []PlanCredential) []string {
	var vars []runtime.Var
	for _, b := range c.Env {
		// Bind forwards a literal with a value, unless the denylist drops it.
		if len(b.RefList()) == 0 && b.Value != "" && (c.AllowDenied || !c.Profile.Denied(b.Name)) {
			vars = append(vars, runtime.Var{Name: b.Name})
		}
	}
	for _, cr := range planned {
		if cr.Delivery == creds.DeliveryEnv && cr.State == creds.StateResolved {
			vars = append(vars, runtime.Var{Name: cr.Name, Secret: cr.Source == creds.SourceSecret})
		}
	}
	plumbing := []string{"GIT_TERMINAL_PROMPT"}
	if c.GitIdentity && c.GitName != "" {
		plumbing = append(plumbing, "GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME")
	}
	if c.GitIdentity && c.GitEmail != "" {
		plumbing = append(plumbing, "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL")
	}
	if c.GitConfig {
		plumbing = append(plumbing, "BRIG_GIT_USER", "URUNC_GIT_USER")
	}
	for _, name := range plumbing {
		vars = append(vars, runtime.Var{Name: name})
	}
	names := runtime.ArgvExposed(vars)
	sort.Strings(names)
	return names
}

// PlanDocument is the Plan payload. Every list in it is sorted and none is
// null, so two plans of the same run encode to the same bytes and the digest
// over them is stable.
type PlanDocument struct {
	Session string `json:"session,omitempty"`
	Profile string `json:"profile"`
	Sandbox string `json:"sandbox"`
	// Refused is why `brig run` refuses this run, from the checks it makes
	// before it prepares anything that a plan can ask without writing (see
	// Plan), and absent when none of them refuses. The plan still describes
	// the run, and the command still exits 0.
	Refused   *PlanRefusal `json:"refused,omitempty"`
	Runtime   PlanRuntime  `json:"runtime"`
	Isolation string       `json:"isolation"`
	Home      PlanMount    `json:"home"`
	Project   *PlanMount   `json:"project,omitempty"`
	// ProjectRefused is why the project this session remembers is not
	// mounted, as in the Info payload. Refused carries the same reason,
	// because the run stops on it.
	ProjectRefused string `json:"projectRefused,omitempty"`
	// Skills are the host directories --skills copies into the guest home.
	Skills []PlanMount `json:"skills"`
	Image  InfoImage   `json:"image"`
	Verify InfoVerify  `json:"verify"`
	// Network is the sentence the Info payload carries, and Posture the word
	// for the next boot, or unknown when the run refuses a sandbox whose
	// network brig cannot read.
	Network string   `json:"network"`
	Posture string   `json:"posture"`
	Ports   []string `json:"ports"`
	// Policies names every policy bound to this run. NoPolicy says none is,
	// so a reader does not have to infer it from an empty list.
	Policies    []string         `json:"policies"`
	NoPolicy    bool             `json:"noPolicy"`
	Egress      *PlanEgress      `json:"egress,omitempty"`
	Credentials []PlanCredential `json:"credentials"`
	// ArgvExposed names what the Info payload's field of the same name
	// names: the values BRIG_ENV_ARGV would put on the runtime's command
	// line. Sorted, where info keeps the order the run adds them in, because
	// the order of a profile's bindings is not a permission. Omitted when the
	// setting is off, which is the default.
	ArgvExposed []string   `json:"argvExposed,omitempty"`
	Limits      PlanLimits `json:"limits"`
	// Digest is sha256 over the compact encoding of this document with
	// Digest empty. See planDigest.
	Digest string `json:"digest"`
}

// PlanRefusal is a refusal `brig run` makes before it starts anything. It is
// an object rather than a string so that a field can be added beside Reason
// later, such as which class of refusal it is, without renaming this one.
type PlanRefusal struct {
	// Reason is the refusal as `brig run` prints it.
	Reason string `json:"reason"`
}

// PlanRuntime is the runtime and the backend under it. Backend is the
// hypervisor the profile or BRIG_HYPERVISOR names, empty for the runtime's
// own default.
type PlanRuntime struct {
	InfoRuntime
	Backend string `json:"backend,omitempty"`
}

// PlanMount is one host directory the guest reaches, where it lands and how.
type PlanMount struct {
	Host  string `json:"host"`
	Guest string `json:"guest"`
	Mode  string `json:"mode"`
}

// PlanEgress is the merged rule set of every bound policy.
type PlanEgress struct {
	Default string        `json:"default"`
	Allow   []policy.Rule `json:"allow"`
	Deny    []policy.Rule `json:"deny"`
}

// PlanLimits is what the guest is given: memory in MB and vCPUs.
type PlanLimits struct {
	Mem  int `json:"mem"`
	CPUs int `json:"cpus"`
}

// PlanCredential is one credential by name, delivery and source. It is
// creds.Planned with JSON names. Plan converts one to the other, so a field
// added to one and not the other fails to build.
type PlanCredential struct {
	Name     string `json:"name"`
	Delivery string `json:"delivery"`
	Source   string `json:"source,omitempty"`
	Secret   string `json:"secret,omitempty"`
	Required bool   `json:"required"`
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
}

// sortedRules copies rules in host, then CIDR order, never nil.
func sortedRules(rules []policy.Rule) []policy.Rule {
	out := append([]policy.Rule{}, rules...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].CIDR < out[j].CIDR
	})
	return out
}

// planDigest is the digest a plan carries. The encoding is encoding/json's
// compact one for the struct: its field order is fixed, the lists are sorted
// by Plan, and <, > and & are escaped as \u003c, \u003e and \u0026. That is
// the data object `brig plan --json` prints, with digest set to "" and the
// indentation taken out. docs/cli.md says so, for a reader who re-hashes it.
func planDigest(d PlanDocument) string {
	d.Digest = ""
	blob, err := json.Marshal(d)
	if err != nil {
		// Every field is a string, a number, a bool or a list of them.
		panic(err)
	}
	sum := sha256.Sum256(blob)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// PrintPlan writes the plan as aligned rows to Out, the form a person reads.
func (c *Config) PrintPlan(d PlanDocument) {
	var rows []envelopeRow
	// First, because it changes how every row under it reads: they describe a
	// run that does not happen until the reason is dealt with. A reason runs
	// to several lines when it carries advice, and each takes a row of its
	// own so the column stays aligned.
	if d.Refused != nil {
		for i, line := range strings.Split("brig run refuses this: "+d.Refused.Reason, "\n") {
			rows = append(rows, envelopeRow{labelOnce(i, "REFUSED"), line})
		}
	}
	if d.Session != "" {
		rows = append(rows, envelopeRow{"SESSION", d.Session})
	}
	runtimeLine := "unavailable (no runtime on PATH)"
	if d.Runtime.Available {
		runtimeLine = d.Runtime.Kind + " (" + d.Runtime.Bin + ")"
		if d.Runtime.Backend != "" {
			runtimeLine += ", backend " + d.Runtime.Backend
		}
	}
	rows = append(rows,
		envelopeRow{"PROFILE", d.Profile},
		envelopeRow{"SANDBOX", d.Sandbox},
		envelopeRow{"RUNTIME", runtimeLine},
		envelopeRow{"ISOLATION", d.Isolation},
		envelopeRow{"HOME", fmt.Sprintf("%s (%s, mounted at %s)", d.Home.Host, d.Home.Mode, d.Home.Guest)},
	)
	if d.Project != nil {
		rows = append(rows, envelopeRow{"PROJECT",
			fmt.Sprintf("%s (%s, mounted at %s)", d.Project.Host, d.Project.Mode, d.Project.Guest)})
	}
	// The run stops on a refused project rather than going on without it, so
	// the row names the directory and leaves the reason to REFUSED.
	if d.ProjectRefused != "" {
		rows = append(rows, envelopeRow{"PROJECT", strings.TrimSpace(c.refusedProject + " (refused, see REFUSED)")})
	}
	for i, s := range d.Skills {
		rows = append(rows, envelopeRow{labelOnce(i, "SKILLS"), fmt.Sprintf("%s (copied to %s)", s.Host, s.Guest)})
	}
	rows = append(rows,
		envelopeRow{"IMAGE", fmt.Sprintf("%s (pull %s)", d.Image.Ref, d.Image.Pull)},
		envelopeRow{"VERIFY", c.verifyLine()},
		envelopeRow{"NETWORK", d.Network},
	)
	for i, p := range d.Ports {
		rows = append(rows, envelopeRow{labelOnce(i, "PORTS"), p})
	}
	if d.NoPolicy {
		rows = append(rows, envelopeRow{"POLICY", "(none)"})
	} else {
		rows = append(rows, envelopeRow{"POLICY", strings.Join(d.Policies, ", ")})
	}
	if d.Egress != nil {
		rows = append(rows, envelopeRow{"EGRESS", "default " + d.Egress.Default})
		for _, r := range d.Egress.Allow {
			rows = append(rows, envelopeRow{"", "allow " + ruleText(r)})
		}
		for _, r := range d.Egress.Deny {
			rows = append(rows, envelopeRow{"", "deny " + ruleText(r)})
		}
	}
	if len(d.Credentials) == 0 {
		rows = append(rows, envelopeRow{"CREDENTIALS", "(none)"})
	}
	// A withheld credential's reason is a notice with advice on lines of its
	// own, and each line takes a row so the column stays aligned, as the
	// REFUSED rows do.
	n := 0
	for _, cr := range d.Credentials {
		for _, line := range strings.Split(credentialText(cr), "\n") {
			rows = append(rows, envelopeRow{labelOnce(n, "CREDENTIALS"), line})
			n++
		}
	}
	if len(d.ArgvExposed) > 0 {
		rows = append(rows, envelopeRow{"ARGV", "BRIG_ENV_ARGV=1, so these values go on the runtime's " +
			"command line, where `ps` can read them: " + strings.Join(d.ArgvExposed, " ")})
	}
	rows = append(rows,
		envelopeRow{"LIMITS", fmt.Sprintf("%d MB, %d vCPUs", d.Limits.Mem, d.Limits.CPUs)},
		envelopeRow{"DIGEST", d.Digest},
	)
	writeRows(c.Out, rows)
}

// labelOnce is the label on the first row of a group and blank on the rest,
// the way the PORTS rows of the envelope read.
func labelOnce(i int, label string) string {
	if i == 0 {
		return label
	}
	return ""
}

func ruleText(r policy.Rule) string {
	if r.Host != "" {
		return r.Host
	}
	return r.CIDR
}

// credentialText is one CREDENTIALS row: the name, how it arrives, and where
// from or why not. Names only.
func credentialText(cr PlanCredential) string {
	var detail string
	switch {
	case cr.State == creds.StateResolved && cr.Delivery == creds.DeliveryNone:
		// A required secret the chain passed over. The store has it, so the
		// run starts, but its value reaches no guest variable or file, and
		// "from the secret" would read as if it did.
		detail = "the secret " + cr.Secret + " is in the store, and no binding delivers it"
	case cr.State == creds.StateResolved:
		detail = "from the environment"
		if cr.Source == creds.SourceSecret {
			detail = "from the secret " + cr.Secret
		}
	default:
		detail = cr.State + ": " + cr.Reason
	}
	if cr.Required && (cr.State != creds.StateResolved || cr.Delivery == creds.DeliveryNone) {
		// On the first line, beside the name, rather than after the last
		// line of a reason's advice.
		first, rest, more := strings.Cut(detail, "\n")
		detail = first + " (required)"
		if more {
			detail += "\n" + rest
		}
	}
	return fmt.Sprintf("%s (%s) %s", cr.Name, cr.Delivery, detail)
}
