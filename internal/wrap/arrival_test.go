package wrap

import (
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/runtime"
)

// tpRules is a rule set a moved session carries.
var tpRules = policy.Egress{Default: "deny", Allow: []policy.Rule{{Host: "api.anthropic.com"}}}

// A session moved here keeps the rules its gateway enforced on the source
// when this host binds no policy to it: the guest is still behind them, and
// resolving this host's files would read as no rules at all. The posture
// goes with the rules.
func TestAnArrivalCarriesItsRulesWhenThisHostBindsNone(t *testing.T) {
	teleportState(t)
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	p := teleportProfile(t)
	if err := writeArrivals(map[string]Arrival{"brig-tp": {Ref: "tp", TxID: "tx1", From: "me@src",
		Network: "isolated", Slot: 3, Egress: tpRules, Policies: []string{"anthropic-only"},
		State: arrivalCommitted}}); err != nil {
		t.Fatal(err)
	}
	c := loadTeleport(t, p, Options{}, nil)
	if c.Egress.Default != "deny" || len(c.Egress.Allow) != 1 || c.Network != NetIsolated {
		t.Errorf("the carried rules did not stand: %+v on %s", c.Egress, c.Network)
	}
	if len(c.Policies) != 1 || !strings.Contains(c.Policies[0], "carried from me@src") {
		t.Errorf("POLICY names %v", c.Policies)
	}
	// Another session's arrival is not this one's.
	other := loadTeleport(t, p, Options{Name: "x"}, nil)
	if other.Egress.Default != "" {
		t.Errorf("a session that did not arrive took the carried rules: %+v", other.Egress)
	}
}

// The destination files a moved session before hull resumes it, and
// refuses one it cannot keep apart from what it already has: a session of
// that ref, a sandbox of that name, another move arriving on it, a name this
// host derives differently, and rules on a network that cannot hold them.
func TestPrepareArrivalRefusesWhatItCannotKeepApart(t *testing.T) {
	teleportState(t)
	p := teleportProfile(t)
	spec := ArrivalSpec{Sandbox: "brig-tp", TxID: "tx1", From: "me@src", Network: "none"}

	if _, err := PrepareArrival(p, "", spec, &guestRuntime{exists: true}); err == nil ||
		!strings.Contains(err.Error(), "already has a sandbox brig-tp") {
		t.Errorf("an existing sandbox: %v", err)
	}
	if _, err := PrepareArrival(p, "", spec, unaskableRuntime{&guestRuntime{}}); err == nil {
		t.Errorf("a runtime that cannot say what exists was trusted")
	}
	renamed := spec
	renamed.Sandbox = "brig-elsewhere"
	if _, err := PrepareArrival(p, "", renamed, &guestRuntime{}); err == nil ||
		!strings.Contains(err.Error(), "BRIG_NAME") {
		t.Errorf("a name derived differently: %v", err)
	}
	ruled := spec
	ruled.Network, ruled.Egress = "shared", tpRules
	if _, err := PrepareArrival(p, "", ruled, &guestRuntime{}); err == nil ||
		!strings.Contains(err.Error(), "isolated") {
		t.Errorf("rules on a network that holds none: %v", err)
	}
	broken := spec
	broken.Network, broken.Egress = "isolated", policy.Egress{Default: "sometimes"}
	if _, err := PrepareArrival(p, "", broken, &guestRuntime{}); err == nil {
		t.Errorf("rules that do not read as a policy were taken")
	}

	if _, err := PrepareArrival(p, "", spec, &guestRuntime{}); err != nil {
		t.Fatal(err)
	}
	entry := readSessionIndex()["tp"]
	if entry.Sandbox != "brig-tp" || !entry.isTeleportable() || entry.away() {
		t.Errorf("the arrival filed %+v", entry)
	}
	if net, _ := runtime.BootedNet("brig-tp"); net != "none" {
		t.Errorf("posture %q", net)
	}
	// The same move asked again is answered again: a source that lost the
	// answer can repeat the question.
	if _, err := PrepareArrival(p, "", spec, &guestRuntime{}); err != nil {
		t.Errorf("the same move asked twice: %v", err)
	}
	second := spec
	second.TxID = "tx2"
	if _, err := PrepareArrival(p, "", second, &guestRuntime{}); err == nil ||
		!strings.Contains(err.Error(), "tx1") {
		t.Errorf("a second move onto an arriving one: %v", err)
	}
	if err := writeArrivals(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareArrival(p, "", spec, &guestRuntime{}); err == nil ||
		!strings.Contains(err.Error(), "already has a session tp") {
		t.Errorf("a ref this host already files: %v", err)
	}
}

// A session coming back to a host it left finds that host's record of the
// move away: an entry that says the session runs on another host. That entry
// does not refuse the arrival, whichever host it names, since the source
// names itself by hostname and may have moved the session on since. The
// arrival keeps it, an abort puts it back, as the session still runs there,
// and the commit drops it. A move out of this host that has not finished
// still refuses, and so does the fenced copy while hull here still has it.
func TestAnArrivalTakesTheRecordOfASessionThatRunsElsewhere(t *testing.T) {
	teleportState(t)
	p := teleportProfile(t)
	away := sessionEntry{Sandbox: "brig-tp", Teleportable: true, Host: "me@rp2", RemoteStore: "/s", TxID: "tx0"}
	if err := writeSessionIndex(map[string]sessionEntry{"tp": away}); err != nil {
		t.Fatal(err)
	}
	spec := ArrivalSpec{Sandbox: "brig-tp", TxID: "tx1", From: "me@elsewhere", Network: "none"}

	if _, err := PrepareArrival(p, "", spec, &guestRuntime{exists: true}); err == nil ||
		!strings.Contains(err.Error(), "already has a sandbox brig-tp") {
		t.Errorf("the fenced copy still here: %v", err)
	}
	if _, err := PrepareArrival(p, "", spec, &guestRuntime{}); err != nil {
		t.Fatalf("the session coming back was refused: %v", err)
	}
	if e := readSessionIndex()["tp"]; e.away() || e.Sandbox != "brig-tp" || e.TxID != "" {
		t.Errorf("the arrival filed %+v", e)
	}
	if a := readArrivals()["brig-tp"]; a.Replaced == nil || *a.Replaced != away {
		t.Errorf("the arrival kept %+v of the entry it replaced", a.Replaced)
	}
	// The entry named rp2 and the move came from elsewhere: the source is
	// told what was replaced, in case it was another session.
	if r, ok := ArrivalReplacedElsewhere("brig-tp", "tx1"); !ok || r.Host != "me@rp2" || r.TxID != "tx0" {
		t.Errorf("the record replaced from another host: %+v, %v", r, ok)
	}
	// The same move asked again keeps what the first ask replaced.
	if _, err := PrepareArrival(p, "", spec, &guestRuntime{}); err != nil {
		t.Fatal(err)
	}
	if a := readArrivals()["brig-tp"]; a.Replaced == nil || *a.Replaced != away {
		t.Errorf("a repeated ask lost the replaced entry: %+v", a.Replaced)
	}
	if err := AbortArrival("brig-tp", "tx1", &guestRuntime{}); err != nil {
		t.Fatal(err)
	}
	if e, ok := readSessionIndex()["tp"]; !ok || e != away {
		t.Errorf("the abort left %+v (%v), not the record of where the session runs", e, ok)
	}

	second := spec
	second.TxID = "tx2"
	if _, err := PrepareArrival(p, "", second, &guestRuntime{}); err != nil {
		t.Fatal(err)
	}
	if err := CommitArrival("brig-tp", "tx2"); err != nil {
		t.Fatal(err)
	}
	if a := readArrivals()["brig-tp"]; a.Replaced != nil || a.State != arrivalCommitted {
		t.Errorf("the commit kept %+v", a)
	}
	if e := readSessionIndex()["tp"]; e.away() {
		t.Errorf("the committed arrival left the session recorded away: %+v", e)
	}

	// The session left again, and its committed arrival stayed behind, as
	// `hull rm` of the fenced copy leaves it. Neither refuses the next move
	// back.
	again := sessionEntry{Sandbox: "brig-tp", Teleportable: true, Host: "me@rp3", TxID: "tx3"}
	if err := writeSessionIndex(map[string]sessionEntry{"tp": again}); err != nil {
		t.Fatal(err)
	}
	third := spec
	third.TxID = "tx4"
	if _, err := PrepareArrival(p, "", third, &guestRuntime{}); err != nil {
		t.Errorf("a stale committed arrival refused the move back: %v", err)
	}

	// An entry away under another sandbox name, as after BRIG_NAME changed,
	// is another session, and refuses.
	if err := writeArrivals(nil); err != nil {
		t.Fatal(err)
	}
	other := sessionEntry{Sandbox: "brig-other", Teleportable: true, Host: "me@rp2", TxID: "tx7"}
	if err := writeSessionIndex(map[string]sessionEntry{"tp": other}); err != nil {
		t.Fatal(err)
	}
	seventh := spec
	seventh.TxID = "tx8"
	if _, err := PrepareArrival(p, "", seventh, &guestRuntime{}); err == nil ||
		!strings.Contains(err.Error(), "already has a session") {
		t.Errorf("an arrival onto an away entry of another sandbox: %v", err)
	}
	if e := readSessionIndex()["tp"]; e != other {
		t.Errorf("the refused arrival changed the entry to %+v", e)
	}

	// A move out of here whose end is not known refuses.
	if err := writeArrivals(nil); err != nil {
		t.Fatal(err)
	}
	pending := sessionEntry{Sandbox: "brig-tp", Teleportable: true, PendingHost: "me@rp2", TxID: "tx5"}
	if err := writeSessionIndex(map[string]sessionEntry{"tp": pending}); err != nil {
		t.Fatal(err)
	}
	fifth := spec
	fifth.TxID = "tx6"
	if _, err := PrepareArrival(p, "", fifth, &guestRuntime{}); err == nil ||
		!strings.Contains(err.Error(), "has not finished") || !strings.Contains(err.Error(), "--status") {
		t.Errorf("an arrival onto a move out of here: %v", err)
	}
}

// The source of a move back is told about the record it replaced only when
// that record named another host: from the host it names, the move is the
// session coming back, however the two spell the host.
func TestAnArrivalSaysOnlyWhenItReplacedARecordOfAnotherHost(t *testing.T) {
	teleportState(t)
	p := teleportProfile(t)
	for _, c := range []struct {
		host, from string
		said       bool
	}{
		{"me@rp2", "me@rp2", false},
		{"rp2.local", "pmoust@rp2", false},
		{"me@RP2", "me@rp2.", false},
		{"me@rp3", "me@rp2", true},
		{"me@100.64.0.2", "me@100.64.0.3", true},
		{"me@rp2", "", true},
	} {
		if err := writeArrivals(nil); err != nil {
			t.Fatal(err)
		}
		away := sessionEntry{Sandbox: "brig-tp", Teleportable: true, Host: c.host, TxID: "tx0"}
		if err := writeSessionIndex(map[string]sessionEntry{"tp": away}); err != nil {
			t.Fatal(err)
		}
		spec := ArrivalSpec{Sandbox: "brig-tp", TxID: "tx1", From: c.from, Network: "none"}
		if _, err := PrepareArrival(p, "", spec, &guestRuntime{}); err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		r, said := ArrivalReplacedElsewhere("brig-tp", "tx1")
		if said != c.said || (said && r.Host != c.host) {
			t.Errorf("entry on %s, move from %q: said %v (%+v), want %v", c.host, c.from, said, r, c.said)
		}
	}
	if _, said := ArrivalReplacedElsewhere("brig-tp", "tx9"); said {
		t.Error("another move's arrival said it replaced a record")
	}
}

// An offline session under a policy moves with its rules and no gateway to
// hold them: it has no network for them to act on. The brig here then
// resolves the carried rules for it, and keeps it offline.
func TestAnOfflineArrivalKeepsItsRulesWithNoGateway(t *testing.T) {
	teleportState(t)
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	p := teleportProfile(t)
	spec := ArrivalSpec{Sandbox: "brig-tp", TxID: "tx1", From: "me@src", Network: "none", Egress: tpRules}
	sock, err := PrepareArrival(p, "", spec, &guestRuntime{})
	if err != nil || sock != "" {
		t.Fatalf("an offline arrival under a policy: %q, %v", sock, err)
	}
	c := loadTeleport(t, p, Options{}, nil)
	if c.Egress.Default != "deny" || c.Network != NetOffline {
		t.Errorf("the offline arrival resolves %+v on %s", c.Egress, c.Network)
	}
}

// An abort gives back the network and the filing, but never while hull here
// runs the guest: the move reached this host, and the session is here.
func TestAbortArrivalLeavesARunningGuestItsNetwork(t *testing.T) {
	teleportState(t)
	p := teleportProfile(t)
	spec := ArrivalSpec{Sandbox: "brig-tp", TxID: "tx1", From: "me@src", Network: "none"}
	if _, err := PrepareArrival(p, "", spec, &guestRuntime{}); err != nil {
		t.Fatal(err)
	}
	if err := AbortArrival("brig-tp", "tx2", &guestRuntime{}); err == nil {
		t.Errorf("an abort of another move was taken")
	}
	if err := AbortArrival("brig-tp", "tx1", &guestRuntime{running: true}); err == nil {
		t.Errorf("an abort took the network of a guest that runs here")
	}
	if net, _ := runtime.BootedNet("brig-tp"); net != "none" {
		t.Errorf("a refused abort released the posture")
	}
	if err := AbortArrival("brig-tp", "tx1", &guestRuntime{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := readSessionIndex()["tp"]; ok {
		t.Errorf("an aborted arrival left the session filed")
	}
	if len(readArrivals()) != 0 {
		t.Errorf("an aborted arrival left its record")
	}
	if net, _ := runtime.BootedNet("brig-tp"); net != "" {
		t.Errorf("an aborted arrival left posture %q", net)
	}
}

// A move that has not finished here holds the session back, until hull's
// own record says it restored the guest under this move: then the source
// only lost the answer, and the session is here to use.
func TestAnUnfinishedArrivalIsRefusedUntilHullSaysTheGuestRuns(t *testing.T) {
	teleportState(t)
	p := teleportProfile(t)
	if _, err := PrepareArrival(p, "", ArrivalSpec{Sandbox: "brig-tp", TxID: "tx1", From: "me@src",
		Network: "none"}, &guestRuntime{}); err != nil {
		t.Fatal(err)
	}
	rt := &guestRuntime{running: true}
	c := loadTeleport(t, p, Options{}, rt)
	err := c.EnsureRunning(creds.Set{})
	if err == nil || !strings.Contains(err.Error(), "has not finished") ||
		!strings.Contains(err.Error(), "brig teleport tp --status") {
		t.Fatalf("an unfinished arrival: %v", err)
	}
	if rt.boots+rt.stops+rt.removes+len(rt.execs) != 0 {
		t.Errorf("a refused arrival touched the sandbox: %+v", rt)
	}
	rt.migration = runtime.Migration{State: "restored", TxID: "other"}
	if err := c.sessionRefusal(); err == nil {
		t.Errorf("hull's record of another move finished this one")
	}
	rt.migration = runtime.Migration{State: "restored", TxID: "tx1"}
	if err := c.sessionRefusal(); err != nil {
		t.Errorf("hull restored the guest under this move, and the session was still refused: %v", err)
	}
	if err := CommitArrival("brig-tp", "tx1"); err != nil {
		t.Fatal(err)
	}
	rt.migration = runtime.Migration{}
	if err := loadTeleport(t, p, Options{}, rt).sessionRefusal(); err != nil {
		t.Errorf("a committed arrival was refused: %v", err)
	}
}

// UnfinishedArrival answers from arrivals.json as it is when asked, for
// the ref and sandbox the arrival names, until the source commits the move
// or hull here restored the guest under it. A committed arrival is not one.
func TestUnfinishedArrivalReadsTheRecordAsItIsNow(t *testing.T) {
	teleportState(t)
	p := teleportProfile(t)
	rt := &guestRuntime{}
	if _, ok := UnfinishedArrival("tp", "brig-tp", rt); ok {
		t.Fatalf("no arrival, and one was reported")
	}
	if _, err := PrepareArrival(p, "", ArrivalSpec{Sandbox: "brig-tp", TxID: "tx1", From: "me@src",
		Network: "none"}, rt); err != nil {
		t.Fatal(err)
	}
	a, ok := UnfinishedArrival("tp", "brig-tp", rt)
	if !ok || a.From != "me@src" || a.TxID != "tx1" {
		t.Errorf("a prepared arrival: %+v, %v", a, ok)
	}
	if _, ok := UnfinishedArrival("other", "brig-tp", rt); ok {
		t.Errorf("an arrival of tp was reported for another ref")
	}
	rt.migration = runtime.Migration{State: "restored", TxID: "tx1"}
	if _, ok := UnfinishedArrival("tp", "brig-tp", rt); ok {
		t.Errorf("hull restored the guest under this move, and the arrival was still unfinished")
	}
	rt.migration = runtime.Migration{}
	if err := CommitArrival("brig-tp", "tx1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := UnfinishedArrival("tp", "brig-tp", rt); ok {
		t.Errorf("a committed arrival was reported unfinished")
	}
}

// An exec into a session moved here carries only what this host has. A
// secret this host lacks is not a refusal, as it is on a boot, and it is
// not fetched from anywhere: the exec runs without it and says why. The same
// profile on a session that did not arrive still fails closed.
func TestAnArrivedSessionRunsWithoutASecretThisHostLacks(t *testing.T) {
	body := "secrets:\n  - gh_token\nenv:\n  - name: GH\n    ref: secrets.gh_token\n"
	c := bindingConfig(t, body)
	c.VMName = "brig-claude-code"
	c.OpenStore = func() (creds.SecretReader, error) { return fakeStore{}, nil }
	if _, err := c.BuildEnv(); err == nil {
		t.Fatal("a missing secret did not fail an ordinary session")
	}
	c.arrival = &Arrival{From: "me@src", TxID: "tx1"}
	var warned strings.Builder
	c.Err = &warned
	set, err := c.BuildEnv()
	if err != nil {
		t.Fatalf("an arrived session refused over a secret this host lacks: %v", err)
	}
	for _, v := range set.Vars {
		if v.Name == "GH" {
			t.Errorf("GH was bound with no value to give: %+v", v)
		}
	}
	for _, want := range []string{"moved here from me@src", "gh_token", "asks no other host"} {
		if !strings.Contains(warned.String(), want) {
			t.Errorf("the warning does not say %q: %s", want, warned.String())
		}
	}
	c.OpenStore = func() (creds.SecretReader, error) { return fakeStore{"gh_token": "ghp_here"}, nil }
	set, err = c.BuildEnv()
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := findVar(set, "GH"); !ok || v.Value != "ghp_here" {
		t.Errorf("this host's own value was not handed in: %+v", set.Vars)
	}
}

func findVar(set creds.Set, name string) (runtime.Var, bool) {
	for _, v := range set.Vars {
		if v.Name == name {
			return v, true
		}
	}
	return runtime.Var{}, false
}
