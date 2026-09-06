package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/secret"
)

// doctorRuntime is the runtime doctor detects when a test forces a working host.
// Its own name, as is doctorStore's: the package's fakeRuntime and fakeStore
// belong to the telemetry and secret tests and carry state these do not need.
// The interface is embedded rather than implemented, the repo's pattern: a
// check that reached any method other than the two below is doing something a
// check has no business doing, and the nil panic says that louder than a stub.
type doctorRuntime struct {
	runtime.Runtime
	bin string
}

// Bin is whatever the test put there: healthyHost writes a script that answers
// --version, because the runtime check now looks the binary up before it
// vouches for it, and a name that is not on disk is the failure a separate
// test asks for rather than the fixture every test starts from.
func (doctorRuntime) Kind() string  { return "hull" }
func (r doctorRuntime) Bin() string { return r.bin }

// doctorStore is a secret store that opens. Kind is all the secrets check reads;
// it never reaches for a value, and a store that answered one would be a bug.
type doctorStore struct{ secret.Store }

func (doctorStore) Kind() string { return "keychain" }

// healthyHost forces every seam and environment variable doctor reads to the
// answer a working macOS-or-Linux host would give, so a test can then break one
// of them in isolation and watch that one line change. It returns the boot
// asset directory so a case can empty it.
func healthyHost(t *testing.T) string {
	t.Helper()
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	t.Setenv("BRIG_STATE_DIR", t.TempDir())
	t.Setenv("BRIG_VERIFY", "off")
	// A brigd socket of no one's, so the daemon check is informational rather
	// than reaching whatever socket the host running the test happens to have.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	assets := t.TempDir()
	// Both kernel names, so the check finds the one this architecture wants.
	for _, name := range []string{"Image", "bzImage", "container-initrd"} {
		if err := os.WriteFile(filepath.Join(assets, name), []byte("stand-in\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("BRIG_BOOT_ASSETS", assets)

	swap(t, &virtualization, func() (bool, string) { return true, "virtualization available" })
	bin := filepath.Join(t.TempDir(), "hull")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho hull version 0.0.0-test\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	swap(t, &detectRuntime, func() (runtime.Runtime, error) { return doctorRuntime{bin: bin}, nil })
	swap(t, &openStore, func() (secret.Store, error) { return doctorStore{}, nil })
	// The gateway takes egress rules by default, so a case can break just this
	// one and watch the gateway line change without a real network-gateway to
	// ask. A test that wants no attachments read points BRIG_POLICY_DIR at an
	// empty directory of its own.
	swap(t, &gatewayEnforces, func(string) error { return nil })
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	return assets
}

// swap sets a package-level seam for the duration of a test and restores it.
func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	orig := *p
	*p = v
	t.Cleanup(func() { *p = orig })
}

// findCheck returns the one line of a report with the given name.
func findCheck(t *testing.T, checks []check, name string) check {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("the report has no %q check: %+v", name, checks)
	return check{}
}

// Every prerequisite in place: the report is all ok (or informational for the
// parts a healthy host still has nothing to say about), nothing is flagged, and
// the command exits 0.
func TestDoctorAllChecksPass(t *testing.T) {
	healthyHost(t)
	checks := runDoctor(nil, nil)

	for _, c := range checks {
		if c.State == stateFail {
			t.Errorf("check %q failed on a healthy host: %s", c.Name, c.Finding)
		}
	}
	for _, name := range []string{"host", "virtual", "runtime", "gateway", "boot", "verify", "profiles", "secrets"} {
		if got := findCheck(t, checks, name).State; got != statePass {
			t.Errorf("check %q is %q on a healthy host, want ok", name, got)
		}
	}
	if err := doctorExit(checks); err != nil {
		t.Errorf("a healthy host exits non-zero: %v (code %d)", err, exitCode(err))
	}
}

// A missing runtime is exit 4, the runtime line carries its fix, and the two
// checks that need a runtime say they were not reached rather than failing in
// the words of the layer they never reached.
func TestDoctorMissingRuntimeExits4(t *testing.T) {
	healthyHost(t)
	swap(t, &detectRuntime, func() (runtime.Runtime, error) { return nil, runtime.ErrNoRuntime })

	checks := runDoctor(&fakeProfile, nil)

	rt := findCheck(t, checks, "runtime")
	if rt.State != stateFail || rt.Fix == "" {
		t.Errorf("a missing runtime is %q with fix %q, want !! with a fix", rt.State, rt.Fix)
	}
	for _, name := range []string{"gateway", "boot", "image"} {
		c := findCheck(t, checks, name)
		if c.State != stateInfo || c.Finding != "not reached" {
			t.Errorf("%q after a missing runtime is %q/%q, want -- / not reached", name, c.State, c.Finding)
		}
	}
	if code := exitCode(doctorExit(checks)); code != exitRuntime {
		t.Errorf("a missing runtime exits %d, want %d", code, exitRuntime)
	}
}

// A runtime whose binary is not on disk is exit 4 as well. Detect takes
// BRIG_RUNTIME_BIN on trust, so this is the case doctor has to catch itself
// rather than inherit from the seam.
func TestDoctorRuntimeBinaryMissingExits4(t *testing.T) {
	healthyHost(t)
	missing := filepath.Join(t.TempDir(), "hull-not-here")
	swap(t, &detectRuntime, func() (runtime.Runtime, error) { return doctorRuntime{bin: missing}, nil })

	checks := runDoctor(&fakeProfile, nil)

	rt := findCheck(t, checks, "runtime")
	if rt.State != stateFail || rt.Fix == "" || !strings.Contains(rt.Finding, missing) {
		t.Errorf("a missing binary is %q/%q with fix %q, want !! naming %s with a fix", rt.State, rt.Finding, rt.Fix, missing)
	}
	for _, name := range []string{"gateway", "boot", "image"} {
		c := findCheck(t, checks, name)
		if c.State != stateInfo || c.Finding != "not reached" {
			t.Errorf("%q after a missing binary is %q/%q, want -- / not reached", name, c.State, c.Finding)
		}
	}
	if code := exitCode(doctorExit(checks)); code != exitRuntime {
		t.Errorf("a missing binary exits %d, want %d", code, exitRuntime)
	}
}

// A secret store that is present but will not open is exit 6, with its fix on
// the line. A store that is simply absent (ErrUnsupported) is not a failure --
// brig runs without one -- so that case stays informational and exits 0.
func TestDoctorClosedKeychainExits6(t *testing.T) {
	healthyHost(t)
	swap(t, &openStore, func() (secret.Store, error) { return nil, errors.New("keychain is locked") })

	checks := runDoctor(nil, nil)
	sec := findCheck(t, checks, "secrets")
	if sec.State != stateFail || sec.Fix == "" {
		t.Errorf("a locked keychain is %q with fix %q, want !! with a fix", sec.State, sec.Fix)
	}
	if code := exitCode(doctorExit(checks)); code != exitCredentials {
		t.Errorf("a locked keychain exits %d, want %d", code, exitCredentials)
	}

	swap(t, &openStore, func() (secret.Store, error) { return nil, secret.ErrUnsupported })
	checks = runDoctor(nil, nil)
	if got := findCheck(t, checks, "secrets").State; got != stateInfo {
		t.Errorf("no store on this platform is %q, want -- (informational)", got)
	}
	if err := doctorExit(checks); err != nil {
		t.Errorf("a platform with no store exits non-zero: %v", err)
	}
}

// The unknown-agent operand is the same not-found error every other verb
// returns -- exit 3 -- rather than a check that fails partway down the report.
func TestDoctorUnknownAgentExits3(t *testing.T) {
	healthyHost(t)
	_, err := captureStdout(t, func() error { return doctorCmd(os.Stdout, []string{"nosuchagent"}) })
	if err == nil {
		t.Fatal("an unknown agent was accepted")
	}
	if code := exitCode(err); code != exitNotFound {
		t.Errorf("an unknown agent exits %d, want %d", code, exitNotFound)
	}
}

// Each check prints its exact fix on the next line when it fails, indented
// under the finding. Driven through the renderer so what is asserted is the
// text a reader sees.
func TestDoctorFailedCheckPrintsItsFix(t *testing.T) {
	healthyHost(t)
	// Break several at once: no virtualization, no runtime (so boot and image
	// are not reached but the runtime line has a fix), and a require mode with
	// no cosign so verify fails too.
	swap(t, &virtualization, func() (bool, string) { return false, "no virtualization here" })
	swap(t, &detectRuntime, func() (runtime.Runtime, error) { return nil, runtime.ErrBadRuntime })
	t.Setenv("BRIG_VERIFY", "require")
	t.Setenv("BRIG_COSIGN_BIN", filepath.Join(t.TempDir(), "no-cosign"))

	var buf bytes.Buffer
	checks := runDoctor(nil, nil)
	renderDoctor(&buf, checks)
	out := buf.String()

	for _, name := range []string{"virtual", "runtime", "verify"} {
		c := findCheck(t, checks, name)
		if c.State != stateFail {
			t.Errorf("check %q did not fail as arranged: %q", name, c.State)
			continue
		}
		if c.Fix == "" || !strings.Contains(out, c.Fix) {
			t.Errorf("the rendered report does not print %q's fix %q:\n%s", name, c.Fix, out)
		}
	}
}

// --json carries the same facts as the text: it parses, it has the apiVersion
// and kind of the shared envelope, one entry per check, and no credential value
// anywhere in it.
func TestDoctorJSONCarriesSameFacts(t *testing.T) {
	healthyHost(t)
	// A store whose name looks like a value would be, so the "no secret value"
	// assertion has something real to miss if a check ever leaked one.
	const planted = "super-secret-token-value"
	swap(t, &openStore, func() (secret.Store, error) { return doctorStore{}, nil })

	out, err := captureStdout(t, func() error { return doctorCmd(os.Stdout, []string{"--json"}) })
	if err != nil {
		t.Fatalf("brig doctor --json failed: %v", err)
	}
	if strings.Contains(out, planted) {
		t.Fatal("the test planted nothing, so this can only mean the assertion is wrong")
	}

	var doc struct {
		APIVersion string  `json:"apiVersion"`
		Kind       string  `json:"kind"`
		Data       []check `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("--json did not parse: %v\n%s", err, out)
	}
	if doc.APIVersion != jsonAPIVersion {
		t.Errorf("apiVersion is %q, want %q", doc.APIVersion, jsonAPIVersion)
	}
	if doc.Kind != "Doctor" {
		t.Errorf("kind is %q, want %q", doc.Kind, "Doctor")
	}
	textChecks := runDoctor(nil, nil)
	if len(doc.Data) != len(textChecks) {
		t.Errorf("--json has %d checks, the text report has %d", len(doc.Data), len(textChecks))
	}
	for _, c := range doc.Data {
		if c.Name == "" || c.State == "" {
			t.Errorf("a --json check is missing its name or state: %+v", c)
		}
	}
}

// A gateway that takes egress rules is an ok line and nothing to act on.
func TestDoctorGatewayEnforces(t *testing.T) {
	healthyHost(t)
	checks := runDoctor(nil, nil)
	if got := findCheck(t, checks, "gateway").State; got != statePass {
		t.Errorf("gateway is %q on a host whose gateway enforces, want ok", got)
	}
}

// A gateway that cannot enforce is diagnostic: it carries a finding and a fix
// but does not move the exit status, since a host with no policy bound has
// nothing to fail over. Asserting the exit code does not move is the property
// that keeps this check from becoming a gate.
func TestDoctorGatewayCannotEnforceIsDiagnostic(t *testing.T) {
	healthyHost(t)
	swap(t, &gatewayEnforces, func(string) error { return errors.New("no egress flags") })

	checks := runDoctor(nil, nil)
	gw := findCheck(t, checks, "gateway")
	if gw.State != stateFail || gw.Fix == "" {
		t.Errorf("a gateway that cannot enforce is %q with fix %q, want !! with a fix", gw.State, gw.Fix)
	}
	if err := doctorExit(checks); err != nil {
		t.Errorf("a gateway that cannot enforce moved the exit status: %v (code %d)", err, exitCode(err))
	}
}

// A policy bound to the named agent on a gateway that cannot enforce is said
// more sharply -- that boot will be refused -- and still does not gate the exit
// status.
func TestDoctorGatewayRefusesBoundPolicy(t *testing.T) {
	healthyHost(t)
	swap(t, &gatewayEnforces, func(string) error { return errors.New("no egress flags") })
	p, err := profile.Parse([]byte("name: bound\nimage: ghcr.io/brig-sh/claude-code-stock:latest\n" +
		"guestHome: /home/bound\nbinary: x\nmem: 1\ncpus: 1\npolicy:\n  - no-net\n"))
	if err != nil {
		t.Fatal(err)
	}

	checks := runDoctor(&p, nil)
	gw := findCheck(t, checks, "gateway")
	if gw.State != stateFail || !strings.Contains(gw.Finding, p.Name) {
		t.Errorf("a bound policy on a gateway that cannot enforce is %q/%q, want !! naming %s",
			gw.State, gw.Finding, p.Name)
	}
	if err := doctorExit(checks); err != nil {
		t.Errorf("the sharper finding still moved the exit status: %v", err)
	}
}

// The gateway check needs a runtime to own the gateway, so a missing runtime
// leaves it not reached rather than failing it in the words of a gateway it
// never asked.
func TestDoctorGatewayNotReachedWithoutRuntime(t *testing.T) {
	healthyHost(t)
	swap(t, &detectRuntime, func() (runtime.Runtime, error) { return nil, runtime.ErrNoRuntime })

	checks := runDoctor(nil, nil)
	gw := findCheck(t, checks, "gateway")
	if gw.State != stateInfo || gw.Finding != "not reached" {
		t.Errorf("gateway after a missing runtime is %q/%q, want -- / not reached", gw.State, gw.Finding)
	}
}

// fakeProfile is an agent whose image is under brig's own registry, for the
// cases that name an operand. It is never actually verified in a test -- the
// runtime is missing in the one case that uses it, so the image check is not
// reached -- so its image only has to be a plausible reference.
var fakeProfile = func() profile.Profile {
	p, err := profile.Parse([]byte("name: x\nimage: ghcr.io/brig-sh/claude-code-stock:latest\n" +
		"guestHome: /home/x\nbinary: x\nmem: 1\ncpus: 1\n"))
	if err != nil {
		panic(err)
	}
	return p
}()
