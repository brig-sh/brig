package wrap

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/notice"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/session"
)

// The destination's half of a move. Before hull restores and resumes a moved
// guest here, `brig teleport-receive` gives it the network it left and files
// the session, so the brig here can reach it the way it reaches any of its
// own: `brig sh <ref>` on this host, or over ssh from the source.
//
// What the session was under on the source comes with it, recorded here in
// arrivals.json beside the session index: the egress rules its gateway
// enforced, by value, and the names of the policies they came from. The
// policies themselves stay on the source. This host's own policy files say
// nothing about a session started elsewhere, and resolving them would put the
// guest under no rules at all, or under rules that differ from the ones its
// gateway enforces, which a teleportable session refuses as a change it would
// have to recreate the sandbox to apply. So the carried rules stand in when
// this host binds none to the session, and a binding here that differs is
// refused as any changed policy is.
//
// A file of its own, not fields on the session index, because an older
// release rewrites that index and drops fields it does not know, and losing
// these would lose the rules.

// arrivalsName is the file, inside stateDir.
const arrivalsName = "arrivals.json"

// The states of an arrival. Prepared is taken before hull is asked: the
// network is up and the session filed, and the guest may not exist here yet.
// Committed is a move the source saw finish.
const (
	arrivalPrepared  = "prepared"
	arrivalCommitted = "committed"
)

// Arrival is what this host keeps about a session moved here.
type Arrival struct {
	Ref      string        `json:"ref"`
	TxID     string        `json:"txid"`
	From     string        `json:"from"`
	Network  string        `json:"network"`
	Slot     int           `json:"slot"`
	Egress   policy.Egress `json:"egress"`
	Policies []string      `json:"policies,omitempty"`
	State    string        `json:"state"`
}

func readArrivals() map[string]Arrival         { return readIndex[Arrival](arrivalsName) }
func writeArrivals(a map[string]Arrival) error { return writeIndex(arrivalsName, a) }

// ArrivalSpec is what the source sends with a move: the session, the move,
// and the network and rules the guest had.
type ArrivalSpec struct {
	Sandbox  string
	TxID     string
	From     string
	Network  string
	Slot     int
	Egress   policy.Egress
	Policies []string
}

// PrepareArrival takes this host's half of a move before hull resumes the
// guest here: it checks the session and the sandbox name are free, gives the
// sandbox the network it left with the rules it was under, and files the
// session and the arrival. It returns the gateway's control socket, or "" for
// a guest with no network. A second call for the same move does the same
// again and is not refused, so a source that lost the answer can ask again.
func PrepareArrival(t profile.Profile, name string, spec ArrivalSpec, rt runtime.Runtime) (string, error) {
	env := NewEnv(t.Name, os.LookupEnv)
	slug, _, vmName, err := sessionNames(t, env, name)
	if err != nil {
		return "", err
	}
	if vmName != spec.Sandbox {
		return "", fmt.Errorf("the session arrives as sandbox %s, and this host names it %s; "+
			"unset BRIG_NAME here, or the move cannot be reached here afterwards", spec.Sandbox, vmName)
	}
	if spec.TxID == "" {
		return "", fmt.Errorf("an arrival needs the move's txid")
	}
	if spec.Egress.Default != "" {
		doc := policy.Policy{APIVersion: policy.APIVersion, Name: "carried", Egress: spec.Egress}
		if err := doc.Validate(); err != nil {
			return "", fmt.Errorf("the rules the session carries do not read as a policy: %w", err)
		}
	}
	if spec.Egress.Default != "" && spec.Network != "isolated" {
		return "", fmt.Errorf("the session carries egress rules on the %s network, and only an isolated "+
			"network holds rules for one sandbox", spec.Network)
	}
	key := sessionKey(t.Name, slug)
	arrivals := readArrivals()
	if old, ok := arrivals[vmName]; ok && old.TxID != spec.TxID {
		return "", fmt.Errorf("move %s of %s is already arriving here; it has to finish or be "+
			"called off from %s first", old.TxID, key, old.From)
	}
	_, again := arrivals[vmName]
	if !again {
		if entry, ok := readSessionIndex()[key]; ok {
			return "", fmt.Errorf("this host already has a session %s, on sandbox %s; remove it with "+
				"`brig rm %s` here before moving another onto it", key, entry.Sandbox, key)
		}
		if exister, ok := rt.(runtime.Exister); ok {
			exists, err := exister.Exists(vmName)
			if err != nil {
				return "", fmt.Errorf("cannot tell whether this host already has a sandbox %s: %w", vmName, err)
			}
			if exists {
				return "", fmt.Errorf("this host already has a sandbox %s", vmName)
			}
		} else {
			return "", fmt.Errorf("the runtime here cannot say whether a sandbox %s exists", vmName)
		}
	}
	sock, err := runtime.ReceiveNetwork(rt.Bin(), vmName, spec.Network, spec.Slot, runtimeEgress(spec.Egress))
	if err != nil {
		return "", err
	}
	arrivals[vmName] = Arrival{Ref: key, TxID: spec.TxID, From: spec.From, Network: spec.Network,
		Slot: spec.Slot, Egress: spec.Egress, Policies: spec.Policies, State: arrivalPrepared}
	if err := writeArrivals(arrivals); err != nil {
		runtime.ReleaseReceivedNetwork(vmName)
		return "", err
	}
	index := readSessionIndex()
	index[key] = sessionEntry{Sandbox: vmName, Teleportable: true}
	if err := writeSessionIndex(index); err != nil {
		return "", err
	}
	return sock, nil
}

// CommitArrival marks the move txid as one the source saw finish.
func CommitArrival(sandbox, txid string) error {
	arrivals := readArrivals()
	a, ok := arrivals[sandbox]
	if !ok || a.TxID != txid {
		return fmt.Errorf("no move %s of %s is arriving here", txid, sandbox)
	}
	if a.State == arrivalCommitted {
		return nil
	}
	a.State = arrivalCommitted
	arrivals[sandbox] = a
	return writeArrivals(arrivals)
}

// AbortArrival gives back what PrepareArrival took, for a move hull called
// off. It refuses while hull here runs the guest: the move reached the
// destination, and taking its network away would cut a running session off.
func AbortArrival(sandbox, txid string, rt runtime.Runtime) error {
	arrivals := readArrivals()
	a, ok := arrivals[sandbox]
	if !ok {
		return nil
	}
	if a.TxID != txid {
		return fmt.Errorf("the move arriving here as %s is %s, not %s", sandbox, a.TxID, txid)
	}
	running, err := rt.Running(sandbox)
	if err != nil {
		return fmt.Errorf("cannot tell whether %s runs here, so its network stays: %w", sandbox, err)
	}
	if running {
		return fmt.Errorf("%s runs on this host, so the move reached it; its network stays", sandbox)
	}
	runtime.ReleaseReceivedNetwork(sandbox)
	index := readSessionIndex()
	if entry, ok := index[a.Ref]; ok && entry.Sandbox == sandbox && !entry.away() {
		delete(index, a.Ref)
		if err := writeSessionIndex(index); err != nil {
			return err
		}
	}
	delete(arrivals, sandbox)
	return writeArrivals(arrivals)
}

// arrivalOf is the arrival of this session's sandbox, or nil when the session
// did not arrive here by a move.
func arrivalOf(ref, vmName string) *Arrival {
	a, ok := readArrivals()[vmName]
	if !ok || a.Ref != ref {
		return nil
	}
	return &a
}

// ArrivingSandboxes is every sandbox a move is bringing here and has not
// finished. `brig ls` keeps them out of its prune: hull here may not list a
// reserved or incoming instance yet, and pruning the session and the posture
// a receive filed would leave the guest, once it runs, with no record brig
// can reach it by.
func ArrivingSandboxes() []string {
	var out []string
	for name, a := range readArrivals() {
		if a.State != arrivalCommitted {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ForgetArrival is forgetArrival for `brig rm --all`, which removes
// sandboxes without a Config.
func ForgetArrival(vmName string) { forgetArrival(vmName) }

// forgetArrival drops the arrival of a removed sandbox, with the rest of its
// bookkeeping.
func forgetArrival(vmName string) {
	arrivals := readArrivals()
	if _, ok := arrivals[vmName]; !ok {
		return
	}
	delete(arrivals, vmName)
	_ = writeArrivals(arrivals)
}

// arrivalRefusal refuses a session whose move here has not finished. A
// prepared arrival whose guest hull here has restored, under this move's
// txid, has finished: the source lost the answer, and hull's record is the
// one that says where the guest runs. Anything else waits for the source.
func (c *Config) arrivalRefusal() error {
	a := c.arrival
	if a == nil || a.State == arrivalCommitted {
		return nil
	}
	if mi, ok := c.Runtime.(runtime.MigrationInspector); ok {
		if m, err := mi.Migration(c.VMName); err == nil && m.TxID == a.TxID && arrivedState(m.State) {
			return nil
		}
	}
	return fmt.Errorf("%s is arriving on this host from %s (move %s), and the move has not finished. "+
		"`brig teleport %s --status` on %s finishes it or says why it cannot", c.VMName, a.From, a.TxID,
		a.Ref, a.From)
}

// arrivedState reports whether hull's record of a guest on the destination
// says the move put it there and let it run.
func arrivedState(state string) bool {
	switch state {
	case "restored", runtime.MigrationMigrated:
		return true
	}
	return false
}

// carriedPolicies names the policies a moved session's rules came from, for
// the POLICY row, marked as carried: they are not files on this host.
func carriedPolicies(a *Arrival) []string {
	if len(a.Policies) == 0 {
		return []string{"carried from " + a.From}
	}
	out := make([]string, 0, len(a.Policies))
	for _, name := range a.Policies {
		out = append(out, name+" (carried from "+a.From+")")
	}
	return out
}

// resolveArrivedSecrets resolves what an exec into a session moved here can
// carry, from this host's store and environment and nowhere else.
//
// The session holds its credentials already: they are in the guest's memory,
// which is what moved. What an exec hands in is a fresh copy for the process
// it starts, and this host can only hand in what it has. So a secret this
// host does not have is not a refusal here, as it is on a boot: the exec runs
// without it, and says which ones and why. brig never asks the source, or any
// other host, for a value: a credential leaves a host only inside the guest's
// memory, over the ssh of the move.
func (c *Config) resolveArrivedSecrets(open func() (creds.SecretReader, error)) (creds.Resolution, error) {
	p := c.Profile
	p.Secrets = slices.Clone(p.Secrets)
	optional := false
	for i := range p.Secrets {
		p.Secrets[i].Required = &optional
	}
	res, err := creds.ResolveSecrets(p, c.VMName, open, os.LookupEnv)
	if err != nil {
		return res, err
	}
	var missing []string
	for _, d := range creds.Needed(p, os.LookupEnv) {
		if _, ok := res.Values[d.Name]; !ok {
			missing = append(missing, d.Name)
		}
	}
	if len(missing) > 0 {
		head := notice.Newf("%s was moved here from %s, and this host has no value for %s",
			c.VMName, c.arrival.From, strings.Join(missing, ", ")).
			Note("this exec carries none of them; the agent keeps the values it already holds in memory").
			Note("brig asks no other host for a credential. Store one here with `brig secret` to hand it in")
		res.Warnings = append([]string{head.String()}, res.Warnings...)
	}
	return res, nil
}

// ArrivingMove is the txid of the move arriving on this host as sandbox, or
// "" when none is. `brig teleport-probe` reports it, so a source asking again
// about its own move is told the sandbox here is its own.
func ArrivingMove(sandbox string) string {
	return readArrivals()[sandbox].TxID
}

// ArrivalReceive is PrepareArrival for a ref as typed, which is how the
// source names the session over ssh.
func ArrivalReceive(ref string, spec ArrivalSpec, rt runtime.Runtime) (string, error) {
	t, label, err := profileOfRef(ref)
	if err != nil {
		return "", err
	}
	return PrepareArrival(t, label, spec, rt)
}

// profileOfRef is the profile and label of a ref.
func profileOfRef(ref string) (profile.Profile, string, error) {
	r, err := session.ParseRef(ref)
	if err != nil {
		return profile.Profile{}, "", err
	}
	t, ok := profile.Lookup(r.Agent)
	if !ok {
		return profile.Profile{}, "", fmt.Errorf("this host has no agent %q, so it cannot run %s; "+
			"add the agent here first", r.Agent, ref)
	}
	return t, r.Label, nil
}
