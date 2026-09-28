// Command conformance runs the network conformance suite (#264) against a
// real guest and writes a record of the run under docs/manual-tests/.
//
// It boots three sandboxes in turn through brig, one at a time: one with no
// policy, one under a default: deny policy and one under default: allow. The
// probe from test/netprobe goes in as the run's project, and each case is one
// probe run through brig sh. script/egress-conformance.sh builds both and
// runs this:
//
//	script/egress-conformance.sh
//
// It exits 0 when every case passed or is a known gap, 1 when anything
// failed, and 2 on a usage error. No runtime is a failure, never a skip. An
// interrupted run removes its sandbox and writes no record.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	os.Exit(mainErr(os.Args[1:], os.Stdout, os.Stderr))
}

func mainErr(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("conformance", flag.ContinueOnError)
	fs.SetOutput(stderr)
	brig := fs.String("brig", "", "the brig binary to test (required)")
	probe := fs.String("probe", "", "netprobe built for the guest (required)")
	runtime := fs.String("runtime", "", "the runtime brig boots, whose version the record names (default $BRIG_RUNTIME_BIN, else hull)")
	image := fs.String("image", defaultImage, "guest image; it needs curl for the proxy case")
	backend := fs.String("backend", "hvi", "backend to boot on; hvi is the only one that enforces a policy")
	record := fs.String("record", "", "where to write the record (default docs/manual-tests/egress-conformance-<backend>-<runtime version>.md)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *brig == "" || *probe == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: conformance -brig PATH -probe PATH [-runtime PATH] [-image REF] [-record PATH]")
		return 2
	}
	if *runtime == "" {
		*runtime = os.Getenv("BRIG_RUNTIME_BIN")
	}
	if *runtime == "" {
		*runtime = "hull"
	}

	ctx, stop := signal.NotifyContext(context.Background(), stopSignals...)
	defer stop()
	s := &suite{
		brig: *brig, runtime: *runtime, probe: *probe, image: *image, backend: *backend,
		exec: runCmd, now: time.Now, log: stdout,
		hostAddr: outboundAddr, lookup: lookupV4,
		hviPath: func() (string, error) { return exec.LookPath("hvi") },
		env:     os.Environ(),
	}
	return runSuite(ctx, s, *record, stdout, stderr)
}

// stopSignals end a run early. Each one cancels the run's context, so the
// sandbox is removed on the way out. SIGTERM and SIGHUP are what a process
// manager and a closed terminal send.
var stopSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

// runSuite runs s and writes the record to path, or to the record for the
// runtime's version when path is empty. An interrupted run writes nothing:
// that default path is the committed record, and a run cut short is no
// replacement for it.
func runSuite(ctx context.Context, s *suite, path string, stdout, stderr io.Writer) int {
	rep := s.do(ctx)
	if ctx.Err() != nil {
		fmt.Fprintf(stdout, "egress conformance: interrupted, no record written\n")
		return 1
	}
	if path == "" {
		path = recordPath(s.backend, rep.runtime)
	}
	result := "PASS"
	if rep.failed() {
		result = "FAIL"
	}
	for _, p := range rep.problems {
		fmt.Fprintln(stdout, "FAIL "+rep.scrub(p))
	}
	for _, r := range rep.rows {
		if r.verdict != pass {
			fmt.Fprintf(stdout, "%s %s: %s\n", r.verdict, r.c.id, rep.scrub(r.why))
		}
	}
	if path == "" {
		// No runtime version, so no record to file it under. The reason is
		// printed above.
		fmt.Fprintf(stdout, "egress conformance: %s, no record written\n", result)
		return 1
	}
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	if err == nil {
		err = os.WriteFile(path, []byte(rep.markdown()), 0o644)
	}
	if err != nil {
		fmt.Fprintf(stderr, "conformance: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "egress conformance: %s (%s), record at %s\n", result, rep.counts(), path)
	if result != "PASS" {
		return 1
	}
	return 0
}

// runCmd runs one command with no stdin, so a prompt from brig fails instead
// of waiting on a terminal nobody watches.
func runCmd(ctx context.Context, env []string, name string, args ...string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return out.String(), errb.String(), 0, nil
	case ctx.Err() != nil:
		return out.String(), errb.String(), -1, fmt.Errorf("%s: %w", name, ctx.Err())
	case errors.As(err, &exitErr):
		return out.String(), errb.String(), exitErr.ExitCode(), nil
	}
	return out.String(), errb.String(), -1, err
}
