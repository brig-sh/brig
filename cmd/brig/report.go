package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/redact"
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

// reportExecutable is the brig that `brig ls` runs as. A test replaces it:
// under go test, os.Executable is the test binary, which would run the suite.
var reportExecutable = os.Executable

// reportDetect finds the runtime when there is no sandbox to resolve. A test
// replaces it so a report of the host does not list the real runtime.
var reportDetect = runtime.Detect

// reportCmd is `brig report [<ref>] [-o <file>] [--tail N]`.
func reportCmd(stdout, stderr io.Writer, args []string) error {
	o, err := parseReportArgs(args)
	if err != nil {
		return err
	}
	now := time.Now().UTC()

	in := report.Input{
		Tail:         o.tail,
		ProbeTimeout: reportProbeTimeout,
		BrigVersion:  brigCheck().Finding,
	}
	// Reloaded for the problems the load reports, as brig doctor does, and
	// here rather than in the doctor step: that step can be left behind at its
	// deadline, and a reload there would write the registry while the resolve
	// below reads it.
	loadErr := profile.Load(profile.Dir())

	// Resolved before `brig ls` runs, so the listing cannot change what the
	// resolve reads.
	var rt runtime.Runtime
	var cfg *wrap.Config
	var name string
	if o.ref != "" {
		var err error
		cfg, rt, name, err = reportResolve(o.ref)
		if err != nil {
			// A ref that does not resolve, because the runtime or the profile
			// is broken, is often why the user is here. Write the host part
			// and say why the rest is missing.
			in.Ref, in.ResolveError = o.ref, err.Error()
			cfg = nil
			rt, _ = reportDetect()
		}
	} else if r, err := reportDetect(); err == nil {
		rt = r
	}

	// The file is opened before anything is collected, so a mistyped -o
	// fails now and not after every step has waited on a hung guest.
	file, err := reportPath(o.out, o.ref, now)
	if err != nil {
		return err
	}
	if cfg != nil {
		warnIfMounted(stderr, file, cfg)
	}
	f, err := openReport(file)
	if err != nil {
		return err
	}

	in.Doctor = reportDoctorRows(loadErr)
	in.Sessions = reportSessions()
	if cfg != nil {
		cfg.OpenStore = refuseStore
		in.Ref, in.Sandbox = o.ref, name
		in.Network = string(cfg.Network)
		var timedOut bool
		in.Running, in.RunningUnknown, timedOut = reportRunning(rt, name)
		if timedOut {
			in.TimedOut++
		}
		in.SessionRecord = reportSessionRecord(cfg)
		var gw runtime.GatewayFiles
		in.Logs, gw = reportLogs(cfg, rt, name)
		in.GatewayPIDFile, in.GatewayAlways, in.GatewayOwns = gw.PID, gw.Always, gw.Owns
		in.Tmpfs = reportTmpfs(cfg.Profile)
	}
	return writeReport(stdout, stderr, f, rt, in)
}

// reportPath is where the report goes. The default is a directory of brig's
// own state that no sandbox mounts: the current directory is usually the
// project, which the guest under diagnosis can write, so it could plant a
// link at the predictable name, and could read the report there. -o naming
// a directory puts the default name in it.
func reportPath(out, ref string, now time.Time) (string, error) {
	name := reportFileName(ref, now)
	if out == "" {
		dir, err := reportDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, name), nil
	}
	if info, err := os.Stat(out); err == nil && info.IsDir() {
		return filepath.Join(out, name), nil
	}
	return out, nil
}

// reportDir is <state>/host/reports, made 0700 if it is not there.
func reportDir() (string, error) {
	host, err := runtime.HostDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(host, "reports")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("refusing %s: it is not a directory", dir)
	}
	return dir, nil
}

// openReport creates the file, 0600: it holds host logs and whatever the
// masking missed, and is for the user to read before anyone else does.
// O_EXCL refuses a file that is already there, and with O_CREATE it also
// refuses a link at that name, wherever the link points.
func openReport(file string) (*os.File, error) {
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil, usagef("%s already exists; name a new file or a directory", file)
	}
	return f, err
}

// warnIfMounted warns when the report would land where the sandbox can read
// it: its project or its home, which the guest mounts.
func warnIfMounted(w io.Writer, file string, cfg *wrap.Config) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return
	}
	abs = realDir(abs)
	for _, dir := range []string{cfg.Project, cfg.Workspace} {
		if dir == "" {
			continue
		}
		if rel, err := filepath.Rel(realDir(dir), abs); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			fmt.Fprintf(w, "brig report: warning: %s is inside %s, which the sandbox mounts; "+
				"the agent can read the report there\n", file, dir)
			return
		}
	}
}

// realDir resolves links in p's directory, so /tmp and /private/tmp on macOS
// compare equal. The file itself does not exist yet.
func realDir(p string) string {
	if dir, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(dir, filepath.Base(p))
	}
	return p
}

// writeReport collects the report into f. A report that could not be written
// whole is removed rather than left half written.
func writeReport(stdout, stderr io.Writer, f *os.File, rt runtime.Runtime, in report.Input) error {
	r := report.Collect(context.Background(), rt, in, stderr)
	var text strings.Builder
	err := r.Render(io.MultiWriter(f, &text))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	fmt.Fprintf(stdout, "wrote %s\n", f.Name())
	printSuspects(stdout, redact.Suspects(text.String()))
	fmt.Fprintln(stdout, reportClosing)
	return nil
}

// reportSuspectsShown is how many suspects are listed by line.
const reportSuspectsShown = 10

// printSuspects lists the strings left in the report that look random. The
// masking knows shapes, not values, so these are where a credential of
// another shape would be. Each is named by line and its first characters,
// never in full.
func printSuspects(w io.Writer, s []redact.Suspect) {
	if len(s) == 0 {
		return
	}
	if len(s) == 1 {
		fmt.Fprintln(w, "1 string looks random and was not masked. Check it:")
	} else {
		fmt.Fprintf(w, "%d strings look random and were not masked. Check them:\n", len(s))
	}
	for _, x := range s[:min(len(s), reportSuspectsShown)] {
		fmt.Fprintf(w, "  line %d: %s… (%d characters)\n", x.Line, x.Start, x.Len)
	}
	if len(s) > reportSuspectsShown {
		fmt.Fprintf(w, "  and %d more\n", len(s)-reportSuspectsShown)
	}
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
// of one hang do not overwrite each other. The ref is not parsed yet and may
// be wrong, which is often why the user is here, so every character that is
// not plainly safe in a file name becomes a dash: a / would make it a path.
func reportFileName(ref string, now time.Time) string {
	stamp := now.Format("20060102T150405Z")
	if ref == "" {
		return "brig-report-" + stamp + ".txt"
	}
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '-'
	}, ref)
	return "brig-report-" + safe + "-" + stamp + ".txt"
}

// reportDoctorRows runs the doctor checks for the host, with no agent so no
// registry is contacted, and with the secrets check left out: it opens the
// keychain, which can prompt and block, and this command reads no secrets.
func reportDoctorRows(loadErr error) []report.Row {
	done := make(chan []check, 1)
	go func() {
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
	self, err := reportExecutable()
	if err != nil {
		return "brig ls: " + err.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), reportStepTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "ls")
	cmd.WaitDelay = time.Second
	// The user ran one command. The child is part of it and is not counted.
	// It lists without pruning: a stuck sandbox may be missing from the
	// runtime's list at this moment, and pruning would drop its entry, and
	// with it the network it booted on, from the index the next run reads.
	cmd.Env = append(os.Environ(), "DO_NOT_TRACK=1", lsKeepIndexEnv+"=1")
	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return string(out) + "\nbrig ls timed out after " + reportStepTimeout.String()
	}
	if err != nil {
		return string(out) + "\nbrig ls: " + err.Error()
	}
	return string(out)
}

// reportRunning asks the runtime whether the sandbox runs. A runtime that
// fails or does not answer is not a stopped sandbox: the second value says
// why the state is unknown, and the third whether that was a timeout.
func reportRunning(rt runtime.Runtime, name string) (bool, string, bool) {
	type result struct {
		ok  bool
		err error
	}
	done := make(chan result, 1)
	go func() {
		ok, err := rt.Running(name)
		done <- result{ok, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			return false, "the runtime said: " + res.err.Error(), false
		}
		return res.ok, "", false
	case <-time.After(reportStepTimeout):
		return false, "the runtime did not answer within " + reportStepTimeout.String(), true
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

// reportTmpfs is where the guest sees each tmpfs the profile mounts: its at:
// when it has one, else its path under the guest home.
func reportTmpfs(p profile.Profile) []string {
	var out []string
	for _, v := range p.Tmpfs() {
		at := v.At
		if at == "" {
			at = path.Join(p.GuestHome, v.Path)
		}
		out = append(out, at)
	}
	return out
}

// reportLogs lists the gateway and broker logs for the sandbox, and the files
// of its own gateway when it is isolated. An offline sandbox has no gateway.
func reportLogs(cfg *wrap.Config, rt runtime.Runtime, name string) ([]report.LogFile, runtime.GatewayFiles) {
	var logs []report.LogFile
	var gw runtime.GatewayFiles
	switch cfg.Network {
	case wrap.NetIsolated:
		if gf, ok := rt.(runtime.GatewayFiler); ok {
			if files, err := gf.GatewayFiles(name); err == nil {
				gw = files
				logs = append(logs, report.LogFile{Title: "Gateway log", Path: files.Log})
			}
		}
	case wrap.NetShared:
		if p, err := runtime.GatewayLogPath(); err == nil {
			logs = append(logs, report.LogFile{Title: "Gateway log", Path: p})
		}
	}
	if dir, err := injector.Dir(); err == nil {
		logs = append(logs,
			report.LogFile{Title: "Broker log", Path: filepath.Join(dir, injector.LogName)},
			report.LogFile{Title: "Broker audit log", Path: filepath.Join(dir, injector.AuditName), Grep: "sandbox=" + name + " "})
	}
	return logs, gw
}
