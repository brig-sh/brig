package runtime

import "fmt"

// Capability is a run path's answer to whether it enforces a property a run
// asks for. Brig boots a run that needs a property only on an answer of
// Enforced. The other two refuse, because a sandbox that reports a policy and
// filters nothing is worse than no policy: someone relies on it.
type Capability string

const (
	// Enforced means the run path enforces the property.
	Enforced Capability = "enforced"
	// CannotEnforce means the run path is known not to enforce it.
	CannotEnforce Capability = "cannot enforce"
	// Unknown means nothing confirms either answer: a run path with no
	// record, or a runtime whose probe failed.
	Unknown Capability = "unknown"
)

// Property is something a run asks a runtime to enforce.
type Property string

// EgressPolicy is the egress rule set a policy puts on a sandbox.
const EgressPolicy Property = "egress policy"

// Teleportable is a session that owns all of its storage: a block root with
// the guest home inside it and no host share. Only a run path that can keep
// the whole session in its own disk can run one, so it is a property asked
// of the run path like a policy is, and refused the same way, with exit 7.
const Teleportable Property = "teleportable session"

// RunPath is one runtime with one backend: hull with vz, hvi or qemu, or
// nerdctl or docker with its containerd shim. A property is enforced or not
// by the pair, and hull alone says nothing: the same binary enforces a
// policy on hvi and cannot on vz.
type RunPath struct {
	Runtime string
	Backend string
}

// String names the runtime alone for a record that holds on every backend,
// which has no backend to name.
func (p RunPath) String() string {
	if p.Backend == anyBackend {
		return p.Runtime
	}
	return p.Runtime + " on " + p.Backend
}

// Record is one run path's answer for one property, with the reason it holds.
type Record struct {
	Property Property
	Path     RunPath
	State    Capability
	// Why says what makes this the answer. A refusal quotes it.
	Why string
	// Remedy says what to do about a refusal on this path.
	Remedy string
}

// anyBackend is a record's backend when the answer holds for every backend of
// its runtime. On the nerdctl path the rules sit on the bridge the shim's
// guest is attached to, so the shim makes no difference to the answer there.
const anyBackend = ""

// records is the one table of answers. supports and the nerdctl adapter read
// it, so the two refusals cannot come to different answers about the same
// question. hvi and nerdctl read Enforced here, and a probe confirms it at
// boot: the gateway probe against the binary that starts the gateway on hvi,
// and an nft probe in the bridges' network namespace on nerdctl. See
// gatewayEnforces and egressEnforces.
var records = []Record{
	{EgressPolicy, RunPath{"hull", "hvi"}, Enforced,
		"brig puts the rules on the user-mode network gateway that is this sandbox's only way out",
		""},
	{EgressPolicy, RunPath{"hull", "vz"}, CannotEnforce,
		"vz takes its network from vmnet, which brig does not filter. brig enforces a policy " +
			"at the user-mode network gateway that only the hvi backend uses",
		"Run it on hvi (BRIG_HYPERVISOR=hvi), or detach the policy"},
	{EgressPolicy, RunPath{"hull", "qemu"}, CannotEnforce,
		"qemu takes its network from vmnet, which brig does not filter. brig enforces a policy " +
			"at the user-mode network gateway that only the hvi backend uses",
		"Run it on hvi (BRIG_HYPERVISOR=hvi), or detach the policy"},
	{EgressPolicy, RunPath{"nerdctl", anyBackend}, Enforced,
		"brig puts the rules in nftables on the bridge of the sandbox's own network, and answers its DNS",
		""},
	// The nerdctl adapter drives docker too, and names it. docker manages its
	// own bridges and firewall, and brig does not put rules on them.
	{EgressPolicy, RunPath{"docker", anyBackend}, CannotEnforce, dockerWhy,
		"Use nerdctl (BRIG_RUNTIME_BIN=nerdctl), or detach the policy"},
}

const dockerWhy = "brig enforces a policy on nerdctl's container network, and does not filter docker's"

// teleportRecords answer whether a run path can run a teleportable session.
// A separate table from records because the reasons are not about networks,
// and a caller printing the egress table must not grow rows about disks.
//
// hvi is the only backend that boots the guest from a block disk of its own
// and is driven by the checkpoint work that moving a session needs. vz and
// qemu could boot a block root, but nothing brig drives captures them, and a
// session marked movable on a backend that cannot move it is the same false
// promise as a policy nothing enforces.
var teleportRecords = []Record{
	{Teleportable, RunPath{"hull", "hvi"}, Enforced,
		"hvi boots the guest from a block disk of its own, with no host directory shared in",
		""},
	{Teleportable, RunPath{"hull", "vz"}, CannotEnforce, teleportBackendWhy, teleportBackendRemedy},
	{Teleportable, RunPath{"hull", "qemu"}, CannotEnforce, teleportBackendWhy, teleportBackendRemedy},
	{Teleportable, RunPath{"nerdctl", anyBackend}, CannotEnforce, teleportContainerWhy, teleportContainerRemedy},
	{Teleportable, RunPath{"docker", anyBackend}, CannotEnforce, teleportContainerWhy, teleportContainerRemedy},
}

const (
	teleportBackendWhy = "brig keeps a teleportable session in a disk that only the hvi backend " +
		"can capture"
	teleportBackendRemedy   = "Run it on hvi (BRIG_HYPERVISOR=hvi), or run the session without --teleportable"
	teleportContainerWhy    = "a container runtime keeps the guest root in its own snapshotter, not in a disk brig can move"
	teleportContainerRemedy = "Run it on macOS under hull's hvi backend, or run the session without --teleportable"
)

// Records returns every answer brig holds for a property.
func Records(p Property) []Record {
	var out []Record
	for _, r := range table(p) {
		if r.Property == p {
			out = append(out, r)
		}
	}
	return out
}

// Answer is the record for one property on one run path. A path with no
// record answers Unknown, so a backend brig has not been taught about is
// refused.
func Answer(p Property, path RunPath) Record {
	for _, exact := range []bool{true, false} {
		for _, r := range table(p) {
			if r.Property != p || r.Path.Runtime != path.Runtime {
				continue
			}
			if (exact && r.Path.Backend == path.Backend) ||
				(!exact && r.Path.Backend == anyBackend) {
				r.Path = path
				return r
			}
		}
	}
	remedy := "Run it on hull's hvi backend (BRIG_HYPERVISOR=hvi), or detach the policy"
	if p == Teleportable {
		remedy = teleportBackendRemedy
	}
	return Record{Property: p, Path: path, State: Unknown,
		Why:    "brig holds no answer for this run path",
		Remedy: remedy}
}

// table is the rows that answer one property.
func table(p Property) []Record {
	if p == Teleportable {
		return teleportRecords
	}
	return records
}

// CapabilityError refuses a run because its run path does not enforce a
// property the run asks for. It names the property, the runtime and the
// backend, so a caller can tell this refusal from any other failure, and the
// person reading it knows which of the three to change.
type CapabilityError struct {
	Property Property
	Path     RunPath
	State    Capability
	// Why and Remedy come from the record. For a failed probe, Why names
	// the probe command.
	Why    string
	Remedy string
	// Err is what went wrong while asking, such as a probe that did not
	// run. Nil when the answer came from the table.
	Err error
}

func (e *CapabilityError) Error() string {
	if e.Property == Teleportable {
		return e.teleportError()
	}
	var head string
	if e.State == CannotEnforce {
		head = fmt.Sprintf("a policy applies to this sandbox, and %s cannot enforce the %s",
			e.Path, e.Property)
	} else {
		head = fmt.Sprintf("a policy applies to this sandbox, and whether %s enforces the %s is %s",
			e.Path, e.Property, Unknown)
	}
	msg := head + ": " + e.Why
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	msg += ". " + e.Remedy + ". brig will not boot a sandbox under a policy nothing enforces"
	return msg
}

func (e *CapabilityError) Unwrap() error { return e.Err }

// teleportError is Error for a teleportable session. Worded apart from the
// policy refusal because it is not one: no policy applies, and a message
// saying so would send the reader to `brig policy` for a disk question.
func (e *CapabilityError) teleportError() string {
	var head string
	if e.State == CannotEnforce {
		head = fmt.Sprintf("this session is teleportable, and %s cannot run a teleportable session", e.Path)
	} else {
		head = fmt.Sprintf("this session is teleportable, and whether %s can run one is %s", e.Path, Unknown)
	}
	msg := head + ": " + e.Why
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg + ". " + e.Remedy
}

// RequireTeleportable refuses a teleportable session on a run path that
// cannot keep it whole. kind is the runtime's Kind and hv the hypervisor the
// run asked for, empty for the runtime's default. On hull the default is
// resolved here, so an unset hypervisor is refused as the vz it boots rather
// than as a backend with no name.
func RequireTeleportable(kind, hv string) error {
	path := RunPath{Runtime: kind, Backend: hv}
	if kind == "hull" {
		path.Backend = hypervisorOrDefault(hv)
	}
	r := Answer(Teleportable, path)
	if r.State == Enforced {
		return nil
	}
	return &CapabilityError{Property: Teleportable, Path: r.Path, State: r.State,
		Why: r.Why, Remedy: r.Remedy}
}

// requireEgress refuses a run whose path does not enforce the policy it
// carries. A run with no policy asks nothing. An offline run asks nothing
// either: it reaches no network, so it satisfies every rule set. Refusing it
// refuses the stricter posture for not being the weaker one.
func requireEgress(spec RunSpec, path RunPath) error {
	if !spec.Egress.Filtered() || spec.Net == "none" {
		return nil
	}
	r := Answer(EgressPolicy, path)
	if r.State == Enforced {
		return nil
	}
	return &CapabilityError{Property: EgressPolicy, Path: path, State: r.State,
		Why: r.Why, Remedy: r.Remedy}
}
