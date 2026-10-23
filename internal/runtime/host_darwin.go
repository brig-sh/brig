package runtime

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/brig-sh/brig/internal/proc"
	"github.com/brig-sh/brig/internal/telemetry"
)

// host runs the agent as a process on this Mac, under sandbox-exec. There is
// no guest: a sandbox is a Seatbelt profile and a record in ~/.brig/host/<name>,
// and every exec is a new process under that profile.
type host struct {
	bin string
}

// newHost takes a named binary only when it is called sandbox-exec. A
// BRIG_RUNTIME_BIN meant for hull would otherwise become the sandbox, and
// run the agent with no profile at all.
func newHost(bin string) (Runtime, error) {
	if bin != "" {
		if filepath.Base(bin) != "sandbox-exec" {
			return nil, fmt.Errorf("%w: the host kind drives sandbox-exec, and the binary named "+
				"is %s. Unset BRIG_RUNTIME_BIN, or name a sandbox-exec", ErrBadRuntime, bin)
		}
		return &host{bin: bin}, nil
	}
	if p, err := exec.LookPath("sandbox-exec"); err == nil {
		return &host{bin: p}, nil
	}
	return nil, fmt.Errorf("%w: the host kind runs the agent under sandbox-exec, and "+
		"none was on PATH. It ships with macOS as /usr/bin/sandbox-exec", ErrNoRuntime)
}

func (h *host) Kind() string { return HostKind }
func (h *host) Bin() string  { return h.bin }

// Isolation is a process whatever the profile says about a hypervisor.
func (h *host) Isolation(string) Isolation { return hostIsolation() }

func (h *host) PinsDigest() bool { return false }

func (h *host) LocalDigest(ref string) (string, error) {
	return "", fmt.Errorf("the host kind runs the profile's binary from PATH, not an image, "+
		"so it holds no digest for %s", ref)
}

// CanRun is hostSupports for a caller that has not started anything yet. See
// RunChecker.
func (h *host) CanRun(spec RunSpec) error { return hostSupports(spec) }

// instance is one host sandbox's directory, opened through os.Root.
type instance struct {
	root *os.Root
	// dir is the real path of the directory, for the profile and the denies.
	dir string
}

func (i *instance) Close() error { return i.root.Close() }

func (i *instance) profilePath() string { return filepath.Join(i.dir, "profile.sb") }

// openHostRoot opens ~/.brig/host, creating it when create is set. It is
// refused when it is a link: every write below goes through this root, and a
// root opened through a link would hold whatever the link names.
func openHostRoot(create bool) (*os.Root, string, error) {
	dir, err := hostDir()
	if err != nil {
		return nil, "", err
	}
	if err := CheckHostDirLinks(dir); err != nil {
		return nil, "", err
	}
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, "", fmt.Errorf("cannot create %s: %w", dir, err)
		}
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, "", err
	}
	if !info.IsDir() {
		return nil, "", fmt.Errorf("refusing %s: it is not a directory", dir)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(real)
	if err != nil {
		return nil, "", err
	}
	return root, real, nil
}

// openInstance opens the directory of the named sandbox. A sandbox that was
// never run is reported with fs.ErrNotExist.
func openInstance(name string, create bool) (*instance, error) {
	if err := hostName(name); err != nil {
		return nil, err
	}
	top, dir, err := openHostRoot(create)
	if err != nil {
		return nil, err
	}
	defer func() { _ = top.Close() }()
	if create {
		if err := top.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	info, err := top.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("refusing %s: it is not a directory", filepath.Join(dir, name))
	}
	root, err := top.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	return &instance{root: root, dir: filepath.Join(dir, name)}, nil
}

// record reads spec.json. A sandbox without one was never run.
func (i *instance) record() (hostRecord, error) {
	blob, err := i.root.ReadFile("spec.json")
	if err != nil {
		return hostRecord{}, err
	}
	return parseRecord(blob)
}

// event appends one line to events.log, which is what `brig logs` shows.
func (i *instance) event(format string, a ...any) {
	f, err := i.root.OpenFile("events.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, a...))
}

// realShare checks one share's host path and returns its real path.
//
// The process reads the share by its path, so a link on the way to it decides
// what the agent's home or project is. A component with .. is refused, and so
// is a link at or below the first directory you can write: the agent runs as
// you and could have put it there. A link above that, such as /tmp or /var,
// is the system's and is followed. The real path is then opened as an
// os.Root and compared with the path, so a swap since the walk is refused.
func realShare(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("refusing the share %s: it is not an absolute path", p)
	}
	if slices.Contains(strings.Split(filepath.ToSlash(p), "/"), "..") {
		return "", fmt.Errorf("refusing the share %s: it names a parent with ..", p)
	}
	trusted := true
	walked := string(filepath.Separator)
	for _, c := range strings.Split(strings.Trim(filepath.ToSlash(p), "/"), "/") {
		if c == "" || c == "." {
			continue
		}
		if trusted {
			if info, err := os.Stat(walked); err == nil && hostWritable(walked, info) {
				trusted = false
			}
		}
		next := filepath.Join(walked, c)
		info, err := os.Lstat(next)
		if err != nil {
			return "", fmt.Errorf("refusing the share %s: %w", p, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if !trusted {
				target, _ := os.Readlink(next)
				return "", fmt.Errorf("refusing the share %s: %s is a link to %s, in a "+
					"directory the agent can write. Name the real directory instead", p, next, target)
			}
			resolved, err := filepath.EvalSymlinks(next)
			if err != nil {
				return "", fmt.Errorf("refusing the share %s: %w", p, err)
			}
			next = resolved
		}
		walked = next
	}
	root, err := os.OpenRoot(walked)
	if err != nil {
		return "", fmt.Errorf("refusing the share %s: %w", p, err)
	}
	defer func() { _ = root.Close() }()
	held, err := root.Stat(".")
	if err != nil {
		return "", err
	}
	byPath, err := os.Stat(p)
	if err != nil || !os.SameFile(held, byPath) {
		return "", fmt.Errorf("refusing the share %s: it stopped naming %s while brig was "+
			"checking it", p, walked)
	}
	return walked, nil
}

// hostWritable reports whether you can write the directory, and so whether an
// entry in it could have been swapped. The owner always can. For anyone else
// access(2) answers, and a failure other than EACCES or EROFS counts as
// writable, which is the safe direction.
func hostWritable(dir string, info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) == os.Getuid() {
		return true
	}
	err := syscall.Access(dir, 0x2)
	return err == nil || (!errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EROFS))
}

// Run writes the sandbox: its profile and its record. Nothing starts here;
// the agent starts at Replace, under the profile written now.
func (h *host) Run(spec RunSpec) error {
	if err := hostSupports(spec); err != nil {
		return err
	}
	rec := hostRecord{Name: spec.Name, Net: orDefault(spec.Net, "isolated"), LoopbackPorts: spec.LoopbackPorts}
	inst, err := openInstance(spec.Name, true)
	if err != nil {
		return err
	}
	defer func() { _ = inst.Close() }()
	stateDir := filepath.Dir(inst.dir)
	protect, err := hostProtect(stateDir, spec.Protect)
	if err != nil {
		return err
	}
	var shares []string
	for _, s := range spec.Shares {
		real, err := realShare(s.Host)
		if err != nil {
			return err
		}
		for _, d := range append([]string{stateDir}, protect...) {
			if real == d || strings.HasPrefix(d, real+"/") {
				return fmt.Errorf("refusing the share %s: it holds %s, which brig reads and a "+
					"host sandbox must not write. Name a directory that does not contain it", s.Host, d)
			}
		}
		rec.Shares = append(rec.Shares, hostShare{Guest: s.Guest, Host: real})
		shares = append(shares, real)
	}
	profile, err := sandboxProfile(sandboxPolicy{
		Net:           rec.Net,
		Filtered:      spec.Egress.Filtered(),
		LoopbackPorts: spec.LoopbackPorts,
		StateDir:      stateDir,
		SecretDir:     filepath.Join(inst.dir, "secret"),
		Protect:       protect,
		Shares:        shares,
		Ancestors:     renamableAncestors(append([]string{stateDir}, protect...)),
	})
	if err != nil {
		return err
	}
	blob, err := marshalRecord(rec)
	if err != nil {
		return err
	}
	if err := inst.root.WriteFile("profile.sb", []byte(profile), 0o600); err != nil {
		return err
	}
	if err := inst.root.WriteFile("spec.json", blob, 0o600); err != nil {
		return err
	}
	if err := inst.root.MkdirAll("pids", 0o700); err != nil {
		return err
	}
	inst.event("run: network %s, loopback ports %v", rec.Net, rec.LoopbackPorts)
	return nil
}

// hostProtect is the real path of every directory of brig's own the agent
// must not write: the state directory above ~/.brig/host, the gateway
// directory, and the ones the caller names.
func hostProtect(stateDir string, named []string) ([]string, error) {
	dirs := []string{filepath.Dir(stateDir)}
	if gw, err := gatewayDir(); err == nil {
		dirs = append(dirs, gw)
	}
	var out []string
	for _, d := range append(dirs, named...) {
		if d == "" {
			continue
		}
		real, err := realPath(d)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, real) {
			out = append(out, real)
		}
	}
	return out, nil
}

// realPath resolves the links in the part of p that exists and keeps the rest
// as written. A directory brig has not created yet is still denied by the
// path it will have.
func realPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rest := ""
	for dir := abs; ; dir = filepath.Dir(dir) {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, rest), nil
		}
		if dir == filepath.Dir(dir) {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(dir), rest)
	}
}

// renamableAncestors lists the directories above dirs that you can rename,
// which is every one whose parent you can write.
func renamableAncestors(dirs []string) []string {
	var out []string
	for _, d := range dirs {
		for a := filepath.Dir(d); a != filepath.Dir(a); a = filepath.Dir(a) {
			parent := filepath.Dir(a)
			info, err := os.Stat(parent)
			if err == nil && !hostWritable(parent, info) {
				continue
			}
			if !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}
	return out
}

// command is the argv and environment of one exec in the named sandbox.
//
// internal says whether brig runs the command for itself, such as the read of
// the workspace marker. Those name guest paths in their arguments, which are
// mapped to the host. The agent's own words are passed as they are.
func (h *host) command(spec ExecSpec, internal bool) (*instance, []string, []string, error) {
	if len(spec.Cmd) == 0 {
		return nil, nil, nil, errors.New("no command to run in the host sandbox")
	}
	if spec.User != "" {
		if u, err := user.Current(); err != nil || u.Username != spec.User {
			return nil, nil, nil, fmt.Errorf("the host kind runs every command as you, so it "+
				"cannot run one as %s", spec.User)
		}
	}
	inst, err := openInstance(spec.Name, false)
	if err != nil {
		return nil, nil, nil, h.notRunning(spec.Name, err)
	}
	rec, err := inst.record()
	if err != nil {
		_ = inst.Close()
		return nil, nil, nil, h.notRunning(spec.Name, err)
	}
	cmd := spec.Cmd
	if internal {
		cmd = make([]string, len(spec.Cmd))
		for i, a := range spec.Cmd {
			cmd[i] = hostPath(rec.Shares, a)
		}
	}
	argv := hostArgv(h.bin, inst.profilePath(), hostPath(rec.Shares, spec.Cwd), cmd)
	return inst, argv, hostEnv(spec.Env, rec.Shares), nil
}

func (h *host) notRunning(name string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("the host sandbox %s has not been run; start it with brig run", name)
	}
	return fmt.Errorf("the host sandbox %s: %w", name, err)
}

// call runs one exec to completion within agentCallTimeout.
func (h *host) call(spec ExecSpec, stdin io.Reader, stdout io.Writer) error {
	inst, argv, env, err := h.command(spec, true)
	if err != nil {
		return err
	}
	_ = inst.Close()
	ctx, cancel := context.WithTimeout(context.Background(), agentCallTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = agentCallWaitDelay
	cmd.Env = env
	cmd.Stdin, cmd.Stdout = stdin, stdout
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, firstLines(msg, 3))
		}
		return err
	}
	return nil
}

func (h *host) Probe(spec ExecSpec) bool { return h.call(spec, nil, nil) == nil }

func (h *host) Output(spec ExecSpec) (string, error) {
	var out bytes.Buffer
	if err := h.call(spec, nil, &out); err != nil {
		return "", err
	}
	return out.String(), nil
}

// Feed streams spec.Stdin whole. A pipe into a local process has no frame
// limit, so the host kind is no FeedLimiter.
func (h *host) Feed(spec ExecSpec) error { return h.call(spec, spec.Stdin, nil) }

// Replace becomes the agent. The pid is recorded first: exec keeps it, so the
// record names the agent once it runs.
func (h *host) Replace(spec ExecSpec) error {
	inst, argv, env, err := h.command(spec, false)
	if err != nil {
		return err
	}
	defer func() { _ = inst.Close() }()
	pid := os.Getpid()
	start, err := processStart(pid)
	if err != nil {
		return err
	}
	pidFile := filepath.Join("pids", strconv.Itoa(pid))
	if err := inst.root.WriteFile(pidFile, []byte(pidLine(pid, start)), 0o600); err != nil {
		return err
	}
	inst.event("exec: pid %d, %s", pid, firstOr(spec.Cmd, "a shell"))
	telemetry.Handover()
	if err := execHandover(argv[0], argv, env); err != nil {
		telemetry.HandoverFailed()
		_ = inst.root.Remove(pidFile)
		return err
	}
	return nil
}

// Attach runs the same command as a child of brig, and records its pid while
// it runs, so `brig stop` reaches it and `brig rm` waits for it.
func (h *host) Attach(spec ExecSpec) (int, error) {
	inst, argv, env, err := h.command(spec, false)
	if err != nil {
		return 0, err
	}
	defer func() { _ = inst.Close() }()
	var pidFile string
	status, err := attachStarted(argv, env, func(pid int) {
		start, err := processStart(pid)
		if err != nil {
			return
		}
		pidFile = filepath.Join("pids", strconv.Itoa(pid))
		_ = inst.root.WriteFile(pidFile, []byte(pidLine(pid, start)), 0o600)
		inst.event("exec: pid %d, %s", pid, firstOr(spec.Cmd, "a shell"))
	})
	if pidFile != "" {
		_ = inst.root.Remove(pidFile)
	}
	return status, err
}

// processStart is the start time the kernel keeps for pid. The brig that
// stops an agent can run under another locale than the one that ran it, so
// never the text ps prints.
func processStart(pid int) (string, error) { return proc.StartTime(pid) }

// livePids returns the recorded pids that are still the process they were
// recorded for, and drops the files of the rest.
func (i *instance) livePids() []int {
	entries, err := fs.ReadDir(i.root.FS(), "pids")
	if err != nil {
		return nil
	}
	var live []int
	for _, e := range entries {
		name := filepath.Join("pids", e.Name())
		blob, err := i.root.ReadFile(name)
		if err != nil {
			continue
		}
		f, start, _ := strings.Cut(strings.TrimSpace(string(blob)), " ")
		pid, err := strconv.Atoi(f)
		if err == nil && pid > 1 {
			if now, err := processStart(pid); err == nil && now != "" && now == start {
				live = append(live, pid)
				continue
			}
		}
		_ = i.root.Remove(name)
	}
	return live
}

// Running reports whether an agent this sandbox started is still alive.
func (h *host) Running(name string) (bool, error) {
	inst, err := openInstance(name, false)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = inst.Close() }()
	return len(inst.livePids()) > 0, nil
}

func (h *host) Exists(name string) (bool, error) {
	inst, err := openInstance(name, false)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = inst.Close() }()
	if _, err := inst.record(); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// SandboxNetwork is the posture recorded at Run.
func (h *host) SandboxNetwork(name string) (string, error) {
	inst, err := openInstance(name, false)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = inst.Close() }()
	rec, err := inst.record()
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return rec.Net, nil
}

func (h *host) List() ([]Instance, error) {
	top, _, err := openHostRoot(false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = top.Close() }()
	entries, err := fs.ReadDir(top.FS(), ".")
	if err != nil {
		return nil, err
	}
	var list []Instance
	for _, e := range entries {
		if !e.IsDir() || hostName(e.Name()) != nil {
			continue
		}
		running, err := h.Running(e.Name())
		if err != nil {
			continue
		}
		state := "stopped"
		if running {
			state = "running"
		}
		list = append(list, Instance{Name: e.Name(), State: state})
	}
	return list, nil
}

// Stop ends every agent the sandbox started, and every process below it.
// The sandbox stays, and the next run starts under the same profile.
func (h *host) Stop(name string) error {
	inst, err := openInstance(name, false)
	if err != nil {
		return h.notRunning(name, err)
	}
	defer func() { _ = inst.Close() }()
	for _, pid := range inst.livePids() {
		if !stopTree(pid) {
			return fmt.Errorf("pid %d of the host sandbox %s would not stop", pid, name)
		}
		inst.event("stop: pid %d", pid)
	}
	_ = inst.livePids()
	return nil
}

// hostProcRow is one row of the process table: its parent and its start time.
type hostProcRow struct {
	ppid  int
	start string
}

// processTable reads every process with its parent and start time.
func processTable() (map[int]hostProcRow, error) {
	all, err := proc.Table()
	if err != nil {
		return nil, err
	}
	table := make(map[int]hostProcRow, len(all))
	for pid, e := range all {
		table[pid] = hostProcRow{ppid: e.PPID, start: e.Start}
	}
	return table, nil
}

// stopWalks bounds how often stopTree reads the process table while it
// freezes a tree that keeps forking.
const stopWalks = 50

// stopTree stops root and its descendants with SIGSTOP, kills them with
// SIGKILL, and reports whether all of them are gone.
//
// A stopped process cannot fork, so the table is read again until a walk
// finds no process it has not stopped. No SIGTERM goes first: a handler
// could start a process the walk never sees, and an agent that dies first
// leaves its children to launchd, where nothing names them as its any more. Only the tree is signalled, never a process group, which can
// hold processes outside the sandbox. A process counts as gone when its pid
// no longer carries the start time it had, so a reused pid is never
// signalled.
func stopTree(root int) bool {
	tree := map[int]string{}
	for walk := 0; walk < stopWalks; walk++ {
		table, err := processTable()
		if err != nil {
			return false
		}
		added := false
		seen := map[int]bool{}
		queue := []int{root}
		for pid := range tree {
			queue = append(queue, pid)
		}
		for len(queue) > 0 {
			pid := queue[0]
			queue = queue[1:]
			row, ok := table[pid]
			if !ok || seen[pid] {
				continue
			}
			seen[pid] = true
			if start, known := tree[pid]; known && start != row.start {
				continue
			} else if !known {
				tree[pid] = row.start
				_ = syscall.Kill(pid, syscall.SIGSTOP)
				added = true
			}
			for child, r := range table {
				if r.ppid == pid {
					queue = append(queue, child)
				}
			}
		}
		if !added {
			break
		}
	}
	alive := func() []int {
		now, err := processTable()
		if err != nil {
			return nil
		}
		var left []int
		for pid, start := range tree {
			if r, ok := now[pid]; ok && r.start == start {
				left = append(left, pid)
			}
		}
		return left
	}
	for _, pid := range alive() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	deadline := time.Now().Add(gatewayStopTimeout)
	for time.Now().Before(deadline) && len(alive()) > 0 {
		time.Sleep(gatewayPollInterval)
	}
	return len(alive()) == 0
}

// Remove deletes the sandbox's directory. A sandbox with an agent still alive
// is refused, the way a runtime refuses to remove a running instance.
func (h *host) Remove(name string) error {
	inst, err := openInstance(name, false)
	if err != nil {
		return h.notRunning(name, err)
	}
	live := inst.livePids()
	_ = inst.Close()
	if len(live) > 0 {
		return fmt.Errorf("the host sandbox %s is still running (pid %d); stop it first", name, live[0])
	}
	top, _, err := openHostRoot(false)
	if err != nil {
		return err
	}
	defer func() { _ = top.Close() }()
	return top.RemoveAll(name)
}

// Logs prints events.log: when the profile was written, and which pid ran
// what. What the sandbox denied is in the system log; see LogsHint.
func (h *host) Logs(spec LogsSpec) error {
	inst, err := openInstance(spec.Name, false)
	if err != nil {
		return h.notRunning(spec.Name, err)
	}
	defer func() { _ = inst.Close() }()
	f, err := inst.root.Open("events.log")
	if err != nil {
		return h.notRunning(spec.Name, err)
	}
	defer func() { _ = f.Close() }()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if spec.Tail >= 0 && len(lines) > spec.Tail {
		lines = lines[len(lines)-spec.Tail:]
	}
	for _, l := range lines {
		if _, err := fmt.Fprintln(spec.Out, l); err != nil {
			return err
		}
	}
	if !spec.Follow {
		return nil
	}
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			if _, werr := io.WriteString(spec.Out, line); werr != nil {
				return werr
			}
		}
		if err != nil {
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// LogsHint names the system log, where Seatbelt reports what it denied.
func (h *host) LogsHint(string) string {
	return `log show --last 10m --style compact --predicate 'sender == "Sandbox"'`
}

// firstOr is the command's name for the event log. The arguments stay out of
// it: a prompt is the agent's business, not a line in a file under ~/.brig.
func firstOr(cmd []string, fallback string) string {
	if len(cmd) == 0 {
		return fallback
	}
	return cmd[0]
}
