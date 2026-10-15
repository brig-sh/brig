package injector

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/brig-sh/brig/internal/proc"
	"github.com/brig-sh/brig/internal/runtime"
)

// readyTimeout bounds the wait for a broker brig started to answer. The
// first run of a new binary on macOS can wait on the system's security scan
// for seconds; see gatewayProbeTimeout in internal/runtime.
var readyTimeout = 30 * time.Second

// pollInterval spaces the probes of a broker that is starting or stopping.
const pollInterval = 50 * time.Millisecond

// BinEnv names a brig-broker to run instead of the one beside brig.
const BinEnv = "BRIG_BROKER_BIN"

// Broker is a running broker, as brig found it.
type Broker struct {
	Client Client
	Status Status
}

// Running returns the broker serving dir, or false when none is. It starts
// nothing, so brig info and brig doctor can ask it.
func Running(dir string) (*Broker, bool) {
	if _, ok := livePID(dir); !ok {
		return nil, false
	}
	c := Client{Socket: filepath.Join(dir, SocketName)}
	st, err := c.Status()
	if err != nil {
		return nil, false
	}
	return &Broker{Client: c, Status: st}, true
}

// LivePID returns the pid of the broker serving dir, while that process is
// still the one its pid file records. It asks the broker nothing, so a broker
// that does not answer still has one.
func LivePID(dir string) (int, bool) { return livePID(dir) }

// beforeAdd runs between finding the broker and registering with it. A test
// stops the broker there.
var beforeAdd = func() {}

// Register registers s with the broker serving dir, and starts that broker
// when none is running. A broker that was stopping when it was asked, or
// that was gone by then, is waited for and replaced: Forget does not take
// the start lock, so another brig can stop an idle broker after ensure found
// it.
func Register(dir string, s Sandbox) (*Broker, error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		b, started, err := ensure(dir)
		if err != nil {
			return nil, err
		}
		beforeAdd()
		err = b.Client.AddSandbox(s)
		if err == nil {
			return b, nil
		}
		if !stopping(err) && !errors.Is(err, ErrNotRunning) {
			// Nothing forgets a sandbox that was never registered, so a
			// broker this run started for it would serve nothing until the
			// next rm --all. IfIdle: another run that registered since
			// keeps it.
			if started {
				_, _ = b.Client.Stop(true)
			}
			return nil, err
		}
		last = err
		waitExit(dir)
	}
	return nil, last
}

// Forget removes the sandbox id from the broker serving dir, which refuses
// its tokens from the next request on, and stops that broker when no sandbox
// is left. It starts nothing: with no broker running there is nothing to
// revoke.
//
// It asks the control socket whatever the pid file says. A revocation that
// trusted a pid check it got wrong would leave every token working, so only
// a socket nobody listens on means no broker, and any other failure is
// returned.
func Forget(dir, id string) error {
	c, ok := controlClient(dir)
	if !ok {
		return nil
	}
	r, err := c.RemoveSandbox(id)
	if errors.Is(err, ErrNotRunning) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.Remaining == 0 {
		return stopIfIdle(c)
	}
	return nil
}

// ForgetAll removes every sandbox the broker serving dir has registered,
// and stops it. It returns the ids it removed. brig rm --all calls it after
// its own removals, for the sandboxes no runtime lists any more.
func ForgetAll(dir string) ([]string, error) {
	c, ok := controlClient(dir)
	if !ok {
		return nil, nil
	}
	st, err := c.Status()
	if errors.Is(err, ErrNotRunning) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, id := range st.Sandboxes {
		r, err := c.RemoveSandbox(id)
		if errors.Is(err, ErrNotRunning) {
			return removed, nil
		}
		if err != nil {
			return removed, err
		}
		if r.Removed {
			removed = append(removed, id)
		}
	}
	return removed, stopIfIdle(c)
}

// controlClient is the client of the control socket of dir, and false when
// no broker can listen there: a path too long for a unix socket is one
// Register refused to start a broker on.
func controlClient(dir string) (Client, bool) {
	sock := filepath.Join(dir, SocketName)
	return Client{Socket: sock}, len(sock) <= sockaddrUnMax
}

// stopIfIdle stops a broker with no sandbox left. IfIdle: a run that
// registered a sandbox since the removal keeps the broker it registered
// with.
func stopIfIdle(c Client) error {
	if _, err := c.Stop(true); err != nil && !errors.Is(err, ErrNotRunning) {
		return err
	}
	return nil
}

// ensure returns the running broker, starting one when none is running, and
// reports whether it started it.
//
// The check and the start happen under the start lock, so two runs started
// at once start one broker, and the second finds the first.
func ensure(dir string) (*Broker, bool, error) {
	if err := prepareDir(dir); err != nil {
		return nil, false, err
	}
	sock := filepath.Join(dir, SocketName)
	if len(sock) > sockaddrUnMax {
		return nil, false, fmt.Errorf("the credential broker's socket %s is %d bytes, and a unix socket path is at "+
			"most %d; point BRIG_STATE_DIR at a shorter directory", sock, len(sock), sockaddrUnMax)
	}
	unlock, err := lockStart(dir)
	if err != nil {
		return nil, false, err
	}
	defer unlock()
	c := Client{Socket: sock}
	if pid, ok := livePID(dir); ok {
		if st, err := waitAnswer(c, pid); err == nil {
			return &Broker{Client: c, Status: st}, false, nil
		}
		// A broker on its way out answers nothing and then exits.
		waitExit(dir)
		if pid, ok := livePID(dir); ok {
			return nil, false, fmt.Errorf("the credential broker (pid %d) is running and does not answer on %s; "+
				"see %s", pid, sock, filepath.Join(dir, LogName))
		}
	}
	bin, err := Bin()
	if err != nil {
		return nil, false, err
	}
	st, err := start(bin, dir)
	if err != nil {
		return nil, false, err
	}
	return &Broker{Client: c, Status: st}, true, nil
}

// sockaddrUnMax is what a unix socket path fits in on macOS, counting the
// trailing NUL.
const sockaddrUnMax = 103

// lockStart takes the start lock of dir, waiting for another brig that holds
// it, and returns its release.
func lockStart(dir string) (func(), error) {
	path := filepath.Join(dir, StartLockName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cannot open %s: %w", path, err)
	}
	deadline := time.Now().Add(readyTimeout + 5*time.Second)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("another brig has been starting the credential broker for too long; "+
				"see %s: %w", filepath.Join(dir, LogName), err)
		}
		time.Sleep(pollInterval)
	}
}

// prepareDir creates the broker directory when it is missing, and refuses
// one that is not brig's own. The host directory above it is checked before
// anything is created, so nothing is written through a link.
func prepareDir(dir string) error {
	if err := runtime.CheckHostDirLinks(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return CheckDir(dir)
}

// PortHeldError is a broker that could not start because another process
// holds the port recorded in its directory.
type PortHeldError struct {
	Port int
	Dir  string
}

func (e *PortHeldError) Error() string {
	return fmt.Sprintf("%s, 127.0.0.1:%d, so brig refuses the run. brig does not move the broker to another "+
		"port: a client of that port sends its session tokens to whatever listens there. Find the "+
		"process with `lsof -nP -iTCP:%d -sTCP:LISTEN` and stop it, or remove %s so that the next run "+
		"picks a new port",
		PortHeldMessage, e.Port, e.Port, filepath.Join(e.Dir, PortName))
}

// start runs brig-broker on dir and waits for it to answer.
func start(bin, dir string) (Status, error) {
	logPath := filepath.Join(dir, LogName)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return Status{}, fmt.Errorf("could not open the broker log: %w", err)
	}
	defer func() { _ = logFile.Close() }()
	offset, _ := logFile.Seek(0, io.SeekEnd)

	cmd := exec.Command(bin, "--dir", dir)
	// What it writes before it opens its own log, a refusal to start
	// included, lands here for brig to read.
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Dir = "/"
	// brig's own environment can hold the credentials a run forwards, and
	// the broker needs none of them.
	cmd.Env = brokerEnv()
	// The broker serves sandboxes after this brig exits, so it does not take
	// the terminal's signals with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return Status{}, fmt.Errorf("could not start the credential broker %s: %w", bin, err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

	c := Client{Socket: filepath.Join(dir, SocketName)}
	deadline := time.Now().Add(readyTimeout)
	for time.Now().Before(deadline) {
		if st, err := c.Status(); err == nil {
			return st, nil
		}
		select {
		case <-exited:
			said := logSince(logPath, offset)
			if strings.Contains(said, PortHeldMessage) {
				port, _ := recordedPort(dir)
				return Status{}, &PortHeldError{Port: port, Dir: dir}
			}
			// A broker started by hand, or by a brig that took no start
			// lock, holds the directory. Wait for it to answer.
			if !strings.Contains(said, "another broker is serving") {
				return Status{}, fmt.Errorf("the credential broker exited: %s", firstLine(said))
			}
			exited = nil
		case <-time.After(pollInterval):
		}
	}
	if exited != nil {
		_ = cmd.Process.Kill()
	}
	return Status{}, fmt.Errorf("the credential broker did not answer within %s; see %s",
		readyTimeout, logPath)
}

// recordedPort is the port in the port file of dir.
func recordedPort(dir string) (int, error) {
	blob, err := os.ReadFile(filepath.Join(dir, PortName))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(blob)))
}

// brokerEnv is the whole environment of the broker. HOME and USER are what
// security(1) needs to find the login keychain. On Linux the store is the
// Secret Service on the session bus, which DBUS_SESSION_BUS_ADDRESS and
// XDG_RUNTIME_DIR name.
func brokerEnv() []string {
	names := []string{"PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "LANG"}
	if goruntime.GOOS == "linux" {
		names = append(names, "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR")
	}
	var env []string
	for _, k := range names {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// Bin finds brig-broker: BRIG_BROKER_BIN, then beside the brig that runs,
// then on PATH.
func Bin() (string, error) {
	if bin := os.Getenv(BinEnv); bin != "" {
		if _, err := os.Stat(bin); err != nil {
			return "", fmt.Errorf("%s names %s, which is not there: %w", BinEnv, bin, err)
		}
		return bin, nil
	}
	if self, err := os.Executable(); err == nil {
		dirs := []string{filepath.Dir(self)}
		if real, err := filepath.EvalSymlinks(self); err == nil {
			dirs = append(dirs, filepath.Dir(real))
		}
		for _, d := range dirs {
			p := filepath.Join(d, "brig-broker")
			if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
				return p, nil
			}
		}
	}
	if p, err := exec.LookPath("brig-broker"); err == nil {
		return p, nil
	}
	return "", errors.New("brig-broker, which serves routes, is not installed beside brig or on PATH; " +
		"reinstall brig, or name it with " + BinEnv)
}

// waitAnswer gives a live broker the time a starting one needs to answer.
func waitAnswer(c Client, pid int) (Status, error) {
	deadline := time.Now().Add(readyTimeout)
	for {
		st, err := c.Status()
		if err == nil || time.Now().After(deadline) || !pidAlive(pid) {
			return st, err
		}
		time.Sleep(pollInterval)
	}
}

// waitExit waits for a broker that is stopping to be gone.
func waitExit(dir string) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := livePID(dir); !ok {
			return
		}
		time.Sleep(pollInterval)
	}
}

// PIDLine is the content of the pid file: the pid and the start time the
// system reports for it. A pid alone can be reused by an unrelated process.
func PIDLine(pid int, start string) string { return strconv.Itoa(pid) + " " + start + "\n" }

// livePID returns the pid in the pid file when that process is still the one
// recorded there.
func livePID(dir string) (int, bool) {
	blob, err := os.ReadFile(filepath.Join(dir, PIDName))
	if err != nil {
		return 0, false
	}
	f, start, _ := strings.Cut(strings.TrimSpace(string(blob)), " ")
	pid, err := strconv.Atoi(f)
	if err != nil || pid <= 1 || start == "" {
		return 0, false
	}
	now, err := ProcessStart(pid)
	if err != nil || now != start {
		return 0, false
	}
	return pid, true
}

func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// ProcessStart is the start time the system records for pid, the same
// whatever the locale of the process that reads it.
func ProcessStart(pid int) (string, error) { return proc.StartTime(pid) }

// logSince is what the broker wrote to its log after offset.
func logSince(path string, offset int64) string {
	blob, err := os.ReadFile(path)
	if err != nil || int64(len(blob)) < offset {
		return ""
	}
	return strings.TrimSpace(string(blob[offset:]))
}

func firstLine(s string) string {
	if s == "" {
		return "it wrote nothing"
	}
	line, _, _ := strings.Cut(s, "\n")
	return line
}
