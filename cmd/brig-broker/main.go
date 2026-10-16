// Command brig-broker is the credential broker of brokered routes, one per
// host. brig starts it on the first run that needs a route, and stops it when
// the last sandbox it serves is stopped or removed.
//
// It serves internal/broker on a loopback port, the one recorded in its
// directory, and takes its orders from
// brig over a unix socket in its directory: add a sandbox's routes, mint a
// session token, remove a sandbox, report, stop. It reads the credentials a
// route injects from brig's secret store, and refreshes the ones with a
// lifecycle through internal/renew. That refresher is why this is a binary of
// its own: no package on brig's run path links it.
//
// Nothing it holds goes on a command line. The host key that signs the tokens
// is a file in its directory that only it reads. docs/broker.md describes it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/brig-sh/brig/internal/secret"
)

func main() { os.Exit(run(os.Args[1:], os.Stderr, serverConfig{openStore: secret.Open})) }

// run parses the command line and serves cfg on the directory it names until
// stopped. It returns the exit status. cfg carries everything but the
// directory: main passes brig's secret store, and the tests a memory store
// and an upstream of their own.
func run(args []string, stderr io.Writer, cfg serverConfig) int {
	fs := flag.NewFlagSet("brig-broker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "the broker directory, ~/.brig/host/broker (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" || fs.NArg() > 0 {
		_, _ = fmt.Fprintln(stderr, "usage: brig-broker --dir <dir>; brig starts it, there is no need to run it by hand")
		return 2
	}
	// Every file the broker creates is its own: the host key, the CA key,
	// the audit log and the control socket.
	syscall.Umask(0o077)
	cfg.dir = *dir
	srv, err := newServer(cfg)
	if err == errAlreadyRunning {
		_, _ = fmt.Fprintln(stderr, "brig-broker: another broker is serving", *dir)
		return 0
	}
	var held *portHeldError
	if errors.As(err, &held) {
		// brig reads this line from the log and refuses the run.
		_, _ = fmt.Fprintln(stderr, "brig-broker:", err)
		return 3
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "brig-broker:", err)
		return 1
	}
	// What goes to stderr, a panic included, follows the broker log when
	// it rotates.
	if srv.log != nil {
		srv.log.reopened = followStderr
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	// The broker has its own session, and a hangup of the terminal brig ran
	// in is not a reason to drop every route on the host.
	signal.Ignore(syscall.SIGHUP)
	if err := srv.serve(ctx); err != nil {
		_, _ = fmt.Fprintln(stderr, "brig-broker:", err)
		return 1
	}
	return 0
}
