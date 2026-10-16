package injector

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/runtime"
)

func expand(t *testing.T, r v1alpha1.Route) v1alpha1.Route {
	t.Helper()
	e, err := policy.ExpandRoute(r)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func presetRoute(t *testing.T, name string, preset v1alpha1.Preset, secret string) v1alpha1.Route {
	return expand(t, v1alpha1.Route{Name: name, Preset: preset,
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: secret}}})
}

func vars(vs []runtime.Var) map[string]runtime.Var {
	out := map[string]runtime.Var{}
	for _, v := range vs {
		out[v.Name] = v
	}
	return out
}

// A microVM reaches brig.internal over TLS, and is pointed at the brig CA.
func TestGuestEnvOnBrigInternal(t *testing.T) {
	ep := HostEndpoint{Transport: TransportBrigInternal}
	if ep.URL("brig-a", "anthropic") != "https://brig.internal/brig-a/r/anthropic" {
		t.Fatalf("endpoint %+v", ep)
	}
	got, err := ep.GuestEnv("brig-a", []v1alpha1.Route{presetRoute(t, "anthropic", v1alpha1.PresetAnthropicAPIKey, "k")},
		map[string]string{"anthropic": "tok"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := vars(got)
	if env["ANTHROPIC_API_KEY"].Value != "tok" || !env["ANTHROPIC_API_KEY"].Secret {
		t.Errorf("ANTHROPIC_API_KEY = %+v", env["ANTHROPIC_API_KEY"])
	}
	// Node adds the CA to its roots. The others take one file in place of
	// theirs, so they read the bundle that keeps the image's roots.
	want := map[string]string{"NODE_EXTRA_CA_CERTS": GuestCAPath, "SSL_CERT_FILE": GuestCABundlePath,
		"REQUESTS_CA_BUNDLE": GuestCABundlePath, "GIT_SSL_CAINFO": GuestCABundlePath}
	for _, name := range caEnv {
		if env[name].Value != want[name] || env[name].Secret {
			t.Errorf("%s = %+v, want %s", name, env[name], want[name])
		}
	}
	if len(want) != len(caEnv) {
		t.Errorf("caEnv is %v", caEnv)
	}
	if env["ANTHROPIC_BASE_URL"].Value != "https://brig.internal/brig-a/r/anthropic" {
		t.Errorf("ANTHROPIC_BASE_URL = %q", env["ANTHROPIC_BASE_URL"].Value)
	}
}

// On brig.internal, git is pointed at the route through GIT_CONFIG_COUNT:
// both of the preset's prefixes are rewritten to the route URL, and its
// credential prompt is answered by a helper that reads the token from the
// environment, never from the config. Only the token variables are Secret,
// and no other variable carries a token.
func TestGuestEnvPointsGitAtBrigInternal(t *testing.T) {
	ep := HostEndpoint{Transport: TransportBrigInternal}
	routes := []v1alpha1.Route{
		presetRoute(t, "github", v1alpha1.PresetGitHubToken, "gh-token"),
		presetRoute(t, "anthropic", v1alpha1.PresetClaudeCode, "claude-credentials"),
	}
	got, err := ep.GuestEnv("brig-a", routes, map[string]string{"anthropic": "tok-a", "github": "tok-g"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := vars(got)
	want := map[string]string{
		"ANTHROPIC_BASE_URL":      "https://brig.internal/brig-a/r/anthropic",
		"CLAUDE_CODE_OAUTH_TOKEN": "tok-a",
		"BRIG_ROUTE_GITHUB_TOKEN": "tok-g",
		"GIT_CONFIG_COUNT":        "4",
		"GIT_CONFIG_KEY_0":        "url.https://brig.internal/brig-a/r/github/.insteadOf",
		"GIT_CONFIG_VALUE_0":      "https://github.com/",
		"GIT_CONFIG_KEY_1":        "url.https://brig.internal/brig-a/r/github/.insteadOf",
		"GIT_CONFIG_VALUE_1":      "git@github.com:",
		"GIT_CONFIG_KEY_2":        "credential.https://brig.internal/brig-a/r/github/.helper",
		"GIT_CONFIG_VALUE_2":      "",
		"GIT_CONFIG_KEY_3":        "credential.https://brig.internal/brig-a/r/github/.helper",
	}
	for name, value := range want {
		if v, ok := env[name]; !ok || v.Value != value {
			t.Errorf("%s = %q (present %v), want %q", name, v.Value, ok, value)
		}
	}
	helper := env["GIT_CONFIG_VALUE_3"].Value
	if !strings.Contains(helper, "$BRIG_ROUTE_GITHUB_TOKEN") || strings.Contains(helper, "tok-g") ||
		!strings.Contains(helper, "username=x-access-token") {
		t.Errorf("the credential helper is %q", helper)
	}
	for _, v := range got {
		secret := v.Value == "tok-a" || v.Value == "tok-g"
		if secret != v.Secret {
			t.Errorf("%s: Secret = %v", v.Name, v.Secret)
		}
		if !v.Secret && (strings.Contains(v.Value, "tok-a") || strings.Contains(v.Value, "tok-g")) {
			t.Errorf("%s carries a token outside a Secret variable", v.Name)
		}
	}
	if _, err := ep.GuestEnv("brig-a", routes, map[string]string{"anthropic": "tok-a"}, nil); err == nil {
		t.Error("a route without a token was handed to the guest")
	}
}

// The broker directory holds keys, so a link or one anyone else can enter is
// refused.
func TestCheckDirRefusesALinkOrAnOpenDirectory(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	if err := os.Mkdir(good, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := CheckDir(good); err != nil {
		t.Errorf("a 0700 directory of your own: %v", err)
	}
	open := filepath.Join(dir, "open")
	if err := os.Mkdir(open, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := CheckDir(open); err == nil {
		t.Error("a group-readable directory was accepted")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if err := CheckDir(link); err == nil || !strings.Contains(err.Error(), "link") {
		t.Errorf("a link: %v", err)
	}
}

// A pid file names a live broker only while that pid is still the process
// that wrote it: a reused pid with another start time is not the broker.
func TestAPidFileOfAnotherProcessIsNotLive(t *testing.T) {
	dir := t.TempDir()
	start, err := ProcessStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	write := func(line string) {
		if err := os.WriteFile(filepath.Join(dir, PIDName), []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(PIDLine(os.Getpid(), start))
	if pid, ok := livePID(dir); !ok || pid != os.Getpid() {
		t.Errorf("this process's own pid file is not live: %d %v", pid, ok)
	}
	write(PIDLine(os.Getpid(), "Thu Jan  1 00:00:00 1970"))
	if _, ok := livePID(dir); ok {
		t.Error("a pid file with another start time is live")
	}
	if err := Forget(dir, "brig-a"); err != nil {
		t.Errorf("forgetting with no broker running: %v", err)
	}
}

// brig starts brig-broker in a session of its own, reuses it while its pid
// file is live, and the broker stops when the last sandbox is forgotten.
// This builds the real binary and runs it; it reads no secret.
func TestRegisterStartsTheBrokerAndForgetStopsIt(t *testing.T) {
	if testing.Short() {
		t.Skip("builds brig-broker")
	}
	setBroker(t)
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	sandbox := Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []Route{{Policy: "p", Route: presetRoute(t, "anthropic", v1alpha1.PresetClaudeCode, "claude-credentials")}}}
	b, err := Register(dir, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = b.Client.Stop(false) })
	pid, ok := livePID(dir)
	if !ok || pid != b.Status.PID || b.Status.Port == 0 {
		t.Fatalf("pid file %d %v, status %+v", pid, ok, b.Status)
	}
	// Its own session: a hangup of brig's terminal does not reach it.
	out, err := exec.Command("ps", "-o", "pgid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if pgid, _ := strconv.Atoi(strings.TrimSpace(string(out))); pgid != pid {
		t.Errorf("the broker's process group is %d, want its own %d", pgid, pid)
	}
	for _, name := range []string{HostKeyName, CAKeyName, AuditName, SocketName, PortName} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s: %v %v", name, info, err)
		}
	}
	// The host key never goes on the command line.
	key, err := os.ReadFile(filepath.Join(dir, HostKeyName))
	if err != nil {
		t.Fatal(err)
	}
	args, err := exec.Command("ps", "-ww", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, form := range []string{string(key), hex.EncodeToString(key), base64.StdEncoding.EncodeToString(key),
		base64.RawURLEncoding.EncodeToString(key)} {
		if strings.Contains(string(args), form) {
			t.Errorf("the broker's command line holds the host key: %q", args)
		}
	}
	if strings.TrimSpace(string(args)) != os.Getenv(BinEnv)+" --dir "+dir {
		t.Errorf("the broker's command line is %q", args)
	}
	// A second registration reuses it.
	again, err := Register(dir, Sandbox{ID: "brig-b", Profile: "claude-code", Routes: sandbox.Routes})
	if err != nil || again.Status.PID != pid {
		t.Fatalf("the second run started another broker: %+v %v", again, err)
	}
	if _, err := again.Client.Mint(MintRequest{Sandbox: "brig-a", Route: "anthropic"}); err != nil {
		t.Fatal(err)
	}
	if err := Forget(dir, "brig-a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := livePID(dir); !ok {
		t.Fatal("the broker stopped with a sandbox still registered")
	}
	if err := Forget(dir, "brig-b"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := livePID(dir); !ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, ok := livePID(dir); ok {
		t.Fatal("the broker did not stop when the last sandbox was forgotten")
	}
	if _, err := (Client{Socket: filepath.Join(dir, SocketName)}).Status(); !errors.Is(err, ErrNotRunning) {
		t.Errorf("status after the stop = %v", err)
	}
	// The port is kept for the next start.
	if blob, err := os.ReadFile(filepath.Join(dir, PortName)); err != nil ||
		strings.TrimSpace(string(blob)) != strconv.Itoa(b.Status.Port) {
		t.Errorf("port file %q %v, want %d", blob, err, b.Status.Port)
	}
}

// brokerBinary is brig-broker built from this tree, once per test binary.
var brokerBinary = struct {
	once sync.Once
	path string
	err  error
}{}

// TestMain removes the brig-broker the tests built. Tests share it, so no
// one test's cleanup can.
func TestMain(m *testing.M) {
	code := m.Run()
	if brokerBinary.path != "" {
		_ = os.RemoveAll(filepath.Dir(brokerBinary.path))
	}
	os.Exit(code)
}

// setBroker points BinEnv at a brig-broker built from this tree, and
// BRIG_STATE_DIR at a fresh directory short enough for a unix socket.
func setBroker(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds brig-broker")
	}
	brokerBinary.once.Do(func() {
		dir, err := os.MkdirTemp("", "bib")
		if err != nil {
			brokerBinary.err = err
			return
		}
		brokerBinary.path = filepath.Join(dir, "brig-broker")
		out, err := exec.Command("go", "build", "-o", brokerBinary.path, "github.com/brig-sh/brig/cmd/brig-broker").CombinedOutput()
		if err != nil {
			brokerBinary.err = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if brokerBinary.err != nil {
		t.Fatal(brokerBinary.err)
	}
	tmp, err := os.MkdirTemp("", "bi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv(BinEnv, brokerBinary.path)
	t.Setenv("BRIG_STATE_DIR", filepath.Join(tmp, "state"))
	return tmp
}

func claudeSandbox(t *testing.T, id string) Sandbox {
	return Sandbox{ID: id, Profile: "claude-code",
		Routes: []Route{{Policy: "p", Route: presetRoute(t, "anthropic", v1alpha1.PresetClaudeCode, "claude-credentials")}}}
}

// stopBroker stops the broker serving dir, if any.
func stopBroker(dir string) {
	_, _ = (Client{Socket: filepath.Join(dir, SocketName)}).Stop(false)
	waitExit(dir)
}

// Runs started at once on a fresh host start one broker, and all of them
// register with it on the one port it bound and recorded.
func TestConcurrentRegistrationsStartOneBroker(t *testing.T) {
	setBroker(t)
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopBroker(dir) })
	const n = 12
	var wg sync.WaitGroup
	ports := make([]int, n)
	pids := make([]int, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := Register(dir, claudeSandbox(t, fmt.Sprintf("brig-%d", i)))
			if err != nil {
				errs[i] = err
				return
			}
			ports[i], pids[i] = b.Status.Port, b.Status.PID
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("registration %d failed: %v", i, err)
		}
	}
	for i := range ports {
		if ports[i] != ports[0] || pids[i] != pids[0] || ports[0] == 0 {
			t.Fatalf("the runs found different brokers: ports %v pids %v", ports, pids)
		}
	}
	if port, err := recordedPort(dir); err != nil || port != ports[0] {
		t.Errorf("the port file holds %d (%v), and the broker bound %d", port, err, ports[0])
	}
	st, err := (Client{Socket: filepath.Join(dir, SocketName)}).Status()
	if err != nil || len(st.Sandboxes) != n {
		t.Errorf("the broker serves %v (%v), want all %d sandboxes", st.Sandboxes, err, n)
	}
	t.Logf("%d concurrent registrations: one broker, pid %d, port %d", n, pids[0], ports[0])
}

// The recorded port is the one the broker binds at every start. When another process
// holds it, brig refuses the run, starts nothing, and says why.
func TestRegisterRefusesWhenThePortIsHeld(t *testing.T) {
	setBroker(t)
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	squatter, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = squatter.Close() }()
	port := squatter.Addr().(*net.TCPAddr).Port
	if err := prepareDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, PortName), []byte(fmt.Sprintf("%d\n", port)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Register(dir, claudeSandbox(t, "brig-a"))
	var held *PortHeldError
	if !errors.As(err, &held) || held.Port != port || !strings.Contains(err.Error(), PortHeldMessage) {
		t.Fatalf("Register = %v, want the port-held refusal for %d", err, port)
	}
	t.Logf("refusal: %v", err)
	if _, ok := livePID(dir); ok {
		t.Error("a broker runs after the refusal")
	}
	if p, _ := recordedPort(dir); p != port {
		t.Errorf("the port file moved to %d", p)
	}
}

// brig refuses a host directory that is a link, before it writes anything
// through it.
func TestRegisterRefusesALinkedHostDirectory(t *testing.T) {
	tmp := setBroker(t)
	target := filepath.Join(tmp, "elsewhere")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(tmp, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(state, "host")); err != nil {
		t.Fatal(err)
	}
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Register(dir, claudeSandbox(t, "brig-a")); err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("Register through a linked host directory = %v", err)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Errorf("brig wrote through the link: %v", entries)
	}
}

// A username from the policy is quoted in the credential helper, so the
// shell prints it and runs nothing.
func TestTheGitHelperQuotesTheUsername(t *testing.T) {
	r := presetRoute(t, "github", v1alpha1.PresetGitHubToken, "gh-token")
	r.Inject.BasicAuth.Username = "x';touch planted;'"
	got, err := (HostEndpoint{Transport: TransportBrigInternal}).GuestEnv("brig-a", []v1alpha1.Route{r},
		map[string]string{"github": "tok-g"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	helper := strings.TrimPrefix(vars(got)["GIT_CONFIG_VALUE_3"].Value, "!")
	dir := t.TempDir()
	cmd := exec.Command("/bin/sh", "-c", helper+` "$@"`, "sh", "get")
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "BRIG_ROUTE_GITHUB_TOKEN=tok-g"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "username=x';touch planted;'\npassword=tok-g\n" {
		t.Errorf("the helper printed %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "planted")); err == nil {
		t.Error("the username ran a command")
	}
}

// The git helper reads the token from BRIG_ROUTE_<NAME>_TOKEN, with - read
// as _. A dot in the name ends the variable for the shell, so the helper of
// gh.x read $BRIG_ROUTE_GH and git sent the wrong password, and a_b shared
// the variable of a-b. The policy grammar refuses such a name, and GuestEnv
// refuses one that got past it.
func TestGuestEnvRefusesARouteNameThatIsNotOneVariable(t *testing.T) {
	ep := HostEndpoint{Transport: TransportBrigInternal}
	for _, name := range []string{"gh.x", "a_b", "Gh", "gh x"} {
		r := presetRoute(t, "github", v1alpha1.PresetGitHubToken, "gh-token")
		r.Name = name
		if _, err := ep.GuestEnv("brig-a", []v1alpha1.Route{r}, map[string]string{name: "tok"}, nil); err == nil {
			t.Errorf("GuestEnv accepted the route name %q", name)
		}
	}
	got, err := ep.GuestEnv("brig-a", []v1alpha1.Route{presetRoute(t, "gh-x", v1alpha1.PresetGitHubToken, "gh-token")},
		map[string]string{"gh-x": "tok-g"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := vars(got)
	if v := env[TokenVar("gh-x")]; v.Name != "BRIG_ROUTE_GH_X_TOKEN" || v.Value != "tok-g" {
		t.Fatalf("the token variable of gh-x is %+v", v)
	}
	helper := strings.TrimPrefix(env["GIT_CONFIG_VALUE_3"].Value, "!")
	cmd := exec.Command("/bin/sh", "-c", helper+` "$@"`, "sh", "get")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "BRIG_ROUTE_GH=wrong", "BRIG_ROUTE_GH_X_TOKEN=tok-g"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(out), "password=tok-g\n") {
		t.Errorf("the helper of gh-x printed %q", out)
	}
}

// git entries the guest already gets are kept, and the routes' entries are
// numbered after them. A count that is not one is refused.
func TestGitEntriesAddToTheGuestsOwn(t *testing.T) {
	routes := []v1alpha1.Route{presetRoute(t, "github", v1alpha1.PresetGitHubToken, "gh-token")}
	own := []runtime.Var{{Name: "GIT_CONFIG_COUNT", Value: "2"}, {Name: "GIT_CONFIG_KEY_0", Value: "core.editor"}}
	got, err := (HostEndpoint{Transport: TransportBrigInternal}).GuestEnv("brig-a", routes, map[string]string{"github": "t"}, own)
	if err != nil {
		t.Fatal(err)
	}
	env := vars(got)
	if env["GIT_CONFIG_COUNT"].Value != "6" || env["GIT_CONFIG_KEY_2"].Value == "" || env["GIT_CONFIG_KEY_5"].Value == "" {
		t.Errorf("count %q, keys 2 %q and 5 %q", env["GIT_CONFIG_COUNT"].Value, env["GIT_CONFIG_KEY_2"].Value,
			env["GIT_CONFIG_KEY_5"].Value)
	}
	if _, ok := env["GIT_CONFIG_KEY_0"]; ok {
		t.Error("a route entry took the guest's own key 0")
	}
	bad := []runtime.Var{{Name: "GIT_CONFIG_COUNT", Value: "two"}}
	if _, err := (HostEndpoint{Transport: TransportBrigInternal}).GuestEnv("brig-a", routes, map[string]string{"github": "t"}, bad); err == nil {
		t.Error("a GIT_CONFIG_COUNT that is not a count was accepted")
	}
}

// registered is what the broker serving dir reports it serves, or nil when
// none answers.
func registered(t *testing.T, dir string) []string {
	t.Helper()
	st, err := (Client{Socket: filepath.Join(dir, SocketName)}).Status()
	if errors.Is(err, ErrNotRunning) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return st.Sandboxes
}

// brig stop and brig rm revoke a sandbox's tokens whatever locale they run
// under. The broker records its start time under the locale brig started it
// with, and a later brig can run under another one.
func TestForgetRevokesUnderAnotherLocale(t *testing.T) {
	setBroker(t)
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopBroker(dir) })
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_TIME", "")
	for _, id := range []string{"brig-a", "brig-b"} {
		if _, err := Register(dir, claudeSandbox(t, id)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LC_ALL", "el_GR.UTF-8")
	t.Setenv("LC_TIME", "el_GR.UTF-8")
	if _, ok := Running(dir); !ok {
		t.Error("under another locale, brig does not see the running broker")
	}
	if err := Forget(dir, "brig-a"); err != nil {
		t.Fatal(err)
	}
	if got := registered(t, dir); strings.Join(got, " ") != "brig-b" {
		t.Errorf("after forgetting brig-a under another locale the broker serves %v, want [brig-b]", got)
	}
}

// A sandbox name can be longer than 64 bytes. A routed run under such a
// name registers, mints and is forgotten like any other.
func TestALongSandboxNameRegisters(t *testing.T) {
	setBroker(t)
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopBroker(dir) })
	id := "brig-routed-payment-service-refactor-for-the-q4-billing-migration-x"
	if len(id) != 67 {
		t.Fatalf("the name is %d bytes", len(id))
	}
	b, err := Register(dir, claudeSandbox(t, id))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Client.Mint(MintRequest{Sandbox: id, Route: "anthropic"}); err != nil {
		t.Fatal(err)
	}
	if err := Forget(dir, id); err != nil {
		t.Fatal(err)
	}
	waitExit(dir)
	if _, ok := livePID(dir); ok {
		t.Error("the broker did not stop when its one sandbox was forgotten")
	}
}

// A registration the broker refuses leaves no broker behind that this run
// started: nothing would ever forget a sandbox that was never registered.
func TestAFailedRegistrationStopsTheBrokerItStarted(t *testing.T) {
	setBroker(t)
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopBroker(dir) })
	if _, err := Register(dir, claudeSandbox(t, "brig a")); err == nil {
		t.Fatal("a sandbox id with a space was registered")
	}
	waitExit(dir)
	if pid, ok := livePID(dir); ok {
		t.Errorf("the broker this run started, pid %d, still runs with nothing registered", pid)
	}
	// A broker another run is using keeps running.
	if _, err := Register(dir, claudeSandbox(t, "brig-b")); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(dir, claudeSandbox(t, "brig a")); err == nil {
		t.Fatal("a sandbox id with a space was registered")
	}
	if got := registered(t, dir); strings.Join(got, " ") != "brig-b" {
		t.Errorf("after a refused registration beside brig-b the broker serves %v", got)
	}
}

// ForgetAll removes every registration the broker holds, including one no
// runtime lists, and the broker stops.
func TestForgetAllRevokesEveryRegistration(t *testing.T) {
	setBroker(t)
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopBroker(dir) })
	if got, err := ForgetAll(dir); err != nil || len(got) != 0 {
		t.Fatalf("ForgetAll with no broker = %v, %v", got, err)
	}
	for _, id := range []string{"brig-a", "brig-unlisted"} {
		if _, err := Register(dir, claudeSandbox(t, id)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ForgetAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "brig-a brig-unlisted" {
		t.Errorf("ForgetAll removed %v, want both sandboxes", got)
	}
	waitExit(dir)
	if _, ok := livePID(dir); ok {
		t.Error("the broker still runs after ForgetAll")
	}
}

// Forget asks the broker's socket even when the pid file names no live
// broker: a pid check that is wrong must not leave every token working.
func TestForgetAsksTheSocketWhateverThePidFileSays(t *testing.T) {
	setBroker(t)
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopBroker(dir) })
	for _, id := range []string{"brig-a", "brig-b"} {
		if _, err := Register(dir, claudeSandbox(t, id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, PIDName), []byte(PIDLine(os.Getpid(), "0.0")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := livePID(dir); ok {
		t.Fatal("the rewritten pid file is live")
	}
	if err := Forget(dir, "brig-a"); err != nil {
		t.Fatal(err)
	}
	if got := registered(t, dir); strings.Join(got, " ") != "brig-b" {
		t.Errorf("with a pid file naming no broker, Forget left the broker serving %v", got)
	}
}

// Forget does not take the start lock, so another brig's Forget can stop an
// idle broker after Register found it and before it registered. The
// registration then finds no broker listening, and Register starts one, as
// it does for a broker that answered 503.
func TestRegisterOutlivesABrokerStoppedUnderIt(t *testing.T) {
	setBroker(t)
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopBroker(dir) })
	first, err := Register(dir, claudeSandbox(t, "brig-a"))
	if err != nil {
		t.Fatal(err)
	}
	stops := 0
	beforeAdd = func() {
		if stops > 0 {
			return
		}
		stops++
		// Another brig forgets the last sandbox, and the idle broker exits.
		if err := Forget(dir, "brig-a"); err != nil {
			t.Error(err)
		}
		waitExit(dir)
	}
	t.Cleanup(func() { beforeAdd = func() {} })
	b, err := Register(dir, claudeSandbox(t, "brig-b"))
	if err != nil {
		t.Fatalf("Register after the broker was stopped under it: %v", err)
	}
	if stops != 1 {
		t.Fatalf("the broker was stopped %d times, want once", stops)
	}
	if b.Status.PID == first.Status.PID {
		t.Errorf("Register returned the broker that stopped, pid %d", b.Status.PID)
	}
	if got := registered(t, dir); strings.Join(got, " ") != "brig-b" {
		t.Errorf("the broker serves %v, want brig-b", got)
	}
}
