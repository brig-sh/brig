// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

// Package recordings is what Brig Enterprise adds to a run for a person who
// signed in with brig login: it asks the deployment whether the run is
// recorded, and records the terminal output of the run when the answer is yes.
// It also adds brig recordings flush, which sends what a run left behind.
//
// # When a run is recorded
//
// Only when all of these hold, checked in this order, each before the next
// step can touch the network or the disk:
//
//   - the runtime is hull, which is the one runtime that can hand the attached
//     exec to a relay;
//   - stdin and stdout are the same terminal: a run whose input or output is a
//     pipe or a file, or whose stdin and stdout are two terminals, is not
//     recorded and runs as it does without recording;
//   - the deployment resolves as brig login resolves it with no argument:
//     BRIG_NOFIRE_URL, else the default;
//   - the person has a stored, unexpired token for exactly that deployment;
//   - the environment sets no trust store (SSL_CERT_FILE, SSL_CERT_DIR), or each
//     one it sets has the value the person accepted at brig login, which is
//     stored with the token;
//   - the deployment answers, within 3 seconds, that this run is recorded.
//
// Any other outcome is a run that goes the way it always has, with nothing
// printed, except one: a signed-in person whose trust store was not accepted
// gets one line at the start of the run. A run cannot ask a question, so it
// takes only the choices a person already made at brig login.
// BRIG_NOFIRE_URL can therefore pick among the deployments the person signed in
// to, each of which brig login confirmed. It cannot make a run send a token to a
// host the token was not stored for. A trust store from the environment is
// accepted for the same reason brig login and brig logout ask about it: with a
// certificate authority of its choosing, whoever set the variable could read the
// token in transit. login.AccessToken returns no token for a variable that was
// not accepted with exactly its current value, and names the variable instead.
//
// # The token
//
// This is the one place that reads a login token during a run. It calls
// login.AccessToken, and an architecture test pins that nothing else does. The
// token goes to the recorder as a value, in a client that sends it as a bearer
// header to the deployment's URL, path included, and follows no redirect. The recorder has
// no way to open the store. The token is not in the argv or the environment of
// the relay's child, it is not written to the spool, and it is never printed.
package recordings

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brig-sh/brig/enterprise/internal/login"
	"github.com/brig-sh/brig/enterprise/internal/nofireauth"
	"github.com/brig-sh/brig/enterprise/internal/ptyrelay"
	"github.com/brig-sh/brig/enterprise/internal/recording"
	"github.com/brig-sh/brig/internal/buildinfo"
	"github.com/brig-sh/brig/internal/cli"
	"github.com/brig-sh/brig/internal/wrap"
)

// recordedNotice is the one line a recorded run prints before it starts.
const recordedNotice = "brig: this run is recorded for your organization"

// requestTimeout is the whole of one request to the deployment.
const requestTimeout = 30 * time.Second

// The seams the tests replace. Nothing here is a switch for a person: whether
// a run is recorded is the deployment's answer, and there is no flag,
// variable, profile key or Brigfile key that changes it.
var (
	// accessToken is the token stored for a deployment, or "", and the name of
	// the trust-store variable that was not accepted at sign-in, if one stops it
	// being used. It is the one call of login.AccessToken in the module.
	accessToken = login.AccessToken
	// httpClient is the client for the deployment; nil is the standard one,
	// which never follows a redirect.
	httpClient *http.Client
	// isTerminal reports whether f is a terminal. Only a run whose stdin and
	// stdout both are one is recorded.
	isTerminal = wrap.IsTerminal
	// sameTerminal reports whether stdin and stdout are one terminal. A session
	// is one terminal: with two, the relay's single pseudo-terminal is not what
	// the command would have had, so the run is not recorded.
	sameTerminal = ptyrelay.SameTerminal
	// stateDir is brig's state directory, ~/.brig.
	stateDir = defaultStateDir
	// notice is where the lines of a recorded run go.
	notice io.Writer = os.Stderr
)

func defaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".brig")
}

// Commands is brig recordings, for cli.Options. Telemetry does not count it:
// it is not a command a person types to use brig.
func Commands() []cli.Command {
	return []cli.Command{{
		Verb: "recordings",
		Synopsis: `  brig recordings flush                          send recordings still waiting
                                                 on this machine
`,
		Usage:     recordingsUsage,
		Complete:  completeRecordings,
		Run:       recordingsCmd,
		Uncounted: true,
	}}
}

// completeRecordings offers flush, the one subcommand.
func completeRecordings(rest []string, cur string) []string {
	if len(rest) == 0 && !strings.HasPrefix(cur, "-") {
		return []string{"flush"}
	}
	return nil
}

// newClient builds the client for a deployment. base is the deployment's full
// URL, path included: the API is at base plus the recorder's route, so a
// deployment under a path is called under that path.
func newClient(base, token string) *recording.Client {
	h := httpClient
	if h == nil {
		h = nofireauth.NewHTTPClient(requestTimeout)
	}
	return &recording.Client{Base: strings.TrimSuffix(base, "/"), Token: token, HTTP: h, UserAgent: "brig/" + buildinfo.Read().Version}
}

// Attach is cli.Options.Attach. It decides from what is on this machine
// whether this run may be recorded, and touches neither the network nor the
// disk to do so. It returns nil for a run that cannot be.
//
// The relay it returns is called by the runtime once the exec's command is
// built, so a run that fails before the exec asks the deployment nothing,
// creates no file and prints nothing. The relay then asks the deployment. Any
// error, timeout or "not recorded" answer makes it return cli.ErrRelayDeclined
// before it has printed or run anything, and the run goes the way it always
// has. So an outage at the deployment cannot stop a sandbox from running.
func Attach(run cli.AttachedRun) func(argv, env []string) (int, error) {
	// Only a terminal session is recorded, and this is decided first, before
	// the token is read or anything is printed. A run whose stdin or stdout is a
	// pipe or a file, or whose stdin and stdout are two different terminals, is
	// handed over as an unrecorded run is.
	if run.Runtime != "hull" || !isTerminal(os.Stdin) || !isTerminal(os.Stdout) || !sameTerminal(os.Stdin, os.Stdout) {
		return nil
	}
	d, _, err := nofireauth.ResolveDeployment("", os.Getenv(nofireauth.EnvURL))
	if err != nil {
		return nil
	}
	token, unaccepted := accessToken(d)
	if unaccepted != "" {
		// A signed-in person whose trust store was not accepted at sign-in. The
		// relay says so once the exec is about to run, and the run goes on.
		return func(argv, env []string) (int, error) {
			fmt.Fprintf(notice, "brig: could not check whether this run is recorded: %s is set and was not accepted when you signed in. Run `brig login` to accept it\n", unaccepted)
			return 0, cli.ErrRelayDeclined
		}
	}
	if token == "" {
		return nil
	}
	state := stateDir()
	if state == "" {
		return nil
	}
	host, _ := os.Hostname()
	params := recording.Params{
		Client: newClient(d.String(), token),
		URL:    d.String(),
		Dir:    recording.Dir(state),
		// The lines a run prints when it ends go where the opening notice does.
		Notice: notice,
		// What earlier runs left is sent in the background, with the same
		// logins and clients as brig recordings flush.
		Token:     tokenForSpool,
		NewClient: clientForURL,
		Open: recording.OpenRequest{
			Sandbox: run.Sandbox, Ref: run.Ref, Profile: run.Profile, Host: printable(host),
			Runtime: run.Runtime, BrigVersion: run.Version,
			Cols: run.Cols, Rows: run.Rows,
		},
	}
	return func(argv, env []string) (int, error) {
		sess, err := recording.Begin(context.Background(), params)
		if err != nil || sess == nil {
			return 0, cli.ErrRelayDeclined
		}
		fmt.Fprintln(notice, recordedNotice)
		return sess.Run(argv, env)
	}
}

// tokenForURL is the token stored for the deployment URL a spool names, or "".
// A URL that does not parse, or that names a deployment the person never
// signed in to, has no token, so nothing is sent to it.
func tokenForURL(url string) string {
	tok, _ := tokenAndBlock(url)
	return tok
}

// tokenForSpool is tokenForURL for the flush, and whether the URL is one a login
// could ever exist for.
func tokenForSpool(url string) (string, bool) {
	if _, err := nofireauth.ParseDeployment(url); err != nil {
		return "", false
	}
	return tokenForURL(url), true
}

// tokenAndBlock is tokenForURL, and the name of the trust-store variable that
// was not accepted at sign-in when that is why there is no token.
func tokenAndBlock(url string) (token, unaccepted string) {
	d, err := nofireauth.ParseDeployment(url)
	if err != nil {
		return "", ""
	}
	return accessToken(d)
}

// clientForURL builds the client for the deployment URL a spool names.
func clientForURL(url, token string) *recording.Client {
	d, _ := nofireauth.ParseDeployment(url)
	return newClient(d.String(), token)
}

// printable keeps the printable ASCII of s, cut to 128 bytes.
func printable(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 128 {
		out = out[:128]
	}
	return out
}

const recordingsUsage = `brig recordings -- recordings of this machine's runs

usage:
  brig recordings flush   send the recordings still waiting on this machine
                          and close them

A run is recorded when you are signed in with brig login, your organization
has recording on, and it is a terminal session on hull: your stdin and stdout
are the same terminal. Brig says so before the run starts. A run that is quiet
stays recorded. A run whose input or output is a pipe or a file is not recorded
and runs as it does without recording. What the network did not take while the
run ended waits in ~/.brig/recordings until this command, or the next recorded
run's upload, sends it. A later recorded run also sends what an earlier run left, in the
background. This command also finishes a run that continued in a second
recording.

brig help lists every command, every flag and every setting.
`

// recordingsCmd runs brig recordings.
func recordingsCmd(out io.Writer, args []string) error {
	verb := "flush"
	if len(args) > 0 {
		verb = args[0]
	}
	switch verb {
	case "--help", "-h", "help":
		_, err := io.WriteString(out, recordingsUsage)
		return err
	case "flush":
		if len(args) > 1 {
			return cli.Usagef("recordings flush takes no argument")
		}
	default:
		return cli.Usagef("unknown recordings subcommand %q (flush)", verb)
	}
	state := stateDir()
	if state == "" {
		return fmt.Errorf("no home directory, so there are no recordings to send")
	}
	// A trust store from the environment that was not accepted at sign-in
	// stops the flush before it sends anything.
	if d, _, err := nofireauth.ResolveDeployment("", os.Getenv(nofireauth.EnvURL)); err == nil {
		if _, v := accessToken(d); v != "" {
			return unacceptedFlush(v)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// A spool for another deployment can need a trust store that was not
	// accepted for that one. Its token is not read, and the command says why.
	var blocked string
	res, err := recording.Flush(ctx, recording.Dir(state), func(url string) (string, bool) {
		if _, err := nofireauth.ParseDeployment(url); err != nil {
			return "", false
		}
		tok, v := tokenAndBlock(url)
		if v != "" && blocked == "" {
			blocked = v
		}
		return tok, true
	}, clientForURL)
	if err != nil {
		return err
	}
	line := fmt.Sprintf("recordings: %d sent and closed, %d waiting for a login or a live run, %d not sent yet",
		res.Closed, res.Skipped, res.Pending)
	if res.NotOurs > 0 {
		line += fmt.Sprintf(", %d waiting for the login that recorded them", res.NotOurs)
	}
	if res.Dropped > 0 {
		line += fmt.Sprintf(", %d removed without being sent", res.Dropped)
	}
	if res.Expired > 0 {
		line += fmt.Sprintf(", %d removed unsent after %d days", res.Expired, int(recording.MaxSpoolAge.Hours()/24))
	}
	fmt.Fprintln(out, line)
	if blocked != "" {
		return unacceptedFlush(blocked)
	}
	if res.AuthRefused > 0 {
		return fmt.Errorf("the deployment refused your login for %d recording(s), which stay on this machine: run `brig login` again, then `brig recordings flush`", res.AuthRefused)
	}
	return nil
}

// unacceptedFlush is the refusal of brig recordings flush when a trust store
// from the environment was not accepted when the person signed in. It names the
// variable and never its value.
func unacceptedFlush(v string) error {
	return fmt.Errorf("%s is set and was not accepted when you signed in, so brig recordings flush did not send your login token. Run `brig login` to accept it, then run `brig recordings flush` again", v)
}
