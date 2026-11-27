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
//   - the person has a stored, unexpired token for exactly that deployment, or
//     a stored one that has expired, in which case the run pauses before the
//     command starts and signs the person in again (see "An expired sign-in");
//   - the environment sets no trust store (SSL_CERT_FILE, SSL_CERT_DIR), or each
//     one it sets has the value the person accepted at brig login, which is
//     stored with the token;
//   - the deployment answers, within 3 seconds, that this run is recorded.
//
// Any other outcome is a run that goes the way it always has, with nothing
// printed, except two: a signed-in person whose trust store was not accepted
// gets one line at the start of the run, and a person whose sign-in expired
// gets the pause below. Only that pause asks a question, the ones brig login
// asks, because it is the sign-in. Every other run takes only the choices a
// person already made at brig login.
// BRIG_NOFIRE_URL can therefore pick among the deployments the person signed in
// to, each of which brig login confirmed. It cannot make a run send a token to a
// host the token was not stored for. A trust store from the environment is
// accepted for the same reason brig login and brig logout ask about it: with a
// certificate authority of its choosing, whoever set the variable could read the
// token in transit. login.AccessToken returns no token for a variable that was
// not accepted with exactly its current value, and names the variable instead.
//
// # An expired sign-in
//
// A run that passes the rules above up to the deployment, and whose stored
// sign-in has expired, is not run unrecorded without a word. Its relay is
// called before the command starts, so it prints one line, runs the sign-in of
// brig login for that deployment (login.SignIn: the same flow, the same
// questions about a trust store, the same storage), reads the token again and
// goes on as a signed-in run does. A sign-in that fails, times out or is ended
// with Ctrl-C prints its error, then one line, and the run goes unrecorded.
// Everything the pause prints goes to stderr, or to stdout when stderr is not
// the terminal the run uses, so the person always sees it. Ctrl-C ends the
// sign-in only. A run that cannot be recorded for the local rules above never
// pauses and never starts a sign-in.
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
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
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

// flushKeyringWait is how long brig recordings flush waits for the keychain to
// give up a login. The person asked for the flush and a locked keychain can ask
// for a password, so it waits longer than a run does.
const flushKeyringWait = 30 * time.Second

// requestTimeout is the whole of one request to the deployment.
const requestTimeout = 30 * time.Second

// The seams the tests replace. Nothing here is a switch for a person: whether
// a run is recorded is the deployment's answer, and no setting exists to turn
// recording on or off. A deployment the person is not signed in to, --json,
// brig exec and a runtime other than hull exist for other reasons, and each
// leaves a run unrecorded.
var (
	// accessToken is the token stored for a deployment, or "", the name of
	// the trust-store variable that was not accepted at sign-in, if one stops it
	// being used, and whether a stored sign-in has expired. It is the one call
	// of login.AccessToken in the module.
	accessToken = login.AccessToken
	// signIn is brig login's sign-in for a deployment. It is the one way a run
	// writes the login store, and only for a sign-in that has expired.
	signIn = login.SignIn
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
	// stateDir is brig's state directory, ~/.brig or BRIG_STATE_DIR.
	stateDir = defaultStateDir
	// notice is where the lines of a recorded run go.
	notice io.Writer = os.Stderr
)

// defaultStateDir is brig's state directory as the rest of brig resolves it:
// BRIG_STATE_DIR, else ~/.brig. The spool goes below it, where
// wrap.CheckProjectState keeps every project out, so a sandbox that mounts a
// project cannot write it.
func defaultStateDir() string {
	dir, err := wrap.StateDir()
	if err != nil {
		return ""
	}
	return dir
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
// whether this run may be recorded. It touches neither the network nor the
// disk. It reads the login from the keychain, which it waits for at most
// login.KeyringWait, and a keychain that does not answer in that time leaves the
// run unrecorded with one line. It returns nil for a run that cannot be.
//
// The relay it returns is called by the runtime once the exec's command is
// built, so a run that fails before the exec asks the deployment nothing,
// creates no file and prints nothing. The relay then asks the deployment. Any
// error, timeout or "not recorded" answer makes it return cli.ErrRelayDeclined
// before it has run the command, and the run goes the way it always has. So an
// outage at the deployment cannot stop a sandbox from running.
//
// For a stored sign-in that has expired, the relay first prints a line and
// signs the person in again, which can take minutes and is the one thing a
// relay does before the deployment is asked. The command has not started, so
// the pause costs the run nothing but time. If the sign-in does not finish, the
// relay prints the sign-in's error, then one line, and declines.
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
	token, unaccepted, timedOut, expired := accessToken(d, login.KeyringWait)
	if timedOut {
		// The keychain did not answer, for example a locked one waiting on an
		// unlock prompt nobody sees. Say so when the exec is about to run.
		return func(argv, env []string) (int, error) { return 0, declineKeyring(notice) }
	}
	if unaccepted != "" {
		// A signed-in person whose trust store was not accepted at sign-in. The
		// relay says so once the exec is about to run, and the run goes on.
		return func(argv, env []string) (int, error) { return 0, declineUnaccepted(notice, unaccepted) }
	}
	if token == "" && !expired {
		return nil
	}
	state := stateDir()
	if state == "" {
		return nil
	}
	if expired {
		// The sign-in ran out. Pause before the command starts and sign the
		// person in again, so the run can be recorded.
		return func(argv, env []string) (int, error) {
			w := pauseWriter()
			fmt.Fprintf(w, "brig: your sign-in to %s expired. Signing you in again so this run can be recorded. Ctrl-C runs it without recording\n", d)
			if err := signIn(w, d); err != nil {
				return 0, declineSignIn(w, err)
			}
			tok, unaccepted, timedOut, _ := accessToken(d, login.KeyringWait)
			switch {
			case timedOut:
				return 0, declineKeyring(w)
			case unaccepted != "":
				return 0, declineUnaccepted(w, unaccepted)
			case tok == "":
				return 0, declineSignIn(w, nil)
			}
			return record(run, d, tok, state)(argv, env)
		}
	}
	return record(run, d, token, state)
}

// pauseWriter is where the pause for an expired sign-in prints: the lines of
// the pause, everything the sign-in prints and asks, and the line when it did
// not finish. It is notice, except when notice is a file that is not the
// terminal the run uses: stderr sent to /dev/null or a log, or to another
// terminal. The sign-in reads its answers from the run's terminal, so the
// person must see the pause there. It goes to stdout then, which Attach checked
// is that terminal.
func pauseWriter() io.Writer {
	if f, ok := notice.(*os.File); ok && !(isTerminal(f) && sameTerminal(os.Stdin, f)) {
		return os.Stdout
	}
	return notice
}

// declineSignIn is the lines of a run whose sign-in did not finish, and the
// decline that hands the run over unrecorded. When the sign-in returned an
// error, its text comes first, cleaned as brig login cleans it: it can say that
// a token the deployment issued is still valid and must be revoked from the
// dashboard, which the person must not miss. The text holds no token.
func declineSignIn(w io.Writer, err error) error {
	if err != nil {
		fmt.Fprintf(w, "brig: %s\n", nofireauth.CleanMessage(err.Error()))
	}
	fmt.Fprintln(w, "brig: the sign-in did not finish, so this run is not recorded. Run `brig login` to sign in")
	return cli.ErrRelayDeclined
}

// declineKeyring is the line of a run whose keychain did not answer.
func declineKeyring(w io.Writer) error {
	fmt.Fprintf(w, "brig: could not read your login within %d seconds, so this run is not recorded\n", int(login.KeyringWait/time.Second))
	return cli.ErrRelayDeclined
}

// declineUnaccepted is the line of a run whose trust store was not accepted.
func declineUnaccepted(w io.Writer, v string) error {
	fmt.Fprintf(w, "brig: could not check whether this run is recorded: %s is set and was not accepted when you signed in. Run `brig login` to accept it\n", v)
	return cli.ErrRelayDeclined
}

// record is the relay of a run that has a token for d: it asks the deployment
// and records when the deployment says so.
func record(run cli.AttachedRun, d nofireauth.Deployment, token, state string) func(argv, env []string) (int, error) {
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
		code, err := sess.Run(argv, env)
		if errors.Is(err, ptyrelay.ErrNotStarted) {
			// Nothing ran. The session closed its recording and said the run is
			// not recorded after all, and the runtime runs the command as an
			// unrecorded run does.
			return 0, cli.ErrRelayDeclined
		}
		return code, err
	}
}

// tokenForURL is the token stored for the deployment URL a spool names, or "".
// A URL that does not parse, or that names a deployment the person never
// signed in to, has no token, so nothing is sent to it.
func tokenForURL(url string) string {
	tok, _ := tokenAndBlock(url, login.KeyringWait)
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
// It waits at most wait for the keychain.
func tokenAndBlock(url string, wait time.Duration) (token, unaccepted string) {
	d, err := nofireauth.ParseDeployment(url)
	if err != nil {
		return "", ""
	}
	token, unaccepted, _, _ = accessToken(d, wait)
	return token, unaccepted
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
run ended waits in the recordings directory of brig's state directory
(~/.brig, or BRIG_STATE_DIR) until this command, or the next recorded
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
		if _, v, _, _ := accessToken(d, flushKeyringWait); v != "" {
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
		tok, v := tokenAndBlock(url, flushKeyringWait)
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
