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

	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/secret"
	"github.com/brig-sh/brig/internal/wrap"
)

// reportRuntime knows one sandbox, running or not, and records what the
// report asked of it. Any call the report should not make panics on the nil
// embedded interface.
type reportRuntime struct {
	runtime.Runtime
	running bool
	asked   int
}

func (r *reportRuntime) Kind() string                      { return "stub" }
func (r *reportRuntime) Running(string) (bool, error)      { return r.running, nil }
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
	for _, row := range reportDoctorRows() {
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
