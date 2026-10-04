package wrap

import (
	"errors"
	"fmt"
	"os"
	goruntime "runtime"

	"github.com/brig-sh/brig/internal/notice"
	"github.com/brig-sh/brig/internal/runtime"
)

// A runtime probe has one outcome. Unknown metadata must not also look like
// confirmed absence, which would authorize applying the new default.
type networkRecoveryState uint8

const (
	networkNoRecovery networkRecoveryState = iota
	networkUnknown
	networkAbsent
	networkRecovered
)

// Network is the posture a sandbox runs with.
type Network string

const (
	// NetShared is one network for sandboxes on the host. Whether they can
	// reach each other depends on the backend. See docs/security.md.
	NetShared Network = "shared"
	// NetIsolated is a network of this sandbox's own, so no other sandbox is
	// on it whatever the backend does with a shared one.
	NetIsolated Network = "isolated"
	// NetOffline is a sandbox with no route out. The agent runs, the workspace
	// is mounted, nothing leaves.
	NetOffline Network = "offline"
)

// ParseNetworkStrict reads a posture and refuses anything it does not
// recognise. source is how the value reached brig -- a flag, a setting, a
// profile field -- and it is named in the refusal, because being told about
// BRIG_NETWORK when you typed --network sends you looking for a variable you
// never set.
//
// Strict for the same reason the security switches are: this decides whether a
// sandbox can reach the network at all, so a typo must stop the run rather
// than quietly pick a posture nobody asked for. The empty string is the unset
// case and keeps the default.
func ParseNetworkStrict(s, source string) (Network, error) {
	switch s {
	case "":
		return NetIsolated, nil
	case string(NetShared):
		return NetShared, nil
	case string(NetIsolated):
		return NetIsolated, nil
	case string(NetOffline):
		return NetOffline, nil
	default:
		return NetShared, fmt.Errorf("%s %q is not a posture: use shared, isolated or offline",
			source, s)
	}
}

// sharedNetworkBackend names the backends that need shared as their unset
// default: vz and qemu use vmnet, which cannot honour isolated. Load asks only
// when no posture was named, so an explicit isolated choice is still refused.
// It also answers for info when no runtime is installed. The selected kind
// follows DetectFor; a Linux run must never inherit the macOS fallback just
// because its portable profile has no hypervisor field.
func sharedNetworkBackend(rt runtime.Runtime, hypervisor string) string {
	kind := os.Getenv("BRIG_RUNTIME")
	if rt != nil {
		kind = rt.Kind()
	} else if kind == "" && goruntime.GOOS == "darwin" {
		kind = "hull"
	}
	if kind == "hull" {
		switch hypervisor {
		case "", "vz":
			return "vz"
		case "qemu":
			return "qemu"
		}
	}
	return ""
}

func networkFromRuntime(word string) (Network, error) {
	for _, n := range AllNetworks() {
		if n.RuntimeNet() == word {
			return n, nil
		}
	}
	return "", fmt.Errorf("runtime reported an unknown network posture %q", word)
}

// AllNetworks is every posture that exists. A list rather than a comment, so a
// test can walk it and catch a posture that gained a case in one switch and
// not the other.
func AllNetworks() []Network { return []Network{NetShared, NetIsolated, NetOffline} }

// RuntimeNet is the word the runtime adapters take for this posture. The two
// vocabularies are deliberately separate: "offline" is what a person asks for,
// "none" is what a runtime is told.
func (n Network) RuntimeNet() string {
	switch n {
	case NetOffline:
		return "none"
	case NetIsolated:
		return "isolated"
	default:
		return "shared"
	}
}

// Line is the posture as a reader is told it, with the consequence spelled
// out: "offline" on its own is a word, and what a reader needs is what it
// costs them.
//
// Beside RuntimeNet deliberately. A posture has two vocabularies, the
// runtime's and the reader's, and keeping both on the type means a posture
// added later cannot pick up one translation and quietly miss the other.
//
// The shared line describes the topology and stops there. Whether one sandbox
// can actually reach another on that network is a property of the backend, not
// of this setting, and it has changed under brig between runtime releases. A
// row claiming either answer is false somewhere, or false after the next
// release, so it claims what is true everywhere: these sandboxes are on one
// network rather than each on its own. The answer per backend, and when it was
// measured, is in docs/security.md and docs/manual-tests/sandbox-reachability.md.
func (n Network) Line() string {
	switch n {
	case NetOffline:
		return "offline (no egress)"
	case NetIsolated:
		return "isolated (a network of this sandbox's own)"
	default:
		return "shared (one network for every sandbox on this host)"
	}
}

// backendSpec is the part of a run a backend is entitled to refuse: which
// backend, what network, which rules, and whether it opens a window.
//
// One derivation, used both by the check before anything is started and by the
// spec that is actually booted, so the two cannot come to different answers
// about the same run. The boot fills in the rest.
func (c *Config) backendSpec(hypervisor string) runtime.RunSpec {
	return runtime.RunSpec{
		Name:       c.VMName,
		Hypervisor: hypervisor,
		Net:        c.Network.RuntimeNet(),
		Egress:     runtimeEgress(c.Egress),
		Publish:    c.Publish,
		GUI:        c.Profile.IsGUI(),
	}
}

// checkBackend refuses a run this backend cannot honour, before anything is
// started -- and, unlike the check inside Run, on the path that finds the
// sandbox already up and boots nothing.
//
// That path is where this matters most. A policy attached to a sandbox running
// on a backend that cannot enforce it would otherwise be refused on the first
// run and waved through on every one after, printing a POLICY row over a
// sandbox filtering nothing. A runtime with no opinion is not asked.
func (c *Config) checkBackend(hypervisor string) error {
	var err error
	if checker, ok := c.Runtime.(runtime.RunChecker); ok {
		err = checker.CanRun(c.backendSpec(hypervisor))
	}
	// A policy the backend cannot enforce is refused as that, whatever chose
	// the network. Its exit code is 7, and a script reads that code to tell
	// this refusal from every other one. Most shipped profiles set network:
	// isolated, so the profile refusal below would otherwise answer first on
	// vz and qemu, and the policy refusal would exit 1.
	var refusal *runtime.CapabilityError
	if errors.As(err, &refusal) {
		return err
	}
	// Ahead of CanRun's other refusals. Its isolated refusal names --network
	// isolated, which is what the user typed when the flag or BRIG_NETWORK
	// said so. A profile's network: is not that: BRIG_HYPERVISOR=vz, the
	// macOS 14 advice, then refuses a flag that is not on the command line
	// and never says that --network shared or BRIG_NETWORK=shared is the way
	// through.
	if msg := c.profileIsolationOnFallback(hypervisor); msg != "" {
		return errors.New(msg)
	}
	return err
}

// profileIsolationOnFallback is the refusal for a profile that asks for an
// isolated network on a backend that can only share one.
//
// sharedNetworkBackend is the same test Load uses for the unset-posture
// fallback, so this fires only where that fallback would have replaced an
// empty network: and does not fire for hvi. The sentence is also what the
// NETWORK row appends, so info and the refusal name one fix.
func (c *Config) profileIsolationOnFallback(hypervisor string) string {
	if c.askedNetwork != NetIsolated || !c.netFromProfile {
		return ""
	}
	backend := sharedNetworkBackend(c.Runtime, hypervisor)
	if backend == "" {
		return ""
	}
	// An unset hypervisor on hull is that same fallback. Quoting the empty
	// string would blame a variable the user did not set.
	shown := hypervisor
	if shown == "" {
		shown = backend
	}
	return fmt.Sprintf("the %s profile sets network: isolated, and the %s backend cannot "+
		"give a sandbox its own network (BRIG_HYPERVISOR is %q). Pass --network shared "+
		"or set BRIG_NETWORK=shared to run it on the shared network, or run it on hvi",
		c.Profile.Name, backend, shown)
}

// CanPublish reports what this backend cannot honour about opening these ports
// on this sandbox, or nil.
//
// `brig network publish` on a stopped sandbox writes a record and boots
// nothing. Without this the refusal arrives at the next `brig run`, against a
// publication the user typed yesterday and can no longer see. Asked here, the
// refusal names the backend while the port is still on the command line.
func (c *Config) CanPublish(add []runtime.Publication) error {
	checker, ok := c.Runtime.(runtime.RunChecker)
	if !ok {
		return nil
	}
	spec := c.backendSpec(c.hypervisor())
	spec.Publish = add
	return checker.CanRun(spec)
}

// networkStale reports whether the sandbox that is already running differs
// from what this run resolved to.
//
// Asked of the runtime, because the runtime is what knows: brig owns the
// network on hvi and can compare what the sandbox is behind against what these
// rules ask for, and owns none of it on a backend that takes its network from
// somewhere else. A runtime that cannot answer is treated as current -- a
// restart nobody needed costs a boot, and this is not the check that should be
// deciding to spend one on a guess.
func (c *Config) networkStale() bool {
	checker, ok := c.Runtime.(runtime.NetworkChecker)
	if !ok {
		return false
	}
	return checker.NetworkStale(c.VMName, c.hypervisor(), c.Network.RuntimeNet(), runtimeEgress(c.Egress))
}

// postureChanged reports whether this run asks for a different posture than
// the one the running sandbox was booted with.
//
// Decided from the record or recovered legacy configuration. hull on hvi can compare
// an isolated gateway, but it has no answer for offline, vz has none at all, and
// nerdctl is not asked. A running sandbox kept on any of them would leave this
// run reporting a posture the sandbox does not have. If legacy inspection
// failed, an explicit choice authorizes recreation without guessing its old
// posture; a flagless run is refused before this check.
func (c *Config) postureChanged() bool {
	if c.netRecovery == networkUnknown {
		return c.netExplicit
	}
	return c.recordedNet != "" && c.askedNetwork != "" && c.askedNetwork != c.recordedNet
}

// recordPosture records the requested posture after a boot, or the recovered
// posture after successfully reusing an older sandbox with no record.
//
// A warning when it fails, the way rememberSession treats its own write: the
// sandbox is up, and a later command must inspect it to recover its posture.
// That cannot recover a requested posture before a policy narrowed it.
func (c *Config) recordPosture() {
	if c.askedNetwork == "" {
		return
	}
	c.netRecovery = networkNoRecovery
	if err := runtime.RecordBootedNet(c.VMName, c.askedNetwork.RuntimeNet()); err != nil {
		c.warnf("%s", notice.Newf("could not record that %s was started %s: %v", c.VMName, c.askedNetwork, err).
			Note("a later command must inspect its actual network and cannot recover the posture before a policy narrowed it"))
	}
}

// runningNet is the posture the sandbox that is up now runs with, or "" when
// brig cannot tell. Callers ask whether the sandbox is running; this does not.
//
// The record holds the posture that was asked for, and a policy narrows shared
// to isolated without changing it (see recordPosture). So a shared record is
// checked against the runtime: asked about shared with no rules, hull on hvi
// reports stale exactly when an isolated gateway is up for this sandbox. That
// is the "the policy detached" case of runtime's TestNetworkStale. nerdctl
// answers the same way from its egress records. A policy narrows the posture
// on hvi and nerdctl alone, because every other backend refuses one unless the
// sandbox is offline, so a runtime that cannot answer leaves the record as it
// is. A gateway left up by a sandbox that died without a stop
// answers the same way, but nothing reads that answer. networkLine asks
// whether the sandbox is running and reports the next boot when it is not.
// networkChange and keepsPosture are reached only for a sandbox that is
// running. The next run finds the dead sandbox not running and removes it,
// and hull's Remove takes the gateway down once `hull rm` succeeds. When a
// stop or a removal fails and leaves the gateway up, hull's Run takes it down
// once the sandbox boots on another network, so a gateway still up under a
// running shared sandbox is one it is behind.
//
// Load also fills recordedNet from a legacy sandbox's persisted runtime
// configuration. If neither source answers, this cannot infer a posture.
// No runtime, no answer: there is no sandbox to ask about.
func (c *Config) runningNet() Network {
	if c.Runtime == nil {
		return ""
	}
	if c.recordedNet != NetShared {
		return c.recordedNet
	}
	checker, ok := c.Runtime.(runtime.NetworkChecker)
	if ok && checker.NetworkStale(c.VMName, c.hypervisor(), NetShared.RuntimeNet(), runtime.Egress{}) {
		return NetIsolated
	}
	return NetShared
}

// networkLine is the NETWORK row: the posture the running sandbox has, and the
// one its next boot gets when the two differ.
//
// c.Network alone is the next boot. Printed on its own over a sandbox a policy
// isolated and that policy since detached, it told a reader the sandbox was on
// the shared network while its isolated gateway was still up (#368). The other
// way round, a policy attached since a shared boot, it claimed a boundary the
// running sandbox does not have.
//
// The record is checked before the runtime is asked whether the sandbox is up,
// so a run with nothing recorded costs no extra call. A sandbox that is stopped,
// or that brig cannot see, has only the next boot to report.
func (c *Config) networkLine() string {
	if c.netRecovery == networkUnknown {
		reason := "runtime inspection unavailable"
		if c.netInspectErr != nil {
			reason = c.netInspectErr.Error()
		}
		line := "unknown (no recorded posture; " + reason + ")"
		if c.netExplicit {
			line += fmt.Sprintf("; %s from its next boot", c.Network)
		}
		return line
	}
	line := c.Network.Line()
	if c.Network == NetShared && c.netFallback != "" {
		line += "; " + c.netFallback + " does not support isolated networking"
	}
	// A Config built without a settings lookup has nothing to resolve, and
	// hypervisor reads that lookup. info always comes from Load, which sets it.
	hv := ""
	if c.env.get != nil {
		hv = c.hypervisor()
	}
	if msg := c.profileIsolationOnFallback(hv); msg != "" {
		line += "; " + msg
	}
	now := c.runningNet()
	if now == "" || now == c.Network {
		return line
	}
	if up, err := c.Runtime.Running(c.VMName); err != nil || !up {
		return line
	}
	return fmt.Sprintf("%s; %s from its next boot", now.Line(), c.Network)
}

// networkChange is the first half of the warning printed when a running
// sandbox is restarted because its network is stale: which posture it is
// leaving and which it is going to, when that is the change.
//
// That is only known when a posture was recorded at the sandbox's boot and
// this run asked for a different one. A flagless verb takes the recorded
// posture, so a posture change here was named on this line or in the setting,
// and the warning says which. Anything else -- a policy attached or detached
// since the boot, or a sandbox booted before postures were recorded -- keeps
// the general wording.
//
// The posture it is leaving is the one it runs with, not the record. A policy
// isolated the sandbox in #368 and was then detached, and the line said the
// sandbox was started shared.
//
// When the posture it runs with is the one asked for, there is no posture
// change to name. If its rules are stale, a policy was detached and the
// general wording is the change. If they are not, see keepsPosture. The
// general wording there claimed a policy change that did not happen. The
// line names the policy only when one is attached, the same test Load narrows
// on: an older release that booted the sandbox --network isolated leaves the
// same isolated gateway over a shared record, and no policy touched it.
func (c *Config) networkChange() string {
	change, _ := c.postureChange()
	return change
}

// postureChange is networkChange, and whether keepsPosture holds, from one
// look at the running sandbox. On hvi each look dials the sandbox's gateway,
// and one restart warning needs both answers.
func (c *Config) postureChange() (string, bool) {
	now := c.askedNetwork
	if c.netRecovery == networkUnknown && c.netExplicit {
		return fmt.Sprintf("this sandbox's previous network is unknown and %s asks for %s",
			c.networkSource, now), false
	}
	if !c.postureChanged() {
		return generalNetworkChange, false
	}
	was := c.runningNet()
	if was == "" {
		was = c.recordedNet
	}
	if c.keepsPosture(was) {
		if c.Egress.Default != "" {
			return fmt.Sprintf("this sandbox is %s only because a policy narrowed it, and %s "+
				"asks for %s as the posture it keeps", was, c.networkSource, now), true
		}
		return fmt.Sprintf("this sandbox is %s but its record says %s, and %s "+
			"asks for %s as the posture it keeps", was, c.recordedNet, c.networkSource, now), true
	}
	if was == now {
		return generalNetworkChange, false
	}
	return fmt.Sprintf("this sandbox was started with the %s posture and %s asks for %s",
		was, c.networkSource, now), false
}

// keepsPosture reports whether this run asks for the posture the running
// sandbox has, under the rules it has, while the record names another. That is
// a sandbox isolated over a shared record, now asked for isolated. Nothing it
// runs with changes. It is restarted only so that its boot records isolated as
// the posture it keeps. running is what runningNet said.
func (c *Config) keepsPosture(running Network) bool {
	return c.postureChanged() && running == c.askedNetwork && !c.networkStale()
}

// networkRestart is the whole warning printed when a running sandbox is
// restarted for its network. The reason is that rules are fixed at boot,
// except where keepsPosture holds: there the rules stay the same, and the
// reason is the record.
func (c *Config) networkRestart() string {
	change, keeps := c.postureChange()
	reason := "rules are fixed when a sandbox boots, so brig restarts it"
	if keeps {
		reason = "its network and rules stay the same. brig restarts it to record that posture"
	}
	return notice.New(change).Note("%s", reason).Note("%s", disconnects).String()
}

// disconnects is the note on every restart of a running sandbox. It is word
// for word the same in each, because test/conformance reads a restart out of
// brig's output by it.
const disconnects = "any other session using this sandbox will be disconnected"

// generalNetworkChange is the restart warning when no change of posture can be
// named.
const generalNetworkChange = "this sandbox is running under a different network policy " +
	"than the one that applies now"

// mergePublications is what a sandbox will be offering: everything it already
// publishes, with what this command line asked for laid over it.
//
// Laid over rather than appended. Two publications on one host port are one
// host listener, so `--publish 8080:3000` on a sandbox already publishing 8080
// to port 80 moves that listener rather than asking for a second one. The
// same holds when either is on 0.0.0.0; see runtime.Publication.Overlaps.
func mergePublications(have, asked []runtime.Publication) []runtime.Publication {
	out := append([]runtime.Publication(nil), have...)
	for _, p := range asked {
		replaced := false
		for i, q := range out {
			if q.Overlaps(p) {
				out[i], replaced = p, true
				break
			}
		}
		if !replaced {
			out = append(out, p)
		}
	}
	return out
}

// publicationLines is the PORTS row's value: every published port, one per
// line, with the loopback default and anything wider said out loud.
func publicationLines(ps []runtime.Publication) []string {
	lines := make([]string, 0, len(ps))
	for _, p := range ps {
		lines = append(lines, p.Line())
	}
	return lines
}
