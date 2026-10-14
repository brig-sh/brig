package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/wrap"
)

// --to, --status and --abort are brig's on the teleport line only. On the
// run line they are words an agent may have, so they reach the agent and
// brig says nothing about them.
func TestTheTeleportLineIsItsOwn(t *testing.T) {
	o, name, tail, err := parse("teleport", []string{"claude@x", "--to", "me@box", "--json"})
	if err != nil || o.teleport.to != "me@box" || !o.json || name != "claude" || len(tail) != 0 {
		t.Errorf("teleport line: %+v %q %q %v", o.teleport, name, tail, err)
	}
	if o, _, _, err := parse("teleport", []string{"--status", "claude"}); err != nil || !o.teleport.status {
		t.Errorf("--status before the ref: %+v %v", o.teleport, err)
	}
	o, _, tail, err = parse("run", []string{"codex", "--status", "--to", "x"})
	if err != nil || o.teleport.status || o.teleport.to != "" || strings.Join(tail, " ") != "--status --to x" {
		t.Errorf("the run line took the agent's --status: %+v, tail %q, %v", o.teleport, tail, err)
	}
	if _, _, _, err := parse("teleport", []string{"--image", "x", "claude"}); err == nil {
		t.Errorf("a run-line flag was read on the teleport line")
	}
	if got := helpFlagTakesValue("teleport", "--to"); !got {
		t.Errorf("help's scan does not know --to takes a value")
	}
}

// The teleport line asks one thing. None, two, or a destination ssh could
// read as an option is a usage error, exit 2, before anything is loaded.
func TestTeleportAsksOneThing(t *testing.T) {
	for _, o := range []teleportOptions{
		{},
		{to: "me@box", status: true},
		{status: true, abort: true},
		{to: "-oProxyCommand=sh@box"},
		{to: "box"},
	} {
		err := teleportCmd(nil, profile.Profile{}, "claude-code", o, false)
		if code := exitCode(err); code != exitUsage {
			t.Errorf("%+v exits %d, want %d: %v", o, code, exitUsage, err)
		}
	}
}

// The remote command line is parsed again by a shell on the other host, so
// every word must come back as it was, whatever it holds. Checked against a
// real sh.
func TestShellJoinRoundTripsThroughASh(t *testing.T) {
	words := []string{"sh", "claude@x", "-c", `echo "$HOME"; rm -rf / && $(id) ` + "`id`", "it's", "a b", "", "*", "~", "line\nbreak", `back\slash`}
	out, err := exec.Command("/bin/sh", "-c", "for a in "+shellJoin(words)+`; do printf '%s\0' "$a"; done`).Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if strings.Join(got, "|") != strings.Join(words, "|") {
		t.Errorf("a shell read back\n%q\nfrom\n%q", got, words)
	}
}

// The ssh line names the host and nothing else as a destination: ssh's
// options, `--`, the host, and the remote command as one quoted word. A
// terminal only when asked for, a password prompt refused only on brig's own
// calls, the store the session runs in handed to the brig there.
func TestSSHArgvNamesOnlyTheHost(t *testing.T) {
	t.Setenv(sshSetting, "")
	t.Setenv(remoteBrigSetting, "")
	t.Setenv(remoteEnvSetting, "")
	for _, c := range []struct {
		call remoteCall
		want []string
	}{
		{remoteCall{host: "me@box", args: []string{"sh", "claude", "-c", "echo hi"}},
			[]string{"ssh", "--", "me@box", "brig sh claude -c 'echo hi'"}},
		{remoteCall{host: "me@box", tty: true, args: []string{"sh", "claude"}},
			[]string{"ssh", "-t", "--", "me@box", "brig sh claude"}},
		{remoteCall{host: "me@box", batch: true, store: "/st/b x", args: []string{"teleport-probe", "--json"}},
			[]string{"ssh", "-o", "BatchMode=yes", "--", "me@box", "env 'BRIG_HULL_STORE_DIR=/st/b x' brig teleport-probe --json"}},
	} {
		got, err := sshArgv(c.call)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(got, "\x00") != strings.Join(c.want, "\x00") {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
	t.Setenv(sshSetting, "/tmp/fake-ssh")
	t.Setenv(remoteBrigSetting, "/opt/brig/bin/brig")
	t.Setenv(remoteEnvSetting, "BRIG_STATE_DIR=/s/b BRIG_GATEWAY_DIR=/g/b")
	got, _ := sshArgv(remoteCall{host: "me@box", args: []string{"info", "claude"}})
	want := []string{"/tmp/fake-ssh", "--", "me@box", "env BRIG_STATE_DIR=/s/b BRIG_GATEWAY_DIR=/g/b /opt/brig/bin/brig info claude"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("got %q, want %q", got, want)
	}
	for _, env := range []string{"X", "BAD-NAME=1", "A=1;B"} {
		t.Setenv(remoteEnvSetting, env)
		if _, err := sshArgv(remoteCall{host: "me@box", args: []string{"info"}}); err == nil && env != "A=1;B" {
			t.Errorf("%s=%q was accepted", remoteEnvSetting, env)
		}
	}
	t.Setenv(remoteEnvSetting, "")
	for _, host := range []string{"-oProxyCommand=x@h", "me@h;id", "me@h id", "h"} {
		if _, err := sshArgv(remoteCall{host: host}); exitCode(err) != exitUsage {
			t.Errorf("host %q: %v", host, err)
		}
	}
}

// movedHost is a scratch host whose index says claude's default session was
// moved to me@box. Runtime detection fails the test: a verb on a moved
// session must not ask this host's runtime anything. execRemote and
// attachRemote record the ssh line instead of running it.
func movedHost(t *testing.T, entry string) *[][]string {
	t.Helper()
	scratchHost(t)
	t.Setenv(sshSetting, "")
	t.Setenv(remoteBrigSetting, "")
	t.Setenv(remoteEnvSetting, "")
	index := `{"claude-code": ` + entry + `}`
	if err := os.WriteFile(filepath.Join(os.Getenv("BRIG_STATE_DIR"), "sessions.json"), []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
	prevDetect, prevExec, prevAttach := detectRuntimeFor, execRemote, attachRemote
	detectRuntimeFor = func(runtime.Preference) (runtime.Runtime, error) {
		t.Errorf("a verb on a moved session asked this host for its runtime")
		return nil, errors.New("no runtime here")
	}
	var calls [][]string
	execRemote = func(argv []string) error { calls = append(calls, argv); return nil }
	attachRemote = func(argv, _ []string) (int, error) { calls = append(calls, argv); return 3, nil }
	t.Cleanup(func() { detectRuntimeFor, execRemote, attachRemote = prevDetect, prevExec, prevAttach })
	return &calls
}

const movedEntry = `{"home": "", "sandbox": "brig-claude-code", "teleportable": true, "host": "me@box", "remoteStore": "/st/b", "txid": "0123456789abcdef0123456789abcdef"}`

// sh, run, stop, info and logs on a moved session are the same verb on its
// host, handed over through ssh with the line as typed and the store the
// session runs in. Nothing here is asked: not the runtime, and not the
// secret store, so no credential value reaches the ssh line.
func TestAMovedSessionsVerbsRunOnItsHost(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"sh", "claude", "-c", "echo $GH_TOKEN"}, "brig sh claude -c 'echo $GH_TOKEN'"},
		{[]string{"run", "claude", "-p", "fix it"}, "brig run claude -p 'fix it'"},
		{[]string{"claude", "-p", "hi"}, "brig run claude -p hi"},
		{[]string{"-q", "stop", "claude"}, "brig -q stop claude"},
		{[]string{"info", "claude", "--json"}, "brig info claude --json"},
		{[]string{"logs", "claude", "--tail", "5"}, "brig logs claude --tail 5"},
	} {
		calls := movedHost(t, movedEntry)
		t.Setenv("GH_TOKEN", "ghp_from_this_host")
		if _, err := captureStdout(t, func() error { return run(c.args) }); err != nil {
			t.Errorf("brig %s: %v", strings.Join(c.args, " "), err)
			continue
		}
		if len(*calls) != 1 {
			t.Errorf("brig %s: ssh ran %d times", strings.Join(c.args, " "), len(*calls))
			continue
		}
		want := []string{"ssh", "--", "me@box", "env BRIG_HULL_STORE_DIR=/st/b " + c.want}
		if got := (*calls)[0]; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("brig %s: ssh %q, want %q", strings.Join(c.args, " "), got, want)
		}
		if strings.Contains(strings.Join((*calls)[0], " "), "ghp_from_this_host") {
			t.Errorf("a credential value reached the ssh line")
		}
	}
}

// --json on a moved session's run is the Run line of the brig there, through
// a child, so its exit status is brig's and no second Run line is printed.
func TestAMovedSessionsJSONRunIsTheRemoteRunLine(t *testing.T) {
	calls := movedHost(t, movedEntry)
	out, err := captureStdout(t, func() error { return run([]string{"--json", "run", "claude"}) })
	if code := exitCode(err); code != 3 || len(*calls) != 1 || strings.Contains(out, `"Run"`) {
		t.Errorf("exit %d, %d calls, stdout %q", code, len(*calls), out)
	}
}

// The verbs that act on this host are refused for a moved session, naming
// the line that works, and a session whose move has not finished answers
// only to teleport: nothing is sent anywhere.
func TestAMovedSessionRefusesWhatActsOnThisHost(t *testing.T) {
	for _, c := range []struct {
		entry string
		args  []string
		want  string
	}{
		{movedEntry, []string{"plan", "claude"}, "ssh me@box brig plan claude-code"},
		{movedEntry, []string{"network", "ls", "claude"}, "runs on me@box"},
		{`{"home": "", "sandbox": "brig-claude-code", "teleportable": true, "pendingHost": "me@box", "txid": "0123456789abcdef0123456789abcdef"}`,
			[]string{"sh", "claude"}, "brig teleport claude-code --status"},
	} {
		calls := movedHost(t, c.entry)
		_, err := captureStdout(t, func() error { return run(c.args) })
		if err == nil || !strings.Contains(err.Error(), c.want) || exitCode(err) != exitFailure {
			t.Errorf("brig %s: %v", strings.Join(c.args, " "), err)
		}
		if len(*calls) != 0 {
			t.Errorf("brig %s reached ssh", strings.Join(c.args, " "))
		}
	}
}

// probeFor is a probe answer from a destination that can take the session,
// for a case to spoil one field of.
func probeFor() probeReport {
	return probeReport{Format: probeFormat, Runtime: probeRuntime{Kind: "hull", Bin: "/opt/hull"},
		Hvi: probeTool{Path: "/opt/hvi"}, Egress: probeEgress{Enforced: true},
		Store: probeStore{Dir: "/st", FreeBytes: 1 << 40}, Agent: &probeAgent{Name: "claude-code", Known: true},
		Sandbox: &probeSandbox{Name: "brig-claude-code"}, Network: &probeNetwork{Net: "isolated", Slot: 3},
		Policy: &probePolicy{}}
}

// Every reason a destination cannot take the session is a refusal before
// anything is paused, and says which. A policy the destination cannot
// enforce is a capability, exit 7, the way a policy nothing enforces here
// is; the rest are 1.
func TestCheckProbeRefusesEachReason(t *testing.T) {
	p, _ := profile.Lookup("claude")
	cfg := &wrap.Config{Profile: p, VMName: "brig-claude-code"}
	ruled := wrap.MoveNetwork{Net: "isolated", Slot: 3, Egress: policy.Egress{Default: "deny"}}
	for _, c := range []struct {
		what  string
		spoil func(*probeReport)
		net   wrap.MoveNetwork
		code  int
		want  string
	}{
		{"no runtime", func(r *probeReport) { r.Runtime = probeRuntime{Error: "no runtime found"} }, ruled, exitFailure, "no runtime"},
		{"not hull", func(r *probeReport) { r.Runtime.Kind = "nerdctl" }, ruled, exitFailure, "BRIG_RUNTIME=hull"},
		{"no hvi", func(r *probeReport) { r.Hvi.Path = "" }, ruled, exitFailure, "no hvi"},
		{"no such agent", func(r *probeReport) { r.Agent.Known = false }, ruled, exitFailure, "no agent claude-code"},
		{"a policy it cannot enforce", func(r *probeReport) { r.Egress = probeEgress{Why: "no --egress-default"} }, ruled, exitCapability, "cannot enforce"},
		{"the name taken", func(r *probeReport) { r.Sandbox.Exists = true }, ruled, exitFailure, "already has a sandbox"},
		{"another move arriving", func(r *probeReport) { r.Sandbox.Arriving = "other" }, ruled, exitFailure, "arriving"},
		{"the address taken", func(r *probeReport) { r.Network.Holder = "brig-other" }, ruled, exitFailure, "brig-other"},
		{"no room", func(r *probeReport) { r.Store.FreeBytes = 0 }, wrap.MoveNetwork{Net: "none"}, exitFailure, "MiB free"},
		{"no word on its policy", func(r *probeReport) { r.Policy = nil }, ruled, exitFailure, "which policy"},
		{"its policy unread", func(r *probeReport) { r.Policy.Error = "a policy file does not parse" }, ruled, exitFailure, "does not parse"},
		{"other rules bound there", func(r *probeReport) {
			r.Policy = &probePolicy{Egress: policy.Egress{Default: "allow"}, Policies: []string{"open"}}
		}, ruled, exitFailure, "binds open to claude-code"},
		{"rules bound there, none here", func(r *probeReport) {
			r.Policy = &probePolicy{Egress: policy.Egress{Default: "deny"}, Policies: []string{"shut"}}
		}, wrap.MoveNetwork{Net: "isolated", Slot: 3}, exitFailure, "changed policy"},
	} {
		r := probeFor()
		c.spoil(&r)
		cfg.Mem = 1024
		err := checkProbe(cfg, r, "me@box", "claude-code", "tx", c.net)
		if err == nil || exitCode(err) != c.code || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: exit %d, %v", c.what, exitCode(err), err)
		}
	}
	// A policy on a destination that enforces one, and no policy on one that
	// cannot, both pass.
	if err := checkProbe(cfg, probeFor(), "me@box", "claude-code", "tx", ruled); err != nil {
		t.Errorf("a destination that can take it: %v", err)
	}
	r := probeFor()
	r.Egress = probeEgress{Why: "no --egress-default"}
	if err := checkProbe(cfg, r, "me@box", "claude-code", "tx", wrap.MoveNetwork{Net: "isolated", Slot: 3}); err != nil {
		t.Errorf("no policy, and the destination's gateway was asked for one: %v", err)
	}
	// The same rules bound there, in another order, are the rules it runs
	// under: the carried ones stand in for nothing, and the brig there reads
	// no change.
	two := wrap.MoveNetwork{Net: "isolated", Slot: 3, Egress: policy.Egress{Default: "deny",
		Allow: []policy.Rule{{Host: "a.example"}, {Host: "b.example"}}}}
	r = probeFor()
	r.Policy = &probePolicy{Egress: policy.Egress{Default: "deny",
		Allow: []policy.Rule{{Host: "b.example"}, {Host: "a.example"}}}, Policies: []string{"same"}}
	if err := checkProbe(cfg, r, "me@box", "claude-code", "tx", two); err != nil {
		t.Errorf("the same rules bound there: %v", err)
	}
	// An offline guest under a policy has no gateway on either host, so a
	// destination that cannot enforce rules can still take it.
	r = probeFor()
	r.Egress, r.Network = probeEgress{Why: "no --egress-default"}, nil
	if err := checkProbe(cfg, r, "me@box", "claude-code", "tx",
		wrap.MoveNetwork{Net: "none", Egress: policy.Egress{Default: "deny"}}); err != nil {
		t.Errorf("an offline guest under a policy: %v", err)
	}
	// The sandbox there is this move's own, asked again.
	r = probeFor()
	r.Sandbox.Exists, r.Sandbox.Arriving = true, "tx"
	if err := checkProbe(cfg, r, "me@box", "claude-code", "tx", ruled); err != nil {
		t.Errorf("this move's own arrival: %v", err)
	}
}

// The probe reports what the destination's own policy files bind to the
// session it is asked about, and the probe of a session with no binding
// reports none.
func TestTheProbeReportsThePolicyBoundThere(t *testing.T) {
	scratchHost(t)
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	writePolicyFile(t, dir, "no-net", noNetBody)
	loadTestProfiles(t)
	if err := attachPolicy([]string{"no-net", "claude-code", "-n", "mv"}); err != nil {
		t.Fatal(err)
	}
	for ref, want := range map[string]string{"claude-code@mv": "deny", "claude-code": ""} {
		var b bytes.Buffer
		if err := teleportProbeCmd(&b, []string{"--json", "--agent", "claude-code", "--ref", ref}); err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Data probeReport `json:"data"`
		}
		if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		p := doc.Data.Policy
		if p == nil || p.Error != "" || p.Egress.Default != want || (want != "" && len(p.Policies) != 1) {
			t.Errorf("%s: the probe reported %+v", ref, p)
		}
	}
}

// moveRuntime is hull for a teleportable session that can be moved: it runs,
// its record shows a block root with no share, and its move does what the
// case says, recording what it was asked.
type moveRuntime struct {
	runtime.Runtime
	gone            bool
	notTeleportable bool
	running         bool
	inspect         runtime.Migration
	// state is hull's move record after a move; moveErr what the move call
	// returns. dryErr refuses the dry run.
	state   string
	moveErr error
	dryErr  error
	// unread is a move after which hull's record does not read: Migrate
	// returns the zero status, and MigrateStatus an error.
	unread bool
	// abortErr is what an abort returns beside the aborted record: hull
	// records the abort, then fails to restore the guest here.
	abortErr error
	asked    []string
	specs    []runtime.MigrateSpec
	removed  int
	txid     string
}

func (r *moveRuntime) Kind() string                                { return "hull" }
func (r *moveRuntime) Bin() string                                 { return "hull" }
func (r *moveRuntime) Running(string) (bool, error)                { return r.running, nil }
func (r *moveRuntime) Exists(string) (bool, error)                 { return true, nil }
func (r *moveRuntime) SandboxNetwork(string) (string, error)       { return "none", nil }
func (r *moveRuntime) OwnsItsDisk(string) (bool, error)            { return !r.notTeleportable, nil }
func (r *moveRuntime) Migration(string) (runtime.Migration, error) { return r.inspect, nil }
func (r *moveRuntime) List() ([]runtime.Instance, error) {
	if r.gone {
		return nil, nil
	}
	return []runtime.Instance{{Name: "brig-claude-code", State: "running"}}, nil
}
func (r *moveRuntime) Stop(string) error   { return nil }
func (r *moveRuntime) Remove(string) error { r.removed++; return nil }
func (r *moveRuntime) Migrate(name, to string, spec runtime.MigrateSpec) (runtime.MoveStatus, error) {
	r.specs = append(r.specs, spec)
	if spec.DryRun {
		r.asked = append(r.asked, "dry-run "+to)
		return runtime.MoveStatus{}, r.dryErr
	}
	r.asked = append(r.asked, "migrate "+to)
	r.txid = spec.TxID
	if r.unread {
		return runtime.MoveStatus{}, r.moveErr
	}
	return runtime.MoveStatus{Read: true, TxID: spec.TxID, State: r.state, Peer: to}, r.moveErr
}
func (r *moveRuntime) MigrateStatus(string) (runtime.MoveStatus, error) {
	r.asked = append(r.asked, "status")
	if r.unread {
		return runtime.MoveStatus{}, errors.New("hull's move status of brig-claude-code did not read")
	}
	if r.state == "" {
		return runtime.MoveStatus{Read: true}, nil
	}
	return runtime.MoveStatus{Read: true, TxID: r.txid, State: r.state, Peer: "me@box"}, nil
}
func (r *moveRuntime) MigrateAbort(_, txid string) (runtime.MoveStatus, error) {
	r.asked = append(r.asked, "abort "+txid)
	r.state = runtime.MoveAborted
	return runtime.MoveStatus{Read: true, TxID: txid, State: r.state}, r.abortErr
}

// moveHost is a source host with claude's default session running as a
// teleportable session on rt, and a destination whose brig answers through
// outputRemote: the probe from probe, everything else with ok. Every remote
// line is kept.
func moveHost(t *testing.T, rt *moveRuntime, probe probeReport) *[][]string {
	t.Helper()
	scratchHost(t)
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	t.Setenv("BRIG_VERIFY", "off")
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	t.Setenv("BRIG_NETWORK", "")
	t.Setenv(sshSetting, "")
	t.Setenv(remoteBrigSetting, "")
	t.Setenv(remoteEnvSetting, "")
	index := `{"claude-code": {"home": "", "sandbox": "brig-claude-code", "teleportable": true}}`
	if err := os.WriteFile(filepath.Join(os.Getenv("BRIG_STATE_DIR"), "sessions.json"), []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RecordBootedNet("brig-claude-code", "none"); err != nil {
		t.Fatal(err)
	}
	prevDetect, prevOut := detectRuntimeFor, outputRemote
	detectRuntimeFor = func(runtime.Preference) (runtime.Runtime, error) { return rt, nil }
	var calls [][]string
	outputRemote = func(argv []string) ([]byte, error) {
		calls = append(calls, argv)
		var b bytes.Buffer
		if strings.Contains(argv[len(argv)-1], probeVerb) {
			_ = writeJSONDocument(&b, "TeleportProbe", probe)
		} else {
			_ = writeJSONDocument(&b, "TeleportReceive", map[string]string{"state": "ok", "socket": ""})
		}
		return b.Bytes(), nil
	}
	t.Cleanup(func() { detectRuntimeFor, outputRemote = prevDetect, prevOut })
	return &calls
}

func noNetProbe() probeReport {
	r := probeFor()
	r.Network = nil
	return r
}

func entryOf(t *testing.T) map[string]any {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join(os.Getenv("BRIG_STATE_DIR"), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index map[string]map[string]any
	if err := json.Unmarshal(blob, &index); err != nil {
		t.Fatal(err)
	}
	return index["claude-code"]
}

// A move that commits: the destination is probed, takes the network, hull
// dry-runs and moves with the store and hull the destination's brig drives,
// the destination is told it finished, and the session is recorded as
// running there. Every ssh line names the host given and no other.
func TestATeleportThatCommitsRecordsTheHost(t *testing.T) {
	rt := &moveRuntime{running: true, state: runtime.MoveCommitted}
	probe := noNetProbe()
	probe.Store.Named = "/st/b"
	calls := moveHost(t, rt, probe)
	out, err := captureStdout(t, func() error { return run([]string{"teleport", "claude", "--to", "me@box"}) })
	if err != nil {
		t.Fatalf("%v (%s)", err, out)
	}
	if !strings.Contains(out, "teleported claude-code to me@box") {
		t.Errorf("stdout %q", out)
	}
	if strings.Join(rt.asked, ",") != "dry-run me@box,migrate me@box" {
		t.Errorf("hull was asked %v", rt.asked)
	}
	if s := rt.specs[1]; s.RemoteStore != "/st/b" || s.RemoteHull != "/opt/hull" || len(s.TxID) != 32 {
		t.Errorf("the move was asked as %+v", s)
	}
	verbs := []string{}
	for _, argv := range *calls {
		if argv[len(argv)-2] != "me@box" || argv[len(argv)-3] != "--" {
			t.Errorf("an ssh line named something other than the host: %q", argv)
		}
		words := strings.Fields(argv[len(argv)-1])
		for i, w := range words {
			if w == "brig" && i+1 < len(words) {
				verbs = append(verbs, words[i+1])
			}
		}
	}
	if strings.Join(verbs, ",") != "teleport-probe,teleport-receive,teleport-receive" {
		t.Errorf("the destination was asked %v", verbs)
	}
	e := entryOf(t)
	if e["host"] != "me@box" || e["pendingHost"] != nil || e["remoteStore"] != "/st/b" || e["txid"] != rt.txid {
		t.Errorf("the record after the move: %v", e)
	}
	// From here sh goes there.
	prevExec := execRemote
	var sh [][]string
	execRemote = func(argv []string) error { sh = append(sh, argv); return nil }
	t.Cleanup(func() { execRemote = prevExec })
	if _, err := captureStdout(t, func() error { return run([]string{"sh", "claude"}) }); err != nil || len(sh) != 1 {
		t.Errorf("sh after the move: %v, %v", err, sh)
	}
}

// Refusals before anything is paused: a session that is not teleportable,
// and a destination that cannot enforce the session's policy, exit 7; a
// stopped sandbox exits 1. hull is never asked to move, and nothing is
// recorded.
func TestATeleportRefusedBeforeAnythingIsPaused(t *testing.T) {
	for _, c := range []struct {
		what  string
		rt    *moveRuntime
		probe func(*probeReport)
		code  int
	}{
		{"no sandbox for the ref", &moveRuntime{gone: true}, nil, exitNotFound},
		{"not teleportable", &moveRuntime{running: true, notTeleportable: true}, nil, exitCapability},
		{"not running", &moveRuntime{}, nil, exitFailure},
		{"parked by a checkpoint", &moveRuntime{running: true, inspect: runtime.Migration{State: "fenced"}}, nil, exitFailure},
		{"no hvi there", &moveRuntime{running: true}, func(r *probeReport) { r.Hvi.Path = "" }, exitFailure},
	} {
		probe := noNetProbe()
		if c.probe != nil {
			c.probe(&probe)
		}
		calls := moveHost(t, c.rt, probe)
		_, err := captureStdout(t, func() error { return run([]string{"teleport", "claude", "--to", "me@box"}) })
		if exitCode(err) != c.code {
			t.Errorf("%s: exit %d, want %d: %v", c.what, exitCode(err), c.code, err)
		}
		if len(c.rt.asked) != 0 {
			t.Errorf("%s: hull was asked %v", c.what, c.rt.asked)
		}
		for _, argv := range *calls {
			if strings.Contains(argv[len(argv)-1], receiveVerb) {
				t.Errorf("%s: the destination was asked to take the network", c.what)
			}
		}
		if e := entryOf(t); e["pendingHost"] != nil || e["host"] != nil {
			t.Errorf("%s: a refused move was recorded: %v", c.what, e)
		}
	}
}

// A move whose end brig did not see stays recorded as unfinished, resumes
// nothing, and says how to find out. --status settles it from hull's record:
// committed is recorded as moved; and an abort hull confirms gives the
// destination's network back and clears the record without brig resuming
// anything itself.
func TestALostAcknowledgementIsSettledFromHullsRecord(t *testing.T) {
	rt := &moveRuntime{running: true, state: "committing", moveErr: errors.New("ssh: connection reset")}
	calls := moveHost(t, rt, noNetProbe())
	_, err := captureStdout(t, func() error { return run([]string{"teleport", "claude", "--to", "me@box"}) })
	if err == nil || exitCode(err) != exitFailure ||
		!strings.Contains(err.Error(), "brig teleport claude-code --status") ||
		!strings.Contains(err.Error(), "hull migrate --status brig-claude-code") {
		t.Fatalf("a lost acknowledgement: %v", err)
	}
	if e := entryOf(t); e["pendingHost"] != "me@box" || e["host"] != nil {
		t.Errorf("the unfinished move was not kept: %v", e)
	}
	// sh refuses while the move is unfinished.
	if _, err := captureStdout(t, func() error { return run([]string{"sh", "claude"}) }); err == nil ||
		!strings.Contains(err.Error(), "--status") {
		t.Errorf("sh during an unfinished move: %v", err)
	}
	out, err := captureStdout(t, func() error { return run([]string{"teleport", "claude", "--status"}) })
	if err != nil || !strings.Contains(out, "has not finished") {
		t.Errorf("--status while committing: %q, %v", out, err)
	}
	rt.state = runtime.MoveCommitted
	*calls = nil
	if out, err := captureStdout(t, func() error { return run([]string{"teleport", "claude", "--status", "--json"}) }); err != nil ||
		!strings.Contains(out, `"state": "moved"`) {
		t.Errorf("--status once committed: %q, %v", out, err)
	}
	if e := entryOf(t); e["host"] != "me@box" {
		t.Errorf("--status did not record the commit: %v", e)
	}
	if len(*calls) != 1 || !strings.Contains((*calls)[0][len((*calls)[0])-1], "--commit") {
		t.Errorf("the destination was not told: %v", *calls)
	}

	rt = &moveRuntime{running: true, state: "transferring", moveErr: errors.New("killed")}
	calls = moveHost(t, rt, noNetProbe())
	_, _ = captureStdout(t, func() error { return run([]string{"teleport", "claude", "--to", "me@box"}) })
	*calls = nil
	out, err = captureStdout(t, func() error { return run([]string{"teleport", "claude", "--abort"}) })
	if err != nil || !strings.Contains(out, "called off") {
		t.Fatalf("--abort: %q, %v", out, err)
	}
	if rt.asked[len(rt.asked)-1] != "abort "+rt.txid {
		t.Errorf("hull was asked %v", rt.asked)
	}
	if e := entryOf(t); e["pendingHost"] != nil || e["txid"] != nil {
		t.Errorf("an aborted move left %v", e)
	}
	if len(*calls) != 1 || !strings.Contains((*calls)[0][len((*calls)[0])-1], "--abort") {
		t.Errorf("the destination's network was not given back: %v", *calls)
	}
}

// The hull lines a move's messages name are ones that work when copied: with
// BRIG_HULL_STORE_DIR set, each names that store, as every call brig makes
// does. Without it hull looks in its default store and finds no sandbox. The
// lines are hull's own verbs for the source, `hull migrate --status NAME` and
// `hull migrate NAME --txid T`, not the destination's `migrate-status`.
func TestTeleportRemediesNameTheHullStore(t *testing.T) {
	rt := &moveRuntime{running: true, state: "committing", moveErr: errors.New("ssh: connection reset")}
	moveHost(t, rt, noNetProbe())
	t.Setenv(runtime.HullStoreDir, "/st/a")
	_, err := captureStdout(t, func() error { return run([]string{"teleport", "claude", "--to", "me@box"}) })
	if err == nil || !strings.Contains(err.Error(), "`hull --store-dir /st/a migrate --status brig-claude-code`") {
		t.Errorf("a lost acknowledgement names: %v", err)
	}
	out, err := captureStdout(t, func() error { return run([]string{"teleport", "claude", "--status"}) })
	want := "`hull --store-dir /st/a migrate brig-claude-code --txid " + rt.txid + "`"
	if err != nil || !strings.Contains(out, want) ||
		!strings.Contains(out, "`hull --store-dir /st/a migrate --status brig-claude-code`") {
		t.Errorf("--status names: %q, %v", out, err)
	}

	rt = &moveRuntime{running: true, inspect: runtime.Migration{State: "fenced", TxID: "t1"}}
	moveHost(t, rt, noNetProbe())
	t.Setenv(runtime.HullStoreDir, "/st/a")
	_, err = captureStdout(t, func() error { return run([]string{"teleport", "claude", "--to", "me@box"}) })
	if err == nil || !strings.Contains(err.Error(), "`hull --store-dir /st/a migrate --status brig-claude-code`") ||
		strings.Contains(err.Error(), "migrate-status") {
		t.Errorf("a parked guest names: %v", err)
	}
}

// A move after which hull's record did not read is one whose end brig did
// not see, whether the call failed or hull said it succeeded: the record
// stays unfinished, hull is asked to abort nothing, and the destination
// keeps the network it was given.
func TestAMoveWhoseRecordDidNotReadStaysUnfinished(t *testing.T) {
	for _, moveErr := range []error{errors.New("ssh: connection reset"), nil} {
		rt := &moveRuntime{running: true, unread: true, moveErr: moveErr}
		calls := moveHost(t, rt, noNetProbe())
		_, err := captureStdout(t, func() error { return run([]string{"teleport", "claude", "--to", "me@box"}) })
		if err == nil || !strings.Contains(err.Error(), "did not report how it ended") {
			t.Errorf("moveErr %v: %v", moveErr, err)
		}
		if e := entryOf(t); e["pendingHost"] != "me@box" || e["txid"] != rt.txid {
			t.Errorf("moveErr %v: the unfinished move was not kept: %v", moveErr, e)
		}
		for _, a := range rt.asked {
			if strings.HasPrefix(a, "abort ") {
				t.Errorf("moveErr %v: hull was asked to %s", moveErr, a)
			}
		}
		for _, argv := range *calls {
			if strings.Contains(argv[len(argv)-1], receiveVerb+" --json --abort") {
				t.Errorf("moveErr %v: the destination was asked to give the network back: %q", moveErr, argv)
			}
		}
		// --status cannot read the record either, and settles nothing.
		*calls = nil
		if _, err := captureStdout(t, func() error { return run([]string{"teleport", "claude", "--status"}) }); err == nil {
			t.Errorf("moveErr %v: --status settled an unread record", moveErr)
		}
		if e := entryOf(t); e["pendingHost"] != "me@box" || len(*calls) != 0 {
			t.Errorf("moveErr %v: --status changed %v, asked %v", moveErr, e, *calls)
		}
	}
}

// hull's having no move on record says the move never began only while the
// sandbox is here: a source removed after its move committed away takes the
// record with it. --status then keeps the move recorded, resumes nothing and
// gives the destination nothing back, and says where to ask.
func TestNoRecordOfAMoveWhoseSandboxIsGoneSettlesNothing(t *testing.T) {
	rt := &moveRuntime{running: true, state: "committing", moveErr: errors.New("ssh: connection reset")}
	calls := moveHost(t, rt, noNetProbe())
	_, _ = captureStdout(t, func() error { return run([]string{"teleport", "claude", "--to", "me@box"}) })
	// `hull rm` of the stale source: the instance and its record are gone.
	rt.state, rt.gone = "", true
	*calls = nil
	_, err := captureStdout(t, func() error { return run([]string{"teleport", "claude", "--status"}) })
	if err == nil || !strings.Contains(err.Error(), "may have committed") ||
		!strings.Contains(err.Error(), "ssh me@box hull migrate --status brig-claude-code") {
		t.Errorf("--status with the source gone: %v", err)
	}
	if e := entryOf(t); e["pendingHost"] != "me@box" || e["txid"] != rt.txid {
		t.Errorf("--status dropped the move: %v", e)
	}
	if len(*calls) != 0 {
		t.Errorf("--status asked the destination %v", *calls)
	}
	// With the sandbox here, no record does mean the move never began.
	rt.gone = false
	if out, err := captureStdout(t, func() error { return run([]string{"teleport", "claude", "--status"}) }); err != nil ||
		!strings.Contains(out, "never began") {
		t.Errorf("--status with the sandbox here: %q, %v", out, err)
	}
	if e := entryOf(t); e["pendingHost"] != nil {
		t.Errorf("a move that never began was kept: %v", e)
	}
}

// One teleport of a session at a time: while one holds the session's move,
// a second move, --status and --abort each refuse and change nothing.
func TestATeleportRefusesWhileAnotherHoldsTheMove(t *testing.T) {
	rt := &moveRuntime{running: true, state: "transferring", moveErr: errors.New("killed")}
	calls := moveHost(t, rt, noNetProbe())
	_, _ = captureStdout(t, func() error { return run([]string{"teleport", "claude", "--to", "me@box"}) })
	before := entryOf(t)
	unlock, err := wrap.LockMove("brig-claude-code")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	rt.asked, *calls = nil, nil
	for _, args := range [][]string{{"--status"}, {"--abort"}, {"--to", "me@box"}} {
		_, err := captureStdout(t, func() error { return run(append([]string{"teleport", "claude"}, args...)) })
		if err == nil || !strings.Contains(err.Error(), "is running on this host") {
			t.Errorf("%v while the move is held: %v", args, err)
		}
	}
	if len(rt.asked) != 0 || len(*calls) != 0 {
		t.Errorf("a refused teleport asked hull %v and the destination %v", rt.asked, *calls)
	}
	if e := entryOf(t); fmt.Sprint(e) != fmt.Sprint(before) {
		t.Errorf("a refused teleport changed the record: %v, was %v", e, before)
	}
}

// An abort hull recorded and whose restore here then failed still ends the
// move, and says the guest may be stopped here and what brings it back,
// rather than that it runs here again.
func TestAnAbortWhoseRestoreFailedSaysSo(t *testing.T) {
	rt := &moveRuntime{running: true, state: "transferring", moveErr: errors.New("killed"),
		abortErr: errors.New("aborted move, and brig-claude-code stays stopped with its fenced checkpoint")}
	calls := moveHost(t, rt, noNetProbe())
	_, _ = captureStdout(t, func() error { return run([]string{"teleport", "claude", "--to", "me@box"}) })
	*calls = nil
	out, err := captureStdout(t, func() error { return run([]string{"teleport", "claude", "--abort"}) })
	if err == nil || !strings.Contains(err.Error(), "stays stopped") ||
		!strings.Contains(err.Error(), "hull restore brig-claude-code") || strings.Contains(out, "again") {
		t.Errorf("an abort whose restore failed: %q, %v", out, err)
	}
	if e := entryOf(t); e["pendingHost"] != nil || e["txid"] != nil {
		t.Errorf("an aborted move left %v", e)
	}
	if len(*calls) != 1 || !strings.Contains((*calls)[0][len((*calls)[0])-1], "--abort") {
		t.Errorf("the destination's network was not given back: %v", *calls)
	}
}

// A receive whose answer was lost may still have taken the network on the
// destination: the move asks for it back before it drops its record, and
// hull is never asked to move.
func TestALostReceiveAnswerGivesTheNetworkBack(t *testing.T) {
	rt := &moveRuntime{running: true, state: runtime.MoveCommitted}
	calls := moveHost(t, rt, noNetProbe())
	ok := outputRemote
	outputRemote = func(argv []string) ([]byte, error) {
		out, err := ok(argv)
		if line := argv[len(argv)-1]; strings.Contains(line, receiveVerb) && !strings.Contains(line, "--abort") {
			return nil, errors.New("ssh: connection reset")
		}
		return out, err
	}
	_, err := captureStdout(t, func() error { return run([]string{"teleport", "claude", "--to", "me@box"}) })
	if err == nil || !strings.Contains(err.Error(), "nothing was paused") {
		t.Fatalf("a lost receive answer: %v", err)
	}
	last := (*calls)[len(*calls)-1]
	if !strings.Contains(last[len(last)-1], receiveVerb+" --json --abort") {
		t.Errorf("the destination was not asked to give the network back: %q", *calls)
	}
	if len(rt.asked) != 0 {
		t.Errorf("hull was asked %v", rt.asked)
	}
	if e := entryOf(t); e["pendingHost"] != nil || e["txid"] != nil {
		t.Errorf("the move was kept: %v", e)
	}
}

// A move hull refused before it fenced anything is undone on both sides:
// the destination's network is given back and the record cleared.
func TestAMoveHullDidNotBeginIsUndone(t *testing.T) {
	rt := &moveRuntime{running: true, state: runtime.MoveReserved, moveErr: errors.New("refused")}
	calls := moveHost(t, rt, noNetProbe())
	_, err := captureStdout(t, func() error { return run([]string{"teleport", "claude", "--to", "me@box"}) })
	if err == nil || !strings.Contains(err.Error(), "not paused for good") {
		t.Fatalf("%v", err)
	}
	if e := entryOf(t); e["pendingHost"] != nil {
		t.Errorf("the record was kept: %v", e)
	}
	if got := rt.asked[len(rt.asked)-1]; got != "abort "+rt.txid {
		t.Errorf("hull's unfenced record was not ended: %v", rt.asked)
	}
	last := (*calls)[len(*calls)-1]
	if !strings.Contains(last[len(last)-1], receiveVerb+" --json --abort") {
		t.Errorf("the destination's network was not given back: %q", last)
	}
}

// rm on a moved session removes the copy left here only once the brig
// there says the session runs there, and keeps the record that reaches it.
// With no copy left, rm forgets the session here and touches nothing there.
func TestRmOfAMovedSessionRemovesOnlyTheCopyHere(t *testing.T) {
	for _, running := range []bool{false, true} {
		rt := &moveRuntime{running: true}
		probe := noNetProbe()
		probe.Sandbox.Running = running
		calls := moveHost(t, rt, probe)
		if err := os.WriteFile(filepath.Join(os.Getenv("BRIG_STATE_DIR"), "sessions.json"),
			[]byte(`{"claude-code": `+movedEntry+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
		withRuntime(t, rt)
		_, err := captureStdout(t, func() error { return run([]string{"rm", "claude"}) })
		if running && (err != nil || rt.removed != 1) {
			t.Errorf("a copy whose session runs there: %v, removed %d", err, rt.removed)
		}
		if !running && (err == nil || rt.removed != 0) {
			t.Errorf("a copy whose session is not confirmed there was removed: %v", err)
		}
		if e := entryOf(t); e["host"] != "me@box" {
			t.Errorf("rm dropped the record that reaches the session: %v", e)
		}
		if len(*calls) != 1 || !strings.Contains(strings.Join((*calls)[0], " "), "BRIG_HULL_STORE_DIR=/st/b") {
			t.Errorf("the brig there was asked %v", *calls)
		}
	}
}

// hull's own record of the copy here, migrated by the move the index names,
// is proof enough that the copy is stale: rm removes it without asking the
// destination, whose brig may no longer have the session or may not answer.
// A record of another move, or of a guest only fenced, proves nothing, and
// the copy stays while the destination does not say the session runs there.
func TestRmOfACopyHullRecordsAsMigratedNeedsNoWordFromThere(t *testing.T) {
	const txid = "0123456789abcdef0123456789abcdef"
	for _, c := range []struct {
		what   string
		m      runtime.Migration
		remove bool
	}{
		{"migrated by this move", runtime.Migration{State: runtime.MigrationMigrated, MoveTxID: txid}, true},
		{"migrated by another move", runtime.Migration{State: runtime.MigrationMigrated, MoveTxID: "ffff"}, false},
		{"only fenced", runtime.Migration{State: runtime.MigrationFenced, MoveTxID: txid}, false},
	} {
		rt := &moveRuntime{running: true, inspect: c.m}
		probe := noNetProbe()
		probe.Sandbox.Running = false
		calls := moveHost(t, rt, probe)
		if err := os.WriteFile(filepath.Join(os.Getenv("BRIG_STATE_DIR"), "sessions.json"),
			[]byte(`{"claude-code": `+movedEntry+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
		withRuntime(t, rt)
		_, err := captureStdout(t, func() error { return run([]string{"rm", "claude"}) })
		if c.remove && (err != nil || rt.removed != 1 || len(*calls) != 0) {
			t.Errorf("%s: %v, removed %d, asked %v", c.what, err, rt.removed, *calls)
		}
		if !c.remove && (err == nil || rt.removed != 0 || len(*calls) != 1) {
			t.Errorf("%s: the copy was not kept for the destination's word: %v, removed %d", c.what, err, rt.removed)
		}
		if e := entryOf(t); e["host"] != "me@box" {
			t.Errorf("%s: rm dropped the record that reaches the session: %v", c.what, e)
		}
	}
}

// ls names a moved session, here or not, with its host.
func TestListingNamesWhereAMovedSessionRuns(t *testing.T) {
	movedHost(t, movedEntry)
	rows := sandboxRows(nil, nil)
	if len(rows) != 1 || rows[0].ref != "claude-code" || rows[0].state != "moved" ||
		workspaceCell(rows[0]) != "(in the sandbox disk on me@box)" {
		t.Fatalf("rows %+v", rows)
	}
	d := sandboxListData(rows, nil)
	if d.Sandboxes[0].Host != "me@box" {
		t.Errorf("JSON %+v", d.Sandboxes[0])
	}
}

// On the destination, a session a move is still bringing in is not pruned
// by a listing that does not have its sandbox yet: its entry and its
// posture are what the guest is reached by once it runs.
func TestAListingKeepsWhatAMoveIsBringingIn(t *testing.T) {
	scratchHost(t)
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	dir := os.Getenv("BRIG_STATE_DIR")
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"),
		[]byte(`{"claude-code": {"home": "", "sandbox": "brig-claude-code", "teleportable": true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "arrivals.json"),
		[]byte(`{"brig-claude-code": {"ref": "claude-code", "txid": "tx", "from": "me@src", "network": "none", "state": "prepared"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RecordBootedNet("brig-claude-code", "none"); err != nil {
		t.Fatal(err)
	}
	pruneSessionIndex(nil, nil)
	if e := entryOf(t); e["sandbox"] != "brig-claude-code" {
		t.Errorf("the arriving session's entry was pruned: %v", e)
	}
	if net, _ := runtime.BootedNet("brig-claude-code"); net != "none" {
		t.Errorf("the arriving session's posture was pruned: %q", net)
	}
}
