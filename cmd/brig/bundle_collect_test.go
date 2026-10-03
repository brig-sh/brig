package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/buildinfo"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/redact"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/secret"
)

func TestIsWSL(t *testing.T) {
	if !isWSL("5.15.153.1-microsoft-standard-WSL2") {
		t.Error("a WSL2 kernel was not recognised")
	}
	if isWSL("6.8.0-45-generic") {
		t.Error("a stock kernel read as WSL")
	}
}

func TestOSReleasePretty(t *testing.T) {
	got := osReleasePretty("NAME=\"Ubuntu\"\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nID=ubuntu\n")
	if got != "Ubuntu 24.04.1 LTS" {
		t.Errorf("got %q", got)
	}
}

func TestInstallMethod(t *testing.T) {
	tagged := buildinfo.Info{Version: "v0.3.0"}
	cases := []struct {
		exe  string
		info buildinfo.Info
		want string
	}{
		{"/opt/homebrew/Caskroom/brig/0.3.0/brig", tagged, "homebrew (inferred)"},
		{"/usr/local/bin/brig", tagged, "install.sh (inferred)"},
		{"/home/u/brig/brig", buildinfo.Info{Version: "v0.3.1-0.20260916120000-b2b2b2b2b2b2"}, "source (inferred)"},
		{"/home/u/brig/brig", buildinfo.Info{Version: "v0.3.0", Modified: true}, "source (inferred)"},
	}
	for _, c := range cases {
		if got := installMethod(c.exe, c.info); got != c.want {
			t.Errorf("installMethod(%q, %+v) = %q, want %q", c.exe, c.info, got, c.want)
		}
	}
}

// A host with no runtime still gets an environment file, with the reason.
func TestEnvironmentWithoutARuntime(t *testing.T) {
	swap(t, &detectRuntime, func() (runtime.Runtime, error) { return nil, errors.New("no hull on PATH") })
	r := redact.New(redact.Host{}, redact.Allow{})
	entries, err := environmentCollector(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.Contains(entries[0].body, "no hull on PATH") {
		t.Errorf("entries %+v", entries)
	}
}

func TestRedactRefAndSandbox(t *testing.T) {
	r := redact.New(redact.Host{}, redact.Allow{Names: []string{"claude-code"}})
	if got := redactRef(r, "claude-code@acme-canary"); got != "claude-code@<label-1>" {
		t.Errorf("ref: %q", got)
	}
	if got := redactRef(r, "acme-agent"); got != "<profile-1>" {
		t.Errorf("custom ref: %q", got)
	}
	if got := redactSandbox(r, "brig-claude-code"); got != "brig-claude-code" {
		t.Errorf("default sandbox of a built-in: %q", got)
	}
	if got := redactSandbox(r, "brig-claude-code-acme-canary"); got != "<sandbox-1>" {
		t.Errorf("labelled sandbox: %q", got)
	}
}

// The doctor report goes in whole, as the --json document's data.
func TestDoctorCollector(t *testing.T) {
	healthyHost(t)
	r := redact.New(redact.Host{}, redact.Allow{})
	entries, err := doctorCollector(nil, nil)(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].name != "doctor.json" || !strings.Contains(entries[0].body, `"name": "runtime"`) {
		t.Errorf("entries %+v", entries)
	}
}

// bundleRuntimeDouble is a runtime.Runtime that answers only List; sandboxRows
// reads the rest from the seeded session index. Any other call panics.
type bundleRuntimeDouble struct {
	runtime.Runtime
	list []runtime.Instance
}

func (d bundleRuntimeDouble) List() ([]runtime.Instance, error) { return d.list, nil }

// A host with no runtime still gets a sandboxes file, skipped with the reason.
func TestSandboxesCollectorWithoutARuntime(t *testing.T) {
	swap(t, &detectRuntime, func() (runtime.Runtime, error) { return nil, errors.New("no hull on PATH") })
	r := redact.New(redact.Host{}, redact.Allow{})
	entries, err := sandboxesCollector(r)
	if entries != nil {
		t.Errorf("entries %+v, want none", entries)
	}
	var sk skipped
	if !errors.As(err, &sk) || !strings.Contains(string(sk), "no hull on PATH") {
		t.Errorf("err %v, want skipped naming the reason", err)
	}
}

// The last boot's digest is carried verbatim; its image's repository is
// redacted.
func TestSandboxesCollectorKeepsTheBootDigestAndRedactsTheImage(t *testing.T) {
	t.Setenv("BRIG_STATE_DIR", t.TempDir())
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())

	index := `{"claude-code@acme-canary": {"home": "/ws/acme-canary", "sandbox": "brig-claude-code-prod"}}`
	if err := os.WriteFile(filepath.Join(os.Getenv("BRIG_STATE_DIR"), "sessions.json"), []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}

	digest := "sha256:" + strings.Repeat("ab", 32)
	bootAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	if err := runtime.RecordBoot("brig-claude-code-prod", runtime.BootRecord{
		Result: runtime.BootOK,
		At:     bootAt,
		Image:  "ghcr.io/acme-canary/agent:canary-prod",
		Digest: digest,
	}); err != nil {
		t.Fatal(err)
	}

	swap(t, &detectRuntime, func() (runtime.Runtime, error) {
		return bundleRuntimeDouble{list: []runtime.Instance{{Name: "brig-claude-code-prod", State: "running"}}}, nil
	})

	r := redact.New(redact.Host{}, redact.Allow{Names: []string{"claude-code"}})
	entries, err := sandboxesCollector(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].name != "sandboxes.json" {
		t.Fatalf("entries %+v", entries)
	}
	body := entries[0].body
	if !strings.Contains(body, digest) {
		t.Errorf("the digest was not carried verbatim: %s", body)
	}
	if strings.Contains(body, "acme-canary") {
		t.Errorf("acme-canary survived redaction: %s", body)
	}
	if !strings.Contains(body, `"sandbox": "<sandbox-1>"`) {
		t.Errorf("the sandbox name was not redacted: %s", body)
	}
	if !strings.Contains(body, bootAt.Format(time.RFC3339)) {
		t.Errorf("lastBootAt was not the recorded boot time: %s", body)
	}
}

// doctorCollector registers a custom agent's name and image, which
// imageCheck's findings embed; the profiles collector may not have run.
// Neither survives r.Text.
func TestDoctorCollectorRedactsACustomAgentsImageAndName(t *testing.T) {
	healthyHost(t)
	agent := &profile.Profile{Name: "acme-canary", Image: "ghcr.io/acme-canary/agent:canary-prod"}
	r := redact.New(redact.Host{}, redact.Allow{})
	entries, err := doctorCollector(agent, nil)(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries %+v", entries)
	}
	scrubbed := r.Text(entries[0].body).String()
	if strings.Contains(scrubbed, "acme-canary") {
		t.Errorf("the agent's name survived redaction: %s", scrubbed)
	}
	if strings.Contains(scrubbed, "canary-prod") {
		t.Errorf("the agent's image survived redaction: %s", scrubbed)
	}
}

// A built-in agent's name stays readable. With no Image, imageCheck names the
// agent without calling cosign or a registry.
func TestDoctorCollectorKeepsABuiltInAgentsName(t *testing.T) {
	healthyHost(t)
	agent := &profile.Profile{Name: "claude-code"}
	r := redact.New(redact.Host{}, redact.Allow{Names: []string{"claude-code"}})
	entries, err := doctorCollector(agent, nil)(r)
	if err != nil {
		t.Fatal(err)
	}
	scrubbed := r.Text(entries[0].body).String()
	if !strings.Contains(scrubbed, "no image is published for claude-code") {
		t.Errorf("the built-in agent's name was redacted: %s", scrubbed)
	}
}

// Built-in names, images and variables stay readable; an override's image
// does not become brig's.
func TestAllowlistComesFromTheShippedSpecs(t *testing.T) {
	a := allowlist()
	if !slices.Contains(a.Names, "claude-code") || !slices.Contains(a.Names, "claude") {
		t.Errorf("names %v", a.Names)
	}
	p, _ := profile.BuiltIn("claude-code")
	if !slices.Contains(a.Images, imageRepo(p.Image)) {
		t.Errorf("images %v, missing %s", a.Images, imageRepo(p.Image))
	}
}

// Only chosen fields are copied. Env values and file paths never appear.
func TestProjectProfileCopiesNamesNotValues(t *testing.T) {
	r := redact.New(redact.Host{Home: "/home/zz"}, redact.Allow{Vars: []string{"ANTHROPIC_API_KEY"}})
	p := profile.Profile{
		Name:  "acme-canary",
		Image: "ghcr.io/acme-canary/agent:1.0",
		// Parse folds Forward into Env as `ref: env.<name>`, so these are
		// set as Env.
		Env: []profile.EnvBinding{
			{Name: "ANTHROPIC_API_KEY", Ref: "env.ANTHROPIC_API_KEY"},
			{Name: "ACME_CANARY_TOKEN", Ref: "env.ACME_CANARY_TOKEN"},
			{Name: "ACME_MODE", Value: "literal-canary-value"},
		},
		Mem: 2048, CPUs: 2,
	}
	body, err := bundleJSON(projectProfile(r, p))
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"acme-canary", "ACME_CANARY_TOKEN", "ACME_MODE", "literal-canary-value"} {
		if strings.Contains(body, leaked) {
			t.Errorf("%q in %s", leaked, body)
		}
	}
	if !strings.Contains(body, "ANTHROPIC_API_KEY") || !strings.Contains(body, `"mem": 2048`) {
		t.Errorf("lost what a maintainer needs: %s", body)
	}
}

// agentDigestRuntime is a runtime.Runtime that answers only LocalDigest. Any
// other call panics.
type agentDigestRuntime struct {
	runtime.Runtime
	digest string
}

func (d agentDigestRuntime) LocalDigest(string) (string, error) { return d.digest, nil }

// The local store's digest is carried into the agent profile verbatim.
func TestAgentCollectorCarriesTheLocalDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("cd", 32)
	swap(t, &detectRuntime, func() (runtime.Runtime, error) {
		return agentDigestRuntime{digest: digest}, nil
	})
	r := redact.New(redact.Host{}, redact.Allow{Names: []string{"claude-code"}})
	agent := profile.Profile{Name: "claude-code"}
	entries, err := agentCollector(agent)(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.Contains(entries[0].body, `"localDigest": "`+digest+`"`) {
		t.Errorf("entries %+v", entries)
	}
}

// A runtime answering "", as hull does, omits the field.
func TestAgentCollectorOmitsAnUnknownLocalDigest(t *testing.T) {
	swap(t, &detectRuntime, func() (runtime.Runtime, error) {
		return agentDigestRuntime{digest: ""}, nil
	})
	r := redact.New(redact.Host{}, redact.Allow{Names: []string{"claude-code"}})
	agent := profile.Profile{Name: "claude-code"}
	entries, err := agentCollector(agent)(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || strings.Contains(entries[0].body, "localDigest") {
		t.Errorf("entries %+v, want no localDigest", entries)
	}
}

// A custom agent's name is redacted in the entry path, not just the body.
func TestAgentCollectorRedactsACustomAgentsNameInThePath(t *testing.T) {
	swap(t, &detectRuntime, func() (runtime.Runtime, error) { return nil, errors.New("no runtime") })
	r := redact.New(redact.Host{}, redact.Allow{})
	agent := profile.Profile{Name: "acme-canary", Image: "ghcr.io/acme-canary/agent:1.0"}
	entries, err := agentCollector(agent)(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries %+v", entries)
	}
	if strings.Contains(entries[0].name, "acme-canary") {
		t.Errorf("the agent's name survived in the entry path: %s", entries[0].name)
	}
	if !strings.HasPrefix(entries[0].name, "agent/") || !strings.HasSuffix(entries[0].name, "/profile.json") {
		t.Errorf("unexpected entry name shape: %s", entries[0].name)
	}
}

// A profile's declared policies are carried as placeholder names.
func TestProjectProfileCarriesPolicyNamesAsPlaceholders(t *testing.T) {
	r := redact.New(redact.Host{}, redact.Allow{})
	p := profile.Profile{Name: "acme-canary", Policy: []string{"acme-canary-egress"}}
	body, err := bundleJSON(projectProfile(r, p))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "acme-canary-egress") {
		t.Errorf("the policy name was not redacted: %s", body)
	}
	if !strings.Contains(body, `"policies"`) {
		t.Errorf("the policies field was dropped: %s", body)
	}
}

// A host and a CIDR outside the allowlist are redacted; a forge suffix and
// the gateway range stay readable.
func TestProjectPoliciesRedactsRulesButKeepsAllowlisted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	body := "apiVersion: brig.sh/v1alpha1\n" +
		"name: acme-x\n" +
		"egress:\n" +
		"  default: deny\n" +
		"  allow:\n" +
		"  - host: internal.acme.example\n" +
		"  - cidr: 10.1.2.0/24\n" +
		"  - host: \"*.githubusercontent.com\"\n" +
		"  - cidr: 198.18.0.0/15\n"
	if err := os.WriteFile(filepath.Join(dir, "acme-x.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	r := redact.New(redact.Host{}, allowlist())
	out, err := projectPolicies(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("policies %+v", out)
	}
	joined := strings.Join(append([]string{out[0].Name}, out[0].Allow...), " ")
	if strings.Contains(joined, "internal.acme.example") {
		t.Errorf("the private host survived: %v", out[0])
	}
	if strings.Contains(joined, "10.1.2.0/24") {
		t.Errorf("the private CIDR survived: %v", out[0])
	}
	if strings.Contains(joined, "acme-x") {
		t.Errorf("the policy's own name was not redacted: %v", out[0])
	}
	if !strings.Contains(joined, "*.githubusercontent.com") {
		t.Errorf("a public forge suffix was redacted: %v", out[0])
	}
	if !strings.Contains(joined, "198.18.0.0/15") {
		t.Errorf("the gateway range was redacted: %v", out[0])
	}
}

// A broken policy file keeps the good ones, and its filename reaches no entry
// body, entry name or collector status: the redactor does not have it.
func TestBundleDoesNotLeakABrokenPolicyFilename(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)

	good := "apiVersion: brig.sh/v1alpha1\n" +
		"name: acme-good\n" +
		"egress:\n" +
		"  default: deny\n" +
		"  allow:\n" +
		"  - host: internal.acme.example\n"
	if err := os.WriteFile(filepath.Join(dir, "acme-good.yaml"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "acmecorp-canary.yaml"), []byte("not: [valid"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := redact.New(redact.Host{}, redact.Allow{})
	entries, statuses := runCollectors([]collector{{name: "profiles", run: profilesCollector}}, r)
	finished, err := finishBundle(r, entries, statuses, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	var policiesBody, manifest string
	for _, e := range finished {
		name, body := e.name.String(), e.body.String()
		if strings.Contains(name, "acmecorp-canary") || strings.Contains(body, "acmecorp-canary") {
			t.Errorf("the broken policy's filename leaked in %s: %s", name, body)
		}
		switch name {
		case "profiles/policies.json":
			policiesBody = body
		case "MANIFEST.txt":
			manifest = body
		}
	}
	if strings.Contains(manifest, "acmecorp-canary") {
		t.Errorf("MANIFEST.txt leaked the broken policy's filename: %s", manifest)
	}
	if !strings.Contains(manifest, "policy file(s) could not be read") {
		t.Errorf("the manifest did not say a policy file was unreadable: %s", manifest)
	}
	if policiesBody == "" {
		t.Fatal("no profiles/policies.json entry")
	}
	if !strings.Contains(policiesBody, `"default": "deny"`) {
		t.Errorf("the good policy did not survive the broken file: %s", policiesBody)
	}
}

// bundleRuntime lists one sandbox and serves a log for it.
type bundleRuntime struct {
	doctorRuntime
	names []string
	log   string
	// release, when set, blocks List until it is closed.
	release chan struct{}
}

func (r bundleRuntime) List() ([]runtime.Instance, error) {
	if r.release != nil {
		<-r.release
	}
	var out []runtime.Instance
	for _, n := range r.names {
		out = append(out, runtime.Instance{Name: n, State: "stopped"})
	}
	return out, nil
}

func (r bundleRuntime) Logs(spec runtime.LogsSpec) error {
	_, err := io.WriteString(spec.Out, r.log)
	return err
}

// LocalDigest answers "" as hull does. The agent collector calls it.
func (bundleRuntime) LocalDigest(string) (string, error) { return "", nil }

func collectLogs(t *testing.T, o bundleOptions) []rawEntry {
	t.Helper()
	r := redact.New(redact.Host{}, redact.Allow{})
	entries, err := logsCollector(o)(r)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestLogsOnlyForAFailedBoot(t *testing.T) {
	healthyHost(t)
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	rt := bundleRuntime{names: []string{"brig-a", "brig-b", "brig-c"}, log: "boot line\n"}
	swap(t, &detectRuntime, func() (runtime.Runtime, error) { return rt, nil })
	_ = runtime.RecordBoot("brig-a", runtime.BootRecord{Result: runtime.BootFailed, At: time.Now()})
	_ = runtime.RecordBoot("brig-b", runtime.BootRecord{Result: runtime.BootOK, At: time.Now()})
	// brig-c has no record.

	entries := collectLogs(t, bundleOptions{})
	if len(entries) != 1 {
		t.Fatalf("got %d logs, want only the failed boot's: %+v", len(entries), entries)
	}
	all := collectLogs(t, bundleOptions{includeLogs: true})
	if len(all) != 3 {
		t.Errorf("--include-logs: %d logs, want 3", len(all))
	}
}

// The gateway log needs --include-logs even when the boot failed.
func TestGatewayLogNeedsTheFlag(t *testing.T) {
	healthyHost(t)
	gw := t.TempDir()
	t.Setenv("BRIG_GATEWAY_DIR", gw)
	rt := bundleRuntime{names: []string{"brig-a"}, log: "boot\n"}
	swap(t, &detectRuntime, func() (runtime.Runtime, error) { return rt, nil })
	_ = runtime.RecordBoot("brig-a", runtime.BootRecord{Result: runtime.BootFailed, At: time.Now()})
	shared, _ := runtime.GatewayLogPath()
	if err := os.WriteFile(shared, []byte("dialled api.example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, e := range collectLogs(t, bundleOptions{}) {
		if strings.Contains(e.name, "gateway") {
			t.Errorf("gateway log without the flag: %s", e.name)
		}
	}
	found := false
	for _, e := range collectLogs(t, bundleOptions{includeLogs: true}) {
		found = found || strings.Contains(e.name, "gateway")
	}
	if !found {
		t.Error("--include-logs did not add the gateway log")
	}
}

// tripwireStore fails the test if anything reads a secret value.
type tripwireStore struct {
	secret.Store
	t *testing.T
}

func (tripwireStore) Kind() string { return "keychain" }
func (s tripwireStore) Read(name string) ([]byte, error) {
	s.t.Errorf("the bundle read the value of secret %q", name)
	return nil, secret.ErrNotFound
}
func (tripwireStore) List() ([]secret.Secret, error) { return nil, nil }

// canaryHost is a host whose every identifying value is a unique marker, with
// a custom profile, a broken one, a policy, a labelled session, a failed boot
// and a log that mentions them. No marker contains another.
func canaryHost(t *testing.T) (home string) {
	t.Helper()
	healthyHost(t)
	home = filepath.Join(t.TempDir(), "zz-canary-user")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USER", "zz-canary-user")
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	t.Setenv("ACME_CANARY_TOKEN", "envcanaryvalue-7f3a")
	swap(t, &bundleHostname, func() (string, error) { return "zz-canary-box.local", nil })
	swap(t, &openStore, func() (secret.Store, error) { return tripwireStore{t: t}, nil })

	// Boot assets outside $HOME; doctor's boot check names the directory.
	assets := filepath.Join(t.TempDir(), "zz-canary-assets")
	if err := os.MkdirAll(assets, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Image", "bzImage", "container-initrd"} {
		if err := os.WriteFile(filepath.Join(assets, name), []byte("stand-in\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("BRIG_BOOT_ASSETS", assets)

	profiles := os.Getenv("BRIG_PROFILE_DIR")
	prof := `{"name":"acme-canary","image":"ghcr.io/acme-canary/agent:canary-prod",
		"guestHome":"/home/agent","binary":"agent","mem":1024,"cpus":1,
		"forward":["ACME_CANARY_TOKEN"],"secrets":["ZZ_CANARY_SECRET_DECL"],
		"deny":["ZZ_CANARY_DENIED"],"policy":["zz-canary-policy"]}`
	if err := os.WriteFile(filepath.Join(profiles, "acme-canary.json"), []byte(prof), 0o600); err != nil {
		t.Fatal(err)
	}
	// A profile that does not parse; profile.Load's error names its file.
	if err := os.WriteFile(filepath.Join(profiles, "zz-canary-broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A profile that parses but fails validation, with an error quoting the
	// file's content.
	leaky := "name: zz-canary-leaky\nimage: busybox\nguestHome: /h\nbinary: b\nmem: 1\ncpus: 1\n" +
		"kind: zz-canary-filevalue\n"
	if err := os.WriteFile(filepath.Join(profiles, "zz-canary-leaky.yaml"), []byte(leaky), 0o600); err != nil {
		t.Fatal(err)
	}
	pol := "apiVersion: brig.sh/v1alpha1\nname: zz-canary-policy\negress:\n  default: deny\n" +
		"  allow:\n  - host: api.zz-canary-corp.internal\n"
	if err := os.WriteFile(filepath.Join(os.Getenv("BRIG_POLICY_DIR"), "zz-canary-policy.yaml"), []byte(pol), 0o600); err != nil {
		t.Fatal(err)
	}

	// A labelled session, workspace outside $HOME, sandbox name unrelated to
	// the profile's.
	ws := filepath.Join(t.TempDir(), "zz-canary-repo")
	idx := `{"acme-canary@zz-canary-label": {"home": "` + ws + `", "sandbox": "brig-zz-canary-sbx"}}`
	if err := os.WriteFile(filepath.Join(os.Getenv("BRIG_STATE_DIR"), "sessions.json"), []byte(idx), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = runtime.RecordBoot("brig-zz-canary-sbx", runtime.BootRecord{Result: runtime.BootFailed, At: time.Now(),
		Image: "ghcr.io/acme-canary/agent:canary-prod", Digest: "sha256:" + strings.Repeat("ab", 32)})

	log := "booting brig-zz-canary-sbx (acme-canary@zz-canary-label) on zz-canary-box as zz-canary-user in " + ws + "\n" +
		"ACME_CANARY_TOKEN=supersecretcanary1\n" +
		"mail zz@acme-canary.io from 203.0.113.7\n" +
		"token ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123\n" +
		"pulling ghcr.io/acme-canary/agent:canary-prod\n" +
		"policy zz-canary-policy allows api.zz-canary-corp.internal\n"
	rt := bundleRuntime{doctorRuntime: doctorRuntime{bin: detectedBin(t)}, names: []string{"brig-zz-canary-sbx"}, log: log}
	swap(t, &detectRuntime, func() (runtime.Runtime, error) { return rt, nil })
	return home
}

// detectedBin is the stand-in hull binary healthyHost installed.
func detectedBin(t *testing.T) string {
	t.Helper()
	rt, err := detectRuntime()
	if err != nil {
		t.Fatal(err)
	}
	return rt.Bin()
}

var canaries = []string{
	"zz-canary-user", "zz-canary-box", "acme-canary", "ACME_CANARY_TOKEN",
	"supersecretcanary1", "envcanaryvalue-7f3a", "203.0.113.7", "zz@",
	"ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123", "canary-prod",
	"zz-canary-repo", "zz-canary-label", "zz-canary-sbx",
	"ZZ_CANARY_SECRET_DECL", "ZZ_CANARY_DENIED",
	"zz-canary-policy", "zz-canary-corp", "zz-canary-broken", "zz-canary-assets",
	"zz-canary-leaky", "zz-canary-filevalue",
}

// zipText is every entry name and body, decompressed, plus the raw archive.
func zipText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.Write(raw)
	for _, f := range zr.File {
		b.WriteString("\n" + f.Name + "\n")
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(rc)
		rc.Close()
		b.Write(body)
	}
	return b.String()
}

// No identifying value, secret or address reaches the archive, and no secret
// value is ever read.
func TestTheBundleCarriesNoCanary(t *testing.T) {
	home := canaryHost(t)
	path := filepath.Join(t.TempDir(), "b.zip")
	if err := bundleCmd(&bytes.Buffer{}, []string{"acme-canary", "-o", path}); err != nil {
		t.Fatal(err)
	}
	text := zipText(t, path)
	for _, c := range append(canaries, home) {
		if strings.Contains(text, c) {
			i := strings.Index(text, c)
			t.Errorf("%q is in the bundle: ...%s...", c, text[max(0, i-80):min(len(text), i+len(c)+40)])
		}
	}
	// Token kinds are named as "github token", which the scrub leaves.
	replaced := text[strings.Index(text, "\nreplaced: "):]
	replaced, _, _ = strings.Cut(replaced[1:], "\n")
	if !strings.Contains(replaced, "github token") || strings.Contains(replaced, "<redacted:") {
		t.Errorf("manifest %q", replaced)
	}
	for _, want := range []string{"MANIFEST.txt", "environment.json", "doctor.json", "sandboxes.json",
		"profiles/profiles.json", "logs/<sandbox-1>.log", "agent/<profile-1>/profile.json",
		// Both broken profiles, counted rather than quoted.
		"2 profile file(s) could not be read",
		// The booted digest stays readable.
		"sha256:" + strings.Repeat("ab", 32)} {
		if !strings.Contains(text, want) {
			t.Errorf("the bundle has no %s", want)
		}
	}
}

// The logs collector registers each sandbox's workspace, label and last-boot
// image itself, in case the sandboxes collector timed out. The image here is
// one no profile names.
func TestTheLogsRedactTheWorkspaceWithoutTheSandboxesCollector(t *testing.T) {
	canaryHost(t)
	bootImage := "registry.zz-canary-bootreg.io/zz-canary-bootimg:zz-canary-boottag"
	if err := runtime.RecordBoot("brig-zz-canary-sbx", runtime.BootRecord{Result: runtime.BootFailed, At: time.Now(),
		Image: bootImage}); err != nil {
		t.Fatal(err)
	}
	rt := bundleRuntime{doctorRuntime: doctorRuntime{bin: detectedBin(t)}, names: []string{"brig-zz-canary-sbx"},
		log: "session zz-canary-label pulling " + bootImage + "\n"}
	swap(t, &detectRuntime, func() (runtime.Runtime, error) { return rt, nil })
	swap(t, &bundleCollectors, func(o bundleOptions, agent *profile.Profile, loadErr error) []collector {
		var cs []collector
		for _, c := range defaultBundleCollectors(o, agent, loadErr) {
			if c.name != "sandboxes" {
				cs = append(cs, c)
			}
		}
		return cs
	})
	path := filepath.Join(t.TempDir(), "b.zip")
	if err := bundleCmd(&bytes.Buffer{}, []string{"-o", path}); err != nil {
		t.Fatal(err)
	}
	text := zipText(t, path)
	if !strings.Contains(text, "logs/<sandbox-1>.log") {
		t.Fatal("the bundle has no log to check")
	}
	if strings.Contains(text, "sandboxes.json") {
		t.Fatal("the sandboxes collector still ran")
	}
	for _, c := range []string{"zz-canary-repo", "zz-canary-label", "zz-canary-bootreg", "zz-canary-bootimg", "zz-canary-boottag"} {
		if i := strings.Index(text, c); i >= 0 {
			t.Errorf("%q is in the bundle: ...%s...", c, text[max(0, i-80):min(len(text), i+len(c)+40)])
		}
	}
}

// With no runtime the bundle is still written; runtime collectors are skipped.
func TestTheBundleWithoutARuntime(t *testing.T) {
	healthyHost(t)
	swap(t, &detectRuntime, func() (runtime.Runtime, error) { return nil, errors.New("no hull on PATH") })
	path := filepath.Join(t.TempDir(), "b.zip")
	if err := bundleCmd(&bytes.Buffer{}, []string{"-o", path}); err != nil {
		t.Fatal(err)
	}
	text := zipText(t, path)
	for _, want := range []string{"environment.json", "doctor.json", "skipped: no runtime"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// awaitCollectors makes cleanup wait for every collector, abandoned ones
// included. Call it after swapping the seams: cleanups run LIFO, and restoring
// a seam an abandoned collector still reads is a data race.
//
// A collector skipped for budget never calls Done, so a test using this keeps
// the default bundleBudget.
func awaitCollectors(t *testing.T) {
	t.Helper()
	var wg sync.WaitGroup
	swap(t, &bundleCollectors, func(o bundleOptions, agent *profile.Profile, loadErr error) []collector {
		cs := defaultBundleCollectors(o, agent, loadErr)
		for i := range cs {
			run := cs[i].run
			wg.Add(1)
			cs[i].run = func(r *redact.Redactor) ([]rawEntry, error) {
				defer wg.Done()
				return run(r)
			}
		}
		return cs
	})
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("an abandoned collector never returned")
		}
	})
}

// A runtime that hangs costs its collectors their timeout, not the bundle.
func TestAHungRuntimeStillWritesTheBundle(t *testing.T) {
	healthyHost(t)
	swap(t, &bundleCollectorTimeout, 50*time.Millisecond)
	// logs has its own 30s timeout; shortened too.
	swap(t, &bundleLogsTimeout, 50*time.Millisecond)
	release := make(chan struct{})
	rt := bundleRuntime{doctorRuntime: doctorRuntime{bin: detectedBin(t)}, names: []string{"brig-a"}, release: release}
	swap(t, &detectRuntime, func() (runtime.Runtime, error) { return rt, nil })
	awaitCollectors(t)
	// Registered after awaitCollectors, so it runs first and releases List.
	t.Cleanup(func() { close(release) })
	start := time.Now()
	path := filepath.Join(t.TempDir(), "b.zip")
	if err := bundleCmd(&bytes.Buffer{}, []string{"-o", path}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(zipText(t, path), "timed out after 50ms") {
		t.Error("the manifest does not say the runtime timed out")
	}
	// List blocks for the whole run, so finishing means both timeouts held;
	// the bound catches one far too long.
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %v", took)
	}
}

// zipEntries is each entry's body by name.
func zipEntries(t *testing.T, path string) map[string]string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(body)
	}
	return out
}

// A path-valued setting outside $HOME, quoted back by the runtime's and
// cosign's errors into environment.json, doctor.json and the manifest, is
// redacted. The real Detect runs here.
func TestTheBundleRedactsPathSettingsOutsideHome(t *testing.T) {
	healthyHost(t)
	swap(t, &detectRuntime, runtime.Detect)
	t.Setenv("BRIG_RUNTIME_BIN", "/Volumes/zz-canary-rtbin/hull")
	t.Setenv("BRIG_COSIGN_BIN", "/Volumes/zz-canary-cosignbin/cosign")
	path := filepath.Join(t.TempDir(), "b.zip")
	if err := bundleCmd(&bytes.Buffer{}, []string{"-o", path}); err != nil {
		t.Fatal(err)
	}
	entries := zipEntries(t, path)
	if !strings.Contains(entries["environment.json"], "runtimeError") {
		t.Fatalf("the runtime did not fail, so nothing was tested: %s", entries["environment.json"])
	}
	if !strings.Contains(entries["MANIFEST.txt"], "skipped: no runtime") {
		t.Fatalf("no collector was skipped for the runtime: %s", entries["MANIFEST.txt"])
	}
	for _, name := range []string{"environment.json", "doctor.json", "MANIFEST.txt"} {
		for _, c := range []string{"zz-canary-rtbin", "zz-canary-cosignbin"} {
			if i := strings.Index(entries[name], c); i >= 0 {
				body := entries[name]
				t.Errorf("%s carries %q: ...%s...", name, c, body[max(0, i-80):min(len(body), i+len(c)+40)])
			}
		}
	}
}

// A named agent's runtimeBin outside $HOME, quoted in doctor's runtime
// finding, is redacted with only the doctor collector running.
func TestTheBundleRedactsAnAgentsRuntimeBinWithOnlyTheDoctorCollector(t *testing.T) {
	healthyHost(t)
	t.Setenv("BRIG_RUNTIME_BIN", "")
	prof := `{"name":"zz-canary-rtagent","image":"busybox","guestHome":"/h","binary":"b","mem":1,"cpus":1,
		"runtimeBin":"/Volumes/zz-canary-agentbin/hull"}`
	if err := os.WriteFile(filepath.Join(os.Getenv("BRIG_PROFILE_DIR"), "zz-canary-rtagent.json"), []byte(prof), 0o600); err != nil {
		t.Fatal(err)
	}
	swap(t, &bundleCollectors, func(o bundleOptions, agent *profile.Profile, loadErr error) []collector {
		return []collector{{name: "doctor", run: doctorCollector(agent, loadErr)}}
	})
	path := filepath.Join(t.TempDir(), "b.zip")
	if err := bundleCmd(&bytes.Buffer{}, []string{"zz-canary-rtagent", "-o", path}); err != nil {
		t.Fatal(err)
	}
	doctor := zipEntries(t, path)["doctor.json"]
	if !strings.Contains(doctor, "runtimeBin is") {
		t.Fatalf("the profile's runtimeBin was not what failed: %s", doctor)
	}
	if i := strings.Index(doctor, "zz-canary-agentbin"); i >= 0 {
		t.Errorf("doctor.json carries the runtimeBin: ...%s...", doctor[max(0, i-80):min(len(doctor), i+40)])
	}
}

// A policy host glob also registers the bare domain under it.
func TestRedactRuleCoversTheDomainUnderAGlob(t *testing.T) {
	r := redact.New(redact.Host{}, redact.Allow{})
	redactRule(r, policy.Rule{Host: "*.zz-corp.io"})
	got := r.Text("dialled zz-corp.io and api.zz-corp.io").String()
	if strings.Contains(got, "zz-corp") {
		t.Errorf("the glob's domain survived: %s", got)
	}
}

// A registry with a port also registers the host alone, which a pull error
// names.
func TestRegisterImageCoversARegistryHostWithoutItsPort(t *testing.T) {
	r := redact.New(redact.Host{}, redact.Allow{})
	registerImage(r, "registry.zz-corp.io:5000/team/agent:v1")
	got := r.Text("pulling registry.zz-corp.io:5000/team/agent:v1: lookup registry.zz-corp.io: no such host").String()
	if strings.Contains(got, "zz-corp") || strings.Contains(got, "team/agent") {
		t.Errorf("the registry survived: %s", got)
	}
}

// A dotless registry host is not registered: it would rewrite every
// "registry", registry.k8s.io included.
func TestRegisterImageLeavesADotlessRegistryHostAlone(t *testing.T) {
	r := redact.New(redact.Host{}, redact.Allow{})
	registerImage(r, "registry:5000/x")
	for _, s := range []string{"pulling registry.k8s.io/pause", "the registry said no"} {
		if got := r.Text(s).String(); got != s {
			t.Errorf("%q came back as %q", s, got)
		}
	}
	// A dotted host is registered.
	registerImage(r, "registry.zz-corp.io:5000/x")
	if got := r.Text("lookup registry.zz-corp.io: no such host").String(); strings.Contains(got, "zz-corp") {
		t.Errorf("the dotted registry host survived: %s", got)
	}
}

// A setting one segment deep is not registered: it would rewrite
// /usr/local/bin/nerdctl as <path-1>/local/bin/nerdctl.
func TestRegisterSettingPathsSkipsAShallowPath(t *testing.T) {
	for _, v := range []string{"/", "/usr", "/opt/"} {
		t.Setenv("BRIG_STATE_DIR", v)
		r := redact.New(redact.Host{}, redact.Allow{})
		registerSettingPaths(r, nil)
		for _, s := range []string{"/usr/local/bin/nerdctl", "/opt/homebrew/bin/hull"} {
			if got := r.Text(s).String(); got != s {
				t.Errorf("BRIG_STATE_DIR=%s: %q came back as %q", v, s, got)
			}
		}
	}
}
