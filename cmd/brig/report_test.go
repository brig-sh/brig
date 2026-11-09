package main

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/secret"
	"github.com/brig-sh/brig/internal/wrap"
)

// reportRuntime knows one sandbox, running or not, and records what the
// report asked of it. Any call the report should not make panics on the nil
// embedded interface.
type reportRuntime struct {
	runtime.Runtime
	running      bool
	runningErr   error
	asked        int
	runningAsked int
}

func (r *reportRuntime) Kind() string { return "stub" }
func (r *reportRuntime) Running(string) (bool, error) {
	r.runningAsked++
	return r.running, r.runningErr
}
func (r *reportRuntime) List() ([]runtime.Instance, error) { return nil, nil }
func (r *reportRuntime) Logs(spec runtime.LogsSpec) error {
	_, err := io.WriteString(spec.Out, "console\n")
	return err
}
func (r *reportRuntime) Output(runtime.ExecSpec) (string, error) { r.asked++; return "ok\n", nil }

// reportHost is healthyHost with a secret store that fails the test when it
// is opened, and a resolver that hands back cfg and rt.
func reportHost(t *testing.T, cfg *wrap.Config, rt runtime.Runtime) {
	t.Helper()
	healthyHost(t)
	swap(t, &openStore, func() (secret.Store, error) {
		t.Error("brig report opened the secret store")
		return doctorStore{}, nil
	})
	swap(t, &reportResolve, func(string) (*wrap.Config, runtime.Runtime, string, error) {
		return cfg, rt, cfg.VMName, nil
	})
	swap(t, &reportDetect, func() (runtime.Runtime, error) {
		if rt == nil {
			return nil, errors.New("no runtime")
		}
		return rt, nil
	})
	stubBrigLs(t, "echo no sessions")
}

// stubBrigLs stands in for the brig that `brig ls` runs as. The real one
// under go test is the test binary, which would run the suite again.
func stubBrigLs(t *testing.T, body string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "brig")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	swap(t, &reportExecutable, func() (string, error) { return bin, nil })
}

func runReport(t *testing.T, args ...string) (stdout string, path string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "r.txt")
	var out bytes.Buffer
	if err := reportCmd(&out, io.Discard, append(args, "-o", path)); err != nil {
		t.Fatalf("brig report: %v", err)
	}
	return out.String(), path
}

// The report is posted in public. Its doctor section leaves the keychain
// check out, and the resolved session's store seam is closed before
// anything runs.
func TestReportNeverOpensTheStore(t *testing.T) {
	cfg := &wrap.Config{VMName: "brig-claude-code", Network: "shared"}
	reportHost(t, cfg, &reportRuntime{running: true})
	runReport(t, "claude")
	if cfg.OpenStore == nil {
		t.Fatal("the session's store seam was left unset")
	}
	if _, err := cfg.OpenStore(); !errors.Is(err, errReportReadsNoSecrets) {
		t.Errorf("OpenStore = %v, want errReportReadsNoSecrets", err)
	}
}

// The behaviour test above covers what runs today. This one covers what a
// later edit of report.go could add: a direct call into the store.
func TestReportSourceNamesNoSecretRead(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "report.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if x, ok := n.X.(*ast.Ident); ok && x.Name == "secret" {
				t.Errorf("report.go calls secret.%s", n.Sel.Name)
			}
		case *ast.Ident:
			if n.Name == "secretsCheck" || n.Name == "openStore" {
				t.Errorf("report.go names %s", n.Name)
			}
		}
		return true
	})
}

func TestReportDoctorSkipsSecrets(t *testing.T) {
	cfg := &wrap.Config{VMName: "brig-claude-code"}
	reportHost(t, cfg, &reportRuntime{})
	for _, row := range reportDoctorRows(nil) {
		if row.Name == "secrets" {
			if row.Detail != "not checked by brig report" {
				t.Errorf("secrets row = %+v", row)
			}
			return
		}
	}
	t.Error("the doctor rows have no secrets row")
}

// A ref whose sandbox is not running still gets a report, and the guest is
// asked nothing.
func TestReportWithoutSandbox(t *testing.T) {
	rt := &reportRuntime{running: false}
	reportHost(t, &wrap.Config{VMName: "brig-claude-code"}, rt)
	_, path := runReport(t, "claude")
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if rt.asked != 0 {
		t.Errorf("a stopped sandbox was asked %d questions", rt.asked)
	}
	if !strings.Contains(string(blob), "no sandbox named brig-claude-code") {
		t.Errorf("the runtime section does not say the sandbox is absent:\n%s", blob)
	}
}

func TestReportPrintsReadWarning(t *testing.T) {
	reportHost(t, &wrap.Config{VMName: "brig-claude-code"}, &reportRuntime{running: true})
	out, path := runReport(t, "claude")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if lines[len(lines)-1] != reportClosing {
		t.Errorf("last line = %q, want %q", lines[len(lines)-1], reportClosing)
	}
	if !strings.Contains(out, path) {
		t.Errorf("output does not name the file %s:\n%s", path, out)
	}
}

func TestReportArgs(t *testing.T) {
	for _, bad := range [][]string{{"--tail"}, {"--tail", "0"}, {"--tail", "x"}, {"-x"}, {"a", "b"}, {"-o"}} {
		if _, err := parseReportArgs(bad); err == nil {
			t.Errorf("parseReportArgs(%q) accepted it", bad)
		}
	}
	o, err := parseReportArgs([]string{"claude@x", "--tail", "50", "-o", "f.txt"})
	if err != nil || o.ref != "claude@x" || o.tail != 50 || o.out != "f.txt" {
		t.Errorf("parseReportArgs = %+v, %v", o, err)
	}
}

// A ref that does not resolve still gets a report of the host, which says
// why the session part is missing.
func TestReportWhenTheRefDoesNotResolve(t *testing.T) {
	reportHost(t, &wrap.Config{}, nil)
	swap(t, &reportResolve, func(string) (*wrap.Config, runtime.Runtime, string, error) {
		return nil, nil, "", errors.New("unknown profile \"nope\"")
	})
	_, path := runReport(t, "nope")
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `nope, not resolved: unknown profile "nope"`) {
		t.Errorf("the report does not say why the ref did not resolve:\n%s", blob)
	}
}

// The child `brig ls` is part of the command the user ran, so it is not
// counted as a second one.
func TestReportListsSessionsWithoutTelemetry(t *testing.T) {
	stubBrigLs(t, `echo "$1 DO_NOT_TRACK=$DO_NOT_TRACK"`)
	if got := reportSessions(); !strings.Contains(got, "ls DO_NOT_TRACK=1") {
		t.Errorf("brig ls ran as %q, want ls with DO_NOT_TRACK=1", got)
	}
}

// The file holds host logs and whatever the masking missed. Other local
// users cannot read it.
func TestReportFileIsPrivate(t *testing.T) {
	reportHost(t, &wrap.Config{VMName: "brig-claude-code"}, &reportRuntime{running: true})
	_, path := runReport(t, "claude")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("report mode %v, want no access for group or others", perm)
	}
}

// The ref is not parsed before the file is named, so whatever was typed
// stays a plain name in the current directory.
func TestReportFileName(t *testing.T) {
	now := time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)
	for ref, want := range map[string]string{
		"":          "brig-report-20261106T140000Z.txt",
		"claude":    "brig-report-claude-20261106T140000Z.txt",
		"claude@x":  "brig-report-claude-x-20261106T140000Z.txt",
		"foo/bar":   "brig-report-foo-bar-20261106T140000Z.txt",
		"../../etc": "brig-report-..-..-etc-20261106T140000Z.txt",
	} {
		if got := reportFileName(ref, now); got != want {
			t.Errorf("reportFileName(%q) = %q, want %q", ref, got, want)
		}
	}
}

// With no ref the report covers the host alone and asks no guest anything.
func TestReportWithoutRef(t *testing.T) {
	rt := &reportRuntime{running: true}
	reportHost(t, &wrap.Config{}, rt)
	swap(t, &reportResolve, func(string) (*wrap.Config, runtime.Runtime, string, error) {
		t.Error("a report with no ref resolved one")
		return nil, nil, "", errors.New("no ref")
	})
	_, path := runReport(t)
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if rt.asked != 0 {
		t.Errorf("a report of the host asked a guest %d questions", rt.asked)
	}
	for _, want := range []string{"## brig doctor", "no sessions"} {
		if !strings.Contains(string(blob), want) {
			t.Errorf("host report has no %q:\n%s", want, blob)
		}
	}
}

// The df probe and the full-tmpfs verdict ask about the profile's own
// mounts: its at: where it names one, else the path under the guest home.
func TestReportTmpfs(t *testing.T) {
	p := profile.Profile{GuestHome: "/root", Volumes: []profile.Volume{
		{Kind: profile.VolumeTmpfs, Path: ".claude", At: "/brig/claude"},
		{Kind: profile.VolumeTmpfs, Path: ".codex"},
		{Kind: profile.VolumeHostMount, Path: ".claude/projects"},
	}}
	got := reportTmpfs(p)
	if strings.Join(got, " ") != "/brig/claude /root/.codex" {
		t.Errorf("reportTmpfs = %q, want /brig/claude and /root/.codex", got)
	}
}

// A flag's value is its value even when it reads like --help, so the user
// gets the flag's own error rather than the usage text.
func TestReportHelpScanSkipsFlagValues(t *testing.T) {
	for _, args := range [][]string{{"--tail", "--help"}, {"-o", "-h"}} {
		if helpRequested("report", args) {
			t.Errorf("helpRequested(report, %q) = true, want the value read as a value", args)
		}
	}
	if !helpRequested("report", []string{"--tail", "5", "--help"}) {
		t.Error("helpRequested(report, --tail 5 --help) = false")
	}
}

// -o onto a file that is already there is refused, and the file is left as
// it was: the report never truncates or rewrites a file it did not create.
func TestReportRefusesAnExistingFile(t *testing.T) {
	reportHost(t, &wrap.Config{VMName: "brig-claude-code"}, &reportRuntime{running: true})
	path := filepath.Join(t.TempDir(), "r.txt")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reportCmd(io.Discard, io.Discard, []string{"claude", "-o", path}); err == nil {
		t.Fatal("brig report wrote over an existing file")
	}
	if blob, _ := os.ReadFile(path); string(blob) != "old\n" {
		t.Errorf("the existing file now holds %q", blob)
	}
}

// The guest can write the project, so it can plant a link where a report
// would go. The link is refused and what it points at is untouched.
func TestReportRefusesALink(t *testing.T) {
	reportHost(t, &wrap.Config{VMName: "brig-claude-code"}, &reportRuntime{running: true})
	dir := t.TempDir()
	target := filepath.Join(dir, "zshrc")
	if err := os.WriteFile(target, []byte("export PATH\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "r.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := reportCmd(io.Discard, io.Discard, []string{"claude", "-o", link}); err == nil {
		t.Fatal("brig report wrote through a link")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if blob, _ := os.ReadFile(target); string(blob) != "export PATH\n" || info.Mode().Perm() != 0o644 {
		t.Errorf("the link's target was changed: %q, mode %v", blob, info.Mode().Perm())
	}
}

// With no -o the report goes to brig's own state, which no sandbox mounts,
// and not to the current directory, which is usually the project.
func TestReportDefaultsToBrigsState(t *testing.T) {
	reportHost(t, &wrap.Config{VMName: "brig-claude-code"}, &reportRuntime{running: true})
	state := t.TempDir()
	t.Setenv("BRIG_STATE_DIR", state)
	cwd := t.TempDir()
	t.Chdir(cwd)
	var out bytes.Buffer
	if err := reportCmd(&out, io.Discard, []string{"claude"}); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(state, "host", "reports", "brig-report-claude-*.txt"))
	if len(files) != 1 || !strings.Contains(out.String(), files[0]) {
		t.Errorf("reports under the state dir: %q; output:\n%s", files, out.String())
	}
	if left, _ := os.ReadDir(cwd); len(left) != 0 {
		t.Errorf("the current directory got %d entries", len(left))
	}
	if info, err := os.Stat(filepath.Join(state, "host", "reports")); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("reports directory: %v, %v; want 0700", info, err)
	}
}

// -o into the sandbox's project works, with a warning that the agent can
// read the file there.
func TestReportWarnsInsideTheProject(t *testing.T) {
	project := t.TempDir()
	reportHost(t, &wrap.Config{VMName: "brig-claude-code", Project: project}, &reportRuntime{running: true})
	var errb bytes.Buffer
	if err := reportCmd(io.Discard, &errb, []string{"claude", "-o", filepath.Join(project, "r.txt")}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errb.String(), "the agent can read the report there") {
		t.Errorf("no warning for a report inside the project:\n%s", errb.String())
	}
}

// A mistyped -o fails before any step runs, not after every step has waited
// on a hung guest.
func TestReportFailsFastOnABadOutput(t *testing.T) {
	rt := &reportRuntime{running: true}
	reportHost(t, &wrap.Config{VMName: "brig-claude-code"}, rt)
	stubBrigLs(t, "echo ls ran >&2; exit 1")
	err := reportCmd(io.Discard, io.Discard, []string{"claude", "-o", filepath.Join(t.TempDir(), "no", "such", "r.txt")})
	if err == nil {
		t.Fatal("brig report accepted a file in a directory that does not exist")
	}
	if rt.asked != 0 || rt.runningAsked != 0 {
		t.Errorf("the runtime was asked %d questions before the file failed", rt.asked+rt.runningAsked)
	}
}

// A runtime that does not say whether the sandbox runs is not a stopped
// sandbox: the report says the state is unknown.
func TestReportRunningUnknown(t *testing.T) {
	reportHost(t, &wrap.Config{VMName: "brig-claude-code"}, &reportRuntime{runningErr: errors.New("hull: socket refused")})
	_, path := runReport(t, "claude")
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), "running: unknown, the runtime said: hull: socket refused") {
		t.Errorf("the report does not say the state is unknown:\n%s", blob)
	}
}

// The ls the report runs shows the listing and changes nothing: a stuck
// sandbox missing from the runtime's list must keep its index entry.
func TestReportListsWithoutPruning(t *testing.T) {
	stubBrigLs(t, `echo "keep=$`+lsKeepIndexEnv+`"`)
	if got := reportSessions(); !strings.Contains(got, "keep=1") {
		t.Errorf("brig ls ran as %q, want %s=1", got, lsKeepIndexEnv)
	}
}

type gatewayRuntime struct {
	reportRuntime
	log string
}

func (g *gatewayRuntime) GatewayFiles(string) (runtime.GatewayFiles, error) {
	return runtime.GatewayFiles{Log: g.log}, nil
}

// An isolated sandbox has a gateway of its own, a shared one uses the host's
// shared gateway, and an offline one has no gateway, so no gateway log is
// read for it.
func TestReportLogsFollowTheNetwork(t *testing.T) {
	reportHost(t, &wrap.Config{}, nil)
	rt := &gatewayRuntime{log: "/own/gateway.log"}
	for network, want := range map[wrap.Network]string{
		wrap.NetIsolated: "/own/gateway.log",
		wrap.NetShared:   "shared",
		wrap.NetOffline:  "",
	} {
		logs, _ := reportLogs(&wrap.Config{Network: network}, rt, "brig-claude-code")
		var got string
		for _, l := range logs {
			if l.Title == "Gateway log" {
				got = l.Path
			}
		}
		switch {
		case want == "" && got != "":
			t.Errorf("%s: gateway log %q, want none", network, got)
		case want == "shared" && (got == "" || got == rt.log):
			t.Errorf("%s: gateway log %q, want the shared gateway's", network, got)
		case want != "" && want != "shared" && got != want:
			t.Errorf("%s: gateway log %q, want %q", network, got, want)
		}
	}
}
