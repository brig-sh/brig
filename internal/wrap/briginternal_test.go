package wrap

import (
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
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/verify"
)

// testCA is a CA in the shape the broker makes: constrained to brig.internal
// alone. Made once, since key generation is slow under -race.
var testCA struct {
	once     sync.Once
	cert     string
	key      string
	loose    string
	keyBlock *pem.Block
}

func makeTestCAs() {
	testCA.once.Do(func() {
		mk := func(permitted []string) (string, *pem.Block) {
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				panic(err)
			}
			tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "test brig CA"},
				NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
				KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
				PermittedDNSDomainsCritical: len(permitted) > 0, PermittedDNSDomains: permitted}
			der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
			if err != nil {
				panic(err)
			}
			kder, err := x509.MarshalECPrivateKey(key)
			if err != nil {
				panic(err)
			}
			return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
				&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}
		}
		testCA.cert, testCA.keyBlock = mk([]string{injector.BrigInternal})
		testCA.key = string(pem.EncodeToMemory(testCA.keyBlock))
		testCA.loose, _ = mk(nil)
	})
}

// testBrigCA is the CA certificate the fake broker hands a brig.internal
// sandbox.
func testBrigCA() string {
	makeTestCAs()
	return testCA.cert
}

// hullRecorder is hull on hvi as far as a boot goes: its probe answers
// probe, its link-local probe answers linkLocal and counts linkLocalProbes,
// and it records the spec it was handed and boots nothing.
type hullRecorder struct {
	runtime.Runtime
	probe           error
	linkLocal       error
	linkLocalProbes int
	spec            *runtime.RunSpec
}

func (r *hullRecorder) Kind() string                          { return "hull" }
func (r *hullRecorder) Bin() string                           { return "hull" }
func (r *hullRecorder) Running(string) (bool, error)          { return false, nil }
func (r *hullRecorder) Remove(string) error                   { return nil }
func (r *hullRecorder) Stop(string) error                     { return nil }
func (r *hullRecorder) SandboxNetwork(string) (string, error) { return "", nil }
func (r *hullRecorder) ProbeBrigInternal() error              { return r.probe }
func (r *hullRecorder) ProbeLinkLocal() error {
	r.linkLocalProbes++
	return r.linkLocal
}
func (r *hullRecorder) BrigInternalAddr(string) (netip.Addr, error) {
	return netip.MustParseAddr("198.18.1.1"), nil
}
func (r *hullRecorder) Isolation(runtime.RunSpec) runtime.Isolation {
	return runtime.Isolation{Boundary: runtime.BoundaryVM, Detail: "hull, hvi backend"}
}
func (r *hullRecorder) Run(spec runtime.RunSpec) error {
	r.spec = &spec
	return errors.New("stub runtime: not booting")
}

// nerdctlRecorder is nerdctl on urunc as far as a boot goes, in the way
// hullRecorder is hull on hvi.
type nerdctlRecorder struct {
	hullRecorder
}

func (r *nerdctlRecorder) Kind() string { return "nerdctl" }
func (r *nerdctlRecorder) Bin() string  { return "nerdctl" }
func (r *nerdctlRecorder) Isolation(runtime.RunSpec) runtime.Isolation {
	return runtime.Isolation{Boundary: runtime.BoundaryVM, Detail: "nerdctl over containerd, io.containerd.urunc.v2"}
}

// shortStateDir points BRIG_STATE_DIR at a short directory. The divert socket
// is under it, and a unix socket path fits in 103 bytes.
func shortStateDir(t *testing.T) {
	t.Helper()
	short, err := os.MkdirTemp("", "bs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	t.Setenv("BRIG_STATE_DIR", short)
}

// claudeEndpoint binds claude-code to the Claude route in mode endpoint.
const claudeEndpoint = `apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: {name: brokered}
spec:
  routes:
    - name: anthropic
      preset: claude-code
      credential: {secretRef: {name: claude-credentials}}
`

// A route in mode endpoint on hull on hvi and on nerdctl reaches its guest at
// brig.internal. The broker is told the address the runtime answers the name
// with, and its socket is the one the runtime diverts that address to. The
// guest gets the route URL at brig.internal, its session token marked
// Secret, and the CA variables, and the CA the broker handed over is held
// for delivery.
func TestAnEndpointRunOnHviReachesBrigInternal(t *testing.T) {
	for _, c := range []struct {
		name string
		rt   runtime.Runtime
		hv   string
	}{
		{"hull on hvi", &hullRecorder{}, "hvi"},
		{"nerdctl", &nerdctlRecorder{}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("BRIG_HYPERVISOR", c.hv)
			t.Setenv("BRIG_CONTAINERD_RUNTIME", "")
			shortStateDir(t)
			cfg := routedConfig(t, claudeEndpoint, c.rt)
			shortStateDir(t)
			cfg.Verify = verify.Off
			broker := &fakeBroker{}
			cfg.broker = broker
			set, err := cfg.BuildEnv()
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.EnsureRunning(set); err == nil || !strings.Contains(err.Error(), "not booting") {
				t.Fatalf("the run did not reach the runtime: %v", err)
			}
			var spec *runtime.RunSpec
			switch rt := c.rt.(type) {
			case *hullRecorder:
				spec = rt.spec
			case *nerdctlRecorder:
				spec = rt.spec
			}
			if spec == nil {
				t.Fatal("the runtime was never asked to run")
			}
			if len(broker.registered) != 1 {
				t.Fatalf("registered %+v", broker.registered)
			}
			reg := broker.registered[0]
			if reg.BrigInternal != "198.18.1.1" {
				t.Errorf("registered %+v, want brig.internal at the runtime's address", reg)
			}
			dir, _ := injector.Dir()
			sock, _ := injector.DivertSocket(dir, cfg.VMName)
			d := spec.Egress.Divert
			if !d.BrigInternal || d.Socket != sock || len(d.Routes) != 0 {
				t.Errorf("Divert = %+v, want brig.internal alone to %s", d, sock)
			}
			if spec.Net != "isolated" {
				t.Errorf("Net %q", spec.Net)
			}
			if cfg.brigCA != testBrigCA() {
				t.Error("the CA the broker handed over is not held for delivery")
			}
			env := envOf(cfg.execSpec(set, []string{"claude"}, false).Env)
			want := map[string]string{
				"ANTHROPIC_BASE_URL":  "https://brig.internal/" + cfg.VMName + "/r/anthropic",
				"NODE_EXTRA_CA_CERTS": injector.GuestCAPath,
				"SSL_CERT_FILE":       injector.GuestCABundlePath,
			}
			for name, v := range want {
				if env[name].Value != v {
					t.Errorf("%s = %q, want %q", name, env[name].Value, v)
				}
			}
			if v := env["CLAUDE_CODE_OAUTH_TOKEN"]; v.Value != "session-token-"+cfg.VMName+"-anthropic" || !v.Secret {
				t.Errorf("CLAUDE_CODE_OAUTH_TOKEN = %+v, want the session token, marked Secret", v)
			}
			if got := cfg.brokerLine(); !strings.HasPrefix(got, "transport brig.internal, routes anthropic") ||
				!strings.Contains(got, "modes endpoint") {
				t.Errorf("BROKER row: %q", got)
			}
		})
	}
}

// A broker that hands over anything but one certificate of a CA constrained
// to brig.internal is refused before the guest gets it: a key beside the
// certificate, a CA that would vouch for any name, or no CA at all.
func TestTheBrigCAIsCheckedBeforeTheGuestGetsIt(t *testing.T) {
	makeTestCAs()
	if got, err := checkBrigCA(testCA.cert); err != nil || got != testCA.cert {
		t.Fatalf("the broker's CA: %v", err)
	}
	for name, ca := range map[string]string{
		"with its key":   testCA.cert + testCA.key,
		"key first":      testCA.key + testCA.cert,
		"the key alone":  testCA.key,
		"unconstrained":  testCA.loose,
		"two":            testCA.cert + testCA.cert,
		"none":           "",
		"trailing bytes": testCA.cert + "x",
	} {
		if _, err := checkBrigCA(ca); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for _, ca := range []string{testCA.cert + testCA.key, testCA.loose} {
		t.Setenv("BRIG_HYPERVISOR", "hvi")
		rec := &hullRecorder{}
		cfg := routedConfig(t, claudeEndpoint, rec)
		shortStateDir(t)
		cfg.Verify = verify.Off
		cfg.broker = &fakeBroker{ca: ca}
		err := cfg.EnsureRunning(creds.Set{})
		if err == nil || !strings.Contains(err.Error(), "brig CA that brig will not put in the guest") {
			t.Errorf("EnsureRunning = %v", err)
		}
		if rec.spec != nil {
			t.Error("a run whose broker handed over a bad CA booted")
		}
	}
}

// caGuest records what CA delivery runs in the guest, and keeps the files it
// writes.
type caGuest struct {
	runtime.Runtime
	files map[string]string
	argv  []string
	feeds []runtime.ExecSpec
	// dropFeeds reports every Feed done and writes nothing.
	dropFeeds bool
}

func (g *caGuest) Kind() string { return "hull" }
func (g *caGuest) MaxFeed() int { return 256 }

func (g *caGuest) Output(spec runtime.ExecSpec) (string, error) {
	g.argv = append(g.argv, strings.Join(spec.Cmd, "\x00"))
	if spec.User != guestRootUser {
		return "", fmt.Errorf("ran %q as %q", spec.Cmd, spec.User)
	}
	switch {
	case spec.Cmd[0] == "sh" && spec.Cmd[2] == caCreateScript:
		g.files[spec.Cmd[5]] = ""
	case spec.Cmd[0] == "sh" && spec.Cmd[2] == caBundleScript:
		g.files[spec.Cmd[4]] = "ROOTS\n" + g.files[spec.Cmd[5]]
	case spec.Cmd[0] == "sh" && spec.Cmd[2] == caSizeScript:
		if fmt.Sprint(len(g.files[spec.Cmd[4]])) != spec.Cmd[5] {
			return "", errors.New("exit status 1")
		}
	default:
		return "", fmt.Errorf("unexpected %q", spec.Cmd)
	}
	return "", nil
}

func (g *caGuest) Feed(spec runtime.ExecSpec) error {
	body, err := io.ReadAll(spec.Stdin)
	if err != nil {
		return err
	}
	target := spec.Cmd[len(spec.Cmd)-1]
	if _, ok := g.files[target]; !ok {
		return fmt.Errorf("fed %s before it was created", target)
	}
	if g.dropFeeds {
		return nil
	}
	g.files[target] += string(body)
	g.feeds = append(g.feeds, spec)
	return nil
}

// The guest gets the CA certificate at /brig/ca/brig.pem, through stdin and
// in pieces the runtime can carry, and a bundle of its own roots and the CA.
// The CA never goes on a command line, and nothing else is written.
func TestTheBrigCAIsDeliveredWithoutItsKey(t *testing.T) {
	makeTestCAs()
	g := &caGuest{files: map[string]string{}}
	c := &Config{Runtime: g, VMName: "brig-a", brigCA: testCA.cert}
	if err := c.deliverSecretFiles(); err != nil {
		t.Fatal(err)
	}
	if g.files[injector.GuestCAPath] != testCA.cert {
		t.Errorf("%s holds %q", injector.GuestCAPath, g.files[injector.GuestCAPath])
	}
	if g.files[injector.GuestCABundlePath] != "ROOTS\n"+testCA.cert {
		t.Errorf("%s holds %q", injector.GuestCABundlePath, g.files[injector.GuestCABundlePath])
	}
	if len(g.files) != 2 {
		t.Errorf("delivery wrote %v", g.files)
	}
	for _, body := range g.files {
		if strings.Contains(body, "PRIVATE KEY") {
			t.Error("a key reached the guest")
		}
	}
	if len(g.feeds) < 2 {
		t.Errorf("the CA went over in %d feeds, want pieces of at most 256 bytes", len(g.feeds))
	}
	for _, a := range g.argv {
		if strings.Contains(a, "BEGIN CERTIFICATE") {
			t.Errorf("the CA was on a command line: %q", a)
		}
	}
	// A run that reaches no route at brig.internal writes nothing.
	g2 := &caGuest{files: map[string]string{}}
	if err := (&Config{Runtime: g2, VMName: "brig-a"}).deliverSecretFiles(); err != nil || len(g2.argv)+len(g2.feeds) != 0 {
		t.Errorf("a run with no CA ran %q, %v", g2.argv, err)
	}
}

// The bundle script, run by a real shell: the first root file that exists,
// then the CA. A guest image with none gets the CA alone.
func TestTheCABundleKeepsTheImagesRoots(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	ca := filepath.Join(dir, "brig.pem")
	roots := filepath.Join(dir, "roots.crt")
	out := filepath.Join(dir, "bundle.pem")
	for p, body := range map[string]string{ca: "CA\n", roots: "ROOTS"} {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(sources ...string) string {
		t.Helper()
		args := append([]string{"-c", caBundleScript, "sh", out, ca}, sources...)
		if b, err := exec.Command(sh, args...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, b)
		}
		got, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(out + ".new"); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the script left %s.new", out)
		}
		return string(got)
	}
	if got := run(filepath.Join(dir, "absent"), roots, ca); got != "ROOTS\nCA\n" {
		t.Errorf("bundle = %q", got)
	}
	if got := run(filepath.Join(dir, "absent")); got != "CA\n" {
		t.Errorf("bundle with no roots = %q", got)
	}
}

// A delivery that lands short is refused, by the exit status of the size
// check in the guest.
func TestAShortCADeliveryIsRefused(t *testing.T) {
	makeTestCAs()
	g := &caGuest{files: map[string]string{}, dropFeeds: true}
	c := &Config{Runtime: g, VMName: "brig-a", brigCA: testCA.cert}
	if err := c.deliverSecretFiles(); err == nil || !strings.Contains(err.Error(), "does not hold the") {
		t.Fatalf("deliverSecretFiles = %v", err)
	}
	if _, ok := g.files[injector.GuestCABundlePath]; ok {
		t.Error("a bundle was written from a short CA")
	}
}

// awsBesideClaude binds claude-code to a route that vends and an endpoint
// route beside it.
const awsBesideClaude = `apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: {name: brokered}
spec:
  routes:
    - name: anthropic
      preset: claude-code
      credential: {secretRef: {name: claude-credentials}}
    - name: aws
      preset: aws
      credential: {secretRef: {name: aws-credentials}}
`

// recorded is the spec a recording runtime was handed, and its recorder.
func recorded(rt runtime.Runtime) *hullRecorder {
	switch r := rt.(type) {
	case *hullRecorder:
		return r
	case *nerdctlRecorder:
		return &r.hullRecorder
	}
	return nil
}

// A run with a route that vends diverts the link-local address to the
// broker's socket beside brig.internal, on hull on hvi and on nerdctl. The
// aws route's pointer is plain HTTP to that address, and the endpoint route
// beside it keeps brig.internal.
func TestAVendingRunDivertsTheLinkLocalAddress(t *testing.T) {
	for _, c := range []struct {
		name string
		rt   runtime.Runtime
		hv   string
	}{
		{"hull on hvi", &hullRecorder{}, "hvi"},
		{"nerdctl", &nerdctlRecorder{}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("BRIG_HYPERVISOR", c.hv)
			t.Setenv("BRIG_CONTAINERD_RUNTIME", "")
			shortStateDir(t)
			cfg := routedConfig(t, awsBesideClaude, c.rt)
			shortStateDir(t)
			cfg.Verify = verify.Off
			cfg.broker = &fakeBroker{regions: map[string]string{"aws": "eu-west-1"}}
			set, err := cfg.BuildEnv()
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.EnsureRunning(set); err == nil || !strings.Contains(err.Error(), "not booting") {
				t.Fatalf("the run did not reach the runtime: %v", err)
			}
			rec := recorded(c.rt)
			if rec.spec == nil {
				t.Fatal("the runtime was never asked to run")
			}
			dir, _ := injector.Dir()
			sock, _ := injector.DivertSocket(dir, cfg.VMName)
			d := rec.spec.Egress.Divert
			want := []runtime.DivertRoute{runtime.LinkLocalDivert}
			if !d.BrigInternal || d.Socket != sock || !slices.Equal(d.Routes, want) {
				t.Errorf("Divert = %+v, want brig.internal and %v to %s", d, want, sock)
			}
			if got := net.JoinHostPort(runtime.LinkLocalDivert.Host, strconv.Itoa(runtime.LinkLocalDivert.Port)); got != injector.LinkLocalAddr {
				t.Errorf("the runtime diverts %s, and the broker serves %s", got, injector.LinkLocalAddr)
			}
			env := envOf(cfg.execSpec(set, []string{"claude"}, false).Env)
			if v := env["AWS_CONTAINER_CREDENTIALS_FULL_URI"].Value; v != "http://169.254.170.2/"+cfg.VMName+"/r/aws" {
				t.Errorf("AWS_CONTAINER_CREDENTIALS_FULL_URI = %q", v)
			}
			if v := env["ANTHROPIC_BASE_URL"].Value; v != "https://brig.internal/"+cfg.VMName+"/r/anthropic" {
				t.Errorf("ANTHROPIC_BASE_URL = %q", v)
			}
			if v, ok := env["AWS_CA_BUNDLE"]; ok {
				t.Errorf("AWS_CA_BUNDLE = %q, want it unset", v.Value)
			}
		})
	}
}

// A run with no route that vends has no link-local divert, and does not ask
// the runtime whether it can divert one.
func TestARunWithoutAVendingRouteHasNoLinkLocalDivert(t *testing.T) {
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	shortStateDir(t)
	rec := &hullRecorder{linkLocal: errors.New("this hull cannot divert link-local")}
	cfg := routedConfig(t, claudeEndpoint, rec)
	shortStateDir(t)
	cfg.Verify = verify.Off
	cfg.broker = &fakeBroker{}
	if err := cfg.EnsureRunning(creds.Set{}); err == nil || !strings.Contains(err.Error(), "not booting") {
		t.Fatalf("the run did not reach the runtime: %v", err)
	}
	if rec.linkLocalProbes != 0 {
		t.Errorf("a run with no route that vends probed for a link-local divert %d times", rec.linkLocalProbes)
	}
	for _, r := range rec.spec.Egress.Divert.Routes {
		if a, err := netip.ParseAddr(r.Host); err == nil && a.IsLinkLocalUnicast() {
			t.Errorf("a run with no route that vends diverts %s:%d", r.Host, r.Port)
		}
	}
}

// A hull whose gateway cannot divert the link-local address refuses a run
// with a route that vends before anything boots, as a capability (exit 7).
// The broker is never asked to vend for it.
func TestAVendingRunIsRefusedOnAHullThatCannotDivertLinkLocal(t *testing.T) {
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	shortStateDir(t)
	refusal := &runtime.CapabilityError{Property: runtime.RouteModeProperty("endpoint"),
		Path: runtime.RunPath{Runtime: "hull", Backend: "hvi"}, State: runtime.CannotEnforce,
		Why: "this route needs a hull whose gateway can divert 169.254.170.2", Remedy: "Upgrade hull, or detach the policy"}
	rec := &hullRecorder{linkLocal: refusal}
	cfg := routedConfig(t, awsEndpoint, rec)
	shortStateDir(t)
	cfg.Verify = verify.Off
	broker := &fakeBroker{regions: map[string]string{"aws": "eu-west-1"}}
	cfg.broker = broker
	err := cfg.EnsureRunning(creds.Set{})
	var ce *runtime.CapabilityError
	if !errors.As(err, &ce) || ce != refusal {
		t.Fatalf("EnsureRunning = %v, want the runtime's refusal", err)
	}
	if !strings.Contains(err.Error(), "route aws") || !strings.Contains(err.Error(), "Upgrade hull") {
		t.Errorf("the refusal does not name the route and the remedy: %v", err)
	}
	if rec.spec != nil {
		t.Error("a run the runtime cannot serve booted")
	}
	if len(broker.registered) != 0 {
		t.Errorf("the broker was asked to vend for a refused run: %+v", broker.registered)
	}
}

// Under a deny default, the egress a vending run hands the runtime allows
// nothing in 169.254.0.0/16. The divert of the link-local address names it,
// and needs no DNS answer to learn it, so it needs no allow either; an allow
// of it would admit every port of the address wherever the runtime's own
// link-local refusal did not stand in the way.
func TestAVendingRunAllowsNoLinkLocalAddress(t *testing.T) {
	for _, c := range []struct {
		name string
		rt   runtime.Runtime
		hv   string
	}{
		{"hull on hvi", &hullRecorder{}, "hvi"},
		{"nerdctl", &nerdctlRecorder{}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("BRIG_HYPERVISOR", c.hv)
			t.Setenv("BRIG_CONTAINERD_RUNTIME", "")
			shortStateDir(t)
			cfg := routedConfig(t, awsBesideClaude+"  egress:\n    default: deny\n", c.rt)
			shortStateDir(t)
			cfg.Verify = verify.Off
			cfg.broker = &fakeBroker{regions: map[string]string{"aws": "eu-west-1"}}
			set, err := cfg.BuildEnv()
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.EnsureRunning(set); err == nil || !strings.Contains(err.Error(), "not booting") {
				t.Fatalf("the run did not reach the runtime: %v", err)
			}
			e := recorded(c.rt).spec.Egress
			if e.Default != "deny" || !slices.Contains(e.Divert.Routes, runtime.LinkLocalDivert) {
				t.Fatalf("Egress = %+v, want a deny default that diverts the link-local address", e)
			}
			linkLocal := netip.MustParsePrefix("169.254.0.0/16")
			for _, r := range e.Allow {
				if r.CIDR == "" {
					continue
				}
				if p, err := netip.ParsePrefix(r.CIDR); err != nil || p.Overlaps(linkLocal) {
					t.Errorf("the egress allows %+v, which covers link-local", r)
				}
			}
		})
	}
}

// An offline run with a route that vends is refused for having no network,
// which is the refusal the user acts on first, and does not ask the hull
// whether it can divert the link-local address.
func TestAnOfflineVendingRunIsRefusedForItsNetworkFirst(t *testing.T) {
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	shortStateDir(t)
	rec := &hullRecorder{linkLocal: errors.New("this hull cannot divert link-local")}
	cfg := routedConfig(t, awsEndpoint, rec)
	shortStateDir(t)
	cfg.Network = NetOffline
	err := cfg.routeRefusal()
	if err == nil || !strings.Contains(err.Error(), "BRIG_NETWORK=") || strings.Contains(err.Error(), "Upgrade hull") {
		t.Fatalf("routeRefusal = %v, want the offline refusal", err)
	}
	if rec.linkLocalProbes != 0 {
		t.Errorf("an offline run probed for a link-local divert %d times", rec.linkLocalProbes)
	}
}
