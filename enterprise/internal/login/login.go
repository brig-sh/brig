// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

// Package login is brig login and brig logout, the commands Brig Enterprise
// adds to the command line of internal/cli.
package login

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"time"

	"github.com/brig-sh/brig/enterprise/internal/nofireauth"
	"github.com/brig-sh/brig/internal/buildinfo"
	"github.com/brig-sh/brig/internal/cli"
	"github.com/brig-sh/brig/internal/secret/login"
	"github.com/brig-sh/brig/internal/wrap"
)

// Commands are brig login and brig logout, for cli.Options.
func Commands() []cli.Command {
	return []cli.Command{{
		Verb: "login",
		Synopsis: `  brig login [URL] [--browser|--device]          log in to NOFire AI and keep
             [--ttl D]                           the token in your keychain
`,
		Usage:    loginUsage,
		Complete: completeLogin,
		Run:      loginCmd,
	}, {
		Verb: "logout",
		Synopsis: `  brig logout [URL]                              revoke the token and remove it
                                                 from your keychain
`,
		Usage: logoutUsage,
		Run:   logoutCmd,
	}}
}

// completeLogin offers brig login's flags. --ttl takes a lifetime, typed
// rather than offered; the URL is typed too.
func completeLogin(rest []string, cur string) []string {
	if len(rest) > 0 && rest[len(rest)-1] == "--ttl" {
		return nil
	}
	if strings.HasPrefix(cur, "-") {
		return []string{"--browser", "--device", "--ttl"}
	}
	return nil
}

// isTerminal reports whether stdin is a terminal, where brig can ask.
func isTerminal() bool { return wrap.IsTerminal(os.Stdin) }

// The seams the tests replace.
var (
	// openLoginStore opens the keychain service that holds login tokens.
	openLoginStore = login.Open
	// openBrowser shows the sign-in page; nil is the system's browser.
	openBrowser nofireauth.Opener
	// loginSleep waits between device polls; nil is a real timer.
	loginSleep func(context.Context, time.Duration) error
	// loginContext is the context of brig login's requests, done on Ctrl-C.
	loginContext = func() (context.Context, context.CancelFunc) {
		return signal.NotifyContext(context.Background(), os.Interrupt)
	}
)

const loginUsage = `brig login -- log in to NOFire AI

usage:
  brig login [URL] [--browser | --device] [--ttl DURATION]

Logs in to a NOFire AI deployment and keeps the token in your keychain, one
token for each deployment. The deployment is URL, else BRIG_NOFIRE_URL, else
https://my.nofire.ai. When it comes from BRIG_NOFIRE_URL, brig names it and
asks on a terminal before it contacts anything, because a repository's .envrc
can set the variable. It does the same when SSL_CERT_FILE or SSL_CERT_DIR is
set, for any deployment, and names a proxy from HTTPS_PROXY or HTTP_PROXY.

Brig opens your browser, or prints a code to enter on another device when
this host looks headless (an SSH session, or Linux with no display). With no
keychain, brig login fails before it contacts the deployment.

flags:
      --browser       sign in through a browser on this host
      --device        sign in with a code entered on another device
      --ttl DURATION  token lifetime, in h or d, such as 12h or 30d. The
                      deployment can grant less, and brig says so

brig help lists every command, every flag and every setting.
`

const logoutUsage = `brig logout -- log out of NOFire AI

usage:
  brig logout [URL]

Revokes the token on the deployment and removes it from your keychain. The
deployment is chosen as for brig login. If the deployment does not confirm
the revocation, the token is still removed here, and brig logout exits 1.
When SSL_CERT_FILE or SSL_CERT_DIR is set, brig asks first, as brig login does.

brig help lists every command, every flag and every setting.
`

// cleanedError is an error of brig login or brig logout, printed only after
// its text is cleaned. Such an error can quote a deployment, a proxy, a
// certificate or the keychain, and none of those may put a control
// character on the terminal. Unwrap keeps the class cli.ExitCode reads.
type cleanedError struct{ err error }

func (e cleanedError) Error() string { return nofireauth.CleanMessage(e.err.Error()) }
func (e cleanedError) Unwrap() error { return e.err }

func cleanError(err error) error {
	if err == nil {
		return nil
	}
	return cleanedError{err}
}

// loginCmd is brig login. The order is AUTH001 section 4: name the
// deployment, confirm one that came from the environment, open the keychain,
// and only then contact the deployment. Each step that can fail without the
// network fails before the first request, so a host that cannot keep a token
// is never issued one.
func loginCmd(out io.Writer, args []string) (err error) {
	defer func() { err = cleanError(err) }()
	arg, mode, ttl, err := loginArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		_, err = io.WriteString(out, loginUsage)
		return err
	}
	if err != nil {
		return err
	}
	d, src, err := nofireauth.ResolveDeployment(arg, os.Getenv(nofireauth.EnvURL))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Logging in to %s (%s)\n", d, src)
	printProxy(out, d)
	// The default deployment is the one a user gets with no setting at all, so
	// an .envrc that names it adds no risk. Any other deployment from the
	// environment is confirmed. Without a terminal nobody can confirm it, so
	// the login is refused: a script names the deployment on its own line.
	envURL := src == nofireauth.FromEnv && d.String() != nofireauth.DefaultURL
	if err := confirmEnvironment(out, "login", d, envURL); err != nil {
		return err
	}
	store, err := openLoginStore()
	if err != nil {
		return fmt.Errorf("brig login needs a keychain to hold the token: %w", err)
	}

	ctx, stop := loginContext()
	defer stop()
	// The first Ctrl-C ends the wait. A token exchange or a revocation
	// already under way still runs, for up to the 30 s request timeout, so
	// Ctrl-C is handed back to the default once ctx is done: a second one
	// then ends the process.
	go func() {
		<-ctx.Done()
		stop()
	}()
	c := nofireauth.NewClient(d, buildinfo.Read().Version)
	m, err := c.Discover(ctx)
	if err != nil {
		return loginFailed(ctx, err)
	}
	if mode == "" {
		mode = "browser"
		if nofireauth.Headless(os.Getenv, runtime.GOOS) {
			mode = "device"
		}
	}
	var g nofireauth.Grant
	if mode == "device" {
		g, err = nofireauth.DeviceFlow{Client: c, Meta: m, TTL: ttl, Out: out, Sleep: loginSleep}.Run(ctx)
	} else {
		open := openBrowser
		if open == nil {
			open = nofireauth.SystemOpener(runtime.GOOS)
		}
		g, err = nofireauth.BrowserFlow{Client: c, Meta: m, TTL: ttl, Open: open, Out: out}.Run(ctx)
	}
	if err != nil {
		return loginFailed(ctx, err)
	}
	if ctx.Err() != nil {
		// The interrupt came after the deployment issued the token. The
		// user asked to stop, so the token is not saved, and a token that is
		// not saved must not stay valid on the server.
		if rerr := c.Revoke(context.Background(), m, g.AccessToken); rerr != nil {
			return fmt.Errorf("login cancelled. Brig could not revoke the token the deployment issued: %s. Revoke it from the dashboard",
				nofireauth.CleanLine(rerr.Error()))
		}
		return errors.New("login cancelled. Brig revoked the token the deployment issued")
	}

	// The token this login replaces. Once the new one is saved nothing here
	// can log the old one out, so it is revoked below.
	prev, _ := store.Load(d.String())
	t := login.Token{URL: d.String(), AccessToken: g.AccessToken, ExpiresAt: time.Now().Add(g.ExpiresIn)}
	if err := store.Save(t); err != nil {
		// A token that is not stored cannot be used or logged out of, so it
		// must not stay valid on the server. Not under ctx: an interrupt
		// that arrived meanwhile must not leave it live.
		if rerr := c.Revoke(context.Background(), m, g.AccessToken); rerr != nil {
			return fmt.Errorf("cannot store the token: %w. Brig also could not revoke it, so it stays valid until it expires: %v. Revoke it from the dashboard", err, rerr)
		}
		return fmt.Errorf("cannot store the token: %w. Brig revoked the token", err)
	}
	fmt.Fprintf(out, "Logged in to %s. Token expires %s.\n", d, t.ExpiresAt.Format("2006-01-02"))
	if ttl > 0 && g.ExpiresIn < ttl {
		got, asked := lifetimes(g.ExpiresIn, ttl)
		fmt.Fprintf(out, "%s granted %s, less than the %s that --ttl asked for.\n", d, got, asked)
	}
	if prev.AccessToken != "" && prev.AccessToken != t.AccessToken {
		// Best effort: the login itself worked. Said even under -q, since the
		// note is about a credential that is still live.
		if err := c.Revoke(context.Background(), m, prev.AccessToken); err != nil {
			wrap.Stderr.Say(fmt.Sprintf("cannot revoke the earlier token for %s: %s. Revoke it from the dashboard",
				d, nofireauth.CleanLine(err.Error())))
		}
	}
	return nil
}

// loginFailed reports an interrupt as one short line rather than as the
// context error of whichever request it cut short.
func loginFailed(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return errors.New("login cancelled")
	}
	return err
}

// lifetimes formats a granted and an asked lifetime in one unit, so the two
// can be compared at a glance.
func lifetimes(got, asked time.Duration) (string, string) {
	const day = 24 * time.Hour
	switch {
	case got%day == 0 && asked%day == 0:
		return fmt.Sprintf("%dd", got/day), fmt.Sprintf("%dd", asked/day)
	case got%time.Hour == 0:
		return fmt.Sprintf("%dh", got/time.Hour), fmt.Sprintf("%dh", asked/time.Hour)
	}
	return got.String(), fmt.Sprintf("%dh", asked/time.Hour)
}

// loginArgs reads brig login's line. Flags and the URL go in either order,
// as with the secret verbs, because Parse stops at the first bare word.
func loginArgs(args []string) (url, mode string, ttl time.Duration, err error) {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	browser := fs.Bool("browser", false, "")
	device := fs.Bool("device", false, "")
	ttlText := fs.String("ttl", "", "")
	urls, err := cli.ParseInterleaved(fs, args, "--browser, --device and --ttl")
	if err != nil {
		return "", "", 0, err
	}
	switch {
	case len(urls) > 1:
		return "", "", 0, cli.Usagef("brig login takes one URL, not %q", urls[1])
	case len(urls) == 1:
		url = urls[0]
	}
	switch {
	case *browser && *device:
		return "", "", 0, cli.Usagef("--browser and --device cannot be used together. Give one of them")
	case *browser:
		mode = "browser"
	case *device:
		mode = "device"
	}
	if *ttlText != "" {
		if ttl, err = nofireauth.ParseTTL(*ttlText); err != nil {
			return "", "", 0, cli.Usagef("%v", err)
		}
	}
	return url, mode, ttl, nil
}

// logoutCmd is brig logout. The local copy is deleted whatever the
// deployment answers: the user asked to be rid of it, and a token kept
// because the server was down would outlive the logout. The exit status
// still tells a script that the token may be live.
func logoutCmd(out io.Writer, args []string) (err error) {
	defer func() { err = cleanError(err) }()
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	urls, err := cli.ParseInterleaved(fs, args, "")
	if errors.Is(err, flag.ErrHelp) {
		_, err = io.WriteString(out, logoutUsage)
		return err
	}
	if err != nil {
		return err
	}
	if len(urls) > 1 {
		return cli.Usagef("brig logout takes one URL, not %q", urls[1])
	}
	arg := ""
	if len(urls) == 1 {
		arg = urls[0]
	}
	d, _, err := nofireauth.ResolveDeployment(arg, os.Getenv(nofireauth.EnvURL))
	if err != nil {
		return err
	}
	printProxy(out, d)
	// Logout sends the token, so a trust store from the environment is
	// confirmed as for login: with its own CA, whoever set it can answer the
	// revocation, keep the token live and let Brig print "Logged out".
	if err := confirmEnvironment(out, "logout", d, false); err != nil {
		return err
	}
	store, err := openLoginStore()
	if err != nil {
		return fmt.Errorf("brig logout needs the keychain to find the token: %w", err)
	}
	t, err := store.Load(d.String())
	if errors.Is(err, login.ErrNotFound) {
		fmt.Fprintf(out, "Not logged in to %s.\n", d)
		return nil
	}
	if err != nil {
		// Without the token there is nothing to revoke, but the item can
		// still go, so a broken login does not stay stuck in the keychain.
		if derr := store.Delete(d.String()); derr != nil && !errors.Is(derr, login.ErrNotFound) {
			return fmt.Errorf("%v. Brig also could not remove it: %v. Delete the \"brig login\" item for %s from your keychain, then revoke the token from the dashboard", err, derr, d)
		}
		return fmt.Errorf("the stored login for %s is unreadable, so brig removed it without revoking the token. Revoke the token from the dashboard", d)
	}

	ctx, stop := loginContext()
	defer stop()
	// As in brig login: once Ctrl-C has ended the requests, a second one
	// ends the process.
	go func() {
		<-ctx.Done()
		stop()
	}()
	c := nofireauth.NewClient(d, buildinfo.Read().Version)
	m, revokeErr := c.Discover(ctx)
	if revokeErr == nil {
		revokeErr = c.Revoke(ctx, m, t.AccessToken)
	}
	if err := store.Delete(d.String()); err != nil && !errors.Is(err, login.ErrNotFound) {
		if revokeErr != nil {
			return fmt.Errorf("cannot remove the token for %s from the keychain: %w. %s also did not confirm the revocation: %s. Revoke it from the dashboard",
				d, err, d, nofireauth.CleanLine(revokeErr.Error()))
		}
		return fmt.Errorf("cannot remove the token for %s from the keychain: %w. Delete the \"brig login\" item for it yourself", d, err)
	}
	if revokeErr != nil {
		return fmt.Errorf("brig removed the token here, but %s did not confirm the revocation: %s. Revoke it from the dashboard",
			d, nofireauth.CleanLine(revokeErr.Error()))
	}
	fmt.Fprintf(out, "Logged out of %s.\n", d)
	return nil
}

// printProxy names the proxy the requests to d go through, if the
// environment sets one. It is a notice and not a question: TLS still runs end
// to end through a proxy, so the proxy sees where Brig connects but not what
// it sends. A trust store from the environment is different, and brig login
// confirms it.
//
// The proxy is the one net/http uses, so NO_PROXY and the rule that loopback
// is never proxied apply as they do to the requests. The variable is named
// by Go's order for the scheme: HTTPS_PROXY then https_proxy for https,
// HTTP_PROXY then http_proxy for http. Go does not read ALL_PROXY. The user
// name and password in the proxy URL are never printed.
func printProxy(out io.Writer, d nofireauth.Deployment) {
	req, err := http.NewRequest(http.MethodGet, d.String(), nil)
	if err != nil {
		return
	}
	p, err := http.ProxyFromEnvironment(req)
	if err != nil || p == nil {
		return
	}
	names := []string{"HTTPS_PROXY", "https_proxy"}
	if req.URL.Scheme == "http" {
		names = []string{"HTTP_PROXY", "http_proxy"}
	}
	from := names[0]
	for _, n := range names {
		if os.Getenv(n) != "" {
			from = n
			break
		}
	}
	shown := *p
	shown.User = nil
	fmt.Fprintf(out, "Using the proxy %s (from %s)\n", nofireauth.CleanLine(shown.String()), from)
}

// confirmEnvironment names the trust store the environment sets and, when it
// sets one or envURL holds, asks on a terminal whether to go on with verb,
// "login" or "logout", against d. Without a terminal it refuses. login and
// logout share it, so the two cannot drift apart.
//
// A trust store from the environment is confirmed for any deployment, the
// default too: with a CA of its choosing, whoever set the variable can read
// the sign-in, or the token a logout revokes, through TLS. envURL is a
// deployment named by BRIG_NOFIRE_URL that is not the default; only login
// asks about one, since logout sends a token only where it was stored for.
// Go reads SSL_CERT_FILE and SSL_CERT_DIR on Linux and not on macOS, but they
// are confirmed on every OS: the question costs little, and one rule is
// easier to state than two.
func confirmEnvironment(out io.Writer, verb string, d nofireauth.Deployment, envURL bool) error {
	var caVar string
	for _, v := range []string{"SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if val := os.Getenv(v); val != "" {
			fmt.Fprintf(out, "Trusting certificates from %s (from %s)\n", nofireauth.CleanLine(val), v)
			if caVar == "" {
				caVar = v
			}
		}
	}
	if !envURL && caVar == "" {
		return nil
	}
	if !isTerminal() {
		if envURL {
			return fmt.Errorf("%s names %s, and brig needs a terminal to confirm it. Pass the URL as an argument to log in without one", nofireauth.EnvURL, d)
		}
		return fmt.Errorf("%s is set, and brig %s needs a terminal to confirm it. Unset %s, or run brig %s from a terminal", caVar, verb, caVar, verb)
	}
	question, cancelled := "Continue logging in to %s? [y/N] ", "login cancelled. Brig sent nothing to %s"
	if verb == "logout" {
		question, cancelled = "Continue logging out of %s? [y/N] ", "logout cancelled. Brig sent nothing to %s, and the token is still stored here"
	}
	fmt.Fprintf(os.Stderr, question, d)
	line, _ := cli.ReadAnswer(os.Stdin)
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return fmt.Errorf(cancelled, d)
}
