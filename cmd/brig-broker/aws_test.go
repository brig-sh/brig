package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/secret"
)

// Fake values only. TestNothingVendedOnDisk looks for these.
const (
	fakeAWSAccess  = "ASIAFAKEBROKERTESTKEY"
	fakeAWSSecret  = "fake-aws-secret-for-the-broker-tests"
	fakeAWSSession = "fake-aws-session-token-for-the-broker-tests"
	awsPointer     = `{"kind":"aws","awsProfile":"dev","mode":"role",` +
		`"roleArn":"arn:aws:iam::111122223333:role/brig-sandbox"}`
	// ssoExpiredStderr is what the aws CLI prints when the SSO login lapsed.
	ssoExpiredStderr = "Error when retrieving token from sso: Token has expired and refresh failed"
)

// awsBase is the fake clock's start. The fake aws hands out keys that expire
// an hour after it.
var awsBase = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

// fakeAWS is an aws binary that records each call and its environment in its
// directory, and fails with the SSO-expired stderr while the file
// sso-expired is there.
type fakeAWS struct {
	dir string
	bin string

	mu  sync.Mutex
	now time.Time
}

func newFakeAWS(t *testing.T) *fakeAWS {
	t.Helper()
	f := &fakeAWS{dir: t.TempDir(), now: awsBase}
	f.bin = filepath.Join(f.dir, "aws")
	creds := fmt.Sprintf(`{"Credentials":{"AccessKeyId":%q,"SecretAccessKey":%q,"SessionToken":%q,"Expiration":%q}}`,
		fakeAWSAccess, fakeAWSSecret, fakeAWSSession, awsBase.Add(time.Hour).Format(time.RFC3339))
	script := fmt.Sprintf(`#!/bin/sh
d=%q
echo "$*" >> "$d/calls"
env >> "$d/env"
if [ -e "$d/sso-expired" ]; then
	echo %q >&2
	exit 255
fi
case "$1 $2" in
"sts assume-role") printf '%%s' %q ;;
"configure get") echo us-east-1 ;;
*) echo "unexpected: $*" >&2; exit 2 ;;
esac
`, f.dir, ssoExpiredStderr, creds)
	if err := os.WriteFile(f.bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fakeAWS) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeAWS) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func (f *fakeAWS) expireSSO(t *testing.T, expired bool) {
	t.Helper()
	path := filepath.Join(f.dir, "sso-expired")
	if expired {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

// fetches counts the assume-role calls the fake answered or refused.
func (f *fakeAWS) fetches(t *testing.T) int {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join(f.dir, "calls"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return strings.Count(string(blob), "sts assume-role")
}

// awsRoute is the aws route as policy.Routes hands it to brig.
func awsRoute() injector.Route {
	r := v1alpha1.Route{Name: "aws", Preset: v1alpha1.PresetAWS,
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: "aws"}}}
	return injector.Route{Policy: "aws", Route: r}
}

func awsSandbox(id string) injector.Sandbox {
	return injector.Sandbox{ID: id, Profile: "claude-code", Routes: []injector.Route{awsRoute()}}
}

// startAWSBroker serves cfg with the fake aws and its clock, and a store
// that holds a role pointer under the name "aws".
func startAWSBroker(t *testing.T, cfg serverConfig) (*running, *fakeAWS) {
	t.Helper()
	f := newFakeAWS(t)
	if cfg.dir == "" {
		cfg.dir = shortDir(t)
	}
	if cfg.awsBin == "" {
		cfg.awsBin = f.bin
	}
	cfg.now = f.clock
	r := startBrokerWith(t, cfg, nil)
	if err := r.store.Create("aws", []byte(awsPointer)); err != nil {
		t.Fatal(err)
	}
	return r, f
}

// refusal is the RefusedError of err, or fails the test.
func refusal(t *testing.T, err error) *injector.RefusedError {
	t.Helper()
	var ref *injector.RefusedError
	if !errors.As(err, &ref) {
		t.Fatalf("registration = %v, want a refusal", err)
	}
	return ref
}

func TestRegisterReturnsRegion(t *testing.T) {
	r, _ := startAWSBroker(t, serverConfig{logTo: io.Discard})
	added, err := r.client.Add(awsSandbox("brig-a"))
	if err != nil {
		t.Fatal(err)
	}
	if got := added.Regions["aws"]; got != "us-east-1" {
		t.Errorf("Regions[aws] = %q, want us-east-1", got)
	}
	want := injector.VendInfo{Mode: "role", AWSProfile: "dev", RoleARN: "arn:aws:iam::111122223333:role/brig-sandbox"}
	if got := added.Vends["aws"]; got != want {
		t.Errorf("Vends[aws] = %+v, want %+v", got, want)
	}
	// A sandbox with no route that vends is answered with neither.
	plain, err := r.client.Add(injector.Sandbox{ID: "brig-b", Profile: "claude-code",
		Routes: []injector.Route{githubRoute()}})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Regions != nil || plain.Vends != nil {
		t.Errorf("a sandbox with no aws route got %+v", plain)
	}
}

// The same registration again is the shortcut that keeps the sandbox's
// tokens, and still asks AWS: a run started on a lapsed login fails at start,
// with the fix, and not later inside the guest.
func TestIdenticalRegistrationStillChecks(t *testing.T) {
	r, f := startAWSBroker(t, serverConfig{logTo: io.Discard})
	if _, err := r.client.Add(awsSandbox("brig-a")); err != nil {
		t.Fatal(err)
	}
	f.expireSSO(t, true)
	f.advance(50 * time.Minute)
	_, err := r.client.Add(awsSandbox("brig-a"))
	ref := refusal(t, err)
	if ref.Status != http.StatusFailedDependency || ref.Kind != "credential" {
		t.Errorf("refusal = %d kind %q, want 424 kind credential", ref.Status, ref.Kind)
	}
	if !strings.Contains(ref.Message, "aws sso login --profile dev") || !strings.HasPrefix(ref.Message, "route aws: ") {
		t.Errorf("message = %q, want the route and the sso login hint", ref.Message)
	}
	st, err := r.client.Status()
	if err != nil {
		t.Fatal(err)
	}
	// The sandbox registered before keeps its registration, and status names
	// the route whose fetch failed.
	if !slices.Equal(st.Sandboxes, []string{"brig-a"}) || !slices.Equal(st.Vending, []string{"brig-a/aws"}) {
		t.Errorf("status = %+v, want brig-a registered and vending brig-a/aws", st)
	}
	f.expireSSO(t, false)
	if _, err := r.client.Add(awsSandbox("brig-a")); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.client.Status(); len(st.Vending) != 0 {
		t.Errorf("vending = %v after a fetch that worked", st.Vending)
	}
}

// A first registration that fails registers nothing: no sandbox, no divert
// socket, and nothing in status that a later run would read as its own.
func TestRefusedRegistrationLeavesNoState(t *testing.T) {
	r, f := startAWSBroker(t, serverConfig{logTo: io.Discard})
	f.expireSSO(t, true)
	sb := awsSandbox("brig-a")
	sb.BrigInternal = internalA
	_, err := r.client.Add(sb)
	if ref := refusal(t, err); ref.Kind != "credential" {
		t.Errorf("kind = %q, want credential", ref.Kind)
	}
	st, err := r.client.Status()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Sandboxes) != 0 || len(st.Vending) != 0 {
		t.Errorf("status = %+v after a refused registration", st)
	}
	sock, err := injector.DivertSocket(r.dir, "brig-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the divert socket of a refused registration exists: %v", err)
	}
	// A route of the refused sandbox is not served.
	if _, err := r.client.Mint(injector.MintRequest{Sandbox: "brig-a", Route: "aws"}); err == nil {
		t.Error("a token was minted for a sandbox whose registration was refused")
	}
}

// A pointer the store does not hold is a refusal of the same kind: the fix
// is on the host, in brig secret import.
func TestRegisterRefusesAMissingPointer(t *testing.T) {
	r, f := startAWSBroker(t, serverConfig{logTo: io.Discard})
	if err := r.store.Delete("aws"); err != nil {
		t.Fatal(err)
	}
	_, err := r.client.Add(awsSandbox("brig-a"))
	if ref := refusal(t, err); ref.Status != http.StatusFailedDependency || ref.Kind != "credential" {
		t.Errorf("refusal = %d kind %q, want 424 kind credential", ref.Status, ref.Kind)
	}
	if n := f.fetches(t); n != 0 {
		t.Errorf("aws ran %d times with no pointer to run it for", n)
	}
}

// With no aws binary the refusal says where to get one.
func TestRegisterNamesTheAWSInstall(t *testing.T) {
	r, _ := startAWSBroker(t, serverConfig{logTo: io.Discard, awsBin: filepath.Join(t.TempDir(), "aws")})
	_, err := r.client.Add(awsSandbox("brig-a"))
	ref := refusal(t, err)
	if ref.Kind != "credential" || !strings.Contains(ref.Message, "install the AWS CLI v2") {
		t.Errorf("refusal = kind %q %q, want the install hint", ref.Kind, ref.Message)
	}
}

func TestRemoveForgetsVendCache(t *testing.T) {
	r, f := startAWSBroker(t, serverConfig{logTo: io.Discard})
	for range 2 {
		if _, err := r.client.Add(awsSandbox("brig-a")); err != nil {
			t.Fatal(err)
		}
	}
	// Keys with an hour left are fresh enough for a second registration.
	if n := f.fetches(t); n != 1 {
		t.Fatalf("two registrations fetched %d times, want 1", n)
	}
	if _, err := r.client.RemoveSandbox("brig-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.client.Add(awsSandbox("brig-a")); err != nil {
		t.Fatal(err)
	}
	if n := f.fetches(t); n != 2 {
		t.Errorf("a registration after remove fetched %d times in all, want 2", n)
	}
}

// brig registers the routes that policy.Routes expanded, with the preset's
// inject filled in, and the broker expands them again. The broker tests that
// register awsRoute() send the route as written, so this one takes the path
// a real run takes: a policy document, parsed and expanded by brig.
func TestRegisterAcceptsARouteBrigExpanded(t *testing.T) {
	r, _ := startAWSBroker(t, serverConfig{logTo: io.Discard})
	p, err := policy.Parse([]byte("apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: aws}\n" +
		"spec:\n  routes:\n    - name: aws\n      preset: aws\n      credential: {secretRef: {name: aws}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	routes, err := policy.Routes([]policy.Policy{p})
	if err != nil {
		t.Fatal(err)
	}
	sb := injector.Sandbox{ID: "brig-a", Profile: "claude-code"}
	for _, rt := range routes {
		sb.Routes = append(sb.Routes, injector.Route{Policy: "aws", Route: rt})
	}
	added, err := r.client.Add(sb)
	if err != nil {
		t.Fatalf("registering the route brig expanded: %v", err)
	}
	if got := added.Regions["aws"]; got != "us-east-1" {
		t.Errorf("Regions[aws] = %q, want us-east-1", got)
	}
}

// A registration can pass the AWS check and still be refused after it: a
// route's lifecycle with no renewer, or a sandbox the handler will not take.
// A sandbox that was not registered before leaves no keys behind then
// either, or a later sandbox of the same id would be served keys fetched for
// a registration that never happened.
func TestRegistrationRefusedAfterTheCheckForgetsTheKeys(t *testing.T) {
	cases := map[string]struct {
		cfg serverConfig
		sb  injector.Sandbox
	}{
		"lifecycle with no renewer": {
			cfg: serverConfig{logTo: io.Discard, renewerFor: func(v1alpha1.Lifecycle) (secret.Renewer, error) {
				return nil, errors.New("no renewer in this test")
			}},
			sb: injector.Sandbox{ID: "brig-a", Profile: "claude-code",
				Routes: []injector.Route{awsRoute(), claudeRoute()}},
		},
		"handler refuses the sandbox": {
			cfg: serverConfig{logTo: io.Discard},
			sb: injector.Sandbox{ID: "brig-a", Profile: "claude-code",
				Routes: []injector.Route{awsRoute(), awsRoute()}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r, f := startAWSBroker(t, tc.cfg)
			_, err := r.client.Add(tc.sb)
			if ref := refusal(t, err); ref.Status != http.StatusBadRequest {
				t.Fatalf("refusal = %d %q, want 400", ref.Status, ref.Message)
			}
			if n := f.fetches(t); n != 1 {
				t.Fatalf("the refused registration fetched %d times, want 1", n)
			}
			if _, err := r.client.Add(awsSandbox("brig-a")); err != nil {
				t.Fatal(err)
			}
			if n := f.fetches(t); n != 2 {
				t.Errorf("fetches = %d, want 2: keys of the refused registration were kept", n)
			}
		})
	}
}

// The broker's own environment can hold AWS_ variables, from the shell that
// started the first brig. They would decide which identity a sandbox gets,
// or hand aws keys the user never named, so the aws subprocess gets none.
func TestBrokerAWSEnvHasNoAWSVars(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "x")
	t.Setenv("AWS_PROFILE", "someone-else")
	r, f := startAWSBroker(t, serverConfig{logTo: io.Discard})
	if _, err := r.client.Add(awsSandbox("brig-a")); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(f.dir, "env"))
	if err != nil {
		t.Fatal(err)
	}
	var sawPath bool
	for _, line := range strings.Split(string(blob), "\n") {
		if strings.HasPrefix(line, "AWS_") {
			t.Errorf("the aws subprocess got %s", strings.SplitN(line, "=", 2)[0])
		}
		sawPath = sawPath || strings.HasPrefix(line, "PATH=")
	}
	// The env file is the fake's view; one with no PATH recorded nothing.
	if !sawPath {
		t.Errorf("the fake recorded no PATH, so this test checks nothing: %q", blob)
	}
}

// vendGet asks the aws route of sandbox for credentials, as the guest's SDK
// does.
func (r *running) vendGet(t *testing.T, sandbox string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/%s/r/aws", r.srv.port, sandbox), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", r.mint(t, sandbox, "aws"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// Vended keys live in the broker's memory: not in the store, the audit log,
// the broker log or any other file of its directory.
func TestNothingVendedOnDisk(t *testing.T) {
	r, _ := startAWSBroker(t, serverConfig{})
	if _, err := r.client.Add(awsSandbox("brig-a")); err != nil {
		t.Fatal(err)
	}
	code, body := r.vendGet(t, "brig-a")
	if code != http.StatusOK || !strings.Contains(body, fakeAWSSecret) {
		t.Fatalf("vend = %d %q, want the fake's keys", code, body)
	}
	var files int
	err := filepath.WalkDir(r.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		blob, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		for _, v := range []string{fakeAWSSecret, fakeAWSSession} {
			if bytes.Contains(blob, []byte(v)) {
				t.Errorf("%s holds a vended value", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.dir, injector.LogName)); err != nil || files == 0 {
		t.Errorf("walked %d files and the broker log is %v; this test checks nothing", files, err)
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	for name, v := range r.store.values {
		if bytes.Contains(v, []byte(fakeAWSSecret)) {
			t.Errorf("the store holds a vended value under %s", name)
		}
	}
}

// A vend on a lapsed login tells the guest the command that fixes it, and
// status names the route until a vend works again.
func TestVendNamesTheSSOLogin(t *testing.T) {
	r, f := startAWSBroker(t, serverConfig{logTo: io.Discard})
	if _, err := r.client.Add(awsSandbox("brig-a")); err != nil {
		t.Fatal(err)
	}
	f.expireSSO(t, true)
	f.advance(45 * time.Minute)
	code, body := r.vendGet(t, "brig-a")
	if code != http.StatusBadGateway || !strings.Contains(body, "aws sso login --profile dev") {
		t.Errorf("vend = %d %q, want 502 with the sso login hint", code, body)
	}
	if st, _ := r.client.Status(); !slices.Equal(st.Vending, []string{"brig-a/aws"}) {
		t.Errorf("vending = %v, want brig-a/aws", st.Vending)
	}
	f.expireSSO(t, false)
	if code, _ := r.vendGet(t, "brig-a"); code != http.StatusOK {
		t.Fatalf("vend after login = %d", code)
	}
	if st, _ := r.client.Status(); len(st.Vending) != 0 {
		t.Errorf("vending = %v after a vend that worked", st.Vending)
	}
}

// lockedBuffer is a broker log a test reads while the broker writes it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A failure the user cannot fix from a hint goes to the broker log, with
// the sandbox and route, and the guest is pointed there. A self pointer
// makes the fake fail: it answers only assume-role.
func TestAWSFailureGoesToTheBrokerLog(t *testing.T) {
	logs := &lockedBuffer{}
	r, _ := startAWSBroker(t, serverConfig{logTo: logs})
	if err := r.store.Update("aws", []byte(`{"kind":"aws","awsProfile":"dev","mode":"self"}`)); err != nil {
		t.Fatal(err)
	}
	_, err := r.client.Add(awsSandbox("brig-a"))
	ref := refusal(t, err)
	if ref.Kind != "credential" || !strings.Contains(ref.Message, "configure export-credentials failed") {
		t.Errorf("refusal = kind %q %q", ref.Kind, ref.Message)
	}
	if got := logs.String(); !strings.Contains(got, "sandbox brig-a: route aws: ") ||
		!strings.Contains(got, "configure export-credentials failed") {
		t.Errorf("the broker log is %q, want the failure with its sandbox and route", got)
	}
	audit, err := os.ReadFile(filepath.Join(r.dir, injector.AuditName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(audit, []byte("export-credentials")) {
		t.Errorf("the audit log holds the aws failure: %q", audit)
	}
}
