package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"sigs.k8s.io/yaml"
)

const (
	// defaultImage carries curl, which the proxy case needs, and boots on
	// hvi as a plain OCI image.
	defaultImage = "ghcr.io/brig-sh/claude-code-stock:root"
	profileName  = "egress-conformance"
	// sandboxName is the runtime's name for the profile's sandbox.
	sandboxName = "brig-" + profileName
	// The probe goes into the guest as the run's project, which brig mounts
	// at /work/<basename>.
	projectBase = "netprobe"
	guestProbe  = "/work/" + projectBase + "/netprobe"
)

// A boot pulls the image on a first run. A probe can wait out a sandbox
// restart, which takes about as long as a boot.
const (
	bootTimeout  = 10 * time.Minute
	probeTimeout = 3 * time.Minute
	rmTimeout    = 2 * time.Minute
	// maxRechecks is how many times one case runs again after brig
	// restarted the sandbox under it.
	maxRechecks = 3
)

// brigNotFound is brig's exit status for a sandbox that is not there, which
// brig rm gives when there is nothing to remove.
const brigNotFound = 3

// execFunc runs one command and returns what it printed and its exit status.
// err is set only when the command did not run to an exit: it was not found,
// or the deadline killed it.
type execFunc func(ctx context.Context, env []string, name string, args ...string) (stdout, stderr string, code int, err error)

type suite struct {
	brig    string
	runtime string
	probe   string
	image   string
	backend string
	exec    execFunc
	now     func() time.Time
	log     io.Writer
	// hostAddr is the host's outbound address, where the guest finds a
	// service bound on the host. The gateway address does not lead there.
	hostAddr func() (netip.Addr, error)
	lookup   func(ctx context.Context, name string) (netip.Addr, error)
	// hviPath finds the hvi the runtime boots. hull runs the first one on
	// PATH.
	hviPath func() (string, error)
	env     []string

	root     string
	svc      string
	literals map[string]netip.Addr
	accepted atomic.Int64
	// stranded holds the cleanup commands that failed, to be run by hand.
	stranded []string
}

type bootResult struct {
	policy     string
	err        string
	obs        map[string]observation
	controlsOK bool
	notes      []string
	accepted   int
	// hits is what the host listener accepted while each case ran.
	hits map[string]int
	// digest is the image the runtime booted, by digest.
	digest string
}

type row struct {
	c       testCase
	with    observation
	without observation
	verdict verdict
	why     string
}

type report struct {
	backend string
	runtime string
	brig    string
	// brigBin and root are the brig binary and the suite's state, which the
	// record leaves out.
	brigBin string
	root    string
	image   string
	hvi     string
	assets  string
	date    time.Time
	svc     string
	host    string
	// problems are failures of the run as a whole, apart from any case.
	problems []string
	boots    map[string]*bootResult
	rows     []row
}

func (r *report) failed() bool {
	if len(r.problems) > 0 || len(r.rows) == 0 {
		return true
	}
	for _, row := range r.rows {
		if row.verdict == fail {
			return true
		}
	}
	return false
}

func (s *suite) printf(format string, a ...any) {
	fmt.Fprintf(s.log, format+"\n", a...)
}

// do runs the whole suite. Every way it can go wrong ends in a report that
// fails. There is no skip: a release checklist reads a skip as a pass.
func (s *suite) do(ctx context.Context) *report {
	rep := &report{backend: s.backend, image: s.image, date: s.now().UTC(), boots: map[string]*bootResult{}, brigBin: s.brig}
	problem := func(format string, a ...any) *report {
		rep.problems = append(rep.problems, fmt.Sprintf(format, a...))
		return rep
	}
	if s.backend != "hvi" {
		return problem("backend %q: this runner boots the hvi backend only", s.backend)
	}

	out, stderr, code, err := s.exec(ctx, s.env, s.runtime, "--version")
	rep.runtime = firstLine(out)
	if err != nil || code != 0 {
		return problem("no runtime: %s --version: %s", s.runtime, failure(stderr, code, err))
	}
	if versionOf(rep.runtime) == "" {
		return problem("no runtime: %s --version printed %q, which names no version", s.runtime, rep.runtime)
	}
	out, stderr, code, err = s.exec(ctx, s.env, s.brig, "version")
	rep.brig = firstLine(out)
	if err != nil || code != 0 || rep.brig == "" {
		return problem("brig version: %s", failure(stderr, code, err))
	}

	root, err := os.MkdirTemp("", "egress-conformance-")
	if err != nil {
		return problem("%v", err)
	}
	// brig finds a sandbox the suite booted only through the state under
	// root. A cleanup call that failed leaves that sandbox up, so root stays
	// for the commands that finish the cleanup by hand.
	defer func() {
		if len(s.stranded) == 0 {
			os.RemoveAll(root)
			return
		}
		s.printf("a cleanup call failed, so %s is kept. Finish the cleanup with:", root)
		for _, cmd := range s.stranded {
			s.printf("  %s", cmd)
		}
		s.printf("or with the runtime: %s rm -f %s", shellQuote(s.runtime), sandboxName)
		rep.problems = append(rep.problems, "a cleanup call failed, and the suite kept its state for the cleanup by hand")
	}()
	s.root, rep.root = root, root
	env, err := s.prepare(root)
	if err != nil {
		return problem("%v", err)
	}
	rep.hvi = s.hviLine(ctx)
	rep.assets = s.assetsLine(ctx, env)

	addr, err := s.hostAddr()
	if err != nil {
		return problem("the host's outbound address: %v", err)
	}
	// Bound to that address alone, so a connection from elsewhere on the
	// network does not count toward a case.
	ln, err := net.Listen("tcp4", net.JoinHostPort(addr.String(), "0"))
	if err != nil {
		return problem("host listener: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.accepted.Add(1)
			c.Close()
		}
	}()
	s.svc = ln.Addr().String()
	rep.svc, rep.host = s.svc, addr.String()

	s.literals = map[string]netip.Addr{}
	for _, c := range cases {
		for _, a := range c.args {
			if name, ok := literalName(a); ok {
				if _, done := s.literals[name]; done {
					continue
				}
				// A failed lookup leaves the name out, and each case that
				// needs it measures nothing.
				if ip, err := s.lookup(ctx, name); err == nil {
					s.literals[name] = ip
				} else {
					s.printf("host lookup of %s: %v", name, err)
				}
			}
		}
	}

	for _, p := range []string{noPolicy, denyPolicy, allowPolicy} {
		// An interrupted run boots nothing more, and judges nothing.
		if ctx.Err() != nil {
			break
		}
		rep.boots[p] = s.boot(ctx, env, p)
	}
	if ctx.Err() != nil {
		return problem("interrupted")
	}
	base := rep.boots[noPolicy]
	if base.err != "" {
		rep.problems = append(rep.problems, "the boot with no policy: "+base.err)
	} else if !base.controlsOK {
		rep.problems = append(rep.problems, "an allowed control failed in the boot with no policy")
		// A guest that reached nothing did not reach the IPv6 literal
		// either, and that says nothing about the gateway.
		for id, o := range base.obs {
			if o.invalid == "" {
				o.invalid = "an allowed control failed in this boot, " + string(o.outcome)
				base.obs[id] = o
			}
		}
	}
	// The host listener is the suite's own fixture. With no policy, each
	// host service case has to leave a connection on it, whatever the probe
	// said. A guest that cannot reach the listener leaves both cases nothing
	// to stand on, and a probe that said it got through reached something
	// else. It is counted per case, since host-service and proxy-env share
	// this boot and one's connection is no word for the other.
	if base.err == "" {
		for _, c := range cases {
			o := base.obs[c.id]
			if usesHostSvc(c) && base.hits[c.id] == 0 && o.invalid == "" {
				o.invalid = fmt.Sprintf("the probe said %s and the host listener accepted nothing while it ran", o.outcome)
				base.obs[c.id] = o
			}
		}
	}
	if n := rep.boots[denyPolicy].accepted; n > 0 {
		rep.problems = append(rep.problems, fmt.Sprintf("the host listener accepted %d connections under %s", n, denyPolicy))
	}
	for _, c := range cases {
		b := rep.boots[c.policy]
		r := row{c: c, with: b.obs[c.id], without: base.obs[c.id]}
		r.verdict, r.why = judge(c, r.with, r.without, b.controlsOK)
		rep.rows = append(rep.rows, r)
	}
	return rep
}

func failure(stderr string, code int, err error) string {
	if err != nil {
		return err.Error()
	}
	if l := lastLine(stderr); l != "" {
		return fmt.Sprintf("exit %d: %s", code, l)
	}
	return fmt.Sprintf("exit %d", code)
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(l)
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// keptSettings are the BRIG_ variables that say where the runtime and its
// assets are, and how hard to verify an image. Every other one is dropped
// before brig runs: BRIG_NETWORK=offline left in the shell would refuse every
// denied case for the policy, and the controls would be the only thing to
// notice. BRIG_RUNTIME_BIN is not kept from the shell. prepare sets it to the
// runtime whose version the record names.
var keptSettings = map[string]bool{
	"BRIG_RUNTIME": true, "BRIG_BOOT_ASSETS": true, "BRIG_BOOT_ASSETS_REF": true,
	"BRIG_GATEWAY_SOCK": true, "BRIG_GATEWAY_DIR": true,
	"BRIG_VERIFY": true, "BRIG_VERIFY_REGISTRY": true, "BRIG_VERIFY_IDENTITY": true,
	"BRIG_VERIFY_ISSUER": true, "BRIG_COSIGN_BIN": true, "BRIG_PULL": true,
}

// prepare writes the suite's own profile and policies under root, puts the
// probe where the guest finds it, and returns the environment brig runs in.
// brig's configuration and state live under root for the run, so the suite
// neither reads nor changes the ones in the user's home.
func (s *suite) prepare(root string) ([]string, error) {
	dirs := map[string]string{}
	for _, d := range []string{"profiles", "policies", "state", "home", projectBase} {
		dirs[d] = filepath.Join(root, d)
		if err := os.Mkdir(dirs[d], 0o700); err != nil {
			return nil, err
		}
	}
	probe, err := os.ReadFile(s.probe)
	if err != nil {
		return nil, fmt.Errorf("the probe: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dirs[projectBase], "netprobe"), probe, 0o755); err != nil {
		return nil, err
	}
	// No secrets, files or environment: nothing of the user's reaches this
	// guest. binary is never run, since the suite boots with -d. The profile
	// is marshaled, so an -image value is one YAML string whatever it holds.
	// Two vCPUs: on one, brig sh often reads the guest's share as stale and
	// restarts it, and every restart starts the gateway over.
	profile, err := yaml.Marshal(map[string]any{
		"name":        profileName,
		"desc":        "network conformance suite guest",
		"binary":      "sh",
		"image":       s.image,
		"hypervisor":  s.backend,
		"genericBoot": true,
		"guestHome":   "/root",
		"mem":         1024,
		"cpus":        2,
	})
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dirs["profiles"], profileName+".yaml"), profile, 0o600); err != nil {
		return nil, err
	}
	for name, doc := range policies {
		if err := os.WriteFile(filepath.Join(dirs["policies"], name+".yaml"), []byte(doc), 0o600); err != nil {
			return nil, err
		}
	}
	var env []string
	for _, kv := range s.env {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "BRIG_") && !keptSettings[k] {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"BRIG_PROFILE_DIR="+dirs["profiles"],
		"BRIG_POLICY_DIR="+dirs["policies"],
		"BRIG_STATE_DIR="+dirs["state"],
		"BRIG_WORKSPACE="+dirs["home"],
		"BRIG_HYPERVISOR="+s.backend,
		// A bare name is a PATH lookup to brig too, the one --version ran.
		"BRIG_RUNTIME_BIN="+s.runtime,
	), nil
}

// hviLine names the hvi the runtime boots. hvi --version names no build, so
// the SHA-256 of the binary goes with it.
func (s *suite) hviLine(ctx context.Context) string {
	path, err := s.hviPath()
	if err != nil {
		return "not found: " + err.Error()
	}
	cctx, cancel := context.WithTimeout(ctx, rmTimeout)
	defer cancel()
	out, stderr, code, err := s.exec(cctx, s.env, path, "--version")
	line := firstLine(out)
	if err != nil || code != 0 || line == "" {
		line = "hvi --version: " + failure(stderr, code, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return line + ", " + err.Error()
	}
	return fmt.Sprintf("%s, sha256:%x", line, sha256.Sum256(b))
}

// assetsLine names the kernel and initrd bundle the runtime boots, as the
// runtime reports it. With BRIG_BOOT_ASSETS set, brig boots a directory of
// its own, and the runtime's answer says nothing about the boot.
func (s *suite) assetsLine(ctx context.Context, env []string) string {
	for _, kv := range env {
		if strings.HasPrefix(kv, "BRIG_BOOT_ASSETS=") {
			return "a local directory, from BRIG_BOOT_ASSETS"
		}
	}
	cctx, cancel := context.WithTimeout(ctx, rmTimeout)
	defer cancel()
	out, stderr, code, err := s.exec(cctx, env, s.runtime, "assets", "show")
	if err != nil || code != 0 {
		return "not read: assets show: " + failure(stderr, code, err)
	}
	var ref, sig string
	for _, l := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(l, ":")
		switch strings.TrimSpace(k) {
		case "reference":
			ref = strings.TrimSpace(v)
		case "signature":
			sig = strings.TrimSpace(v)
		}
	}
	if ref == "" {
		return "not read: assets show names no reference"
	}
	if sig == "" {
		return ref
	}
	return ref + ", signature " + sig
}

// imageDigest asks the runtime which image the sandbox booted. The profile
// names a tag, and a tag moves.
func (s *suite) imageDigest(ctx context.Context, env []string) string {
	cctx, cancel := context.WithTimeout(ctx, rmTimeout)
	defer cancel()
	out, stderr, code, err := s.exec(cctx, env, s.runtime, "inspect", sandboxName)
	if err != nil || code != 0 {
		return "not read: inspect: " + failure(stderr, code, err)
	}
	var st struct {
		ImageDigest string `json:"imageDigest"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return "not read: inspect: " + err.Error()
	}
	if st.ImageDigest == "" {
		return "not read: inspect names no image digest"
	}
	return st.ImageDigest
}

// command is the shell line that runs brig with args against the suite's
// state. It carries the BRIG_ settings alone, since those are what point
// brig at that state.
func (s *suite) command(env []string, args ...string) string {
	parts := []string{"env"}
	for _, kv := range env {
		if strings.HasPrefix(kv, "BRIG_") {
			parts = append(parts, shellQuote(kv))
		}
	}
	parts = append(parts, shellQuote(s.brig))
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// shellQuote quotes v for a POSIX shell when it holds anything beyond the
// characters a path or a setting is usually made of.
func shellQuote(v string) string {
	plain := v != "" && strings.IndexFunc(v, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("/._-=:@,+", r))
	}) < 0
	if plain {
		return v
	}
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

func usesHostSvc(c testCase) bool {
	for _, a := range append(append([]string{}, c.env...), c.args...) {
		if strings.Contains(a, hostSvc) {
			return true
		}
	}
	return false
}

func literalName(arg string) (string, bool) {
	i := strings.Index(arg, addrOf)
	if i < 0 {
		return "", false
	}
	name, _, ok := strings.Cut(arg[i+len(addrOf):], "}")
	return name, ok
}

// fill puts the host's values into a case's placeholders. A literal the host
// could not resolve stays a placeholder, and the case is not run.
func (s *suite) fill(c testCase) (env, args []string) {
	sub := func(v string) string {
		v = strings.ReplaceAll(v, hostSvc, s.svc)
		if name, ok := literalName(v); ok {
			if ip, found := s.literals[name]; found {
				v = strings.ReplaceAll(v, addrOf+name+"}", ip.String())
			}
		}
		return v
	}
	for _, e := range c.env {
		env = append(env, sub(e))
	}
	for _, a := range c.args {
		args = append(args, sub(a))
	}
	return env, args
}

// boot runs one policy's cases on a guest of their own, or, for noPolicy,
// every case on a guest with none. The allowed controls go first and last:
// a guest that lost its network halfway would otherwise refuse the rest in
// whatever mode it failed in.
func (s *suite) boot(ctx context.Context, env []string, policy string) *bootResult {
	b := &bootResult{policy: policy, obs: map[string]observation{}, hits: map[string]int{}}
	name := policy
	if name == noPolicy {
		name = "no policy"
	}
	var run, controls []testCase
	for _, c := range cases {
		if policy != noPolicy && c.policy != policy {
			continue
		}
		run = append(run, c)
		if c.want == expectReach && (policy != noPolicy || c.policy == denyPolicy) {
			controls = append(controls, c)
		}
	}
	unrun := func(why string) *bootResult {
		b.err = why
		for _, c := range run {
			if _, done := b.obs[c.id]; !done {
				b.obs[c.id] = observation{invalid: why}
			}
		}
		return b
	}

	brig := func(timeout time.Duration, args ...string) (string, string, int, error) {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return s.exec(cctx, env, s.brig, args...)
	}
	// cleanup runs after a Ctrl-C too. The interrupt cancels ctx, and a
	// command whose context is done never starts, so the sandbox would stay
	// up with the policy attached while do deletes the directories it runs
	// from. A call that fails is kept for the cleanup by hand.
	cleanup := func(args ...string) {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rmTimeout)
		defer cancel()
		_, stderr, code, err := s.exec(cctx, env, s.brig, args...)
		if err == nil && (code == 0 || (args[0] == "rm" && code == brigNotFound)) {
			return
		}
		b.notes = append(b.notes, fmt.Sprintf("brig %s: %s", strings.Join(args, " "), failure(stderr, code, err)))
		if cmd := s.command(env, args...); !slices.Contains(s.stranded, cmd) {
			s.stranded = append(s.stranded, cmd)
		}
	}
	// A sandbox left by a killed run still carries its old rules.
	brig(rmTimeout, "rm", profileName)
	if policy != noPolicy {
		if _, stderr, code, err := brig(rmTimeout, "policy", "attach", policy, profileName); err != nil || code != 0 {
			return unrun("brig policy attach: " + failure(stderr, code, err))
		}
		defer cleanup("policy", "detach", policy, profileName)
	}
	s.printf("boot: %s", name)
	start := s.accepted.Load()
	defer func() { b.accepted = int(s.accepted.Load() - start) }()
	_, stderr, code, err := brig(bootTimeout, "run", profileName, filepath.Join(s.root, projectBase), "-d")
	// rm on every path out, a failed boot included: brig can leave a
	// sandbox behind when it refuses late.
	defer cleanup("rm", profileName)
	if err != nil || code != 0 {
		return unrun("brig run: " + failure(stderr, code, err))
	}
	b.digest = s.imageDigest(ctx, env)

	// probe runs c once and reports whether brig restarted the sandbox on
	// the way. brig restarts a sandbox whose share went stale, or whose
	// network or project changed, and says so on stderr. The rules come back
	// with it, but the gateway starts over.
	probe := func(c testCase) (observation, bool) {
		penv, args := s.fill(c)
		for _, a := range append(append([]string{}, penv...), args...) {
			if strings.Contains(a, addrOf) {
				return observation{invalid: "the host could not resolve " + a}, false
			}
		}
		argv := []string{"sh", profileName}
		if len(penv) > 0 {
			argv = append(append(argv, "env"), penv...)
		}
		argv = append(append(argv, guestProbe), args...)
		// A connection counted after brig returned lands on the next case,
		// which fails this one and passes none.
		before := s.accepted.Load()
		stdout, stderr, code, err := brig(probeTimeout, argv...)
		b.hits[c.id] += int(s.accepted.Load() - before)
		why, restarted := restartReason(stderr)
		if restarted {
			b.notes = append(b.notes, fmt.Sprintf("brig restarted the sandbox on %s, saying %s", c.id, why))
		}
		o := parseProbe(stdout, code, err)
		if o.invalid != "" && stderr != "" {
			o.invalid += ": " + lastLine(stderr)
		}
		s.printf("  %-26s %s", c.id, o.line)
		return o, restarted
	}
	control := func(c testCase, when string) bool {
		o, restarted := probe(c)
		if o.invalid == "" && o.outcome == reached {
			return restarted
		}
		b.controlsOK = false
		b.notes = append(b.notes, fmt.Sprintf("%s did not reach its target %s: %s%s", c.id, when, o.outcome, o.invalid))
		return restarted
	}
	// measure probes c. After a restart the controls run again on the
	// sandbox brig restarted, and then c runs again. c is then measured
	// between controls on the gateway that served it.
	measure := func(c testCase) observation {
		o, restarted := probe(c)
		for i := 0; restarted; i++ {
			if i == maxRechecks {
				b.controlsOK = false
				b.notes = append(b.notes, fmt.Sprintf("brig kept restarting the sandbox on %s, so no control ran on the gateway that served it", c.id))
				break
			}
			restarted = false
			for _, ctl := range controls {
				restarted = control(ctl, "after the restart on "+c.id) || restarted
			}
			if !restarted {
				o, restarted = probe(c)
			}
		}
		return o
	}

	b.controlsOK = len(controls) > 0
	for _, c := range controls {
		o := measure(c)
		b.obs[c.id] = o
		b.controlsOK = b.controlsOK && o.invalid == "" && o.outcome == reached
	}
	for _, c := range run {
		if _, done := b.obs[c.id]; !done {
			b.obs[c.id] = measure(c)
		}
	}
	for _, c := range controls {
		if o := measure(c); o.invalid != "" || o.outcome != reached {
			b.controlsOK = false
			b.notes = append(b.notes, fmt.Sprintf("%s did not reach its target at the end of the boot: %s%s",
				c.id, o.outcome, o.invalid))
		}
	}
	return b
}

// restartReason reads brig's warning that it restarted the sandbox. Every
// such warning ends with the same clause, whatever the reason.
func restartReason(stderr string) (string, bool) {
	if !strings.Contains(stderr, "any other session using this sandbox will be disconnected") {
		return "", false
	}
	switch {
	case strings.Contains(stderr, "went stale"):
		return "its share went stale", true
	case strings.Contains(stderr, "as its project"), strings.Contains(stderr, "no project mounted"):
		return "its project changed", true
	}
	return "its network or its rules changed", true
}

// outboundAddr is the address the host sends from on its default route. A
// UDP connect picks the route and sends nothing. 192.0.2.1 is TEST-NET-1.
func outboundAddr() (netip.Addr, error) {
	c, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return netip.Addr{}, err
	}
	defer c.Close()
	ua, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok {
		return netip.Addr{}, errors.New("no local UDP address")
	}
	return ua.AddrPort().Addr().Unmap(), nil
}

func lookupV4(ctx context.Context, name string) (netip.Addr, error) {
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", name)
	if err != nil {
		return netip.Addr{}, err
	}
	if len(addrs) == 0 {
		return netip.Addr{}, fmt.Errorf("%s has no IPv4 address", name)
	}
	return addrs[0].Unmap(), nil
}
