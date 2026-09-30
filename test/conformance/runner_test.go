package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

const fakeHull = "hull v0.1.0-rc29 (e54923f, 2026-09-25, go1.27.1, darwin/arm64)"

// fakeBrig stands in for brig and hull. It keeps which policy is attached
// and whether the sandbox runs, and answers each probe with answer.
type fakeBrig struct {
	t          *testing.T
	calls      [][]string
	envs       [][]string
	attached   string
	running    bool
	runCode    int
	versionOut string
	versionErr error
	// answer gives the probe outcome for one invocation in the boot of
	// policy. env and args have their placeholders filled in.
	answer  func(policy string, env, args []string, nth int) outcome
	shCount map[string]int
	// restartOn makes brig report a sandbox restart on the first probe
	// whose arguments end with it.
	restartOn string
	// restartIn limits restartOn to the boot of one policy.
	restartIn string
	// restartWarning is what brig says when it restarts the sandbox.
	restartWarning string
	restarted      bool
	// rmFails makes brig rm fail on a sandbox that is there, and leave it.
	rmFails bool
}

func newFake(t *testing.T) *fakeBrig {
	return &fakeBrig{t: t, versionOut: fakeHull + "\n", shCount: map[string]int{}}
}

const (
	fakeHVI    = "hvi 0.1.0 (core 0.1.0)"
	fakeDigest = "sha256:1e1a12685b539e2b89de428be712254858b8b86ad5a46f377d4c373a7a8dd0c3"
	fakeAssets = "ghcr.io/nofireai/hull-assets:darwin-arm64"
)

func (f *fakeBrig) exec(_ context.Context, env []string, name string, args ...string) (string, string, int, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	f.envs = append(f.envs, env)
	switch {
	case filepath.Base(name) == "hvi":
		return fakeHVI + "\n", "", 0, nil
	case name != "brig":
		return f.runtimeCall(args)
	}
	switch args[0] {
	case "version":
		return "v0.3.1-test\n", "", 0, nil
	case "policy":
		switch args[1] {
		case "attach":
			f.attached = args[2]
		case "detach":
			f.attached = ""
		}
		return "", "", 0, nil
	case "run":
		if f.runCode != 0 {
			return "", "brig: no usable runtime\n", f.runCode, nil
		}
		f.running = true
		return "brig-egress-conformance\n", "", 0, nil
	case "rm":
		if !f.running {
			return "", "brig: no such sandbox\n", 3, nil
		}
		if f.rmFails {
			return "", "brig: hull rm: the instance did not stop\n", 1, nil
		}
		f.running = false
		return "", "brig: removed egress-conformance\n", 0, nil
	case "sh":
		if !f.running {
			return "", "brig: no such sandbox\n", 3, nil
		}
		rest := args[2:]
		var env []string
		if rest[0] == "env" {
			rest = rest[1:]
			for strings.Contains(rest[0], "=") {
				env = append(env, rest[0])
				rest = rest[1:]
			}
		}
		if rest[0] != guestProbe {
			f.t.Fatalf("sh runs %q, not the probe at %s", rest[0], guestProbe)
		}
		key := f.attached + " " + strings.Join(rest[1:], " ")
		f.shCount[key]++
		o := f.answer(f.attached, env, rest[1:], f.shCount[key])
		var stderr string
		if !f.restarted && f.restartOn != "" && strings.HasSuffix(strings.Join(rest, " "), f.restartOn) &&
			(f.restartIn == "" || f.restartIn == f.attached) {
			f.restarted = true
			stderr = f.restartWarning
			if stderr == "" {
				stderr = "brig: the running sandbox is not mounting /x: its share went stale\n" +
					"brig:   ↳ brig restarts it, and any other session using this sandbox will be disconnected\n"
			}
		}
		return fmt.Sprintf("%s %s %s detail\n", o, rest[1], rest[len(rest)-1]), stderr, probeExit[o], nil
	}
	f.t.Fatalf("unexpected brig call %q", args)
	return "", "", 1, nil
}

// runtimeCall answers the calls the suite makes of the runtime itself.
func (f *fakeBrig) runtimeCall(args []string) (string, string, int, error) {
	switch args[0] {
	case "assets":
		return "directory: /nowhere/assets\nreference: " + fakeAssets + "\nfetched from: " + fakeAssets +
			"\nsignature: verified against sha256:0e8f69d1ff8a\n", "", 0, nil
	case "inspect":
		if !f.running {
			return "", "instance not found: " + args[1] + "\n", 1, nil
		}
		return `{"id":"` + args[1] + `","imageDigest":"` + fakeDigest + `","status":"running"}`, "", 0, nil
	}
	if f.versionErr != nil {
		return "", "", -1, f.versionErr
	}
	return f.versionOut, "", 0, nil
}

// caseFor finds the case a probe invocation belongs to in the boot of
// policy, by its filled-in arguments.
func caseFor(t *testing.T, s *suite, policy string, env, args []string) testCase {
	t.Helper()
	for _, c := range cases {
		if policy != noPolicy && c.policy != policy {
			continue
		}
		e, a := s.fill(c)
		if slices.Equal(a, args) && slices.Equal(e, env) {
			return c
		}
	}
	t.Fatalf("no case in boot %q runs %q %q", policy, env, args)
	return testCase{}
}

// behaves answers the way the policies say hvi does: everything but IPv6
// reaches with no policy, and under a policy every case gets its mode. The
// host service case with no policy connects to the suite's listener, as the
// real guest does.
func behaves(t *testing.T, s *suite) func(string, []string, []string, int) outcome {
	return func(policy string, env, args []string, _ int) outcome {
		c := caseFor(t, s, policy, env, args)
		if policy == noPolicy {
			switch c.id {
			case "host-service":
				hitListener(t, args[1])
			case "proxy-env":
				hitListener(t, strings.TrimPrefix(env[0], "https_proxy=http://"))
			}
		}
		switch {
		case c.want == expectNoRoute:
			return noRoute
		case policy == noPolicy, c.want == expectReach, c.want == expectGap:
			return reached
		}
		return c.mode
	}
}

func newSuite(t *testing.T, f *fakeBrig) *suite {
	t.Helper()
	probe := t.TempDir() + "/netprobe"
	if err := os.WriteFile(probe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	hvi := filepath.Join(t.TempDir(), "hvi")
	if err := os.WriteFile(hvi, []byte("fake hvi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &suite{
		brig:     "brig",
		runtime:  "hull",
		probe:    probe,
		image:    defaultImage,
		backend:  "hvi",
		exec:     f.exec,
		now:      func() time.Time { return time.Date(2026, 9, 28, 1, 2, 3, 0, time.FixedZone("EEST", 3*3600)) },
		log:      io.Discard,
		hviPath:  func() (string, error) { return hvi, nil },
		hostAddr: func() (netip.Addr, error) { return netip.MustParseAddr("127.0.0.1"), nil },
		lookup: func(_ context.Context, name string) (netip.Addr, error) {
			if name == "example.net" {
				return netip.MustParseAddr("192.0.2.8"), nil
			}
			return netip.MustParseAddr("192.0.2.7"), nil
		},
		env: []string{"PATH=/usr/bin:/bin", "HOME=/nowhere"},
	}
	f.answer = behaves(t, s)
	return s
}

func run(t *testing.T, s *suite) *report {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.do(ctx)
}

func (f *fakeBrig) brigCalls(verb string) [][]string {
	var out [][]string
	for _, c := range f.calls {
		if c[0] == "brig" && c[1] == verb {
			out = append(out, c)
		}
	}
	return out
}

// No runtime is a failed run. A skip would read as a pass in a release
// checklist.
func TestNoRuntimeFailsTheRun(t *testing.T) {
	for name, set := range map[string]func(*fakeBrig){
		"not installed":     func(f *fakeBrig) { f.versionErr = errors.New(`exec: "hull": executable file not found in $PATH`) },
		"prints no version": func(f *fakeBrig) { f.versionOut = "\n" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			set(f)
			s := newSuite(t, f)
			rep := run(t, s)
			if !rep.failed() {
				t.Fatal("a run with no runtime did not fail")
			}
			if len(f.brigCalls("run")) != 0 {
				t.Fatal("booted with no runtime")
			}
			out := rep.markdown()
			if strings.Contains(strings.ToLower(out), "skip") {
				t.Fatalf("record says skip:\n%s", out)
			}
			if !strings.Contains(out, "FAIL") {
				t.Fatalf("record does not say FAIL:\n%s", out)
			}
		})
	}
}

func TestRefusedBootFailsTheRunAndRemovesTheSandbox(t *testing.T) {
	f := newFake(t)
	f.runCode = 4
	s := newSuite(t, f)
	rep := run(t, s)
	if !rep.failed() {
		t.Fatal("a boot brig refused did not fail the run")
	}
	for _, r := range rep.rows {
		if r.verdict != fail {
			t.Errorf("%s: %s after a refused boot", r.c.id, r.verdict)
		}
	}
	runs, rms := len(f.brigCalls("run")), 0
	for i, c := range f.calls {
		if c[0] == "brig" && c[1] == "run" {
			if j := slices.IndexFunc(f.calls[i+1:], func(c []string) bool { return c[0] == "brig" && c[1] == "rm" }); j >= 0 {
				rms++
			}
		}
	}
	if runs == 0 || rms != runs {
		t.Fatalf("%d boots, %d followed by brig rm", runs, rms)
	}
}

// A setting exported in the shell that runs the suite must not reach brig.
// BRIG_NETWORK=offline would make every denied case look refused.
func TestInheritedBrigSettingsDoNotReachBrig(t *testing.T) {
	f := newFake(t)
	s := newSuite(t, f)
	s.env = append(s.env, "BRIG_NETWORK=offline", "BRIG_IMAGE=evil:latest",
		"BRIG_POLICY_DIR=/home/me/policies", "BRIG_FORWARD_ENV=AWS_SECRET_ACCESS_KEY",
		"BRIG_RUNTIME_BIN=/opt/hull/bin/hull", "BRIG_VERIFY=require")
	// main takes the runtime from BRIG_RUNTIME_BIN when -runtime is not given.
	s.runtime = "/opt/hull/bin/hull"
	run(t, s)
	boots := 0
	for i, c := range f.calls {
		if c[0] != "brig" || c[1] != "run" {
			continue
		}
		boots++
		env := envMap(f.envs[i])
		for _, k := range []string{"BRIG_NETWORK", "BRIG_IMAGE", "BRIG_FORWARD_ENV"} {
			if v, ok := env[k]; ok {
				t.Errorf("%s=%s reached brig run", k, v)
			}
		}
		if env["BRIG_POLICY_DIR"] == "/home/me/policies" || env["BRIG_POLICY_DIR"] == "" {
			t.Errorf("BRIG_POLICY_DIR=%q, want the suite's own", env["BRIG_POLICY_DIR"])
		}
		for k, want := range map[string]string{
			"BRIG_RUNTIME_BIN": "/opt/hull/bin/hull", "BRIG_VERIFY": "require", "BRIG_HYPERVISOR": "hvi",
		} {
			if env[k] != want {
				t.Errorf("%s=%q, want %q", k, env[k], want)
			}
		}
		for _, k := range []string{"BRIG_PROFILE_DIR", "BRIG_STATE_DIR", "BRIG_WORKSPACE"} {
			if env[k] == "" {
				t.Errorf("%s not set to the suite's own", k)
			}
		}
	}
	if boots != 3 {
		t.Fatalf("%d boots, want 3", boots)
	}
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestAWellBehavedGuestPasses(t *testing.T) {
	f := newFake(t)
	s := newSuite(t, f)
	rep := run(t, s)
	if rep.failed() {
		t.Fatalf("run failed:\n%s", rep.markdown())
	}
	n := map[verdict]int{}
	for _, r := range rep.rows {
		n[r.verdict]++
	}
	if n[knownGap] == 0 || n[noRouteVerdict] != 1 {
		t.Fatalf("verdicts %v", n)
	}
	if f.running {
		t.Fatal("the sandbox was left running")
	}
}

func TestEveryBootRunsItsControlsFirstAndLast(t *testing.T) {
	f := newFake(t)
	s := newSuite(t, f)
	run(t, s)
	var boot []string
	var boots [][]string
	for _, c := range f.calls {
		switch {
		case c[0] == "brig" && c[1] == "run":
			boot = nil
		case c[0] == "brig" && c[1] == "sh":
			boot = append(boot, strings.Join(c[2:], " "))
		case c[0] == "brig" && c[1] == "rm" && boot != nil:
			boots = append(boots, boot)
			boot = nil
		}
	}
	if len(boots) != 3 {
		t.Fatalf("%d boots ran probes, want 3", len(boots))
	}
	isControl := func(sh string) bool {
		for _, c := range cases {
			if c.want == expectReach && strings.HasSuffix(sh, " "+strings.Join(c.args, " ")) {
				return true
			}
		}
		return false
	}
	for i, b := range boots {
		if !strings.HasSuffix(b[0], "tcp example.com:443") || !isControl(b[len(b)-1]) {
			t.Errorf("boot %d starts with %q and ends with %q, want allowed controls at both ends", i, b[0], b[len(b)-1])
		}
	}
}

// A guest that loses its network halfway fails every denied case, even the
// ones that match their mode by accident.
func TestControlLostAtTheEndFailsTheBoot(t *testing.T) {
	f := newFake(t)
	s := newSuite(t, f)
	good := f.answer
	f.answer = func(policy string, env, args []string, nth int) outcome {
		if policy == denyPolicy && args[1] == "example.com:443" && nth > 1 {
			return timedOut
		}
		return good(policy, env, args, nth)
	}
	rep := run(t, s)
	if !rep.failed() {
		t.Fatal("run passed with a control that failed at the end")
	}
	for _, r := range rep.rows {
		if r.c.policy == denyPolicy && r.c.want != expectReach && r.verdict != fail {
			t.Errorf("%s: %s with the boot's control lost", r.c.id, r.verdict)
		}
		if r.c.policy == allowPolicy && r.verdict == fail {
			t.Errorf("%s: failed in a boot whose control held", r.c.id)
		}
	}
}

// With no policy the IPv6 literal is not reached either, and the gap cases
// need it reached. A baseline whose own control failed measured neither.
func TestFailedControlWithNoPolicyFailsTheRun(t *testing.T) {
	f := newFake(t)
	s := newSuite(t, f)
	good := f.answer
	f.answer = func(policy string, env, args []string, nth int) outcome {
		if policy == noPolicy {
			return timedOut
		}
		return good(policy, env, args, nth)
	}
	rep := run(t, s)
	if !rep.failed() {
		t.Fatal("run passed with no control reached in the boot with no policy")
	}
	for _, r := range rep.rows {
		if r.c.want == expectNoRoute && r.verdict != fail {
			t.Errorf("IPv6 case: %s next to a baseline that measured nothing", r.verdict)
		}
	}
}

func TestPlaceholdersAreFilledOnTheHost(t *testing.T) {
	f := newFake(t)
	s := newSuite(t, f)
	run(t, s)
	var sawProxy, sawSvc, sawLiteral bool
	for _, c := range f.brigCalls("sh") {
		line := strings.Join(c, " ")
		if strings.Contains(line, "{") {
			t.Fatalf("placeholder reached the guest: %s", line)
		}
		sawProxy = sawProxy || strings.Contains(line, "https_proxy=http://127.0.0.1:")
		sawSvc = sawSvc || (strings.Contains(line, " tcp 127.0.0.1:") && !strings.Contains(line, "proxy"))
		sawLiteral = sawLiteral || strings.Contains(line, " tcp 192.0.2.7:443")
	}
	if !sawProxy || !sawSvc || !sawLiteral {
		t.Fatalf("proxy %v, host service %v, literal %v", sawProxy, sawSvc, sawLiteral)
	}
}

// A literal the host cannot resolve is a case that measured nothing.
func TestUnresolvedLiteralFailsItsCase(t *testing.T) {
	f := newFake(t)
	s := newSuite(t, f)
	s.lookup = func(context.Context, string) (netip.Addr, error) {
		return netip.Addr{}, errors.New("no such host")
	}
	f.answer = func(policy string, env, args []string, nth int) outcome {
		for _, a := range args {
			if strings.Contains(a, "{") {
				t.Fatalf("unfilled placeholder in %q", args)
			}
		}
		return reached
	}
	rep := run(t, s)
	for _, r := range rep.rows {
		if strings.Contains(strings.Join(r.c.args, " "), addrOf) && r.verdict != fail {
			t.Errorf("%s: %s with no literal", r.c.id, r.verdict)
		}
	}
}

// The listener on the host is the other half of the host service case: a
// connection it accepted under a policy is a reach whatever the probe said.
func TestHostListenerCountsWhatReachedIt(t *testing.T) {
	f := newFake(t)
	s := newSuite(t, f)
	rep := run(t, s)
	if got := rep.boots[noPolicy].accepted; got != 2 {
		t.Fatalf("listener accepted %d in the boot with no policy, want 2", got)
	}
	if got := rep.boots[denyPolicy].accepted; got != 0 {
		t.Fatalf("listener accepted %d under the policy, want 0", got)
	}
	if rep.failed() {
		t.Fatalf("run failed:\n%s", rep.markdown())
	}
}

// hitListener connects to the suite's host listener and waits for it to
// hang up, which it does only after counting the connection.
func hitListener(t *testing.T, addr string) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("listener did not hang up: %v", err)
	}
}

func TestListenerReachedUnderAPolicyFailsTheRun(t *testing.T) {
	f := newFake(t)
	s := newSuite(t, f)
	good := f.answer
	f.answer = func(policy string, env, args []string, nth int) outcome {
		if policy == denyPolicy && len(env) == 0 && args[0] == "tcp" && strings.HasPrefix(args[1], "127.0.0.1:") {
			hitListener(t, args[1])
		}
		return good(policy, env, args, nth)
	}
	rep := run(t, s)
	if !rep.failed() {
		t.Fatal("run passed with the host listener reached under a deny policy")
	}
}

// A probe that says it reached the host service while the listener saw no
// connection reached something other than the host. Without the listener's
// word the run with no policy is no baseline, and the case fails.
func TestHostServiceReachedWithNothingOnTheListenerFails(t *testing.T) {
	f := newFake(t)
	s := newSuite(t, f)
	good := f.answer
	f.answer = func(policy string, env, args []string, nth int) outcome {
		if policy == noPolicy && caseFor(t, s, policy, env, args).id == "host-service" {
			return reached
		}
		return good(policy, env, args, nth)
	}
	rep := run(t, s)
	if !rep.failed() {
		t.Fatal("run passed with a host service reach the listener never saw")
	}
	for _, r := range rep.rows {
		if r.c.id == "host-service" {
			if r.verdict != fail || !strings.Contains(r.why, "listener accepted nothing") {
				t.Fatalf("host-service: %s: %s", r.verdict, r.why)
			}
			return
		}
	}
	t.Fatal("no host-service row")
}

// hvi refuses 169.254.169.254 with no policy attached. The deny boot's
// refusal is then no evidence for the policy, and the record says so
// without failing the run.
func TestDeniedTargetShutWithoutAPolicyIsUnproven(t *testing.T) {
	f := newFake(t)
	s := newSuite(t, f)
	good := f.answer
	f.answer = func(policy string, env, args []string, nth int) outcome {
		if policy == noPolicy && caseFor(t, s, policy, env, args).id == "metadata" {
			return connectRefused
		}
		return good(policy, env, args, nth)
	}
	rep := run(t, s)
	if rep.failed() {
		t.Fatalf("run failed:\n%s", rep.markdown())
	}
	for _, r := range rep.rows {
		want := pass
		switch r.c.id {
		case "metadata":
			want = unproven
		case "ipv6-literal":
			want = noRouteVerdict
		}
		if r.c.want == expectGap {
			want = knownGap
		}
		if r.verdict != want {
			t.Errorf("%s: %s (%s), want %s", r.c.id, r.verdict, r.why, want)
		}
	}
	md := rep.markdown()
	if !strings.Contains(md, "1 unproven") || !strings.Contains(md, "| `metadata` |") ||
		!strings.Contains(md, "unproven: connect-refused with the policy and connect-refused without one") {
		t.Fatalf("record does not show the unproven case:\n%s", md)
	}
}

func TestRecordNamesBackendRuntimeBrigAndUTCDate(t *testing.T) {
	f := newFake(t)
	s := newSuite(t, f)
	out := run(t, s).markdown()
	for _, want := range []string{"`hvi`", fakeHull, "v0.3.1-test", "2026-09-27 22:02 UTC", defaultImage, "PASS"} {
		if !strings.Contains(out, want) {
			t.Errorf("record lacks %q", want)
		}
	}
	for _, c := range cases {
		if !strings.Contains(out, c.id) {
			t.Errorf("record lacks case %s", c.id)
		}
	}
	if strings.Contains(out, "127.0.0.1") {
		t.Error("record carries the host's address")
	}
}

func TestRecordPath(t *testing.T) {
	for line, want := range map[string]string{
		fakeHull:                  "docs/manual-tests/egress-conformance-hvi-v0.1.0-rc29.md",
		"hull version 0.1.0-rc23": "docs/manual-tests/egress-conformance-hvi-0.1.0-rc23.md",
		"hull":                    "",
		"hull ../../etc/passwd":   "",
	} {
		if got := recordPath("hvi", line); got != want {
			t.Errorf("%q: %q, want %q", line, got, want)
		}
	}
}

// curl names the proxy it could not reach as "HOST port N", so taking out
// HOST:PORT alone left the host's address in the record.
func TestRecordCarriesNoHostAddressInAnyForm(t *testing.T) {
	r := &report{svc: "192.168.1.5:61025", host: "192.168.1.5"}
	for _, line := range []string{
		"connect-refused doh https://dns.google/dns-query curl exit 7: curl: (7) Failed to connect to 192.168.1.5 port 61025 after 0 ms",
		"connect-refused tcp 192.168.1.5:61025 dial tcp 192.168.1.5:61025: connect: connection refused",
	} {
		if got := r.scrub(line); strings.Contains(got, "192.168.1.5") {
			t.Errorf("scrubbed to %q", got)
		}
	}
}

// The brig binary and the suite's state live in temporary directories whose
// paths name the host's user. A committed record carries neither.
func TestRecordCarriesNoLocalPath(t *testing.T) {
	f := newFake(t)
	s := newSuite(t, f)
	s.brig = "/var/folders/xy/T/tmp.abc/brig"
	s.exec = func(ctx context.Context, env []string, name string, args ...string) (string, string, int, error) {
		if name == s.brig {
			name = "brig"
		}
		if name == "brig" && args[0] == "sh" && strings.HasSuffix(strings.Join(args, " "), "tcp 9.9.9.9:443") {
			return "", "", -1, fmt.Errorf("%s: %w", s.brig, context.DeadlineExceeded)
		}
		if name == "brig" && args[0] == "sh" && strings.HasSuffix(strings.Join(args, " "), "tcp 1.1.1.1:443") {
			return "", "brig: the running sandbox is not mounting " + filepath.Join(s.root, "home") + "\n", 1, nil
		}
		return f.exec(ctx, env, name, args...)
	}
	out := run(t, s).markdown()
	for _, local := range []string{s.brig, s.root, "/var/folders"} {
		if strings.Contains(out, local) {
			t.Errorf("record carries %s:\n%s", local, out)
		}
	}
	if !strings.Contains(out, "brig: context deadline exceeded") || !strings.Contains(out, "{root}/home") {
		t.Errorf("record lost the failures around the paths:\n%s", out)
	}
}

// What the guest printed stays in the record next to why nothing was
// measured.
func TestUnmeasuredProbeKeepsWhatTheGuestPrinted(t *testing.T) {
	o := parseProbe("crypto/rand: blocked for 60 seconds\nerror-after-connect dot 8.8.8.8 x\n", 3, nil)
	if o.invalid == "" || !strings.Contains(o.full(), "crypto/rand: blocked for 60 seconds") {
		t.Fatalf("full() = %q", o.full())
	}
}

// A restart puts the gateway through a fresh start, and the probe right
// after it has the slow first flows a fresh boot has. The record names the
// case, so a failure there reads with that in mind.
func TestRecordNamesTheCaseARestartHappenedOn(t *testing.T) {
	f := newFake(t)
	f.restartOn = "doh https://8.8.8.8/dns-query git.kernel.org"
	s := newSuite(t, f)
	out := run(t, s).markdown()
	if !strings.Contains(out, "restarted the sandbox on allow/dns-over-https") {
		t.Fatalf("record does not name the case the restart came on:\n%s", out)
	}
}

// A Ctrl-C cancels the suite's context. The sandbox and the policy attached
// to it still have to go: the suite deletes the directories they live in on
// its way out.
func TestInterruptStillRemovesTheSandboxAndDetachesThePolicy(t *testing.T) {
	f := newFake(t)
	s := newSuite(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// exec.CommandContext does not start a command whose context is done.
	s.exec = func(ctx context.Context, env []string, name string, args ...string) (string, string, int, error) {
		if err := ctx.Err(); err != nil {
			return "", "", -1, fmt.Errorf("%s: %w", name, err)
		}
		return f.exec(ctx, env, name, args...)
	}
	good := f.answer
	f.answer = func(policy string, env, args []string, nth int) outcome {
		if policy == denyPolicy {
			cancel()
		}
		return good(policy, env, args, nth)
	}
	rep := s.do(ctx)
	if !rep.failed() {
		t.Fatal("an interrupted run passed")
	}
	if f.running {
		t.Error("the sandbox was left running after the interrupt")
	}
	if f.attached != "" {
		t.Errorf("%s was left attached after the interrupt", f.attached)
	}
}

// The record names the runtime -runtime points at, so that is the one brig
// has to boot, whatever BRIG_RUNTIME_BIN the shell carries.
func TestRuntimeTheRecordNamesIsTheOneBrigBoots(t *testing.T) {
	for name, inherited := range map[string][]string{
		"none in the shell":        nil,
		"another in the shell":     {"BRIG_RUNTIME_BIN=/opt/hull-rc29/hull"},
		"a bare name in the shell": {"BRIG_RUNTIME_BIN=hull"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			s := newSuite(t, f)
			s.runtime = "/opt/hull-rc30/hull"
			s.env = append(s.env, inherited...)
			run(t, s)
			n := 0
			for i, c := range f.calls {
				if c[0] != "brig" || c[1] == "version" {
					continue
				}
				n++
				var got []string
				for _, kv := range f.envs[i] {
					if strings.HasPrefix(kv, "BRIG_RUNTIME_BIN=") {
						got = append(got, kv)
					}
				}
				if len(got) != 1 || got[0] != "BRIG_RUNTIME_BIN=/opt/hull-rc30/hull" {
					t.Fatalf("brig %s ran with %q, want BRIG_RUNTIME_BIN=/opt/hull-rc30/hull alone", c[1], got)
				}
			}
			if n == 0 {
				t.Fatal("brig never ran")
			}
		})
	}
}

// host-service and proxy-env both point at the host listener and share the
// boot with no policy. One of them reaching the listener is no word for the
// other.
func TestProxyReachWithOnlyTheHostServiceOnTheListenerFails(t *testing.T) {
	f := newFake(t)
	s := newSuite(t, f)
	good := f.answer
	f.answer = func(policy string, env, args []string, nth int) outcome {
		if policy == noPolicy && caseFor(t, s, policy, env, args).id == "proxy-env" {
			return probeError
		}
		return good(policy, env, args, nth)
	}
	rep := run(t, s)
	if got := rep.boots[noPolicy].accepted; got != 1 {
		t.Fatalf("listener accepted %d in the boot with no policy, want the host service's 1", got)
	}
	if !rep.failed() {
		t.Fatal("run passed with a proxy reach the listener never saw")
	}
	for _, r := range rep.rows {
		switch r.c.id {
		case "proxy-env":
			if r.verdict != fail || !strings.Contains(r.why, "listener accepted nothing") {
				t.Errorf("proxy-env: %s: %s", r.verdict, r.why)
			}
		case "host-service":
			if r.verdict != pass {
				t.Errorf("host-service: %s: %s, but the listener saw it", r.verdict, r.why)
			}
		}
	}
}

// caseByID finds one of the suite's cases by its id.
func caseByID(t *testing.T, id string) testCase {
	t.Helper()
	for _, c := range cases {
		if c.id == id {
			return c
		}
	}
	t.Fatalf("no case %s", id)
	return testCase{}
}

// The host listener is the suite's own fixture. A guest that cannot reach it
// with no policy leaves both host service cases without a baseline, and the
// suite is what is broken, so the run fails whatever the probe said.
func TestHostListenerNeverReachedWithoutAPolicyFailsTheRun(t *testing.T) {
	f := newFake(t)
	s := newSuite(t, f)
	good := f.answer
	f.answer = func(policy string, env, args []string, nth int) outcome {
		if policy == noPolicy && usesHostSvc(caseFor(t, s, policy, env, args)) {
			return connectRefused
		}
		return good(policy, env, args, nth)
	}
	rep := run(t, s)
	if !rep.failed() {
		t.Fatalf("run passed with the host listener out of the guest's reach:\n%s", rep.markdown())
	}
	for _, r := range rep.rows {
		if usesHostSvc(r.c) && (r.verdict != fail || !strings.Contains(r.why, "listener accepted nothing")) {
			t.Errorf("%s: %s: %s", r.c.id, r.verdict, r.why)
		}
	}
}

// Every denied target shut with no policy proves nothing about the policy.
// Only metadata says why its target may be shut, so the rest fail the run.
func TestEveryDeniedCaseUnprovenFailsTheRun(t *testing.T) {
	f := newFake(t)
	s := newSuite(t, f)
	good := f.answer
	f.answer = func(policy string, env, args []string, nth int) outcome {
		if policy == noPolicy {
			c := caseFor(t, s, policy, env, args)
			if c.want == expectDeny && !usesHostSvc(c) {
				return c.mode
			}
		}
		return good(policy, env, args, nth)
	}
	rep := run(t, s)
	if !rep.failed() {
		t.Fatalf("run passed with no denied case proven:\n%s", rep.markdown())
	}
	for _, r := range rep.rows {
		if r.c.want != expectDeny || usesHostSvc(r.c) {
			continue
		}
		want := fail
		if r.c.unprovenWhy != "" {
			want = unproven
		}
		if r.verdict != want {
			t.Errorf("%s: %s (%s), want %s", r.c.id, r.verdict, r.why, want)
		}
	}
}

// An interrupted run measured nothing worth keeping, and the default record
// path is the committed record for that runtime version.
func TestInterruptedRunWritesNoRecord(t *testing.T) {
	f := newFake(t)
	s := newSuite(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var late []string
	s.exec = func(ctx context.Context, env []string, name string, args ...string) (string, string, int, error) {
		if err := ctx.Err(); err != nil {
			late = append(late, strings.Join(args, " "))
			return "", "", -1, fmt.Errorf("%s: %w", name, err)
		}
		return f.exec(ctx, env, name, args...)
	}
	good := f.answer
	f.answer = func(policy string, env, args []string, nth int) outcome {
		if policy == denyPolicy {
			cancel()
		}
		return good(policy, env, args, nth)
	}
	record := filepath.Join(t.TempDir(), "record.md")
	const committed = "# the committed record\n"
	if err := os.WriteFile(record, []byte(committed), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	if code := runSuite(ctx, s, record, &out, &errOut); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if got, err := os.ReadFile(record); err != nil || string(got) != committed {
		t.Fatalf("record is now %q (%v), want it untouched", got, err)
	}
	if !strings.Contains(out.String(), "interrupted, no record written") {
		t.Errorf("output does not say the run was interrupted:\n%s", out.String())
	}
	for _, call := range late {
		if strings.HasPrefix(call, "policy attach") || strings.HasPrefix(call, "run ") {
			t.Errorf("brig %s was tried after the interrupt", call)
		}
	}
	if f.running || f.attached != "" {
		t.Errorf("left running=%v attached=%q", f.running, f.attached)
	}
}

// Each of these ends a run early, and each has to go through the cleanup. A
// signal the suite does not catch kills the test binary.
func TestStopSignalsCancelTheRun(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		ctx, stop := signal.NotifyContext(context.Background(), stopSignals...)
		if err := syscall.Kill(os.Getpid(), sig); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Errorf("%v did not cancel the run", sig)
		}
		stop()
	}
}

// A sandbox brig rm could not remove is still up, and brig finds it only
// through the suite's state. That state stays, and the suite says how to
// finish the cleanup.
func TestFailedCleanupKeepsTheStateAndPrintsTheCommand(t *testing.T) {
	f := newFake(t)
	f.rmFails = true
	s := newSuite(t, f)
	var log strings.Builder
	s.log = &log
	rep := run(t, s)
	t.Cleanup(func() { os.RemoveAll(s.root) })
	if _, err := os.Stat(filepath.Join(s.root, "state")); err != nil {
		t.Fatalf("the suite's state is gone after a failed brig rm: %v", err)
	}
	if !rep.failed() {
		t.Fatal("run passed with a sandbox left behind")
	}
	want := "BRIG_STATE_DIR=" + filepath.Join(s.root, "state")
	if !strings.Contains(log.String(), want) || !strings.Contains(log.String(), " brig rm "+profileName) {
		t.Fatalf("log does not give the brig rm to run by hand with %s:\n%s", want, log.String())
	}
}

// The listener takes connections on the host's outbound address alone, and a
// clean run leaves no state behind.
func TestCleanRunRemovesItsState(t *testing.T) {
	f := newFake(t)
	s := newSuite(t, f)
	rep := run(t, s)
	if rep.failed() {
		t.Fatalf("run failed:\n%s", rep.markdown())
	}
	if _, err := os.Stat(s.root); !os.IsNotExist(err) {
		t.Fatalf("the suite's state is still there after a clean run: %v", err)
	}
	if host, _, _ := strings.Cut(rep.svc, ":"); host != "127.0.0.1" {
		t.Errorf("listener at %s, want the host address", rep.svc)
	}
}

// The alternate resolver case passes on silence. A deny boot whose UDP path
// is down gets that silence too, so a UDP control has to reach its resolver
// in the same boot.
func TestUDPControlLostFailsTheAlternateResolverCase(t *testing.T) {
	f := newFake(t)
	s := newSuite(t, f)
	good := f.answer
	f.answer = func(policy string, env, args []string, nth int) outcome {
		if policy == denyPolicy && caseFor(t, s, policy, env, args).id == "allowed-resolver" {
			return timedOut
		}
		return good(policy, env, args, nth)
	}
	rep := run(t, s)
	for _, r := range rep.rows {
		if r.c.id == "alternate-resolver" && r.verdict != fail {
			t.Fatalf("alternate-resolver: %s (%s) with the boot's UDP control lost", r.verdict, r.why)
		}
	}
	if !rep.failed() {
		t.Fatal("run passed with the UDP control lost")
	}
	c := caseByID(t, "allowed-resolver")
	if c.want != expectReach || c.policy != denyPolicy || c.args[0] != "dns" {
		t.Fatalf("allowed-resolver is %+v, want a UDP control in the deny boot", c)
	}
}

// brig restarting the sandbox starts its gateway over. The controls run again
// on the restarted sandbox, and the case runs again after them.
func TestControlsRunAgainAfterARestart(t *testing.T) {
	f := newFake(t)
	f.restartOn = "doh https://8.8.8.8/dns-query git.kernel.org"
	s := newSuite(t, f)
	rep := run(t, s)
	if rep.failed() {
		t.Fatalf("run failed:\n%s", rep.markdown())
	}
	var sh []string
	for _, c := range f.brigCalls("sh") {
		sh = append(sh, strings.Join(c[3:], " "))
	}
	// The first run of the case is in the boot with no policy, whose
	// controls are the deny boot's.
	want := []string{guestProbe + " " + f.restartOn}
	for _, c := range cases {
		if c.want == expectReach && c.policy == denyPolicy {
			want = append(want, guestProbe+" "+strings.Join(c.args, " "))
		}
	}
	want = append(want, want[0])
	i := slices.Index(sh, want[0])
	if i < 0 || i+len(want) > len(sh) || !slices.Equal(sh[i:i+len(want)], want) {
		t.Fatalf("after the restart, want %q; got %q", want, sh)
	}
}

// A control that fails on the restarted sandbox fails the boot, the same as
// one that fails at either end.
func TestControlLostAfterARestartFailsTheBoot(t *testing.T) {
	f := newFake(t)
	f.restartOn = "doh https://8.8.8.8/dns-query git.kernel.org"
	f.restartIn = allowPolicy
	s := newSuite(t, f)
	good := f.answer
	// Only the first probe after the restart fails, so the controls at
	// either end of the boot still reach their targets.
	after := 0
	f.answer = func(policy string, env, args []string, nth int) outcome {
		if f.restarted {
			after++
		}
		if after == 1 && args[1] == "example.com:443" {
			return timedOut
		}
		return good(policy, env, args, nth)
	}
	rep := run(t, s)
	if !rep.failed() {
		t.Fatal("run passed with a control lost after the restart")
	}
	if rep.boots[allowPolicy].controlsOK {
		t.Fatal("the allow boot kept its controls after one failed on the restarted sandbox")
	}
}

// The record names what booted: the image by digest, the hvi binary and the
// boot assets.
func TestRecordNamesWhatBooted(t *testing.T) {
	f := newFake(t)
	s := newSuite(t, f)
	out := run(t, s).markdown()
	for _, want := range []string{
		"| image digest | `" + fakeDigest + "` |",
		fakeHVI + ", sha256:",
		fakeAssets + ", signature verified against sha256:0e8f69d1ff8a",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("record lacks %q:\n%s", want, out)
		}
	}
}

// The image goes into the profile as one YAML string, whatever it holds.
func TestImageIsOneYAMLString(t *testing.T) {
	f := newFake(t)
	s := newSuite(t, f)
	s.image = "evil:latest\nbinary: /bin/rm"
	root := t.TempDir()
	if _, err := s.prepare(root); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "profiles", profileName+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := yaml.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	if p["image"] != s.image || p["binary"] != "sh" {
		t.Fatalf("profile reads back as %v", p)
	}
}

// brig restarts a running sandbox for three reasons, each with its own
// warning. Every one of them starts the gateway over.
func TestRestartIsSeenWhateverTheReason(t *testing.T) {
	for want, warning := range map[string]string{
		"its network or its rules changed": "brig: this sandbox is running under a different network policy than the one that applies now\n" +
			"brig:   ↳ rules are fixed when a sandbox boots, so brig restarts it\n" +
			"brig:   ↳ any other session using this sandbox will be disconnected\n",
		"its project changed": "brig: the running sandbox has /a mounted as its project and this run names /b\n" +
			"brig:   ↳ a share cannot be attached to a live sandbox, so brig restarts it\n" +
			"brig:   ↳ any other session using this sandbox will be disconnected\n",
	} {
		f := newFake(t)
		f.restartOn = "doh https://8.8.8.8/dns-query git.kernel.org"
		f.restartIn = allowPolicy
		f.restartWarning = warning
		s := newSuite(t, f)
		out := run(t, s).markdown()
		if !strings.Contains(out, "restarted the sandbox on allow/dns-over-https, saying "+want) {
			t.Errorf("record does not name the restart for %q:\n%s", want, out)
		}
		if n := f.shCount[allowPolicy+" tcp example.com:443"]; n != 3 {
			t.Errorf("allow/allowed-name ran %d times, want 3: first, after the restart and last", n)
		}
	}
}
