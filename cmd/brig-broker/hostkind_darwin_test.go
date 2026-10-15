//go:build darwin

package main

import (
	"bytes"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/runtime"
)

// hostScript runs in the host sandbox. It reaches the route with the
// sandbox's own token and with another sandbox's, tries a loopback port the
// profile does not allow, the broker's directory and the broker's process,
// and leaves its environment in the guest home for the test to read. No
// token is put on a command line: curl reads its header from stdin.
const hostScript = `
code() { printf 'header = "Authorization: Bearer %s"\n' "$1" | curl -s -o /dev/null -w '%{http_code}' --max-time 10 -K - -X POST -d '{}' "$2"; }
echo "route:$(code "$CLAUDE_CODE_OAUTH_TOKEN" "$ANTHROPIC_BASE_URL/v1/messages")"
echo "foreign:$(code "$FOREIGN_TOKEN" "$ANTHROPIC_BASE_URL/v1/messages")"
curl -s -o /dev/null --max-time 5 "http://127.0.0.1:$OTHER_PORT/"; echo "other:$?"
cat "$BROKER_DIR/hostkey" > /dev/null 2>&1; echo "brokerdir:$?"
kill -0 "$BROKER_PID" 2> /dev/null; echo "signal:$?"
env > "$HOME/env.txt"
`

// The host kind end to end, under the real sandbox-exec, against a broker
// serving in this process with a memory store and a TLS upstream standing in
// for api.anthropic.com.
//
// Inside the sandbox, the route answers the sandbox's own session token with
// the upstream's answer, and the upstream sees the stored access token and
// never the session token. Another sandbox's token is refused with 401. A
// loopback port the profile does not name, and the broker's own directory,
// are denied, and so is a signal to the broker. The credential is nowhere in the sandbox's environment, its
// home, the process table or the audit log. After the sandbox is forgotten,
// as brig stop does, its token gets 404.
func TestTheHostKindReachesItsRouteAndNothingElse(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("no sandbox-exec on this host")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl on this host")
	}
	state := shortDir(t)
	t.Setenv("BRIG_STATE_DIR", state)
	t.Setenv("BRIG_GATEWAY_DIR", filepath.Join(state, "gw"))
	t.Setenv("BRIG_RUNTIME", "host")
	t.Setenv("BRIG_RUNTIME_BIN", "")
	dir, err := injector.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	u := newUpstream(t)
	r := startBroker(t, dir, u, nil)
	for _, id := range []string{"brig-routed", "brig-other"} {
		if err := r.client.AddSandbox(injector.Sandbox{ID: id, Profile: "claude-code",
			Routes: []injector.Route{claudeRoute()}}); err != nil {
			t.Fatal(err)
		}
	}
	own := r.mint(t, "brig-routed", "anthropic")
	foreign := r.mint(t, "brig-other", "anthropic")

	// A loopback listener the sandbox's profile does not name.
	var reachedOther atomic.Int32
	other, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	otherSrv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reachedOther.Add(1)
	})}
	go func() { _ = otherSrv.Serve(other) }()
	t.Cleanup(func() { _ = otherSrv.Close() })

	expanded, err := policy.ExpandRoute(claudeRoute().Route)
	if err != nil {
		t.Fatal(err)
	}
	ep := injector.EndpointFor(runtime.HostKind, r.srv.port)
	vars, err := ep.GuestEnv("brig-routed", []v1alpha1.Route{expanded}, map[string]string{"anthropic": own}, nil)
	if err != nil {
		t.Fatal(err)
	}
	vars = append(vars,
		runtime.Var{Name: "HOME", Value: "/root"},
		runtime.Var{Name: "FOREIGN_TOKEN", Value: foreign, Secret: true},
		runtime.Var{Name: "OTHER_PORT", Value: strconv.Itoa(other.Addr().(*net.TCPAddr).Port)},
		runtime.Var{Name: "BROKER_DIR", Value: dir},
		// The broker serves in this process here.
		runtime.Var{Name: "BROKER_PID", Value: strconv.Itoa(os.Getpid())})

	rt, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	if rt.Kind() != runtime.HostKind {
		t.Fatalf("detected %s, want the host kind", rt.Kind())
	}
	home := filepath.Join(state, "homes", "brig-routed")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := rt.Run(runtime.RunSpec{
		Name:          "brig-routed",
		Net:           "isolated",
		Shares:        []runtime.Share{{Host: home, Guest: "/root"}},
		LoopbackPorts: ep.LoopbackPorts(),
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Remove("brig-routed") })
	out, err := rt.Output(runtime.ExecSpec{Name: "brig-routed", Cmd: []string{"/bin/sh", "-c", hostScript},
		Cwd: "/root", Env: vars})
	if err != nil {
		t.Fatalf("the sandboxed script failed: %v\n%s", err, out)
	}
	t.Logf("inside the host sandbox:\n%s", out)
	for _, want := range []string{"route:200", "foreign:401", "brokerdir:1", "signal:1"} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("the sandbox did not report %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "other:0\n") || reachedOther.Load() != 0 {
		t.Errorf("the sandbox reached a loopback port its profile does not name:\n%s", out)
	}

	seen := u.requests()
	if len(seen) != 1 {
		t.Fatalf("upstream saw %d requests, want the one with the sandbox's own token", len(seen))
	}
	t.Logf("upstream saw Authorization: %s", redactTail(seen[0].Get("Authorization")))
	if seen[0].Get("Authorization") != "Bearer "+fakeAccess {
		t.Errorf("upstream saw %q, want the stored access token", seen[0].Get("Authorization"))
	}
	for name, values := range seen[0] {
		for _, v := range values {
			if strings.Contains(v, own) || strings.Contains(v, foreign) {
				t.Errorf("a session token reached upstream in %s", name)
			}
		}
	}

	// The credential is nowhere the sandbox can see, and nowhere brig left
	// for anyone else.
	leaks := map[string][]byte{}
	if err := filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		blob, err := os.ReadFile(p)
		leaks[p] = blob
		return err
	}); err != nil {
		t.Fatal(err)
	}
	audit, err := os.ReadFile(filepath.Join(dir, injector.AuditName))
	if err != nil {
		t.Fatal(err)
	}
	leaks["audit.log"] = audit
	for where, blob := range leaks {
		if bytes.Contains(blob, []byte(fakeAccess)) || bytes.Contains(blob, []byte("fake-refresh")) {
			t.Errorf("the credential is in %s", where)
		}
	}
	for _, s := range []string{own, foreign} {
		if bytes.Contains(audit, []byte(s)) {
			t.Error("a session token is in the audit log")
		}
	}
	env := string(leaks[filepath.Join(home, "env.txt")])
	if !strings.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN="+own) || !strings.Contains(env, "ANTHROPIC_BASE_URL="+ep.URL("brig-routed", "anthropic")) {
		t.Errorf("the sandbox was not handed its route:\n%s", env)
	}
	// The process table, read on the host while a sandboxed process with
	// both tokens in its environment runs: no command line holds a token or
	// the credential. Seatbelt does not let ps run inside the sandbox.
	done := make(chan error, 1)
	go func() {
		_, err := rt.Output(runtime.ExecSpec{Name: "brig-routed", Cwd: "/root", Env: vars,
			Cmd: []string{"/bin/sh", "-c", "sleep 3.217"}})
		done <- err
	}()
	// sandbox-exec applies the profile and execs the command, so the
	// sandboxed process shows as the command itself.
	var table, line []byte
	for i := 0; i < 100 && line == nil; i++ {
		table, err = exec.Command("ps", "-axww", "-o", "args=").Output()
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range bytes.Split(table, []byte("\n")) {
			if bytes.HasSuffix(bytes.TrimSpace(l), []byte("sleep 3.217")) {
				line = l
			}
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if line == nil {
		t.Fatal("the sandboxed process never showed in the process table, so the argv check proved nothing")
	}
	t.Logf("the sandboxed command line: %s", line)
	for _, s := range []string{own, foreign, fakeAccess} {
		if bytes.Contains(table, []byte(s)) {
			t.Errorf("a token or the credential is on a command line")
		}
	}
	t.Logf("audit log:\n%s", audit)

	// brig stop and brig rm forget the sandbox. Its token is refused from
	// the next request on, and the upstream is not dialled for it.
	if err := injector.Forget(dir, "brig-routed"); err != nil {
		t.Fatal(err)
	}
	after, err := rt.Output(runtime.ExecSpec{Name: "brig-routed", Cwd: "/root", Env: vars,
		Cmd: []string{"/bin/sh", "-c", `printf 'header = "Authorization: Bearer %s"\n' "$CLAUDE_CODE_OAUTH_TOKEN" | ` +
			`curl -s -o /dev/null -w 'route:%{http_code}\n' -K - -X POST -d '{}' "$ANTHROPIC_BASE_URL/v1/messages"`}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after the sandbox is forgotten: %s", strings.TrimSpace(after))
	if strings.TrimSpace(after) != "route:404" {
		t.Errorf("the token of a forgotten sandbox got %q, want route:404", after)
	}
	if n := len(u.requests()); n != 1 {
		t.Errorf("upstream saw %d requests, want none after the sandbox was forgotten", n-1)
	}
}

// redactTail keeps the scheme and the first characters of a header value, so
// the log shows which token went upstream without carrying one.
func redactTail(v string) string {
	if len(v) > 20 {
		return v[:20] + "..."
	}
	return v
}
