package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// hostState points the host kind at a fresh state directory and returns the
// real path of ~/.brig/host under it.
func hostState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BRIG_STATE_DIR", dir)
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(real, "host")
}

// A link the agent could have planted decides what a share is, so it is
// refused, and nothing is written for the sandbox.
func TestHostRunRefusesAShareThroughALinkOrDotDot(t *testing.T) {
	state := hostState(t)
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	home := filepath.Join(base, "home")
	for _, d := range []string{outside, home, filepath.Join(outside, "sub")} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	keep := filepath.Join(outside, "keep")
	if err := os.WriteFile(keep, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	h := &host{bin: "/usr/bin/sandbox-exec"}
	for _, tt := range []struct {
		name, share string
	}{
		{"a link as the share", link},
		{"a link on the way", link + "/sub"},
		{"dot dot", home + "/../outside"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := h.Run(RunSpec{Name: "brig-share", Net: "isolated",
				Shares: []Share{{Host: tt.share, Guest: "/root"}}})
			if err == nil || !strings.Contains(err.Error(), "refusing the share") {
				t.Fatalf("got %v, want the share refused", err)
			}
			if _, err := os.Stat(filepath.Join(state, "brig-share", "profile.sb")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a profile was written for a refused share: %v", err)
			}
		})
	}
	if blob, err := os.ReadFile(keep); err != nil || string(blob) != "untouched" {
		t.Errorf("the file outside changed: %q, %v", blob, err)
	}
}

// The system's own links above your home, such as /var on macOS, are
// followed, and the record holds the real path.
func TestRealShareFollowsTheSystemsLinks(t *testing.T) {
	dir := t.TempDir()
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := realShare(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("realShare(%s) = %s, want %s", dir, got, want)
	}
}

// Run writes the profile and a record, and the record holds no value from
// the environment.
func TestHostRunWritesNoValueToDisk(t *testing.T) {
	state := hostState(t)
	home := t.TempDir()
	h := &host{bin: "/usr/bin/sandbox-exec"}
	err := h.Run(RunSpec{Name: "brig-disk", Net: "isolated", LoopbackPorts: []int{48711},
		Shares: []Share{{Host: home, Guest: "/root"}},
		Env:    []Var{{Name: "GH_TOKEN", Value: "gh-secret-value", Secret: true}, {Name: "PLAIN", Value: "plain-value"}}})
	if err != nil {
		t.Fatal(err)
	}
	inst := filepath.Join(state, "brig-disk")
	entries, err := os.ReadDir(inst)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		blob, err := os.ReadFile(filepath.Join(inst, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range []string{"gh-secret-value", "plain-value"} {
			if strings.Contains(string(blob), v) {
				t.Errorf("%s holds %q", e.Name(), v)
			}
		}
	}
	profile, err := os.ReadFile(filepath.Join(inst, "profile.sb"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(profile), `(deny file-read* file-write* (subpath "`+state+`"))`) {
		t.Errorf("the profile does not deny %s by its real path:\n%s", state, profile)
	}
	if got := h.Isolation("").Line(); got != "process (shared kernel)" {
		t.Errorf("isolation row %q", got)
	}
	if _, err := h.LocalDigest("img"); err == nil || !strings.Contains(err.Error(), "from PATH, not an image") {
		t.Errorf("LocalDigest: %v", err)
	}
}

// The profile's volumes live in a tmpfs, which a process sandbox cannot mount.
func TestHostRunRefusesATmpfs(t *testing.T) {
	hostState(t)
	h := &host{bin: "/usr/bin/sandbox-exec"}
	if err := h.Run(RunSpec{Name: "brig-tmpfs", Net: "isolated", Tmpfs: []string{"/root/.claude:size=1m"}}); err == nil {
		t.Fatal("a tmpfs was accepted")
	}
}

// The handover becomes the agent: the pid is recorded first, the guest home is
// the host directory, and a credential travels in the environment only.
func TestHostReplaceRecordsThePidAndKeepsValuesOutOfArgv(t *testing.T) {
	state := hostState(t)
	home := t.TempDir()
	h := &host{bin: "/usr/bin/sandbox-exec"}
	if err := h.Run(RunSpec{Name: "brig-replace", Net: "isolated", Shares: []Share{{Host: home, Guest: "/root"}}}); err != nil {
		t.Fatal(err)
	}
	var gotArgv, gotEnv []string
	old := execHandover
	execHandover = func(bin string, argv, env []string) error {
		gotArgv, gotEnv = argv, env
		return nil
	}
	t.Cleanup(func() { execHandover = old })

	err := h.Replace(ExecSpec{Name: "brig-replace", Cmd: []string{"claude"}, Cwd: "/root",
		Env: []Var{{Name: "HOME", Value: "/root"}, {Name: "CLAUDE_CODE_OAUTH_TOKEN", Value: "sk-ant-secret", Secret: true}}})
	if err != nil {
		t.Fatal(err)
	}
	realHome, _ := filepath.EvalSymlinks(home)
	if !slices.Contains(gotArgv, realHome) {
		t.Errorf("argv does not start in the host home %s: %q", realHome, gotArgv)
	}
	for _, a := range gotArgv {
		if strings.Contains(a, "sk-ant-secret") {
			t.Errorf("the credential is in argv: %q", gotArgv)
		}
	}
	for _, want := range []string{"HOME=" + realHome, "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-secret"} {
		if !slices.Contains(gotEnv, want) {
			t.Errorf("env lacks %s", want)
		}
	}
	pid := strconv.Itoa(os.Getpid())
	if _, err := os.Stat(filepath.Join(state, "brig-replace", "pids", pid)); err != nil {
		t.Errorf("no pid file for %s: %v", pid, err)
	}
	if err := h.Replace(ExecSpec{Name: "brig-replace", Cmd: []string{"id"}, User: "root"}); err == nil && os.Getuid() != 0 {
		t.Error("a command as another user was accepted")
	}
}

func TestHostExecRefusesASandboxThatWasNeverRun(t *testing.T) {
	hostState(t)
	h := &host{bin: "/usr/bin/sandbox-exec"}
	if _, err := h.Output(ExecSpec{Name: "brig-never", Cmd: []string{"true"}}); err == nil ||
		!strings.Contains(err.Error(), "has not been run") {
		t.Fatalf("got %v", err)
	}
}

// recordAgent starts sh with a child of its own, records sh as an agent of
// the sandbox, and returns both pids. leader makes sh lead a process group,
// as a shell does for a job.
func recordAgent(t *testing.T, state, name string, leader bool) (agent, child int, exited chan error) {
	t.Helper()
	pidOut := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.Command("/bin/sh", "-c", `sleep 60 & echo $! > "$1"; wait`, "sh", pidOut)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: leader}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited = make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	for i := 0; i < 100; i++ {
		if blob, err := os.ReadFile(pidOut); err == nil && strings.TrimSpace(string(blob)) != "" {
			child, _ = strconv.Atoi(strings.TrimSpace(string(blob)))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if child == 0 {
		t.Fatal("the agent's child did not start")
	}
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	start, err := processStart(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(state, name, "pids", strconv.Itoa(cmd.Process.Pid))
	if err := os.WriteFile(pidFile, []byte(pidLine(cmd.Process.Pid, start)), 0o600); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid, child, exited
}

// Running follows the recorded agent, Stop ends it and what it started, and
// Remove waits for that. Both shapes: an agent that leads its process group,
// and one that does not, whose child would go to launchd if only the agent
// were signalled.
func TestHostStopEndsTheRecordedAgent(t *testing.T) {
	for _, leader := range []bool{true, false} {
		t.Run(fmt.Sprintf("group leader %v", leader), func(t *testing.T) {
			state := hostState(t)
			h := &host{bin: "/usr/bin/sandbox-exec"}
			if err := h.Run(RunSpec{Name: "brig-stop", Net: "none"}); err != nil {
				t.Fatal(err)
			}
			_, child, exited := recordAgent(t, state, "brig-stop", leader)
			// A pid recorded with another start time is some other process.
			stale := filepath.Join(state, "brig-stop", "pids", strconv.Itoa(os.Getpid()))
			if err := os.WriteFile(stale, []byte(pidLine(os.Getpid(), "Thu Jan  1 00:00:00 1970")), 0o600); err != nil {
				t.Fatal(err)
			}

			if up, err := h.Running("brig-stop"); err != nil || !up {
				t.Fatalf("Running = %v, %v with an agent alive", up, err)
			}
			if err := h.Remove("brig-stop"); err == nil {
				t.Fatal("Remove went ahead with an agent alive")
			}
			if err := h.Stop("brig-stop"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-exited:
			case <-time.After(10 * time.Second):
				t.Fatal("the agent is still alive after Stop")
			}
			if err := syscall.Kill(child, 0); err == nil {
				t.Errorf("the agent's child %d is still alive after Stop", child)
			}
			if up, err := h.Running("brig-stop"); err != nil || up {
				t.Errorf("Running = %v, %v after Stop", up, err)
			}
			if list, _ := h.List(); !slices.Contains(list, Instance{Name: "brig-stop", State: "stopped"}) {
				t.Errorf("List = %v", list)
			}
			if err := h.Remove("brig-stop"); err != nil {
				t.Fatal(err)
			}
			if ok, err := h.Exists("brig-stop"); err != nil || ok {
				t.Errorf("Exists = %v, %v after Remove", ok, err)
			}
		})
	}
}

// The pid file is written by the brig that runs the agent and read by the one
// that stops it, and the two can run under different locales.
func TestHostStopFindsTheAgentUnderAnotherLocale(t *testing.T) {
	state := hostState(t)
	h := &host{bin: "/usr/bin/sandbox-exec"}
	const name = "brig-stop-locale"
	if err := h.Run(RunSpec{Name: name, Net: "none"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("LC_ALL", "en_US.UTF-8")
	t.Setenv("LC_TIME", "en_US.UTF-8")
	_, child, exited := recordAgent(t, state, name, false)

	t.Setenv("LC_ALL", "el_GR.UTF-8")
	t.Setenv("LC_TIME", "el_GR.UTF-8")
	if up, err := h.Running(name); err != nil || !up {
		t.Fatalf("under another locale, Running = %v, %v with an agent alive", up, err)
	}
	if err := h.Stop(name); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("under another locale, the agent is still alive after Stop")
	}
	if err := syscall.Kill(child, 0); err == nil {
		t.Errorf("the agent's child %d is still alive after Stop", child)
	}
}

// The command line carries the command and its directory, and never a value
// from the environment. Checked on the builder every exec goes through.
func TestHostArgvCarriesNoValue(t *testing.T) {
	hostState(t)
	h := &host{bin: "/usr/bin/sandbox-exec"}
	if err := h.Run(RunSpec{Name: "brig-argv", Net: "isolated"}); err != nil {
		t.Fatal(err)
	}
	for _, internal := range []bool{true, false} {
		inst, argv, env, err := h.command(ExecSpec{Name: "brig-argv", Cmd: []string{"claude", "-p", "hi"},
			Env: []Var{{Name: "CLAUDE_CODE_OAUTH_TOKEN", Value: "sk-secret-value", Secret: true},
				{Name: "PLAIN", Value: "plain-value"}}}, internal)
		if err != nil {
			t.Fatal(err)
		}
		_ = inst.Close()
		for _, a := range argv {
			if strings.Contains(a, "sk-secret-value") || strings.Contains(a, "plain-value") {
				t.Errorf("a value is in argv: %q", argv)
			}
		}
		if !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=sk-secret-value") {
			t.Errorf("the credential is not in the environment")
		}
	}
}

// Brig's own commands name guest paths, such as the workspace marker, and are
// mapped to the host. The agent's words are not.
func TestHostCommandMapsOnlyBrigsOwnArguments(t *testing.T) {
	hostState(t)
	home := t.TempDir()
	realHome, _ := filepath.EvalSymlinks(home)
	h := &host{bin: "/usr/bin/sandbox-exec"}
	if err := h.Run(RunSpec{Name: "brig-args", Net: "isolated", Shares: []Share{{Host: home, Guest: "/root"}}}); err != nil {
		t.Fatal(err)
	}
	spec := ExecSpec{Name: "brig-args", Cmd: []string{"cat", "/root/.brig-workspace"}}
	for internal, want := range map[bool]string{true: realHome + "/.brig-workspace", false: "/root/.brig-workspace"} {
		inst, argv, _, err := h.command(spec, internal)
		if err != nil {
			t.Fatal(err)
		}
		_ = inst.Close()
		if argv[len(argv)-1] != want {
			t.Errorf("internal %v: the argument is %q, want %q", internal, argv[len(argv)-1], want)
		}
	}
	if _, _, _, err := h.command(ExecSpec{Name: "brig-args"}, false); err == nil {
		t.Error("an empty command was accepted")
	}
}

// A first word such as -l is a command that is not found, and a directory
// that cannot be entered has a status of its own.
func TestHostScriptExitStatuses(t *testing.T) {
	for _, tt := range []struct {
		dir  string
		cmd  []string
		want int
	}{
		{".", []string{"-l"}, 127},
		{"/nonexistent-brig-dir", []string{"true"}, 125},
		{".", []string{"true"}, 0},
	} {
		err := exec.Command("/bin/sh", append([]string{"-c", hostScript, "brig-host", tt.dir}, tt.cmd...)...).Run()
		if got := exitCode(err); got != tt.want {
			t.Errorf("%q in %s exits %d, want %d", tt.cmd, tt.dir, got, tt.want)
		}
	}
}

// A BRIG_RUNTIME_BIN meant for another runtime is not taken as the sandbox.
func TestNewHostRefusesAnotherBinary(t *testing.T) {
	if _, err := newHost("/opt/homebrew/bin/hull"); !errors.Is(err, ErrBadRuntime) {
		t.Errorf("hull was taken as sandbox-exec: %v", err)
	}
	if _, err := newHost("/usr/bin/sandbox-exec"); err != nil {
		t.Error(err)
	}
}

// A link where the state directory should be is refused, because the profile
// protects real paths.
func TestHostRefusesALinkedStateDirectory(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "state")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIG_STATE_DIR", filepath.Join(base, "state"))
	h := &host{bin: "/usr/bin/sandbox-exec"}
	if err := h.Run(RunSpec{Name: "brig-link", Net: "none"}); err == nil || !strings.Contains(err.Error(), "is a link") {
		t.Fatalf("got %v", err)
	}
}

// A share that holds brig's own directories would give the agent write
// access to them again, so it is refused.
func TestHostRunRefusesAShareHoldingBrigsDirectories(t *testing.T) {
	hostState(t)
	parent := filepath.Dir(os.Getenv("BRIG_STATE_DIR"))
	h := &host{bin: "/usr/bin/sandbox-exec"}
	err := h.Run(RunSpec{Name: "brig-wide", Net: "none", Shares: []Share{{Host: parent, Guest: "/root"}}})
	if err == nil || !strings.Contains(err.Error(), "must not write") {
		t.Fatalf("got %v", err)
	}
}

// listener counts the connections that reach it.
type listener struct {
	addr  string
	count atomic.Int32
}

func listen(t *testing.T, network, addr string) *listener {
	t.Helper()
	ln, err := net.Listen(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	l := &listener{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			l.count.Add(1)
			_ = c.Close()
		}
	}()
	return l
}

// exitCode is the exit status inside err, or -1 when there is none.
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err == nil {
		return 0
	}
	return -1
}

// waitCount waits briefly for a listener to see n connections.
func waitCount(l *listener, n int32) int32 {
	for i := 0; i < 20 && l.count.Load() < n; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	return l.count.Load()
}

// The profile brig writes, under the real sandbox-exec. Every assertion is on
// what a process inside it could reach, and each deny has a control run
// outside the sandbox that shows the same target answers.
func TestHostSandboxDeniesWhatItShould(t *testing.T) {
	if testing.Short() {
		t.Skip("runs processes under the real sandbox-exec")
	}
	bin, err := exec.LookPath("sandbox-exec")
	if err != nil {
		t.Skip("no sandbox-exec on this host")
	}
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl on this host")
	}
	state := hostState(t)
	home := t.TempDir()
	allowed := listen(t, "tcp", "127.0.0.1:0")
	denied := listen(t, "tcp", "127.0.0.1:0")
	_, port, _ := net.SplitHostPort(allowed.addr)
	allowedPort, _ := strconv.Atoi(port)
	sockDir, err := os.MkdirTemp("/tmp", "brig-hostrt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	agent := listen(t, "unix", filepath.Join(sockDir, "agent.sock"))

	h := &host{bin: bin}
	const name = "brig-hostrt-it"
	if err := h.Run(RunSpec{Name: name, Net: "isolated", LoopbackPorts: []int{allowedPort},
		Shares: []Share{{Host: home, Guest: "/root"}}}); err != nil {
		t.Fatal(err)
	}
	inside := func(cmd ...string) (string, error) {
		return h.Output(ExecSpec{Name: name, Cmd: cmd, Cwd: "/root",
			Env: []Var{{Name: "HOME", Value: "/root"}}})
	}

	_, err = inside(curl, "-s", "-m", "3", "https://1.1.1.1/")
	t.Logf("inside: curl https://1.1.1.1/ exits %d", exitCode(err))
	if exitCode(err) != 7 {
		t.Errorf("curl to 1.1.1.1 exits %d (%v), want 7, could not connect", exitCode(err), err)
	}
	if err := exec.Command(curl, "-s", "-o", "/dev/null", "-m", "5", "https://1.1.1.1/").Run(); err != nil {
		t.Logf("control: outside the sandbox curl to 1.1.1.1 failed too (%v); this host has no route out", err)
	} else {
		t.Logf("control: outside the sandbox curl to 1.1.1.1 exits 0")
	}

	_, err = inside(curl, "-s", "-m", "3", "http://"+allowed.addr+"/")
	t.Logf("inside: curl %s (allowed) exits %d, listener saw %d", allowed.addr, exitCode(err), waitCount(allowed, 1))
	if waitCount(allowed, 1) != 1 {
		t.Errorf("the allowed loopback port saw %d connections, want 1", allowed.count.Load())
	}

	_, err = inside(curl, "-s", "-m", "3", "http://"+denied.addr+"/")
	t.Logf("inside: curl %s (not named) exits %d, listener saw %d", denied.addr, exitCode(err), denied.count.Load())
	if exitCode(err) != 7 || denied.count.Load() != 0 {
		t.Errorf("a loopback port brig did not name: curl exits %d, listener saw %d; want 7 and 0",
			exitCode(err), denied.count.Load())
	}
	if err := exec.Command(curl, "-s", "-m", "3", "http://"+denied.addr+"/").Run(); exitCode(err) == 7 || waitCount(denied, 1) != 1 {
		t.Errorf("control: outside the sandbox the same port saw %d connections (curl %v)", denied.count.Load(), err)
	}

	_, err = inside(curl, "-s", "-m", "3", "--unix-socket", agent.addr, "http://x/")
	t.Logf("inside: curl --unix-socket %s exits %d, listener saw %d", agent.addr, exitCode(err), agent.count.Load())
	if exitCode(err) != 7 || agent.count.Load() != 0 {
		t.Errorf("a unix socket other than the resolver: curl exits %d, listener saw %d; want 7 and 0",
			exitCode(err), agent.count.Load())
	}
	if err := exec.Command(curl, "-s", "-m", "3", "--unix-socket", agent.addr, "http://x/").Run(); waitCount(agent, 1) != 1 {
		t.Errorf("control: outside the sandbox the socket saw %d connections (curl %v)", agent.count.Load(), err)
	}

	secret := filepath.Join(state, name, "secret")
	if err := os.MkdirAll(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secret, "token"), []byte("broker-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Through the /var spelling of the path, which Seatbelt resolves.
	out, err := inside("cat", filepath.Join(os.Getenv("BRIG_STATE_DIR"), "host", name, "secret", "token"))
	t.Logf("inside: cat secret/token exits %d", exitCode(err))
	if err == nil || strings.Contains(out, "broker-token") {
		t.Errorf("the secret dir was readable: %q, %v", out, err)
	}
	out, err = inside("cat", filepath.Join(state, name, "profile.sb"))
	if err == nil || strings.Contains(out, "(version 1)") {
		t.Errorf("the sandbox read its own profile: %v", err)
	}

	// A process outside the sandbox, such as the credential broker, cannot
	// be signalled from inside. A child of the sandbox can.
	outside := exec.Command("sleep", "30")
	if err := outside.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = outside.Process.Kill(); _ = outside.Wait() })
	_, err = inside("/bin/kill", "-TERM", strconv.Itoa(outside.Process.Pid))
	t.Logf("inside: kill -TERM %d (outside) exits %d", outside.Process.Pid, exitCode(err))
	if err == nil || syscall.Kill(outside.Process.Pid, 0) != nil {
		t.Errorf("the sandbox signalled a process outside it: %v", err)
	}
	out, err = inside("/bin/sh", "-c", "sleep 30 & kill -TERM $!; echo kill:$?; wait $!; echo wait:$?")
	t.Logf("inside: kill -TERM of its own child: %q", strings.TrimSpace(out))
	if err != nil || !strings.Contains(out, "kill:0") || !strings.Contains(out, "wait:143") {
		t.Errorf("the sandbox could not signal its own child: %q, %v", out, err)
	}
}

// brig's secret store is the login keychain, which answers any process that
// runs as you. The profile cuts the sandbox off from it.
func TestHostSandboxDeniesTheKeychain(t *testing.T) {
	if testing.Short() {
		t.Skip("runs processes under the real sandbox-exec")
	}
	bin, err := exec.LookPath("sandbox-exec")
	if err != nil {
		t.Skip("no sandbox-exec on this host")
	}
	hostState(t)
	h := &host{bin: bin}
	const name = "brig-hostrt-kc"
	if err := h.Run(RunSpec{Name: name, Net: "none"}); err != nil {
		t.Fatal(err)
	}
	// TestMain points HOME at a scratch directory, where security finds no
	// login keychain. Every call here, inside and outside, gets the real one.
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	realHome := []string{"HOME=" + u.HomeDir}
	security := func(ctx context.Context, stdin string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "security", args...)
		cmd.Env = append(os.Environ(), realHome...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		return string(out), err
	}
	inside := func(cmd ...string) (string, error) {
		return h.Output(ExecSpec{Name: name, Cmd: cmd, Env: []Var{{Name: "HOME", Value: u.HomeDir}}})
	}
	const service, account = "sh.brig.secret.test.hostrt", "probe"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = security(ctx, "", "delete-generic-password", "-s", service, "-a", account)
	if _, err := security(ctx, "add-generic-password -s "+service+" -a "+account+" -w keychain-value\n", "-i"); err != nil {
		t.Skipf("the keychain did not take a test item (%v); nothing to read back", err)
	}
	t.Cleanup(func() {
		_, _ = security(context.Background(), "", "delete-generic-password", "-s", service, "-a", account)
	})
	out, err := inside("security", "find-generic-password", "-s", service, "-a", account, "-w")
	t.Logf("inside: security find-generic-password exits %d", exitCode(err))
	if err == nil || strings.Contains(out, "keychain-value") {
		t.Errorf("the keychain answered inside the sandbox: %q, %v", out, err)
	}
	if got, err := security(ctx, "", "find-generic-password", "-s", service, "-a", account, "-w"); err != nil ||
		strings.TrimSpace(got) != "keychain-value" {
		t.Errorf("control: outside the sandbox the keychain gave %q, %v", got, err)
	}
}

// sandboxBin returns sandbox-exec for a test that runs the real one.
func sandboxBin(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("runs processes under the real sandbox-exec")
	}
	bin, err := exec.LookPath("sandbox-exec")
	if err != nil {
		t.Skip("no sandbox-exec on this host")
	}
	return bin
}

// python returns a python3 that runs, or skips. A bare macOS has a stub that
// asks to install the developer tools instead.
func python(t *testing.T) string {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil || exec.Command(py, "-I", "-c", "pass").Run() != nil {
		t.Skip("no working python3 on this host")
	}
	return py
}

// offline reaches no port, loopback included.
func TestHostSandboxOfflineReachesNothing(t *testing.T) {
	bin := sandboxBin(t)
	hostState(t)
	l := listen(t, "tcp", "127.0.0.1:0")
	_, port, _ := net.SplitHostPort(l.addr)
	p, _ := strconv.Atoi(port)
	h := &host{bin: bin}
	if err := h.Run(RunSpec{Name: "brig-offline", Net: "none", LoopbackPorts: []int{p}}); err != nil {
		t.Fatal(err)
	}
	_, err := h.Output(ExecSpec{Name: "brig-offline", Cmd: []string{"curl", "-s", "-m", "3", "http://" + l.addr + "/"}})
	t.Logf("inside offline: curl %s exits %d, listener saw %d", l.addr, exitCode(err), l.count.Load())
	if exitCode(err) != 7 || l.count.Load() != 0 {
		t.Errorf("offline reached a loopback port: curl exits %d, listener saw %d", exitCode(err), l.count.Load())
	}
}

// Nothing in the sandbox listens: a socket bound on any address would take
// connections from the network this Mac is on.
func TestHostSandboxCannotListen(t *testing.T) {
	bin := sandboxBin(t)
	py := python(t)
	hostState(t)
	h := &host{bin: bin}
	const bind = `import socket; s = socket.socket(); s.bind(("0.0.0.0", 0)); s.listen(1); print("bound")`
	for _, net := range []string{"isolated", "none", "shared"} {
		name := "brig-listen-" + net
		if err := h.Run(RunSpec{Name: name, Net: net}); err != nil {
			t.Fatal(err)
		}
		out, err := h.Output(ExecSpec{Name: name, Cmd: []string{py, "-I", "-c", bind}})
		t.Logf("inside %s: bind exits %d", net, exitCode(err))
		if err == nil || strings.Contains(out, "bound") {
			t.Errorf("%s: a socket was bound inside the sandbox: %q", net, out)
		}
	}
	if out, err := exec.Command(py, "-I", "-c", bind).Output(); err != nil || !strings.Contains(string(out), "bound") {
		t.Errorf("control: outside the sandbox the bind failed: %q, %v", out, err)
	}
}

// open(1) starts an app through LaunchServices, outside the sandbox. The
// probe app writes a file when it runs, and inside the sandbox it must not.
func TestHostSandboxCannotOpenAnApp(t *testing.T) {
	bin := sandboxBin(t)
	hostState(t)
	dir := t.TempDir()
	app := filepath.Join(dir, "BrigProbe.app")
	marker := filepath.Join(dir, "ran")
	if err := os.MkdirAll(filepath.Join(app, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>CFBundleExecutable</key><string>probe</string>` +
		`<key>CFBundleIdentifier</key><string>sh.brig.test.probe</string>` +
		`<key>CFBundlePackageType</key><string>APPL</string></dict></plist>`
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho ran >> '" + marker + "'\n"
	if err := os.WriteFile(filepath.Join(app, "Contents", "MacOS", "probe"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ran := func() bool {
		for i := 0; i < 50; i++ {
			if _, err := os.Stat(marker); err == nil {
				return true
			}
			time.Sleep(100 * time.Millisecond)
		}
		return false
	}
	h := &host{bin: bin}
	if err := h.Run(RunSpec{Name: "brig-open", Net: "none"}); err != nil {
		t.Fatal(err)
	}
	_, err := h.Output(ExecSpec{Name: "brig-open", Cmd: []string{"open", "-g", "-n", app}})
	t.Logf("inside: open exits %d", exitCode(err))
	if err == nil || ran() {
		t.Errorf("open started an app from inside the sandbox (%v)", err)
	}
	if err := exec.Command("open", "-g", "-n", app).Run(); err != nil || !ran() {
		t.Skipf("control: LaunchServices did not start the probe app outside the sandbox either (%v)", err)
	}
	t.Logf("control: outside the sandbox open started the probe app")
}

// The agent cannot change what the next brig reads: not its profile by
// renaming a directory above it, not brig's profiles or policies. It can
// still write its home, which sits inside brig's state directory.
func TestHostSandboxHoldsBrigsOwnFiles(t *testing.T) {
	bin := sandboxBin(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(base, "a", "b", "state")
	profiles := filepath.Join(base, "config", "brig")
	home := filepath.Join(stateRoot, "homes", "brig-own")
	for _, d := range []string{home, profiles} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("BRIG_STATE_DIR", stateRoot)
	h := &host{bin: bin}
	const name = "brig-own"
	if err := h.Run(RunSpec{Name: name, Net: "none", Shares: []Share{{Host: home, Guest: "/root"}},
		Protect: []string{profiles}}); err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(stateRoot, "host", name, "profile.sb")
	before, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	inside := func(script string) error {
		_, err := h.Output(ExecSpec{Name: name, Cmd: []string{"/bin/sh", "-c", script, "sh",
			base, profilePath, profiles, home}})
		return err
	}
	for _, tt := range []struct{ what, script string }{
		// Writes first: a rename that got through would move the profile
		// every later command runs under.
		{"write the profile", `echo '(allow default)' >> "$2"`},
		{"write a profile for brig", `echo 'name: claude' > "$3/claude.yaml"`},
		{"rename brig's profile directory", `mv "$3" "$3-moved"`},
		{"rename the state directory", `mv "$1/a/b/state" "$1/a/b/state-moved"`},
		{"rename the directory above the state directory", `mv "$1/a/b" "$1/a/b-moved"`},
		{"rename a directory higher up", `mv "$1/a" "$1/a-moved"`},
	} {
		err := inside(tt.script)
		t.Logf("inside: %s exits %d", tt.what, exitCode(err))
		if err == nil {
			t.Errorf("the sandbox could %s", tt.what)
		}
	}
	if after, _ := os.ReadFile(profilePath); string(after) != string(before) {
		t.Errorf("the profile changed:\n%s", after)
	}
	if _, err := os.Stat(filepath.Join(profiles, "claude.yaml")); err == nil {
		t.Error("a profile was written into brig's profile directory")
	}
	if err := inside(`echo state > "$4/note" && mkdir -p "$4/.claude"`); err != nil {
		t.Errorf("the sandbox cannot write its own home inside the state directory: %v", err)
	}
	if err := inside(`touch "$1/elsewhere"`); err != nil {
		t.Errorf("control: the sandbox cannot write outside brig's directories either: %v", err)
	}
}

// A run that brig waits for is recorded too, so stop reaches it.
func TestHostAttachIsRecordedAndStopReachesIt(t *testing.T) {
	bin := sandboxBin(t)
	hostState(t)
	h := &host{bin: bin}
	const name = "brig-attach"
	if err := h.Run(RunSpec{Name: name, Net: "none"}); err != nil {
		t.Fatal(err)
	}
	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		status, err := h.Attach(ExecSpec{Name: name, Cmd: []string{"sleep", "60"}})
		done <- result{status, err}
	}()
	up := false
	for i := 0; i < 100 && !up; i++ {
		up, _ = h.Running(name)
		time.Sleep(50 * time.Millisecond)
	}
	if !up {
		t.Fatal("an attached run is not reported as running")
	}
	if err := h.Stop(name); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		// Stop kills the tree it has frozen, so the run ends on SIGKILL.
		if r.err != nil || r.status != 128+int(syscall.SIGKILL) {
			t.Errorf("Attach returned %d, %v, want %d", r.status, r.err, 128+int(syscall.SIGKILL))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Stop did not end the attached run")
	}
	if up, _ := h.Running(name); up {
		t.Error("still running after Stop")
	}
}

// TIOCSTI pushes bytes into the input of the terminal the agent shares with
// your shell, and the shell reads them as typed once the agent exits, outside
// the sandbox. A pty stands in for the terminal and an unsandboxed sh for
// your shell, so nothing reaches a real one.
func TestHostSandboxCannotTypeIntoTheTerminal(t *testing.T) {
	bin := sandboxBin(t)
	py := python(t)
	hostState(t)
	h := &host{bin: bin}
	const name = "brig-tiocsti"
	if err := h.Run(RunSpec{Name: name, Net: "none"}); err != nil {
		t.Fatal(err)
	}
	const typed = "echo brig-tiocsti-ran"
	inject := "import fcntl, termios\nfor c in b'" + typed + "\\n':\n    fcntl.ioctl(0, termios.TIOCSTI, bytes([c]))\n"
	// The shell runs the agent, then reads one line as your shell would.
	shell := `"$@"; IFS= read -t 3 -r line; printf 'shell read: [%s]\n' "$line"`
	driver := `
import os, pty, sys
pid, fd = pty.fork()
if pid == 0:
    os.execv("/bin/sh", ["/bin/sh", "-c"] + sys.argv[1:])
out = b""
while True:
    try:
        b = os.read(fd, 4096)
    except OSError:
        break
    if not b:
        break
    out += b
os.waitpid(pid, 0)
sys.stdout.write(out.decode(errors="replace"))
`
	onPty := func(env []string, agent ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, py, append([]string{"-I", "-c", driver, shell, "sh"}, agent...)...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("the pty driver: %v\n%s", err, out)
		}
		return string(out)
	}
	inst, argv, env, err := h.command(ExecSpec{Name: name, Cmd: []string{py, "-I", "-c", inject}}, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = inst.Close()
	ran := "shell read: [" + typed + "]"
	out := onPty(env, argv...)
	t.Logf("inside: %q", out)
	if strings.Contains(out, ran) {
		t.Errorf("the sandbox typed a command into the terminal, and the shell read it:\n%s", out)
	}
	if out := onPty(os.Environ(), py, "-I", "-c", inject); !strings.Contains(out, ran) {
		t.Errorf("control: outside the sandbox the shell did not read what was typed:\n%s", out)
	}
}

// hdiutil asks diskarbitrationd, outside the sandbox, to mount an image. A
// mount over a directory brig reads would hide it behind the agent's files.
func TestHostSandboxCannotMount(t *testing.T) {
	bin := sandboxBin(t)
	hdiutil, err := exec.LookPath("hdiutil")
	if err != nil {
		t.Skip("no hdiutil on this host")
	}
	hostState(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	protect := filepath.Join(dir, "protected")
	if err := os.MkdirAll(protect, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(protect, "policy.yaml"), []byte("default: deny\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(dir, "probe.dmg")
	if out, err := exec.Command(hdiutil, "create", "-size", "1m", "-fs", "HFS+", "-volname", "BrigProbe",
		"-layout", "NONE", img).CombinedOutput(); err != nil {
		t.Skipf("hdiutil cannot create an image here (%v): %s", err, out)
	}
	mounted := func() bool {
		out, _ := exec.Command("/sbin/mount").Output()
		return strings.Contains(string(out), " on "+protect+" (")
	}
	detach := func() {
		if mounted() {
			_ = exec.Command(hdiutil, "detach", "-force", protect).Run()
		}
	}
	t.Cleanup(detach)
	attach := []string{hdiutil, "attach", "-nobrowse", "-noverify", "-noautoopen", "-mountpoint", protect, img}

	h := &host{bin: bin}
	const name = "brig-mount"
	if err := h.Run(RunSpec{Name: name, Net: "none", Protect: []string{protect}}); err != nil {
		t.Fatal(err)
	}
	// With diskarbitrationd denied, hdiutil can exit 0 having mounted
	// nothing, so the mount table is the answer.
	_, err = h.Output(ExecSpec{Name: name, Cmd: attach})
	got := mounted()
	t.Logf("inside: hdiutil attach over a protected directory exits %d (%v), mounted %v", exitCode(err), err, got)
	if got {
		t.Errorf("the sandbox mounted an image over %s", protect)
	}
	detach()
	if blob, err := os.ReadFile(filepath.Join(protect, "policy.yaml")); err != nil || string(blob) != "default: deny\n" {
		t.Errorf("the protected directory changed: %q, %v", blob, err)
	}
	if out, err := exec.Command(attach[0], attach[1:]...).CombinedOutput(); err != nil || !mounted() {
		t.Skipf("control: outside the sandbox the image did not mount either (%v): %s", err, out)
	}
	t.Logf("control: outside the sandbox the image mounted")
	// Nor can the sandbox take away a mount of yours, such as a hull store.
	for _, cmd := range [][]string{{"/sbin/umount", protect}, {hdiutil, "detach", protect}} {
		_, err = h.Output(ExecSpec{Name: name, Cmd: cmd})
		t.Logf("inside: %s exits %d", filepath.Base(cmd[0]), exitCode(err))
		if !mounted() {
			t.Fatalf("the sandbox unmounted %s with %s", protect, cmd[0])
		}
	}
}

// bgFetch asks for a background download, which nsurlsessiond, a daemon
// outside the sandbox, opens the connection for.
const bgFetch = `ObjC.import('Foundation')
function run(argv) {
  const cfg = $.NSClassFromString('NSURLSessionConfiguration').backgroundSessionConfigurationWithIdentifier(argv[1])
  const s = $.NSClassFromString('NSURLSession').sessionWithConfiguration(cfg)
  s.downloadTaskWithURL($.NSURL.URLWithString(argv[0])).resume
  $.NSRunLoop.currentRunLoop.runUntilDate($.NSDate.dateWithTimeIntervalSinceNow(3))
  s.invalidateAndCancel
  return 'asked'
}
`

// A background download leaves through nsurlsessiond, so the network deny
// alone does not hold it. The listener is on a loopback port the profile does
// not name.
func TestHostSandboxCannotFetchThroughTheDownloadDaemon(t *testing.T) {
	bin := sandboxBin(t)
	osascript, err := exec.LookPath("osascript")
	if err != nil {
		t.Skip("no osascript on this host")
	}
	hostState(t)
	js := filepath.Join(t.TempDir(), "bg.js")
	if err := os.WriteFile(js, []byte(bgFetch), 0o600); err != nil {
		t.Fatal(err)
	}
	// waitLong gives the daemon the time a background task takes to start.
	waitLong := func(l *listener) int32 {
		for i := 0; i < 40 && l.count.Load() == 0; i++ {
			time.Sleep(100 * time.Millisecond)
		}
		return l.count.Load()
	}
	id := func(what string) string { return fmt.Sprintf("sh.brig.test.bg.%s.%d", what, os.Getpid()) }
	ctl := listen(t, "tcp", "127.0.0.1:0")
	if out, err := exec.Command(osascript, "-l", "JavaScript", js, "http://"+ctl.addr+"/", id("control")).CombinedOutput(); err != nil || waitLong(ctl) == 0 {
		t.Skipf("control: outside the sandbox the background download reached nothing (%v): %s", err, out)
	}
	t.Logf("control: outside the sandbox the background download connected")
	h := &host{bin: bin}
	for _, net := range []string{"isolated", "none"} {
		l := listen(t, "tcp", "127.0.0.1:0")
		name := "brig-bg-" + net
		if err := h.Run(RunSpec{Name: name, Net: net}); err != nil {
			t.Fatal(err)
		}
		_, err := h.Output(ExecSpec{Name: name, Cmd: []string{osascript, "-l", "JavaScript", js, "http://" + l.addr + "/", id(net)}})
		got := waitLong(l)
		t.Logf("inside %s: background download exits %d, listener saw %d", net, exitCode(err), got)
		if got != 0 {
			t.Errorf("%s: a background download reached %s from inside the sandbox", net, l.addr)
		}
	}
}

// An AppleEvent hands work to another app, outside the sandbox. Finder is
// running, so an answer that it is not is the sandbox refusing the event.
// Nothing is sent from outside: an AppleEvent from this process could ask
// you for automation consent.
func TestHostSandboxSendsNoAppleEvent(t *testing.T) {
	bin := sandboxBin(t)
	if exec.Command("pgrep", "-x", "Finder").Run() != nil {
		t.Skip("Finder is not running, so a refused event looks like a missing app")
	}
	hostState(t)
	h := &host{bin: bin}
	const name = "brig-appleevent"
	if err := h.Run(RunSpec{Name: name, Net: "none"}); err != nil {
		t.Fatal(err)
	}
	out, err := h.Output(ExecSpec{Name: name, Cmd: []string{"osascript", "-e", "with timeout of 5 seconds",
		"-e", `tell application "Finder" to count windows`, "-e", "end timeout"}})
	t.Logf("inside: an AppleEvent to Finder exits %d (%v)", exitCode(err), err)
	if err == nil || !strings.Contains(err.Error(), "(-600)") {
		t.Errorf("an AppleEvent to Finder was not refused: %q, %v", out, err)
	}
}

// On shared every IP connection is open, and of the unix sockets only the
// resolver's. The socket stands in for your SSH agent.
func TestHostSandboxSharedReachesNoOtherUnixSocket(t *testing.T) {
	bin := sandboxBin(t)
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl on this host")
	}
	hostState(t)
	sockDir, err := os.MkdirTemp("/tmp", "brig-hostrt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	agent := listen(t, "unix", filepath.Join(sockDir, "agent.sock"))
	h := &host{bin: bin}
	const name = "brig-shared-unix"
	if err := h.Run(RunSpec{Name: name, Net: "shared"}); err != nil {
		t.Fatal(err)
	}
	_, err = h.Output(ExecSpec{Name: name, Cmd: []string{curl, "-s", "-m", "3", "--unix-socket", agent.addr, "http://x/"}})
	t.Logf("inside shared: curl --unix-socket exits %d, listener saw %d", exitCode(err), agent.count.Load())
	if exitCode(err) != 7 || agent.count.Load() != 0 {
		t.Errorf("shared: a unix socket other than the resolver: curl exits %d, listener saw %d; want 7 and 0",
			exitCode(err), agent.count.Load())
	}
	if err := exec.Command(curl, "-s", "-m", "3", "--unix-socket", agent.addr, "http://x/").Run(); waitCount(agent, 1) != 1 {
		t.Errorf("control: outside the sandbox the socket saw %d connections (curl %v)", agent.count.Load(), err)
	}
}

// Seatbelt refuses a bpf device with EPERM. Without the deny the device's own
// mode answers: EACCES for a user it does not admit, or an open.
func TestHostSandboxCannotOpenBPF(t *testing.T) {
	bin := sandboxBin(t)
	py := python(t)
	devs, _ := filepath.Glob("/dev/bpf[0-9]*")
	if len(devs) == 0 {
		t.Skip("no bpf device on this host")
	}
	hostState(t)
	h := &host{bin: bin}
	const name = "brig-bpf"
	if err := h.Run(RunSpec{Name: name, Net: "shared"}); err != nil {
		t.Fatal(err)
	}
	const probe = "import os, sys\ntry:\n    os.close(os.open(sys.argv[1], os.O_RDONLY)); print('opened')\n" +
		"except OSError as e:\n    print('errno', e.errno)\n"
	dev := devs[len(devs)-1]
	out, err := h.Output(ExecSpec{Name: name, Cmd: []string{py, "-I", "-c", probe, dev}})
	t.Logf("inside: open %s: %s", dev, strings.TrimSpace(out))
	if err != nil || strings.TrimSpace(out) != fmt.Sprintf("errno %d", syscall.EPERM) {
		t.Errorf("%s inside the sandbox: %q, %v; want the sandbox's EPERM", dev, out, err)
	}
	ctl, _ := exec.Command(py, "-I", "-c", probe, dev).Output()
	t.Logf("control: outside the sandbox open %s: %s", dev, strings.TrimSpace(string(ctl)))
}

// An agent that starts a process when it is told to stop, and a process
// outside the sandbox in the agent's process group, as tee is in
// `brig run claude | tee log`. Stop ends the first and leaves the second.
func TestHostStopReachesAProcessForkedOnTerm(t *testing.T) {
	state := hostState(t)
	h := &host{bin: "/usr/bin/sandbox-exec"}
	const name = "brig-stop-trap"
	if err := h.Run(RunSpec{Name: name, Net: "none"}); err != nil {
		t.Fatal(err)
	}
	forked := filepath.Join(t.TempDir(), "forked.pid")
	agent := exec.Command("/bin/sh", "-c", `trap 'sleep 60 & echo $! > "$1"; exit 0' TERM; sleep 60 & wait`, "sh", forked)
	agent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- agent.Wait() }()
	t.Cleanup(func() { _ = agent.Process.Kill() })
	t.Cleanup(func() {
		if blob, err := os.ReadFile(forked); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(blob))); err == nil && pid > 1 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	bystander := exec.Command("sleep", "60")
	bystander.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: agent.Process.Pid}
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	bystanderDone := make(chan error, 1)
	go func() { bystanderDone <- bystander.Wait() }()
	t.Cleanup(func() { _ = bystander.Process.Kill() })
	// The trap is set once sh has a child.
	for i := 0; i < 100; i++ {
		table, _ := processTable()
		child := false
		for _, r := range table {
			child = child || r.ppid == agent.Process.Pid
		}
		if child {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	start, err := processStart(agent.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(state, name, "pids", strconv.Itoa(agent.Process.Pid))
	if err := os.WriteFile(pidFile, []byte(pidLine(agent.Process.Pid, start)), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := h.Stop(name); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent is still alive after Stop")
	}
	if blob, err := os.ReadFile(forked); err == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(blob)))
		if pid > 1 && syscall.Kill(pid, 0) == nil {
			t.Errorf("pid %d, started by the agent when told to stop, survives Stop", pid)
		}
	}
	select {
	case err := <-bystanderDone:
		t.Errorf("Stop ended a process outside the sandbox that shares the agent's group: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if up, err := h.Running(name); err != nil || up {
		t.Errorf("Running = %v, %v after Stop", up, err)
	}
}
