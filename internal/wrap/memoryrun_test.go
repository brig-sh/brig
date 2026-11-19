package wrap

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/memory"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/verify"
)

// Output keeps what brig writes into the guest as root: the files the create
// scripts make, the CA bundle, and the size check of a fed file. Anything
// else is the liveness runtime's.
func (r *memoryRuntime) Output(spec runtime.ExecSpec) (string, error) {
	r.guestRuns = append(r.guestRuns, spec)
	if r.guestFiles == nil {
		r.guestFiles = map[string]string{}
	}
	if spec.User == guestRootUser && len(spec.Cmd) > 5 && spec.Cmd[0] == "sh" {
		switch spec.Cmd[2] {
		case caCreateScript, guestFileCreateScript:
			r.guestFiles[spec.Cmd[5]] = ""
			return "", nil
		case caBundleScript:
			r.guestFiles[spec.Cmd[4]] = r.guestFiles[spec.Cmd[5]]
			return "", nil
		case caSizeScript:
			if fmt.Sprint(len(r.guestFiles[spec.Cmd[4]])) != spec.Cmd[5] {
				return "", errors.New("exit status 1")
			}
			return "", nil
		}
	}
	return r.livenessRuntime.Output(spec)
}

func (r *memoryRuntime) Feed(spec runtime.ExecSpec) error {
	r.guestRuns = append(r.guestRuns, spec)
	body, err := io.ReadAll(spec.Stdin)
	if err != nil {
		return err
	}
	target := spec.Cmd[len(spec.Cmd)-1]
	if _, ok := r.guestFiles[target]; !ok {
		return fmt.Errorf("fed %s before it was created", target)
	}
	r.guestFiles[target] += string(body)
	return nil
}

// wantMCPConfig is the MCP configuration of sandbox vm, as Claude Code reads
// it. It names the token's variable, which Claude Code expands, never a
// token.
func wantMCPConfig(vm string) string {
	return `{"mcpServers":{"brig-memory":{"type":"http","url":"https://brig.internal/` + vm +
		`/memory","headers":{"Authorization":"Bearer ${BRIG_MEMORY_TOKEN}"}}}}`
}

// An exec of a sandbox with memory registers it with the broker at the
// guest's brig.internal address, mints a memory token, and hands the guest the
// token in its environment, marked Secret, with Claude Code's own memory off
// and the CA variables a memory-only run gets from no route. The divert is
// brig.internal alone: no route, so no link-local address. The run names the
// store, and no command run in the guest carries the token.
func TestMemoryEnvOnExec(t *testing.T) {
	p, home := memorySession(t)
	rt := &memoryRuntime{&livenessRuntime{workspace: home}}
	c := loadMemory(t, p, Options{Workspace: home, Memory: "team"}, rt)
	broker := &fakeBroker{}
	c.broker = broker
	warn := &bytes.Buffer{}
	c.Err = warn
	set := creds.Set{}
	if err := c.EnsureRunning(set); err != nil {
		t.Fatal(err)
	}
	// A named store has no project.
	want := injector.MemorySandbox{ID: c.VMName, Session: c.RawName, Key: "team", BrigInternal: "198.18.1.1"}
	if len(broker.memRegistered) != 1 || broker.memRegistered[0] != want {
		t.Fatalf("registered for memory %+v, want %+v", broker.memRegistered, want)
	}
	if len(broker.registered) != 0 {
		t.Errorf("a run with no route registered routes %+v", broker.registered)
	}
	if len(broker.memMinted) != 1 || broker.memMinted[0].Sandbox != c.VMName ||
		broker.memMinted[0].Session != c.RawName || broker.memMinted[0].TTLSeconds <= 0 {
		t.Errorf("minted %+v", broker.memMinted)
	}
	token := "memory-token-" + c.VMName
	env := envOf(c.execSpec(set, []string{"claude"}, false).Env)
	if v := env["BRIG_MEMORY_TOKEN"]; v.Value != token || !v.Secret {
		t.Errorf("BRIG_MEMORY_TOKEN = %+v, want the memory token, marked Secret", v)
	}
	for name, v := range map[string]string{
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1",
		"NODE_EXTRA_CA_CERTS":             injector.GuestCAPath,
		"SSL_CERT_FILE":                   injector.GuestCABundlePath,
	} {
		if env[name].Value != v {
			t.Errorf("%s = %q, want %q", name, env[name].Value, v)
		}
	}
	for _, name := range []string{"ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if _, ok := env[name]; ok {
			t.Errorf("a memory-only run got the route variable %s", name)
		}
	}
	dir, _ := injector.Dir()
	sock, _ := injector.DivertSocket(dir, c.VMName)
	if d := rt.spec.Egress.Divert; !d.BrigInternal || d.Socket != sock || len(d.Routes) != 0 {
		t.Errorf("Divert = %+v, want brig.internal alone to %s", d, sock)
	}
	if c.MCPConfig() != "/brig/mcp/mcp.json" {
		t.Errorf("MCPConfig = %q", c.MCPConfig())
	}
	if rt.guestFiles[injector.GuestCAPath] != testBrigCA() {
		t.Error("the CA the broker handed the memory registration did not reach the guest")
	}
	for _, spec := range rt.guestRuns {
		if strings.Contains(strings.Join(spec.Cmd, " "), token) {
			t.Errorf("the memory token was on a command line: %q", spec.Cmd)
		}
	}
	if !strings.Contains(warn.String(), "memory: team\n") {
		t.Errorf("the run did not name its store: %q", warn.String())
	}
	if strings.Contains(warn.String(), token) {
		t.Errorf("the memory token was printed: %q", warn.String())
	}
}

// The run names a project's store by its folder, as the user asked for it.
func TestMemoryLineNamesTheProject(t *testing.T) {
	p, home := memorySession(t)
	project := t.TempDir()
	rt := &memoryRuntime{&livenessRuntime{workspace: home}}
	c := loadMemory(t, p, Options{Workspace: home, Project: project, Memory: memoryProject}, rt)
	warn := &bytes.Buffer{}
	c.Err = warn
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if want := "memory: project (" + c.Project + ")\n"; !strings.Contains(warn.String(), want) {
		t.Errorf("the run said %q, want %q", warn.String(), want)
	}
}

// A project's store is registered with its folder, which the broker records
// for `brig memory ls`. A named store is registered with none.
func TestMemoryRegistrationCarriesTheProject(t *testing.T) {
	p, home := memorySession(t)
	project := t.TempDir()
	rt := &memoryRuntime{&livenessRuntime{workspace: home}}
	c := loadMemory(t, p, Options{Workspace: home, Project: project, Memory: memoryProject}, rt)
	broker := &fakeBroker{}
	c.broker = broker
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if len(broker.memRegistered) != 1 || broker.memRegistered[0].Project != c.Project ||
		broker.memRegistered[0].Key != memory.ProjectKey(c.Project) {
		t.Errorf("registered %+v, want the project %s with its key", broker.memRegistered, c.Project)
	}
}

// The MCP configuration reaches the guest outside its home, beside the brig
// CA, and holds the variable the token is in, never the token.
func TestMemoryConfigHasNoToken(t *testing.T) {
	p, home := memorySession(t)
	rt := &memoryRuntime{&livenessRuntime{workspace: home}}
	c := loadMemory(t, p, Options{Workspace: home, Memory: "team"}, rt)
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	got, ok := rt.guestFiles[mcpConfigPath]
	if !ok {
		t.Fatalf("no %s in the guest: %v", mcpConfigPath, rt.guestFiles)
	}
	if got != wantMCPConfig(c.VMName) {
		t.Errorf("%s holds\n%s\nwant\n%s", mcpConfigPath, got, wantMCPConfig(c.VMName))
	}
	if strings.Contains(got, "memory-token") {
		t.Error("the MCP configuration holds the memory token")
	}
	if strings.HasPrefix(mcpConfigPath, c.Profile.GuestHome+"/") {
		t.Errorf("%s is in the guest home %s", mcpConfigPath, c.Profile.GuestHome)
	}
}

// A run without memory registers nothing for it, mints nothing, writes no
// MCP configuration and hands the guest neither variable, and its divert
// has no brig.internal.
func TestNoMemoryNoTokenNoArgs(t *testing.T) {
	p, home := memorySession(t)
	rt := &memoryRuntime{&livenessRuntime{workspace: home}}
	c := loadMemory(t, p, Options{Workspace: home}, rt)
	broker := &fakeBroker{}
	c.broker = broker
	set := creds.Set{}
	if err := c.EnsureRunning(set); err != nil {
		t.Fatal(err)
	}
	if len(broker.memRegistered) != 0 || len(broker.memMinted) != 0 {
		t.Errorf("registered %+v and minted %+v for a run without memory", broker.memRegistered, broker.memMinted)
	}
	for _, call := range broker.calls {
		if strings.HasPrefix(call, "register") {
			t.Errorf("the broker was asked to %s", call)
		}
	}
	env := envOf(c.execSpec(set, []string{"claude"}, false).Env)
	for _, name := range []string{"BRIG_MEMORY_TOKEN", "CLAUDE_CODE_DISABLE_AUTO_MEMORY", "NODE_EXTRA_CA_CERTS"} {
		if _, ok := env[name]; ok {
			t.Errorf("a run without memory got %s", name)
		}
	}
	if c.MCPConfig() != "" {
		t.Errorf("MCPConfig = %q for a run without memory", c.MCPConfig())
	}
	if _, ok := rt.guestFiles[mcpConfigPath]; ok {
		t.Errorf("a run without memory wrote %s", mcpConfigPath)
	}
	if d := rt.spec.Egress.Divert; d.BrigInternal || d.Socket != "" {
		t.Errorf("Divert = %+v for a run without memory or route", d)
	}
}

// A broker that started again since the last exec has lost the sandbox's
// memory. The next exec registers it again before it mints, so the token it
// hands the guest is one the broker accepts. An exec of a running sandbox
// never removes its memory: a token another exec's agent holds stays valid.
func TestMemoryReRegistersEachExec(t *testing.T) {
	p, home, rt := runningMemorySandbox(t, "team")
	broker := &fakeBroker{}
	for i := range 2 {
		c := loadMemory(t, p, Options{Workspace: home, NoProject: true}, rt)
		c.broker = broker
		if err := c.PrepareWorkspace(); err != nil {
			t.Fatal(err)
		}
		if err := c.EnsureRunning(creds.Set{}); err != nil {
			t.Fatalf("exec %d: %v", i+1, err)
		}
		if rt.boots != 0 {
			t.Fatalf("exec %d rebooted the running sandbox", i+1)
		}
		if got := envOf(c.execSpec(creds.Set{}, nil, false).Env)["BRIG_MEMORY_TOKEN"].Value; got != "memory-token-brig-claude-code" {
			t.Errorf("exec %d: BRIG_MEMORY_TOKEN = %q", i+1, got)
		}
		// The broker started again and holds nothing.
		broker.memory = nil
	}
	if len(broker.memRegistered) != 2 || len(broker.memMinted) != 2 {
		t.Errorf("registered %+v and minted %+v, want both on each exec", broker.memRegistered, broker.memMinted)
	}
	if len(broker.forgotten) != 0 {
		t.Errorf("an exec of a running sandbox removed its memory: forgotten %v", broker.forgotten)
	}
	if !slices.Equal(broker.routesForgotten, []string{"brig-claude-code", "brig-claude-code"}) {
		t.Errorf("routes forgotten %v, want the routes of the sandbox removed on each exec", broker.routesForgotten)
	}
}

// brig stop and brig rm remove the memory of a sandbox that has no route, so
// the memory token its agent holds is refused from then on.
func TestStopForgetsMemory(t *testing.T) {
	p, home := memorySession(t)
	rt := &memoryRuntime{&livenessRuntime{workspace: home}}
	c := loadMemory(t, p, Options{Workspace: home, Memory: "team"}, rt)
	broker := &fakeBroker{}
	c.broker = broker
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := broker.memory[c.VMName]; !ok {
		t.Fatal("the run did not register its memory")
	}
	broker.forgotten = nil
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(broker.forgotten, []string{c.VMName}) {
		t.Errorf("forgotten %v on stop, want [%s]", broker.forgotten, c.VMName)
	}
	if _, ok := broker.memory[c.VMName]; ok {
		t.Error("brig stop left the memory registered")
	}
	_ = c.Remove()
	if !slices.Equal(broker.forgotten, []string{c.VMName, c.VMName}) {
		t.Errorf("forgotten %v, want the sandbox on stop and on rm", broker.forgotten)
	}
}

// brig rm removes the sandbox, never its store: what the agent remembered is
// the user's, and the next sandbox on the store reads it.
func TestRmKeepsStore(t *testing.T) {
	p, home := memorySession(t)
	rt := &memoryRuntime{&livenessRuntime{workspace: home}}
	c := loadMemory(t, p, Options{Workspace: home, Memory: "team"}, rt)
	root, err := injector.MemoryDir()
	if err != nil {
		t.Fatal(err)
	}
	f, err := memory.OpenFiles(root, "team")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	_ = c.Remove()
	if fi, err := os.Stat(filepath.Join(root, "team")); err != nil || !fi.IsDir() {
		t.Errorf("brig rm took the store: %v", err)
	}
}

// otherBrigCA is a second CA constrained to brig.internal, as a broker other
// than the one the routes registered with would hand over.
func otherBrigCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(8), Subject: pkix.Name{CommonName: "other brig CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
		PermittedDNSDomainsCritical: true, PermittedDNSDomains: []string{injector.BrigInternal}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// A run with a route and memory registers the routes first, then the memory,
// on the one divert socket and under the one CA. The guest gets the route's
// variables and the memory's, and the CA variables once.
func TestMemoryBesideARoute(t *testing.T) {
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	t.Setenv("BRIG_MEMORY", "team")
	shortStateDir(t)
	rec := &hullRecorder{}
	c := routedConfig(t, claudeEndpoint, rec)
	shortStateDir(t)
	c.Verify = verify.Off
	broker := &fakeBroker{}
	c.broker = broker
	set, err := c.BuildEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureRunning(set); err == nil || !strings.Contains(err.Error(), "not booting") {
		t.Fatalf("the run did not reach the runtime: %v", err)
	}
	if i, j := slices.Index(broker.calls, "register "+c.VMName), slices.Index(broker.calls, "register-memory "+c.VMName); i < 0 || j < i {
		t.Errorf("calls %v, want the routes registered and then the memory", broker.calls)
	}
	if len(broker.memRegistered) != 1 || broker.memRegistered[0].BrigInternal != broker.registered[0].BrigInternal {
		t.Errorf("memory %+v and routes %+v name different brig.internal addresses", broker.memRegistered, broker.registered)
	}
	vars := c.execSpec(set, []string{"claude"}, false).Env
	env := envOf(vars)
	if env["ANTHROPIC_BASE_URL"].Value == "" || env["BRIG_MEMORY_TOKEN"].Value == "" {
		t.Errorf("the guest did not get both the route and the memory: %v", env)
	}
	n := 0
	for _, v := range vars {
		if v.Name == "NODE_EXTRA_CA_CERTS" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("NODE_EXTRA_CA_CERTS is set %d times", n)
	}
}

// A memory registration that hands over another CA than the routes did was
// answered by another broker. The guest would trust one and be served the
// other, so the run is refused before boot.
func TestMemoryAndRoutesMustShareTheCA(t *testing.T) {
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	t.Setenv("BRIG_MEMORY", "team")
	shortStateDir(t)
	rec := &hullRecorder{}
	c := routedConfig(t, claudeEndpoint, rec)
	shortStateDir(t)
	c.Verify = verify.Off
	c.broker = &fakeBroker{memoryCA: otherBrigCA(t)}
	err := c.EnsureRunning(creds.Set{})
	if err == nil || !strings.Contains(err.Error(), "a different brig CA") {
		t.Fatalf("EnsureRunning = %v", err)
	}
	if rec.spec != nil {
		t.Error("a run whose broker handed over two CAs booted")
	}
}

// A running memory sandbox that is recreated is a new guest. The run removes
// the whole registration, memory included, before the new boot registers it
// again, so the broker draws a new key and the memory token the old guest
// holds is refused. Removing the routes alone would keep that token valid.
func TestARecreatedMemorySandboxForgetsItsMemoryFirst(t *testing.T) {
	p, home, rt := runningMemorySandbox(t, "team")
	c := loadMemory(t, p, Options{Workspace: home, NoProject: true}, rt)
	broker := &fakeBroker{memory: map[string]injector.MemorySandbox{
		"brig-claude-code": {ID: "brig-claude-code", Key: "team", BrigInternal: "198.18.1.1"}}}
	c.broker = broker
	if err := c.PrepareWorkspace(); err != nil {
		t.Fatal(err)
	}
	// The running guest no longer mounts this workspace, so brig recreates
	// it.
	rt.workspace = t.TempDir()
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if rt.boots != 1 || rt.stops != 1 {
		t.Fatalf("booted %d times and stopped %d, want one boot after one stop", rt.boots, rt.stops)
	}
	forget := slices.Index(broker.calls, "forget brig-claude-code")
	register := slices.Index(broker.calls, "register-memory brig-claude-code")
	if forget < 0 || register < 0 || forget > register {
		t.Errorf("calls %v, want the sandbox forgotten, memory included, before its memory is registered again",
			broker.calls)
	}
	if len(broker.routesForgotten) != 0 {
		t.Errorf("a recreate removed the routes alone: %v", broker.routesForgotten)
	}
}
