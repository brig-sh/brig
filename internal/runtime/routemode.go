package runtime

import (
	"fmt"
	"slices"
	"sync"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/routemode"
)

// RouteModeProperty is the property a run asks for when a policy bound to it
// declares a route in that mode. A refusal names it.
func RouteModeProperty(m v1alpha1.RouteMode) Property {
	return Property(fmt.Sprintf("route mode %s", m))
}

// ModeRecord is one run path's answer for one route mode, with the reason it
// holds.
type ModeRecord struct {
	Mode  v1alpha1.RouteMode
	Path  RunPath
	State Capability
	// Why says what makes this the answer. A refusal quotes it.
	Why string
	// Note is more of the answer, which a row and a refusal print after
	// Why, or "".
	Note string
	// Remedy says what to do about a refusal on this path.
	Remedy string
	// Transport is how the guest reaches the broker on this path, where the
	// mode is served: brig.internal.
	Transport string
}

// The transports a ModeRecord names. They are the transports of
// internal/injector, which this package does not import.
const (
	transportBrigInternal = BrigInternalName
)

// Line is the answer as one row prints it: the state, the reason with its
// note, and the transport where the mode is served.
func (r ModeRecord) Line() string {
	line := string(r.State)
	if r.Why != "" {
		line += ": " + r.Why
	}
	if r.Note != "" {
		line += "; " + r.Note
	}
	if r.Transport != "" && r.State == Enforced {
		line += "; transport: " + r.Transport
	}
	return line
}

// The reasons the route mode table quotes.
const (
	// EndpointHviWhy is why hull on hvi serves a route in mode endpoint.
	EndpointHviWhy = "hull's gateway answers brig.internal with its own address and diverts port 443 of it " +
		"to the broker, which serves it with TLS under the brig CA"
	// EndpointNerdctlWhy is why nerdctl serves a route in mode endpoint.
	EndpointNerdctlWhy = "the sandbox's resolver answers brig.internal with its bridge's own address, and its " +
		"nftables table diverts port 443 of it to the broker, which serves it with TLS under the brig CA"
	endpointVmnetWhy = "%s takes its network from vmnet, and brig routes brig.internal at the user-mode network " +
		"gateway that only the hvi backend uses"
	endpointVmnetRemedy = "Run it on hvi (BRIG_HYPERVISOR=hvi), or detach the policy"
	endpointDockerWhy   = "brig routes brig.internal with nftables on nerdctl's bridge, and docker manages its own " +
		"bridges and firewall"
	endpointGuestRemedy = "Detach the policy"
	endpointOfflineWhy  = "the guest reaches brig.internal over its network, and this run has none " +
		"(BRIG_NETWORK=offline)"
	// otherModeRemedy is the remedy of a refused route in a mode other
	// than endpoint.
	otherModeRemedy = "Use mode: endpoint, or detach the policy"
)

// runcBackend stands for every shim that boots a container on the host
// kernel, in the route mode table. See modeKey.
const runcBackend = "runc"

// modeRecords is the one table of route mode answers. The endpoint rows say
// where a guest reaches the broker: a guest behind a gateway brig owns at
// brig.internal, which hull on vz and qemu and docker do not have. brig boots
// no runc guest at all. The hvi and urunc rows hold only when
// CheckBrigInternal does.
var modeRecords = []ModeRecord{
	{Mode: v1alpha1.ModeEndpoint, Path: RunPath{"hull", "hvi"}, State: Enforced, Why: EndpointHviWhy,
		Transport: transportBrigInternal, Remedy: endpointVmnetRemedy},
	{Mode: v1alpha1.ModeEndpoint, Path: RunPath{"hull", "vz"}, State: CannotEnforce,
		Why: fmt.Sprintf(endpointVmnetWhy, "vz"), Remedy: endpointVmnetRemedy},
	{Mode: v1alpha1.ModeEndpoint, Path: RunPath{"hull", "qemu"}, State: CannotEnforce,
		Why: fmt.Sprintf(endpointVmnetWhy, "qemu"), Remedy: endpointVmnetRemedy},
	{Mode: v1alpha1.ModeEndpoint, Path: RunPath{"nerdctl", uruncShim}, State: Enforced, Why: EndpointNerdctlWhy,
		Transport: transportBrigInternal, Remedy: endpointGuestRemedy},
	{Mode: v1alpha1.ModeEndpoint, Path: RunPath{"nerdctl", runcBackend}, State: CannotEnforce,
		Why:    "brig boots no shim that gives the guest the host's own kernel, so there is no guest to route brig.internal to",
		Remedy: "Unset BRIG_CONTAINERD_RUNTIME to use urunc, or detach the policy"},
	{Mode: v1alpha1.ModeEndpoint, Path: RunPath{"docker", anyBackend}, State: CannotEnforce,
		Why:    endpointDockerWhy,
		Remedy: "Use nerdctl (BRIG_RUNTIME_BIN=nerdctl), or detach the policy"},
}

// modeRecordsMu guards modeRecords against RegisterModeRecords.
var modeRecordsMu sync.RWMutex

// RegisterModeRecords adds the rows of a route mode registered in this build
// (see internal/routemode) to the table, after the rows already there. It
// returns a function that removes them again, for a test.
func RegisterModeRecords(rs ...ModeRecord) (unregister func()) {
	modeRecordsMu.Lock()
	defer modeRecordsMu.Unlock()
	start := len(modeRecords)
	modeRecords = append(slices.Clip(modeRecords), rs...)
	return func() {
		modeRecordsMu.Lock()
		defer modeRecordsMu.Unlock()
		modeRecords = slices.Delete(slices.Clip(modeRecords), start, start+len(rs))
	}
}

// modeTable returns the route mode table.
func modeTable() []ModeRecord {
	modeRecordsMu.RLock()
	defer modeRecordsMu.RUnlock()
	return modeRecords
}

// ModeRecords returns every answer brig holds for a route mode, in table
// order.
func ModeRecords(m v1alpha1.RouteMode) []ModeRecord {
	var out []ModeRecord
	for _, r := range modeTable() {
		if r.Mode == m {
			out = append(out, r)
		}
	}
	return out
}

// modeKey is the path the table holds for path. nerdctl's backend is the
// containerd runtime it names, placed by shimBoundary: every name that boots
// a container on the host kernel shares one row, and every name that starts
// the urunc shim shares another.
func modeKey(path RunPath) RunPath {
	if path.Runtime != "nerdctl" {
		return path
	}
	switch shimBoundary(path.Backend) {
	case BoundaryContainer:
		return RunPath{path.Runtime, runcBackend}
	case BoundaryVM:
		return RunPath{path.Runtime, uruncShim}
	}
	return path
}

// ModeAnswer is the record for one route mode on one run path. A path with
// no record answers Unknown, so a backend brig has not been taught about is
// refused. A mode this build does not serve cannot be enforced anywhere.
func ModeAnswer(m v1alpha1.RouteMode, path RunPath) ModeRecord {
	if !routemode.Served(m) {
		return ModeRecord{Mode: m, Path: path, State: CannotEnforce, Why: routemode.NotInBuild(m),
			Remedy: otherModeRemedy}
	}
	key := modeKey(path)
	table := modeTable()
	for _, exact := range []bool{true, false} {
		for _, r := range table {
			if r.Mode != m || r.Path.Runtime != key.Runtime {
				continue
			}
			if (exact && r.Path.Backend == key.Backend) || (!exact && r.Path.Backend == anyBackend) {
				r.Path = path
				return r
			}
		}
	}
	remedy := endpointGuestRemedy
	if m != v1alpha1.ModeEndpoint {
		remedy = otherModeRemedy
	}
	return ModeRecord{Mode: m, Path: path, State: Unknown, Why: "brig holds no answer for this run path",
		Remedy: remedy}
}

// ServedModes lists the route modes the run path serves: endpoint, then the
// registered modes in name order. It is empty where no mode is served.
func ServedModes(path RunPath) []v1alpha1.RouteMode {
	out := []v1alpha1.RouteMode{}
	for _, m := range append([]v1alpha1.RouteMode{v1alpha1.ModeEndpoint}, routemode.Names()...) {
		if ModeAnswer(m, path).State == Enforced {
			out = append(out, m)
		}
	}
	return out
}

// RequireRouteMode refuses a route mode the run path does not serve, with a
// CapabilityError that quotes the table.
func RequireRouteMode(m v1alpha1.RouteMode, path RunPath) error {
	r := ModeAnswer(m, path)
	if r.State == Enforced {
		return nil
	}
	why := r.Why
	if r.Note != "" {
		why += "; " + r.Note
	}
	return &CapabilityError{Property: RouteModeProperty(m), Path: path, State: r.State, Why: why, Remedy: r.Remedy}
}

// BrigInternalProber is a runtime that can say whether its binaries route
// brig.internal from a guest to the broker.
type BrigInternalProber interface {
	// ProbeBrigInternal returns a CapabilityError when they do not, or when
	// it cannot tell.
	ProbeBrigInternal() error
}

// CheckBrigInternal refuses a route in mode endpoint that rt cannot serve to
// its guest at brig.internal on path, with a CapabilityError. The route mode
// table answers first. Where it answers enforced, the run must have a
// network, and run on a runtime that reports the brig.internal address and
// whose binaries divert it. net is the run's network: none, shared or
// isolated.
func CheckBrigInternal(rt Runtime, path RunPath, net string) error {
	if err := RequireRouteMode(v1alpha1.ModeEndpoint, path); err != nil {
		return err
	}
	refuse := func(state Capability, why, remedy string) error {
		return &CapabilityError{Property: RouteModeProperty(v1alpha1.ModeEndpoint), Path: path, State: state,
			Why: why, Remedy: remedy}
	}
	if net == "none" {
		return refuse(CannotEnforce, endpointOfflineWhy, "Run it with a network, or detach the policy")
	}
	// hull gives a sandbox that reaches brig.internal a gateway of its own
	// whatever the posture (see isolatedNet). nerdctl's sandbox table sits on
	// a network of the sandbox's own, which the posture has to ask for.
	if path.Runtime == "nerdctl" && net != "isolated" {
		return refuse(CannotEnforce, "brig routes brig.internal on a network of the sandbox's own, and the run "+
			"asks for the "+net+" network", "Run it with --network isolated, or detach the policy")
	}
	if _, ok := rt.(BrigInternalRouter); !ok {
		return refuse(Unknown, "brig cannot ask this runtime where its guest reaches brig.internal",
			endpointGuestRemedy)
	}
	if p, ok := rt.(BrigInternalProber); ok {
		return p.ProbeBrigInternal()
	}
	return nil
}

// RoutePath is the run path a route mode is answered for: the runtime, and
// the hypervisor or the containerd shim the run resolves to. hypervisor is
// the macOS backend the run asked for, or "" for hull's default.
func RoutePath(rt Runtime, hypervisor string) RunPath {
	if n, ok := rt.(*nerdctl); ok {
		return RunPath{n.driver(), containerdRuntime()}
	}
	switch kind := rt.Kind(); kind {
	case "hull":
		return RunPath{kind, hypervisorOrDefault(hypervisor)}
	case "nerdctl":
		return RunPath{kind, containerdRuntime()}
	default:
		return RunPath{kind, anyBackend}
	}
}
