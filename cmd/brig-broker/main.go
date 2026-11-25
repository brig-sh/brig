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
// On a Kubernetes node there is no keychain and no Secret Service, and a pod
// reaches the broker over the network. With --secret-dir the credentials are
// the files of a mounted Secret, one file per secret, and the store is
// read-only: a refreshed credential stays in memory and is logged once as not
// written back. With --listen the routes are served on a TCP address with TLS
// in place of the loopback port, and a request is bound to the sandbox
// registered with the address it came from. docs/broker.md ("Running the
// broker on a node") describes both.
//
// A platform that runs one broker per sandbox starts it with --sandbox, and
// may bind the route listener itself, somewhere only that sandbox reaches,
// and hand it over with --listen-fd. docs/broker.md ("One broker per
// sandbox") describes it.
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
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/brig-sh/brig/internal/broker"
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
	listen := fs.String("listen", "", "serve the routes on this host:port with TLS instead of on a loopback port; "+
		"the port file is neither read nor written")
	secretDir := fs.String("secret-dir", "", "read credentials from the files of this directory, one per secret, "+
		"instead of brig's secret store; the directory is read-only")
	listenFD := fs.Int("listen-fd", -1, "serve the routes with TLS on this inherited file descriptor, a bound and "+
		"listening TCP socket, as --listen serves them on its address")
	sandbox := fs.String("sandbox", "", "serve this sandbox alone: refuse a registration of any other id and a "+
		"second stored policy")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" || fs.NArg() > 0 {
		_, _ = fmt.Fprintln(stderr, "usage: brig-broker --dir <dir> [--listen <host:port> | --listen-fd <n>] [--secret-dir <dir>] [--sandbox <id>]; brig starts it, there is no need to run it by hand")
		return 2
	}
	if *listenFD >= 0 {
		if *listen != "" {
			_, _ = fmt.Fprintln(stderr, "brig-broker: --listen and --listen-fd both name the route listener; pass one")
			return 2
		}
		ln, err := inheritedListener(*listenFD)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "brig-broker:", err)
			return 2
		}
		cfg.listener = ln
	}
	if *sandbox != "" {
		if err := broker.ValidID(*sandbox); err != nil {
			_, _ = fmt.Fprintln(stderr, "brig-broker: --sandbox:", err)
			return 2
		}
		cfg.sandbox = *sandbox
	}
	// Every file the broker creates is its own: the host key, the CA key,
	// the audit log and the control socket.
	syscall.Umask(0o077)
	cfg.dir = *dir
	cfg.secretDir = *secretDir
	cfg.listen = *listen
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

// inheritedListener turns the file descriptor fd, a bound and listening TCP
// socket the parent opened, into a listener. Anything else is refused: a
// unix socket or a socket that is not listening would fail only at the first
// accept, after the platform had already registered a sandbox with it.
func inheritedListener(fd int) (net.Listener, error) {
	f := os.NewFile(uintptr(fd), "listen-fd")
	if f == nil {
		return nil, fmt.Errorf("--listen-fd %d is not an open file descriptor", fd)
	}
	defer f.Close()
	ln, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("--listen-fd %d is not a listening socket: %w", fd, err)
	}
	if _, ok := ln.(*net.TCPListener); !ok {
		_ = ln.Close()
		return nil, fmt.Errorf("--listen-fd %d is a %s socket, not TCP", fd, ln.Addr().Network())
	}
	return ln, nil
}
