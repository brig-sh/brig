package runtime

import (
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The shared gateway outlives the brig that started it. A brig from before
// port publishing started it without --api, and after an upgrade that gateway
// can never publish a port: the sandbox that asks is on it, so "wait until
// every sandbox has stopped" never comes true. These tests drive the
// replacement and the stop against real processes, because that is what the
// code decides on: argv read back through ps, and a pid it signals.

// TestGatewayHelperProcess is not a test. It is the gateway the tests below
// start: the test binary run again with a gateway's argv, listening where a
// gateway listens. Its argv is what ps shows, so it reads to brig as a gateway
// on the socket it names.
func TestGatewayHelperProcess(t *testing.T) {
	if os.Getenv("BRIG_TEST_GATEWAY_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	flag := func(name string) string {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == name {
				return args[i+1]
			}
		}
		return ""
	}
	// A stale file is removed before the bind, the way hull claims a path
	// nothing listens on. The gateway this one replaces left its socket behind.
	qsock := flag("--qemu-socket")
	_ = os.Remove(qsock)
	q, err := net.Listen("unix", qsock)
	if err != nil {
		os.Exit(2)
	}
	go func() {
		for {
			c, err := q.Accept()
			if err != nil {
				return
			}
			// Held until the other end closes it, as hull's gateway holds
			// a member's stream. darwin reads the listener's pid off a
			// connection only while it is up.
			go func() {
				_, _ = io.Copy(io.Discard, c)
				_ = c.Close()
			}()
		}
	}()
	if api := flag("--api"); api != "" && os.Getenv("BRIG_TEST_GATEWAY_DEAF") == "" {
		_ = os.Remove(api)
		l, err := net.Listen("unix", api)
		if err != nil {
			os.Exit(2)
		}
		srv := &http.Server{Handler: &forwardAPI{}, ReadHeaderTimeout: time.Second}
		go func() { _ = srv.Serve(l) }()
	}
	// Gone on its own if a test fails to stop it, so nothing outlives a run
	// for long.
	time.Sleep(60 * time.Second)
	os.Exit(0)
}

// scratchSharedGateway points the gateway directory somewhere short and
// private, and returns the shared socket there. Short, because a unix socket
// path is capped at 104 bytes. Private, because the real shared gateway under
// ~/.brig serves the sandboxes of every session on this host.
func scratchSharedGateway(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "brig-sg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("BRIG_GATEWAY_DIR", dir)
	t.Setenv("BRIG_GATEWAY_SOCK", "")
	sock, err := gatewaySocket()
	if err != nil {
		t.Fatal(err)
	}
	// A gateway that brig starts in a test is started by startGateway, and
	// the test holds no handle to that process. Whatever is left on this
	// socket goes here.
	t.Cleanup(func() { killGatewaysOn(sock) })
	return sock
}

func helperArgs(sock string, api bool) []string {
	args := []string{"-test.run=^TestGatewayHelperProcess$", "--",
		"network-gateway", "--socket", sock, "--qemu-socket", qemuGatewaySocket(sock)}
	if api {
		args = append(args, "--api", gatewayAPISocket(sock))
	}
	return args
}

// startHelperGateway runs a gateway on sock and returns its pid once it
// answers. api puts --api on its argv; deaf leaves that socket unopened,
// which is a gateway brig started a moment ago, or the smoke stub.
func startHelperGateway(t *testing.T, sock string, api, deaf bool) int {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, helperArgs(sock, api)...)
	cmd.Env = append(os.Environ(), "BRIG_TEST_GATEWAY_HELPER=1")
	if deaf {
		cmd.Env = append(cmd.Env, "BRIG_TEST_GATEWAY_DEAF=1")
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	// Reaped at once, so a stopped gateway is gone from ps and not a zombie
	// that still holds its pid.
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	for range 200 {
		if gatewayReachable(sock) && ownsGateway(pid, sock) {
			return pid
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("the helper gateway on %s (pid %d) never came up", sock, pid)
	return 0
}

// spawnMember runs a process whose argv reads as a VMM attached to the gateway
// on sock. hvi is started with --net-gateway <sock>.qemu, and that argv is
// what marks a sandbox as on this gateway.
func spawnMember(t *testing.T, sock string) int {
	t.Helper()
	return spawnWithArgv(t, "hvi", "--net-gateway", qemuGatewaySocket(sock))
}

func spawnWithArgv(t *testing.T, argv ...string) int {
	t.Helper()
	script := filepath.Join(t.TempDir(), "proc")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", append([]string{script}, argv...)...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	want := strings.Join(argv, " ")
	for range 100 {
		out, _ := exec.Command("ps", "-ww", "-o", "command=", "-p", strconv.Itoa(cmd.Process.Pid)).Output()
		if strings.Contains(string(out), want) {
			return cmd.Process.Pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d never showed argv %q", cmd.Process.Pid, want)
	return 0
}

// fakeRuntime is a hull that only knows network-gateway, and records every
// call in the marker file beside it. A call for anything else fails, so a boot
// that reaches `hull run` does not start a gateway by accident.
func fakeRuntime(t *testing.T) (bin, marker string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin, marker = filepath.Join(dir, "hull"), filepath.Join(dir, "called")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> '" + marker + "'\n" +
		"[ \"$1\" = network-gateway ] || exit 1\n" +
		"BRIG_TEST_GATEWAY_HELPER=1 exec '" + self + "' -test.run='^TestGatewayHelperProcess$' -- \"$@\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, marker
}

// killGatewaysOn stops every process whose argv puts a gateway on sock. Read
// from ps here rather than through the code under test, so a cleanup does not
// depend on what it is cleaning up after.
func killGatewaysOn(sock string) {
	out, err := exec.Command("ps", "-A", "-ww", "-o", "pid=,command=").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "--socket "+sock+" ") {
			continue
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if pid, err := strconv.Atoi(f[0]); err == nil && pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// gatewaysOn lists the argv of every gateway process on sock.
func gatewaysOn(t *testing.T, sock string) []string {
	t.Helper()
	out, err := exec.Command("ps", "-A", "-ww", "-o", "pid=,command=").Output()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "network-gateway") && strings.Contains(line, "--socket "+sock+" ") {
			got = append(got, line)
		}
	}
	return got
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// The case the issue is about. An old gateway with nothing on it is replaced
// at the next boot by one with the API socket, so the port can be published.
func TestAnOldSharedGatewayWithNoSandboxIsReplacedAtBoot(t *testing.T) {
	sock := scratchSharedGateway(t)
	old := startHelperGateway(t, sock, false, false)
	bin, _ := fakeRuntime(t)

	got, release, err := ensureGateway(bin)
	if err != nil {
		t.Fatalf("ensureGateway: %v", err)
	}
	release()
	if got != sock {
		t.Fatalf("ensureGateway returned %s, want %s", got, sock)
	}
	waitGone(t, old, sock)
	gws := gatewaysOn(t, sock)
	if len(gws) != 1 || !strings.Contains(gws[0], "--api "+gatewayAPISocket(sock)) {
		t.Fatalf("want one gateway with --api on the socket, got %q", gws)
	}
	if _, err := gatewayForwards(sock); err != nil {
		t.Fatalf("the new gateway's API does not answer: %v", err)
	}
}

// The same through the run: a boot that publishes a port gets past an old
// gateway nothing else is on, and fails later on the boot assets this test
// leaves out. "cannot publish" here is the bug.
func TestARunThatPublishesGetsPastAnUnusedOldGateway(t *testing.T) {
	const sandbox = "brig-claude-web"
	sock := scratchSharedGateway(t)
	startHelperGateway(t, sock, false, false)
	bin, _ := fakeRuntime(t)
	t.Setenv("BRIG_BOOT_ASSETS", t.TempDir())

	want, _ := ParsePublications([]string{"18080:80"})
	h := &hull{bin: bin}
	err := h.Run(RunSpec{Name: sandbox, Image: "ubuntu:latest", Hypervisor: "hvi",
		GenericBoot: true, Publish: want})
	if err == nil {
		t.Fatal("the run succeeded with no boot assets")
	}
	if strings.Contains(err.Error(), "cannot publish") {
		t.Fatalf("the old gateway was kept for a run that publishes: %v", err)
	}
	if !strings.Contains(err.Error(), bootKernelName()) {
		t.Fatalf("the run did not fail on the missing boot assets: %v", err)
	}
}

// Negative: a sandbox on the old gateway keeps it. Stopping it takes the
// network from under that sandbox, which is worse than a port that cannot be
// published.
func TestAnOldSharedGatewayWithASandboxOnItIsKept(t *testing.T) {
	sock := scratchSharedGateway(t)
	old := startHelperGateway(t, sock, false, false)
	member := spawnMember(t, sock)
	bin, marker := fakeRuntime(t)

	got, release, err := ensureGateway(bin)
	if err != nil || got != sock {
		t.Fatalf("ensureGateway = %s, %v; want the old socket", got, err)
	}
	release()
	if !ownsGateway(old, sock) {
		t.Fatal("the old gateway was stopped with a sandbox still on it")
	}
	if !alive(member) {
		t.Fatal("the sandbox on the old gateway is gone")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("a second gateway was started beside one that still serves a sandbox")
	}
}

// Negative: a gateway started with --api is never stopped as an old one, even
// while its API socket does not answer. That is a gateway another brig
// started a moment ago, and stopping it races that brig's boot.
func TestAGatewayStartedWithTheAPIIsNeverStoppedAsOld(t *testing.T) {
	sock := scratchSharedGateway(t)
	pid := startHelperGateway(t, sock, true, true)
	bin, marker := fakeRuntime(t)

	got, release, err := ensureGateway(bin)
	if err != nil || got != sock {
		t.Fatalf("ensureGateway = %s, %v; want the running socket", got, err)
	}
	release()
	if !ownsGateway(pid, sock) {
		t.Fatal("a gateway started with --api was stopped")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("a second gateway was started beside one that answers")
	}
}

// Negative: replacing the shared gateway touches nothing else. An isolated
// gateway, a gateway on a look-alike socket, and a process that names a
// look-alike path all survive, and the look-alike process does not count as a
// sandbox on the shared gateway.
func TestReplacingTheOldGatewayLeavesOtherGatewaysAlone(t *testing.T) {
	sock := scratchSharedGateway(t)
	old := startHelperGateway(t, sock, false, false)
	isolated := filepath.Join(filepath.Dir(sock), "sandbox-x.sock")
	t.Cleanup(func() { killGatewaysOn(isolated) })
	sibling := startHelperGateway(t, isolated, false, false)
	lookalike := sock + ".bak"
	t.Cleanup(func() { killGatewaysOn(lookalike) })
	twin := startHelperGateway(t, lookalike, false, false)
	stranger := spawnWithArgv(t, "hvi", "--net-gateway", qemuGatewaySocket(sock)+".bak")
	bin, _ := fakeRuntime(t)

	_, release, err := ensureGateway(bin)
	if err != nil {
		t.Fatalf("ensureGateway: %v", err)
	}
	release()
	waitGone(t, old, sock)
	if !ownsGateway(sibling, isolated) {
		t.Error("an isolated gateway was stopped with the shared one")
	}
	if !ownsGateway(twin, lookalike) {
		t.Error("a gateway on a look-alike socket was stopped")
	}
	if !alive(stranger) {
		t.Error("a process naming a look-alike path was stopped")
	}
}

// Nothing a gateway without the API holds can be withdrawn through it, so
// `brig network unpublish` on a running sandbox forgets the port and
// succeeds. Before, it pointed the user at `brig network unpublish`, the
// command they had just run.
func TestUnpublishOnAGatewayWithoutTheAPISucceeds(t *testing.T) {
	for _, tc := range []struct {
		name string
		api  func(t *testing.T, sock string)
	}{
		{"no api socket", func(*testing.T, string) {}},
		{"no forwards endpoint", func(t *testing.T, sock string) {
			l, err := net.Listen("unix", gatewayAPISocket(sock))
			if err != nil {
				t.Fatal(err)
			}
			srv := &http.Server{Handler: &forwardAPI{missing: true}, ReadHeaderTimeout: time.Second}
			go func() { _ = srv.Serve(l) }()
			t.Cleanup(func() { _ = srv.Close() })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const sandbox = "brig-claude-web"
			sock := scratchSharedGateway(t)
			listenAt(t, sock)
			tc.api(t, sock)
			alloc, err := sharedIPs()
			if err != nil {
				t.Fatal(err)
			}
			if err := alloc.write(map[string]int{sandbox: 5}); err != nil {
				t.Fatal(err)
			}
			mine, _ := ParsePublications([]string{"18080:80"})
			if _, err := RecordPublications(sandbox, mine); err != nil {
				t.Fatal(err)
			}

			h := &hull{bin: "hull"}
			if err := h.Unpublish(sandbox, mine[0]); err != nil {
				t.Fatalf("Unpublish: %v", err)
			}
			if rec := published(t, sandbox); len(rec) != 0 {
				t.Fatalf("the record kept a withdrawn publication: %+v", rec)
			}
		})
	}
}

// Negative: a gateway started with --api holds the forwards it was given,
// even after its API socket goes. Unpublish still forgets the port, but it
// does not report the forward withdrawn, because the host port keeps
// forwarding to the guest. The same when the process table cannot be read:
// nobody can say the gateway was started without --api.
func TestUnpublishOnAGatewayWhoseAPIIsGoneFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(t *testing.T, sock string)
	}{
		{"started with the api", func(t *testing.T, sock string) {
			startHelperGateway(t, sock, true, true)
		}},
		{"process table unreadable", func(t *testing.T, sock string) {
			listenAt(t, sock)
			saved := listProcesses
			listProcesses = func() ([]hostProc, error) { return nil, syscall.EPERM }
			t.Cleanup(func() { listProcesses = saved })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const sandbox = "brig-claude-web"
			sock := scratchSharedGateway(t)
			tc.start(t, sock)
			alloc, err := sharedIPs()
			if err != nil {
				t.Fatal(err)
			}
			if err := alloc.write(map[string]int{sandbox: 5}); err != nil {
				t.Fatal(err)
			}
			mine, _ := ParsePublications([]string{"18080:80"})
			if _, err := RecordPublications(sandbox, mine); err != nil {
				t.Fatal(err)
			}

			err = (&hull{bin: "hull"}).Unpublish(sandbox, mine[0])
			if err == nil {
				t.Fatal("Unpublish reported a forward withdrawn that the gateway can still hold")
			}
			for _, want := range []string{"18080", "forgotten", "until that gateway stops"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not say %q: %v", want, err)
				}
			}
			if strings.Contains(err.Error(), "`brig network unpublish`") {
				t.Errorf("the error sends the user to the command they ran: %v", err)
			}
			// Both cases get one message, and with the process table
			// unreadable brig cannot tell how the gateway was started, so
			// the message does not claim it.
			if strings.Contains(err.Error(), "was started with an API socket") {
				t.Errorf("the error says how the gateway was started as a fact: %v", err)
			}
			if rec := published(t, sandbox); len(rec) != 0 {
				t.Fatalf("the record kept a withdrawn publication: %+v", rec)
			}
		})
	}
}

// The refusal says what works: stop what is on this sandbox's network, this
// sandbox included, then boot it with its ports, and that boot replaces the
// gateway. It no longer promises a replacement nothing performed. It does not
// name the shared network, because an isolated sandbox booted by an older brig
// reaches it too, and stopping the shared sandboxes changes nothing for it. A gateway started with
// --api whose API socket is gone reaches the same refusal and is never
// replaced, so the older brig is named as the usual cause, not as a fact.
func TestTheOldGatewayRefusalGivesAdviceThatWorks(t *testing.T) {
	for _, cause := range []error{syscall.ENOENT, syscall.ECONNREFUSED} {
		msg := unpublishable(cause).Error()
		for _, want := range []string{"started by an older brig", "this one included",
			"`brig stop", "`brig run --publish"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%v: the refusal does not say %q: %s", cause, want, msg)
			}
		}
		for _, not := range []string{"once every sandbox on it has stopped", "Upgrade the runtime",
			"was started by an older brig", "brig replaces it ", "the shared network"} {
			if strings.Contains(msg, not) {
				t.Errorf("%v: the refusal still says %q: %s", cause, not, msg)
			}
		}
	}
}

// `brig rm --all` stops the shared gateway once nothing is on it, old or new.
// The log stays, as it always has for the shared gateway.
func TestPruneSharedNetworkStopsAGatewayNothingIsOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		api  bool
	}{{"old", false}, {"with the api", true}} {
		t.Run(tc.name, func(t *testing.T) {
			sock := scratchSharedGateway(t)
			pid := startHelperGateway(t, sock, tc.api, false)
			if err := os.WriteFile(gatewayLogPath(sock), []byte("kept\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			h := &hull{bin: "hull"}
			if !h.PruneSharedNetwork() {
				t.Error("PruneSharedNetwork reported nothing stopped")
			}
			waitGone(t, pid, sock)
			if _, err := os.Stat(gatewayLogPath(sock)); err != nil {
				t.Errorf("the shared gateway's log went with it: %v", err)
			}
		})
	}
}

// Negative: a sandbox on the shared gateway keeps it. The process table is
// what shows it there: the list rm --all works from does not say which
// network a sandbox is on.
func TestPruneSharedNetworkLeavesAGatewayWithASandboxOnIt(t *testing.T) {
	sock := scratchSharedGateway(t)
	pid := startHelperGateway(t, sock, true, false)
	member := spawnMember(t, sock)

	h := &hull{bin: "hull"}
	if h.PruneSharedNetwork() {
		t.Error("PruneSharedNetwork reported a gateway with a sandbox on it as stopped")
	}
	if !ownsGateway(pid, sock) {
		t.Fatal("the shared gateway was stopped with a sandbox still on it")
	}
	if !alive(member) {
		t.Fatal("the sandbox on the shared gateway is gone")
	}
}

// Negative: when the process table cannot be read, nobody can say the gateway
// is unused, so it is treated as in use. Nothing is stopped and nothing new is
// started beside it.
func TestNothingIsStoppedWhenTheProcessTableCannotBeRead(t *testing.T) {
	sock := scratchSharedGateway(t)
	old := startHelperGateway(t, sock, false, false)
	bin, marker := fakeRuntime(t)
	saved := listProcesses
	listProcesses = func() ([]hostProc, error) { return nil, syscall.EPERM }
	t.Cleanup(func() { listProcesses = saved })

	got, release, err := ensureGateway(bin)
	if err != nil || got != sock {
		t.Fatalf("ensureGateway = %s, %v; want the old socket", got, err)
	}
	release()
	if (&hull{bin: bin}).PruneSharedNetwork() {
		t.Error("PruneSharedNetwork stopped a gateway without seeing its members")
	}
	if !ownsGateway(old, sock) {
		t.Fatal("the gateway was stopped without a process table to decide on")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("a second gateway was started beside the old one")
	}
}

// A socket path counts only as a whole word. A look-alike that starts or ends
// with it is another path, and reading it as this one counts a stranger as
// a sandbox on the gateway, or a stranger's gateway as this one.
func TestASocketPathIsMatchedAsAWholeWord(t *testing.T) {
	const sock = "/tmp/b/gateway-198-18-0-0_24.sock"
	for _, tc := range []struct {
		argv   string
		member bool
	}{
		{"hvi --net-gateway " + sock + ".qemu --mem 2048", true},
		{"qemu-system-aarch64 -netdev stream,id=n0,addr.type=unix,addr.path=" + sock + ".qemu,server=off", true},
		{"hull run --gateway-sock " + sock + " --gateway-cidr 198.18.0.2/24", true},
		{"hvi --net-gateway " + sock + ".bak", false},
		{"hvi --net-gateway " + sock + ".qemu.bak", false},
		{"hvi --net-gateway /private" + sock + ".qemu", false},
		{"hvi --net-gateway /tmp/b/sandbox-x.sock.qemu", false},
	} {
		if got := namesSocket(tc.argv, sock); got != tc.member {
			t.Errorf("namesSocket(%q) = %t, want %t", tc.argv, got, tc.member)
		}
	}
	if !servesSocket("hull network-gateway --socket "+sock+" --qemu-socket "+sock+".qemu", sock) {
		t.Error("the gateway on the socket was not recognised")
	}
	if servesSocket("hull network-gateway --socket "+sock+".bak --qemu-socket "+sock+".bak.qemu", sock) {
		t.Error("a gateway on a look-alike socket was read as this one")
	}
}

// One process per pid, even when a ps prints a newline inside an argv raw.
func TestParseProcessesKeepsAnArgvWhole(t *testing.T) {
	got := parseProcesses("    1 /sbin/launchd\n  42 python3 -c import time\ntime.sleep(1) --socket /s.sock\n 7 sleep 1\n")
	if len(got) != 3 {
		t.Fatalf("want 3 processes, got %+v", got)
	}
	if got[1].pid != 42 || !strings.Contains(got[1].argv, "--socket /s.sock") {
		t.Errorf("the split argv was not joined: %+v", got[1])
	}
}

// Negative: a boot holds the shared gateway from before it looks at the
// socket until `hull run` returns. Until `hull run` starts, nothing in the
// process table names the socket for it, and the asset fetch between the two
// takes seconds. A `brig rm --all` in another session in that window finds no
// sandbox on the gateway, so the boot's lease is what keeps the gateway up.
func TestPruneSharedNetworkLeavesAGatewayABootIsStartingOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		api  bool
	}{{"old", false}, {"with the api", true}} {
		t.Run(tc.name, func(t *testing.T) {
			sock := scratchSharedGateway(t)
			pid := startHelperGateway(t, sock, tc.api, false)
			_, release, err := leaseSharedGateway(sock)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(release)

			if (&hull{bin: "hull"}).PruneSharedNetwork() {
				t.Error("PruneSharedNetwork reported a gateway a boot holds as stopped")
			}
			if !ownsGateway(pid, sock) {
				t.Fatal("the shared gateway was stopped under a boot that holds it")
			}
		})
	}
}

// Negative: the same for a boot that finds an old gateway. Another boot that
// holds it counts as a sandbox on it, so it is kept, and nothing new is
// started beside it.
func TestAnOldGatewayIsKeptWhileAnotherBootHoldsIt(t *testing.T) {
	sock := scratchSharedGateway(t)
	old := startHelperGateway(t, sock, false, false)
	_, release, err := leaseSharedGateway(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	bin, marker := fakeRuntime(t)

	got, done, err := ensureGateway(bin)
	if err != nil || got != sock {
		t.Fatalf("ensureGateway = %s, %v; want the old socket", got, err)
	}
	done()
	if !ownsGateway(old, sock) {
		t.Fatal("the old gateway was stopped under another boot that holds it")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("a second gateway was started beside the old one")
	}
}

// A lease whose holder is gone does not keep the gateway. A brig that crashed
// mid-boot leaves the file, and its lock went with the process. The prune
// removes the file, and a released lease leaves nothing behind.
func TestALeaseNobodyHoldsDoesNotKeepTheGateway(t *testing.T) {
	sock := scratchSharedGateway(t)
	pid := startHelperGateway(t, sock, true, false)
	lease, release, err := leaseSharedGateway(sock)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := os.Stat(lease); !os.IsNotExist(err) {
		t.Fatalf("a released lease is still on disk: %v", err)
	}
	stale := lease + "-crashed"
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if !(&hull{bin: "hull"}).PruneSharedNetwork() {
		t.Error("PruneSharedNetwork reported nothing stopped")
	}
	waitGone(t, pid, sock)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the lease nobody holds was left on disk: %v", err)
	}
}

// A boot's own lease is not another boot, with a relative BRIG_GATEWAY_SOCK
// too. os.CreateTemp names the lease ./gw.sock.boot-N there, and a lease path
// rebuilt with filepath.Join reads gw.sock.boot-N. Compared as paths, the two
// differ, the boot takes its own lease for another's, and the old gateway is
// never replaced.
func TestARelativeGatewaySocketDoesNotCountItsOwnLease(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "brig-sg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Chdir(dir)
	t.Setenv("BRIG_GATEWAY_SOCK", "gw.sock")
	sock, err := gatewaySocket()
	if err != nil {
		t.Fatal(err)
	}
	own, release, err := leaseSharedGateway(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)

	unlock, err := flock(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if bootsInFlight(sock, own) {
		t.Errorf("the boot's own lease %s counted as another boot", own)
	}
}

// spawnDecoy runs a process whose argv reads exactly as a gateway on sock,
// and that listens on nothing. Any process of the same user can carry that
// argv, so it is not a gateway brig started, and brig must not signal it.
func spawnDecoy(t *testing.T, sock string, api bool) int {
	t.Helper()
	argv := []string{"network-gateway", "--socket", sock, "--qemu-socket", qemuGatewaySocket(sock)}
	if api {
		argv = append(argv, "--api", gatewayAPISocket(sock))
	}
	return spawnWithArgv(t, argv...)
}

// Negative: `brig rm --all` signals only the process listening on the shared
// socket. A process whose argv merely names the socket the way a gateway does
// is left running, with or without a real gateway beside it.
func TestPruneSharedNetworkNeverSignalsAProcessThatOnlyLooksLikeTheGateway(t *testing.T) {
	t.Run("decoy alone", func(t *testing.T) {
		sock := scratchSharedGateway(t)
		decoy := spawnDecoy(t, sock, false)

		if (&hull{bin: "hull"}).PruneSharedNetwork() {
			t.Error("PruneSharedNetwork reported a gateway stopped where nothing listens")
		}
		if !alive(decoy) {
			t.Fatal("a process that only looked like the gateway in ps was killed")
		}
	})
	t.Run("decoy beside the gateway", func(t *testing.T) {
		sock := scratchSharedGateway(t)
		pid := startHelperGateway(t, sock, true, false)
		decoy := spawnDecoy(t, sock, true)

		if !(&hull{bin: "hull"}).PruneSharedNetwork() {
			t.Error("PruneSharedNetwork reported nothing stopped")
		}
		waitGone(t, pid, sock)
		if !alive(decoy) {
			t.Fatal("a process that only looked like the gateway in ps was killed")
		}
	})
}

// Negative: a boot replaces only the old gateway that is listening on the
// shared socket. A look-alike without --api is not taken for an old gateway,
// neither beside a gateway with --api, which is kept, nor beside an old one,
// which is replaced.
func TestABootNeverStopsAProcessThatOnlyLooksLikeAnOldGateway(t *testing.T) {
	t.Run("beside a gateway with the api", func(t *testing.T) {
		sock := scratchSharedGateway(t)
		pid := startHelperGateway(t, sock, true, true)
		decoy := spawnDecoy(t, sock, false)
		bin, marker := fakeRuntime(t)

		got, release, err := ensureGateway(bin)
		if err != nil || got != sock {
			t.Fatalf("ensureGateway = %s, %v; want the running socket", got, err)
		}
		release()
		if !alive(decoy) {
			t.Fatal("a process that only looked like an old gateway in ps was killed")
		}
		if !ownsGateway(pid, sock) {
			t.Fatal("a gateway started with --api was stopped")
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatal("a second gateway was started beside one that answers")
		}
	})
	t.Run("beside an old gateway", func(t *testing.T) {
		sock := scratchSharedGateway(t)
		old := startHelperGateway(t, sock, false, false)
		decoy := spawnDecoy(t, sock, false)
		bin, _ := fakeRuntime(t)

		_, release, err := ensureGateway(bin)
		if err != nil {
			t.Fatalf("ensureGateway: %v", err)
		}
		release()
		waitGone(t, old, sock)
		if !alive(decoy) {
			t.Fatal("a process that only looked like an old gateway in ps was killed")
		}
	})
}
