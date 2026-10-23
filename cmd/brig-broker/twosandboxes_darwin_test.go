//go:build darwin

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/runtime"
)

// twoSandboxScript runs in sandbox A. A holds B's session token in B_TOKEN,
// as a process sandbox can read it from B's environment through sysctl. It
// sends its own token to B's route, B's token in a body to its own route,
// and its own token in a body. curl reads every token from stdin, never
// from its command line.
const twoSandboxScript = `
req() { printf 'header = "Authorization: Bearer %s"\ndata = "note=%s"\n' "$1" "$3" | curl -s -o /dev/null -w '%{http_code}' --max-time 10 -K - "$2"; }
echo "a-token-on-b-route:$(req "$CLAUDE_CODE_OAUTH_TOKEN" "$B_URL/v1/messages" none)"
echo "b-token-in-a-body:$(req "$CLAUDE_CODE_OAUTH_TOKEN" "$ANTHROPIC_BASE_URL/v1/messages" "$B_TOKEN")"
echo "a-token-in-a-body:$(req "$CLAUDE_CODE_OAUTH_TOKEN" "$ANTHROPIC_BASE_URL/v1/messages" "$CLAUDE_CODE_OAUTH_TOKEN")"
`

// filesScript tries B's guest home, the directory of the homes and B's
// sandbox record, and prints each exit status with the error. It runs in A,
// and in B as the control: the same commands on B's home succeed there, so
// a refusal in A is the profile and not a wrong path. Each sandbox also
// enters its own home by its real path, and writes and reads a file there.
const filesScript = `
try() { name=$1; shift; err=$("$@" 2>&1 > /dev/null); echo "$name:$?:$(echo "$err" | tr '\n' ' ')"; }
try write-b-home sh -c 'echo planted > "$1"' sh "$B_HOME/planted-by-$WHO"
try read-b-home cat "$B_HOME/notes.txt"
try list-homes ls "$(dirname "$B_HOME")"
try read-b-record cat "$B_RECORD/profile.sb"
try pwd-own sh -c 'cd "$1" && test "$(pwd -P)" = "$1"' sh "$OWN_HOME"
try rw-own sh -c 'echo mine > "$1/own-$2" && cat "$1/own-$2"' sh "$OWN_HOME" "$WHO"
`

// fileCheck returns the exit status and the error that filesScript printed
// for name.
func fileCheck(out, name string) (status, msg string) {
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, name+":"); ok {
			status, msg, _ = strings.Cut(rest, ":")
			return status, msg
		}
	}
	return "missing", ""
}

// ownRouteScript sends the sandbox's own token to its own route.
const ownRouteScript = `printf 'header = "Authorization: Bearer %s"\n' "$CLAUDE_CODE_OAUTH_TOKEN" | ` +
	`curl -s -o /dev/null -w 'own:%{http_code}\n' --max-time 10 -K - -d note=none "$ANTHROPIC_BASE_URL/v1/messages"`

// Two host sandboxes registered at one broker at once, under the real
// sandbox-exec. The broker is a process of its own, started the way brig
// starts it (injector.Register through BRIG_BROKER_BIN), and serves through
// the code of brig-broker with a memory store and a logging upstream; see
// TestBrokerHelperProcess.
//
// From inside A: A's token on B's route is a 401, and the upstream is not
// dialled. B's token in A's body passes, because the scan looks for A's own
// tokens only (docs/broker.md, Placeholder detection). A's own token in A's
// body is a 403, and the upstream is not dialled. A can neither read nor
// write B's guest home, cannot list the homes directory, and cannot read B's
// sandbox record, while its own home resolves and takes its files. B's token
// is in no file under the state directory, so no file read yields it. The
// documented way A gets it is B's environment, through sysctl, which this
// test stands in for by handing it to A.
//
// /status counts the refusal against A alone. The upstream sees the stored
// credential and no session token in a header. Neither token, the
// credential nor the host key is in the broker's environment or command
// line. Removing A, as brig stop does, refuses A's token with 404 and leaves
// B's working, and removing B stops the broker.
func TestTwoHostSandboxesAtOneBrokerKeepToTheirOwn(t *testing.T) {
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
	scratch := t.TempDir()
	upLog := filepath.Join(scratch, "upstream.log")
	t.Setenv(injector.BinEnv, brokerHelperBin(t, scratch, upLog))
	dir, err := injector.Dir()
	if err != nil {
		t.Fatal(err)
	}

	const a, b = "brig-a", "brig-b"
	t.Cleanup(func() {
		_ = injector.Forget(dir, a)
		_ = injector.Forget(dir, b)
		// A broker that did not stop when asked is killed by its pid file.
		if pid, ok := injector.LivePID(dir); ok {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	var port int
	var client injector.Client
	for _, id := range []string{a, b} {
		running, err := injector.Register(dir, injector.Sandbox{ID: id, Profile: "claude-code",
			Routes: []injector.Route{claudeRoute()}})
		if err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
		port, client = running.Status.Port, running.Client
	}
	mint := func(id string) string {
		t.Helper()
		tok, err := client.Mint(injector.MintRequest{Sandbox: id, Route: "anthropic"})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	tokA, tokB := mint(a), mint(b)

	expanded, err := policy.ExpandRoute(claudeRoute().Route)
	if err != nil {
		t.Fatal(err)
	}
	ep := injector.EndpointFor(runtime.HostKind, port)
	rt, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	if rt.Kind() != runtime.HostKind {
		t.Fatalf("detected %s, want the host kind", rt.Kind())
	}
	homes := map[string]string{}
	for _, id := range []string{a, b} {
		homes[id] = filepath.Join(state, "homes", id)
		if err := os.MkdirAll(homes[id], 0o700); err != nil {
			t.Fatal(err)
		}
		if err := rt.Run(runtime.RunSpec{Name: id, Net: "isolated",
			Shares:        []runtime.Share{{Host: homes[id], Guest: "/root"}},
			LoopbackPorts: ep.LoopbackPorts()}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = rt.Remove(id) })
	}
	// What an agent in B leaves in its home.
	if err := os.WriteFile(filepath.Join(homes[b], "notes.txt"), []byte("b's notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envOf := func(id, tok string, extra ...runtime.Var) []runtime.Var {
		t.Helper()
		vars, err := ep.GuestEnv(id, []v1alpha1.Route{expanded}, map[string]string{"anthropic": tok}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return append(append(vars, runtime.Var{Name: "HOME", Value: "/root"}), extra...)
	}
	inside := func(id string, vars []runtime.Var, script string) string {
		t.Helper()
		out, err := rt.Output(runtime.ExecSpec{Name: id, Cwd: "/root", Env: vars, Cmd: []string{"/bin/sh", "-c", script}})
		if err != nil {
			t.Fatalf("the script in %s failed: %v\n%s", id, err, out)
		}
		return out
	}
	varsA := envOf(a, tokA,
		runtime.Var{Name: "B_TOKEN", Value: tokB, Secret: true},
		runtime.Var{Name: "B_URL", Value: ep.URL(b, "anthropic")},
		runtime.Var{Name: "B_HOME", Value: homes[b]},
		runtime.Var{Name: "B_RECORD", Value: filepath.Join(state, "host", b)},
		runtime.Var{Name: "OWN_HOME", Value: homes[a]},
		runtime.Var{Name: "WHO", Value: "a"})
	varsB := envOf(b, tokB,
		runtime.Var{Name: "B_HOME", Value: homes[b]},
		runtime.Var{Name: "B_RECORD", Value: filepath.Join(state, "host", b)},
		runtime.Var{Name: "OWN_HOME", Value: homes[b]},
		runtime.Var{Name: "WHO", Value: "b"})

	out := inside(a, varsA, twoSandboxScript)
	t.Logf("inside sandbox A:\n%s", out)
	for _, want := range []string{"a-token-on-b-route:401", "b-token-in-a-body:200", "a-token-in-a-body:403"} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("sandbox A did not report %q:\n%s", want, out)
		}
	}

	// The record is there on the host, and B's home holds its notes, so a
	// refusal below is not a missing file.
	for _, p := range []string{filepath.Join(state, "host", b, "profile.sb"), filepath.Join(homes[b], "notes.txt")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("the target of the file checks is not there: %v", err)
		}
	}
	filesB := inside(b, varsB, filesScript)
	t.Logf("inside sandbox B, the control:\n%s", filesB)
	for _, name := range []string{"write-b-home", "read-b-home", "pwd-own", "rw-own"} {
		if status, msg := fileCheck(filesB, name); status != "0" {
			t.Errorf("the control in B: %s exits %s (%s), want 0", name, status, msg)
		}
	}
	filesA := inside(a, varsA, filesScript)
	t.Logf("inside sandbox A:\n%s", filesA)
	for _, name := range []string{"write-b-home", "read-b-home", "list-homes", "read-b-record"} {
		if status, msg := fileCheck(filesA, name); status == "0" || !strings.Contains(msg, "Operation not permitted") {
			t.Errorf("sandbox A: %s exits %s (%s), want a refusal by the profile", name, status, msg)
		}
	}
	for _, name := range []string{"pwd-own", "rw-own"} {
		if status, msg := fileCheck(filesA, name); status != "0" {
			t.Errorf("sandbox A: %s exits %s (%s), want 0 in its own home", name, status, msg)
		}
	}
	if _, err := os.Lstat(filepath.Join(homes[b], "planted-by-a")); !os.IsNotExist(err) {
		t.Errorf("B's guest home holds a file A planted: %v", err)
	}
	out = inside(b, varsB, ownRouteScript)
	t.Logf("inside sandbox B: %s", strings.TrimSpace(out))
	if strings.TrimSpace(out) != "own:200" {
		t.Errorf("B's own token on its own route got %q, want own:200", out)
	}

	st, err := client.Status()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("/status placeholder: %v", st.Placeholder)
	if st.Placeholder[a] != 1 || st.Placeholder[b] != 0 {
		t.Errorf("/status placeholder = %v, want one refusal for %s and none for %s", st.Placeholder, a, b)
	}

	// The upstream saw the two requests that passed: B's token in A's body,
	// and B's own. Each carried the stored credential and no session token
	// in a header. The body of the first went as A sent it.
	seen := readUpstreamLog(t, upLog)
	if len(seen) != 2 {
		t.Fatalf("upstream saw %d requests, want 2: the 401 and the 403 must not dial it", len(seen))
	}
	for i, r := range seen {
		t.Logf("upstream request %d: Authorization %s, body %d bytes", i, redactTail(r.Header.Get("Authorization")), len(r.Body))
		if got := r.Header.Values("Authorization"); len(got) != 1 || got[0] != "Bearer "+fakeAccess {
			t.Errorf("upstream request %d carried Authorization %q, want the stored credential alone", i, got)
		}
		for name, values := range r.Header {
			for _, v := range values {
				if strings.Contains(v, tokA) || strings.Contains(v, tokB) {
					t.Errorf("upstream request %d carried a session token in %s", i, name)
				}
			}
		}
	}
	if !strings.Contains(seen[0].Body, tokB) {
		t.Error("B's token in A's body did not reach the upstream as A sent it")
	}

	// No file under the state directory holds a token, the credential or
	// the refresh token: the homes, the sandbox records, the broker's logs.
	// A file read is not a way to B's token.
	if err := filepath.WalkDir(state, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		blob, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for what, s := range map[string]string{"A's token": tokA, "B's token": tokB, "the credential": fakeAccess,
			"the refresh token": "fake-refresh"} {
			if bytes.Contains(blob, []byte(s)) {
				t.Errorf("%s is in %s", what, p)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The broker's own command line and environment.
	pidLine, err := os.ReadFile(filepath.Join(dir, injector.PIDName))
	if err != nil {
		t.Fatal(err)
	}
	pid, _, _ := strings.Cut(string(pidLine), " ")
	if _, err := strconv.Atoi(pid); err != nil {
		t.Fatalf("pid file %q", pidLine)
	}
	hostKey, err := os.ReadFile(filepath.Join(dir, injector.HostKeyName))
	if err != nil {
		t.Fatal(err)
	}
	proc, err := exec.Command("ps", "-E", "-ww", "-o", "command=", "-p", pid).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(proc, []byte("--dir")) || !bytes.Contains(proc, []byte("PATH=")) {
		t.Fatalf("ps did not show the broker's command line and environment, so the check proves nothing: %s", proc)
	}
	for what, s := range map[string]string{"A's token": tokA, "B's token": tokB, "the credential": fakeAccess,
		"the refresh token": "fake-refresh", "the host key": string(hostKey),
		"the host key in hex": hex.EncodeToString(hostKey), "the host key in base64": base64.StdEncoding.EncodeToString(hostKey)} {
		if bytes.Contains(proc, []byte(s)) {
			t.Errorf("%s is in the broker's command line or environment", what)
		}
	}

	// brig stop and brig rm remove the sandbox from the broker. A's token is
	// refused from the next request on, and B's works.
	if err := injector.Forget(dir, a); err != nil {
		t.Fatal(err)
	}
	if out := strings.TrimSpace(inside(a, varsA, ownRouteScript)); out != "own:404" {
		t.Errorf("A's token after A was removed got %q, want own:404", out)
	}
	if out := strings.TrimSpace(inside(b, varsB, ownRouteScript)); out != "own:200" {
		t.Errorf("B's token after A was removed got %q, want own:200", out)
	}
	if st, err := client.Status(); err != nil || !slices.Equal(st.Sandboxes, []string{b}) {
		t.Errorf("after A was removed the broker serves %v (%v), want B alone", st.Sandboxes, err)
	}
	if n := len(readUpstreamLog(t, upLog)); n != 3 {
		t.Errorf("upstream saw %d requests, want 3: none for A's revoked token", n)
	}
	t.Logf("after A was removed: A own:404, B own:200")

	// Removing the last sandbox stops the broker.
	if err := injector.Forget(dir, b); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := injector.Running(dir); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the broker still runs after its last sandbox was removed")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
