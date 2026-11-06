package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/report"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/wrap"
)

// reportClosing is the last line `brig report` prints. The masking knows
// shapes, not values, so the reader is the last check before a file goes on
// a public issue.
const reportClosing = "Read the file before you post it. Tokens of known shapes were masked. Others may not be."

// errReportReadsNoSecrets is what the secret store answers inside `brig
// report`. The report is written to be posted in public, and this command
// has no reason to open the store, so the one seam that could is closed
// before anything runs.
var errReportReadsNoSecrets = errors.New("brig report reads no secrets")

func refuseStore() (creds.SecretReader, error) { return nil, errReportReadsNoSecrets }

const (
	reportDefaultTail   = 2000
	reportProbeTimeout  = 10 * time.Second
	reportDoctorTimeout = 30 * time.Second
	reportStepTimeout   = 10 * time.Second
)

type reportOptions struct {
	ref  string
	out  string
	tail int
}

// reportResolve is the seam a test replaces to resolve a ref without a real
// runtime.
var reportResolve = resolveSandbox

// reportCmd is `brig report [<ref>] [-o <file>] [--tail N]`.
func reportCmd(stdout, stderr io.Writer, args []string) error {
	o, err := parseReportArgs(args)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if o.out == "" {
		o.out = reportFileName(o.ref, now)
	}

	in := report.Input{
		Tail:         o.tail,
		ProbeTimeout: reportProbeTimeout,
		BrigVersion:  brigCheck().Finding,
	}
	in.Doctor = reportDoctorRows()
	in.Sessions = reportSessions()

	var rt runtime.Runtime
	if o.ref != "" {
		cfg, r, name, err := reportResolve(o.ref)
		if err != nil {
			// A ref that does not resolve, because the runtime or the profile
			// is broken, is often why the user is here. Write the host part
			// and say why the rest is missing.
			in.Ref, in.ResolveError = o.ref, err.Error()
			rt, _ = runtime.Detect()
			return writeReport(stdout, stderr, o.out, rt, in)
		}
		cfg.OpenStore = refuseStore
		rt = r
		in.Ref, in.Sandbox = o.ref, name
		in.Network = string(cfg.Network)
		in.Running = reportRunning(rt, name)
		in.SessionRecord = reportSessionRecord(cfg)
		var gw runtime.GatewayFiles
		in.Logs, gw = reportLogs(cfg, rt, name)
		in.GatewayPIDFile, in.GatewayAlways = gw.PID, gw.Always
	} else if r, err := runtime.Detect(); err == nil {
		rt = r
	}

	return writeReport(stdout, stderr, o.out, rt, in)
}

func writeReport(stdout, stderr io.Writer, path string, rt runtime.Runtime, in report.Input) error {
	r := report.Collect(context.Background(), rt, in, stderr)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := r.Render(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s\n%s\n", path, reportClosing)
	return nil
}

func parseReportArgs(args []string) (reportOptions, error) {
	o := reportOptions{tail: reportDefaultTail}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-o" || a == "--tail":
			if i+1 >= len(args) {
				return o, usagef("%s needs a value", a)
			}
			i++
			if a == "-o" {
				o.out = args[i]
				continue
			}
			n, err := strconv.Atoi(args[i])
			if err != nil || n <= 0 {
				return o, usagef("--tail takes a positive whole number, not %q", args[i])
			}
			o.tail = n
		case strings.HasPrefix(a, "-"):
			return o, usagef("unknown flag %q for `brig report` (it takes a ref, -o and --tail)", a)
		case o.ref != "":
			return o, usagef("`brig report` covers one session, not both %q and %q", o.ref, a)
		default:
			o.ref = a
		}
	}
	return o, nil
}

// reportFileName names the file after the ref and the time, so two reports
// of one hang do not overwrite each other. The ref's @ would need quoting in
// a shell, so it becomes a dash.
func reportFileName(ref string, now time.Time) string {
	stamp := now.Format("20060102T150405Z")
	if ref == "" {
		return "brig-report-" + stamp + ".txt"
	}
	return "brig-report-" + strings.ReplaceAll(ref, "@", "-") + "-" + stamp + ".txt"
}

// reportDoctorRows runs the doctor checks for the host, with no agent so no
// registry is contacted, and with the secrets check left out: it opens the
// keychain, which can prompt and block, and this command reads no secrets.
func reportDoctorRows() []report.Row {
	done := make(chan []check, 1)
	go func() {
		loadErr := profile.Load(profile.Dir())
		done <- doctorChecks(nil, loadErr, func() check {
			return check{Name: "secrets", State: stateInfo, Finding: "not checked by brig report"}
		})
	}()
	select {
	case checks := <-done:
		rows := make([]report.Row, 0, len(checks))
		for _, c := range checks {
			detail := c.Finding
			if c.Fix != "" {
				detail += " (fix: " + c.Fix + ")"
			}
			rows = append(rows, report.Row{State: string(c.State), Name: c.Name, Detail: detail})
		}
		return rows
	case <-time.After(reportDoctorTimeout):
		return []report.Row{{State: string(stateInfo), Name: "doctor",
			Detail: "timed out after " + reportDoctorTimeout.String()}}
	}
}

// reportSessions is what `brig ls` prints. It runs this binary as a child,
// so the listing is the one a user sees and a stuck runtime cannot hold the
// report past the deadline.
func reportSessions() string {
	self, err := os.Executable()
	if err != nil {
		return "brig ls: " + err.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), reportStepTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "ls")
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return string(out) + "\nbrig ls timed out after " + reportStepTimeout.String()
	}
	if err != nil {
		return string(out) + "\nbrig ls: " + err.Error()
	}
	return string(out)
}

func reportRunning(rt runtime.Runtime, name string) bool {
	done := make(chan bool, 1)
	go func() {
		ok, err := rt.Running(name)
		done <- err == nil && ok
	}()
	select {
	case ok := <-done:
		return ok
	case <-time.After(reportStepTimeout):
		return false
	}
}

func reportSessionRecord(cfg *wrap.Config) string {
	project := cfg.Project
	if project == "" {
		project = "(none)"
	}
	return fmt.Sprintf("home     %s\nsandbox  %s\nproject  %s\nprofile  %s\nmem      %d MB (what the next boot gets)\ncpus     %d\nnetwork  %s\n",
		cfg.Workspace, cfg.VMName, project, cfg.Profile.Name, cfg.Mem, cfg.CPUs, cfg.Network)
}

// reportLogs lists the gateway and broker logs for the sandbox, and the files
// of its own gateway when it is isolated.
func reportLogs(cfg *wrap.Config, rt runtime.Runtime, name string) ([]report.LogFile, runtime.GatewayFiles) {
	var logs []report.LogFile
	var gw runtime.GatewayFiles
	if cfg.Network == "isolated" {
		if gf, ok := rt.(runtime.GatewayFiler); ok {
			if files, err := gf.GatewayFiles(name); err == nil {
				gw = files
				logs = append(logs, report.LogFile{Title: "Gateway log", Path: files.Log})
			}
		}
	} else if path, err := runtime.GatewayLogPath(); err == nil {
		logs = append(logs, report.LogFile{Title: "Gateway log", Path: path})
	}
	if dir, err := injector.Dir(); err == nil {
		logs = append(logs,
			report.LogFile{Title: "Broker log", Path: filepath.Join(dir, injector.LogName)},
			report.LogFile{Title: "Broker audit log", Path: filepath.Join(dir, injector.AuditName), Grep: "sandbox=" + name + " "})
	}
	return logs, gw
}
