package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	goruntime "runtime"
	"strings"
	"syscall"

	"github.com/brig-sh/brig/internal/brigsock"
	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/secret"
	"github.com/brig-sh/brig/internal/verify"
	"github.com/brig-sh/brig/internal/wrap"
)

// brig doctor reports, one line each and in the order a boot hits them, the
// prerequisites a first run can fail on: the host OS, hardware virtualization,
// the runtime binary, its boot assets, the signature tooling, the profiles, the
// secret store, the daemon, and -- when an agent is named -- its image. A first
// run that dies at any of these surfaces the failure in the words of the layer
// underneath (a macOS host too old shows up as `dyld: missing symbol` from the
// VMM); this is the command that says which prerequisite it was, in brig's own
// words, before anything boots.
//
// The report is data first, rendered second: runDoctor returns a slice of
// checks and prints nothing, so the text form and the --json form say the same
// facts and a check is testable without capturing stdout. This is the shape
// (*Config).envelope already uses for the execution envelope; a new check
// appends to the slice and nothing downstream moves.

// checkState is the two-letter state of one check. Three, and no more: a reader
// scanning a column wants to find the failures at a glance, and two letters is
// enough to carry pass, fail and "did not run" without a legend.
type checkState string

const (
	// statePass: the prerequisite is there.
	statePass checkState = "ok"
	// stateFail: it is not, and the fix is on the next line.
	stateFail checkState = "!!"
	// stateInfo is skipped, informational, or not reached because a check it
	// depended on failed. All three are "nothing to act on here", which is why
	// they share a mark: a not-reached check must not read as a second failure
	// in the words of the missing layer, which is the whole thing doctor exists
	// to stop.
	stateInfo checkState = "--"
)

// check is one line of the report as data. It is rendered and serialised, never
// printed from inside the probe that produced it.
type check struct {
	Name    string     `json:"name"`
	State   checkState `json:"state"`
	Finding string     `json:"finding"`
	// Fix is what to type when the check failed, printed indented under the
	// finding. Empty on a check that did not fail.
	Fix string `json:"fix,omitempty"`
	// err is the typed error a failed check contributes to the exit status, or
	// nil. It is not serialised -- an exit code is not a fact about the host --
	// and only two checks set it: see the note on doctorExit.
	err error
}

// Package-level seams so a test can force a hostile host without one. The
// virtualization probe and the secret store are the two doctor reaches for that
// have no environment escape hatch of their own; detectRuntime (telemetry.go)
// and the BRIG_* variables cover the rest.
var (
	virtualization = probeVirtualization
	openStore      = secret.Open
)

// doctorCmd is `brig doctor [<agent>]`, with an optional --json flag of its own.
func doctorCmd(out io.Writer, args []string) error {
	jsonOut, agentName, err := parseDoctorArgs(args)
	if err != nil {
		return err
	}

	// Reloaded here rather than reusing the load main already did, so this
	// command holds the problems that load reported -- one line per bad file --
	// for the profiles check below. The registry it rebuilds is the same one,
	// so the lookup that follows is unaffected.
	loadErr := profile.Load(profile.Dir())

	// The operand is resolved before any check runs, so an unknown name is the
	// same notFoundError every other verb returns -- exit 3 -- rather than a
	// check that fails halfway down the report. With no operand the image check
	// is skipped and says how to run it.
	var agent *profile.Profile
	if agentName != "" {
		p, ok := profile.Lookup(agentName)
		if !ok {
			return notFoundf("unknown profile %q. `brig agent ls` lists them", agentName)
		}
		agent = &p
	}

	checks := runDoctor(agent, loadErr)

	if jsonOut {
		if err := writeJSONDocument(out, "Doctor", checks); err != nil {
			return err
		}
	} else {
		renderDoctor(out, checks)
	}
	return doctorExit(checks)
}

// parseDoctorArgs reads doctor's own flag and its one optional operand. --json
// is a flag of the verb, the way `brig policy show --json` is, rather than a
// global left of the verb: an unknown token in the global position is a usage
// error today, and moving --json there is another issue's job.
func parseDoctorArgs(args []string) (jsonOut bool, agent string, err error) {
	for _, a := range args {
		switch {
		case a == "--json":
			jsonOut = true
		case strings.HasPrefix(a, "-"):
			return false, "", usagef("unknown flag %q for `brig doctor` "+
				"(it takes an optional agent and --json)", a)
		default:
			if agent != "" {
				return false, "", usagef("`brig doctor` checks one agent's image, "+
					"not both %q and %q", agent, a)
			}
			agent = a
		}
	}
	return jsonOut, agent, nil
}

// doctorExit is the exit status for the whole report: 0 when nothing that gates
// it failed, otherwise the error of the first gating failure in chain order, of
// a type exitCode already classes.
//
// Only two checks gate the status, and both are ones a script must branch on: a
// missing runtime (exit 4) and a secret store that could not be opened (exit 6).
// Returning their own error rather than a flat "doctor found problems" is what
// gives a caller the same code the equivalent run would have -- so a wrapper
// script reads "fix the runtime" from doctor exactly as it reads it from a
// failed `brig run`, without a second table to parse. #9 asked for a flat exit
// 5; the exit-code table that landed in #104 (README around line 219) is the
// contract now, and 5 there is "verification refused", which a diagnostic that
// boots nothing never does. Every other check is diagnostic: it prints its
// finding and its fix and leaves the status alone, so `brig doctor` on a host
// that merely has no cosign yet still exits 0.
func doctorExit(checks []check) error {
	for _, c := range checks {
		if c.err != nil {
			return c.err
		}
	}
	return nil
}

// runDoctor builds the report, in boot order, and writes down the one
// dependency that matters here: the boot assets and the image are only worth
// checking once there is a runtime to boot them, so a runtime that is not there
// marks both not reached rather than failing them a second time in the words of
// whatever they would have called. The runtime's own version needs its binary,
// which is a dependency inside the runtime check rather than between two of
// them. Everything else -- the host, virtualization, the signature tooling, the
// profiles, the secret store, the daemon -- stands on its own.
func runDoctor(agent *profile.Profile, loadErr error) []check {
	rt, rtCheck := runtimeCheck()
	runtimeOK := rtCheck.State == statePass
	return []check{
		hostCheck(),
		virtualCheck(),
		rtCheck,
		bootCheck(runtimeOK),
		verifyCheck(),
		profilesCheck(loadErr),
		secretsCheck(),
		brigdCheck(),
		imageCheck(agent, runtimeOK),
	}
}

// hostCheck names the operating system and architecture a run resolves against.
// Informational: brig does not refuse a host for being what it is, and the row
// exists so a bug report carries the number the reporter would otherwise have to
// go and find.
func hostCheck() check {
	arch := goruntime.GOARCH
	if goruntime.GOOS == "darwin" {
		if v := wrap.MacOSVersion(); v != "" {
			return check{Name: "host", State: statePass, Finding: fmt.Sprintf("macOS %s on %s", v, arch)}
		}
		return check{Name: "host", State: statePass, Finding: "macOS on " + arch}
	}
	return check{Name: "host", State: statePass, Finding: fmt.Sprintf("%s on %s", goruntime.GOOS, arch)}
}

// virtualCheck reports whether the host can back a guest with a kernel of its
// own. It does not gate the exit status: brig cannot be certain the cheap probe
// is the whole story, and #9 says report the fact rather than refuse over it. So
// an unavailable answer prints !! with its fix -- the diagnostic value is the
// point -- but leaves the exit code to the checks that a script has to act on.
func virtualCheck() check {
	ok, detail := virtualization()
	if ok {
		return check{Name: "virtual", State: statePass, Finding: detail}
	}
	return check{Name: "virtual", State: stateFail, Finding: detail,
		Fix: "a sandbox needs hardware virtualization; this host reports it cannot"}
}

// runtimeCheck detects the runtime the way a run does, through the same seam,
// and reports the binary and its version. It returns the runtime so the image
// check can tell whether it was reached. A missing or broken runtime is the one
// failure here that gates the exit status -- it is exactly "fix the runtime
// before this can run", which is exit 4 -- so its error is carried through
// unchanged for exitCode to class.
func runtimeCheck() (runtime.Runtime, check) {
	rt, err := detectRuntime()
	if err != nil {
		return nil, check{Name: "runtime", State: stateFail, Finding: err.Error(),
			Fix: "install hull on macOS or nerdctl on Linux, or point BRIG_RUNTIME_BIN at a build",
			err: err,
		}
	}
	finding := fmt.Sprintf("%s at %s", rt.Kind(), rt.Bin())
	// Best-effort: a runtime that will not answer --version is still a runtime,
	// so the version is added when it is there and left out when it is not,
	// rather than failing the check over a line brig could not read.
	if v, verr := runtime.Version(rt.Bin()); verr == nil && v != "" {
		finding = fmt.Sprintf("%s %s at %s", rt.Kind(), shortVersion(v), rt.Bin())
	}
	return rt, check{Name: "runtime", State: statePass, Finding: finding}
}

// shortVersion is the version token out of a `--version` line: the last field,
// the same place hullVersionPinsDigest reads it. "hull version 0.1.0-rc23"
// becomes "0.1.0-rc23", so the row reads as a version and not as a sentence.
func shortVersion(out string) string {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return out
	}
	return fields[len(fields)-1]
}

// bootCheck reports whether the kernel and initrd an unmodified image boots on
// are already downloaded. Not reached without a runtime, since the runtime is
// what would boot them. A missing bundle is diagnostic, not a gate: a first run
// downloads it, so this names the directory and how to fill it rather than
// failing the exit status over a file that is one boot away from being there.
func bootCheck(runtimeOK bool) check {
	if !runtimeOK {
		return notReached("boot")
	}
	dir, present, err := runtime.BootAssetsDir()
	if err != nil {
		return check{Name: "boot", State: stateFail, Finding: "cannot locate the boot assets: " + err.Error(),
			Fix: "set BRIG_BOOT_ASSETS to a directory holding the kernel and initrd"}
	}
	if present {
		return check{Name: "boot", State: statePass, Finding: "assets present at " + dir}
	}
	return check{Name: "boot", State: stateFail, Finding: "assets missing at " + dir,
		Fix: "run any agent once to fetch them, or set BRIG_BOOT_ASSETS to a directory that has them"}
}

// verifyCheck names the signature tooling and the mode it runs under. It reads
// BRIG_VERIFY through the same strict parser a run does, so a typo in it is
// named here rather than swallowed. Diagnostic throughout: doctor boots
// nothing, so even require-with-no-cosign -- which would refuse every real boot
// -- is reported for the reader to act on rather than made this command's exit
// code.
func verifyCheck() check {
	mode, err := verify.ParseModeStrict(os.Getenv("BRIG_VERIFY"))
	if err != nil {
		return check{Name: "verify", State: stateFail, Finding: err.Error(),
			Fix: "set BRIG_VERIFY to off, warn or require"}
	}
	policy := verify.DefaultPolicy()
	if bin := os.Getenv("BRIG_COSIGN_BIN"); bin != "" {
		policy.Cosign = bin
	}
	switch path, ok := policy.Tooling(); {
	case ok:
		return check{Name: "verify", State: statePass,
			Finding: fmt.Sprintf("cosign at %s, BRIG_VERIFY=%s", path, mode)}
	case mode == verify.Require:
		return check{Name: "verify", State: stateFail,
			Finding: fmt.Sprintf("cosign is not installed, BRIG_VERIFY=%s", mode),
			Fix:     "install cosign (brew install cosign), or set BRIG_VERIFY=warn"}
	default:
		return check{Name: "verify", State: statePass,
			Finding: fmt.Sprintf("cosign is not installed, BRIG_VERIFY=%s (images boot unchecked)", mode)}
	}
}

// profilesCheck reports how many profiles loaded and from where, and names any
// file that would not parse. It reads the error profile.Load already collected
// -- load reports a bad file and carries on -- so a typo in a profile you are
// not using is diagnostic here rather than a gate: brig still runs the profiles
// that did load.
func profilesCheck(loadErr error) check {
	names := profile.Names()
	custom := 0
	for _, n := range names {
		if profile.IsCustom(n) {
			custom++
		}
	}
	finding := fmt.Sprintf("%d built in, %d in %s", len(names)-custom, custom, profile.Dir())
	if loadErr != nil {
		return check{Name: "profiles", State: stateFail,
			Finding: finding + "; " + oneLine(loadErr.Error()),
			Fix:     "fix or remove the profile file(s) named above"}
	}
	return check{Name: "profiles", State: statePass, Finding: finding}
}

// secretsCheck opens brig's secret store and closes it again, never reading a
// value. A platform with no store at all is informational -- brig runs without
// one -- but a store that is there and will not open is a credential failure
// this run would hit too, so it gates the exit status on the credentials code
// (6), through the same class a run's own store failure joins.
func secretsCheck() check {
	store, err := openStore()
	if err != nil {
		if errors.Is(err, secret.ErrUnsupported) {
			return check{Name: "secrets", State: stateInfo, Finding: "no secret store on this platform"}
		}
		return check{Name: "secrets", State: stateFail,
			Finding: "the secret store could not be opened: " + oneLine(err.Error()),
			Fix:     "unlock your keychain, or check that brig can reach it",
			err:     &creds.StoreUnavailableError{Cause: err}}
	}
	if c, ok := store.(io.Closer); ok {
		_ = c.Close()
	}
	return check{Name: "secrets", State: statePass, Finding: store.Kind() + " reachable"}
}

// brigdCheck reports on the optional daemon: whether its socket is there, that
// it is the invoking user's alone, and whether a daemon is holding the lock.
// Informational when the socket is absent -- brigd is optional and brig runs
// without it -- so this never gates the exit status; the one thing it will call
// out is a socket whose mode is wider than 0600, which is a lifecycle-control
// channel left readable by others.
func brigdCheck() check {
	socket, _ := brigsock.Default()
	info, err := os.Stat(socket)
	if err != nil {
		return check{Name: "brigd", State: stateInfo, Finding: "not running (no socket at " + socket + ")"}
	}
	finding := "socket at " + socket
	if running, pid := brigdHolder(socket); running {
		if pid != "" {
			finding += " (daemon pid " + pid + ")"
		} else {
			finding += " (a daemon holds the lock)"
		}
	} else {
		finding += " (no daemon holds the lock)"
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		return check{Name: "brigd", State: stateFail,
			Finding: fmt.Sprintf("%s, mode %04o", finding, mode),
			Fix:     fmt.Sprintf("chmod 600 %s -- it carries lifecycle control over sandboxes holding live credentials", socket)}
	}
	return check{Name: "brigd", State: statePass, Finding: finding}
}

// brigdHolder reports whether a daemon holds the lock on the socket, and the
// pid recorded beside it. It reads the pid from the sidecar lock file and tests
// the flock non-blocking: if the lock cannot be taken, a daemon has it; if it
// can, it is released at once and nothing did. The probe is read-only about the
// daemon -- taking and dropping a lock nobody holds changes nothing.
func brigdHolder(socket string) (running bool, pid string) {
	lockPath := socket + ".lock"
	if b, err := os.ReadFile(lockPath); err == nil {
		pid = strings.TrimSpace(string(b))
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		// No lock file, or one this user cannot open: nothing to be certain of,
		// so claim nothing.
		return false, pid
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true, pid
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, pid
}

// imageCheck resolves the named agent's image against the trust policy and
// reports the digest it booted. Skipped with a hint when no agent was named --
// brig doctor checks the host and the runtime for everyone, and one agent's
// image only when asked. Not reached without a runtime, which is what would boot
// the image. Diagnostic: a check that reaches a registry and a signature is not
// one to hang an exit code on, so a mismatch prints !! and its fix and leaves
// the status alone.
func imageCheck(agent *profile.Profile, runtimeOK bool) check {
	if agent == nil {
		return check{Name: "image", State: stateInfo,
			Finding: "pass an agent to check its image: brig doctor claude"}
	}
	if !runtimeOK {
		return notReached("image")
	}
	if agent.Image == "" {
		return check{Name: "image", State: stateInfo,
			Finding: fmt.Sprintf("no image is published for %s -- build it yourself and pass --image", agent.Name)}
	}
	policy := verify.DefaultPolicy()
	if bin := os.Getenv("BRIG_COSIGN_BIN"); bin != "" {
		policy.Cosign = bin
	}
	// No local digest: doctor has no runtime store to compare against here, and
	// the question it asks is what the registry serves for this reference, not
	// what a boot on this host already holds.
	res := policy.Verify(agent.Image, "")
	switch res.Outcome {
	case verify.Verified:
		return check{Name: "image", State: statePass,
			Finding: fmt.Sprintf("%s resolves to %s", agent.Image, res.Digest)}
	case verify.Failed, verify.Mismatch:
		return check{Name: "image", State: stateFail, Finding: oneLine(res.Message()),
			Fix: "check the image reference and your BRIG_VERIFY settings"}
	default:
		// NotOurs, NoTooling, Unresolved: nothing brig can positively verify --
		// a bring-your-own image, no cosign, or a registry it could not reach --
		// which is not a failure of the host, so it is reported rather than
		// flagged.
		return check{Name: "image", State: stateInfo, Finding: oneLine(res.Message())}
	}
}

// notReached is a check a failed prerequisite kept from running. It says so in
// those words rather than in the words of the layer it would have called, which
// is the misdiagnosis doctor is here to replace.
func notReached(name string) check {
	return check{Name: name, State: stateInfo, Finding: "not reached"}
}

// oneLine flattens a multi-line message onto one, so a check that borrows an
// error built for a paragraph -- the profile loader's list of bad files, a
// store failure -- still renders as the one line per check the report is.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// renderDoctor writes the report as aligned text: the state, the name, then the
// finding, with a failed check's fix on the next line, indented under it.
func renderDoctor(w io.Writer, checks []check) {
	for _, c := range checks {
		fmt.Fprintf(w, "  %-2s  %-8s  %s\n", c.State, c.Name, c.Finding)
		if c.State == stateFail && c.Fix != "" {
			fmt.Fprintf(w, "          %s\n", c.Fix)
		}
	}
}
