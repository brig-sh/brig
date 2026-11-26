// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package nofireauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// browserWait is how long Brig waits for the browser when the flow sets no
// Timeout.
const browserWait = 5 * time.Minute

// Opener shows the sign-in page to the user, normally by opening path in a
// browser. path is a local HTML file that redirects to the page; see
// writeRedirectPage for why the opener never gets the page's URL.
type Opener func(path string) error

// SystemOpener returns the Opener for goos: open on macOS, xdg-open
// elsewhere. The path is one argument and no shell runs, so nothing in it is
// interpreted. It cannot be read as an option either: Run passes only an
// absolute path.
func SystemOpener(goos string) Opener {
	bin := "xdg-open"
	if goos == "darwin" {
		bin = "open"
	}
	return func(path string) error {
		return exec.Command(bin, path).Run()
	}
}

// BrowserFlow signs in with the authorization code grant and PKCE (RFC 7636)
// through a loopback redirect (RFC 8252, AUTH001 section 8.2).
type BrowserFlow struct {
	Client *Client
	Meta   Metadata
	// TTL is the token lifetime to ask for; 0 asks for none and takes the
	// deployment's default.
	TTL  time.Duration
	Open Opener
	// Out gets the lines for the user. It must be set.
	Out io.Writer
	// Timeout bounds the wait for the browser; 0 is 5 minutes.
	Timeout time.Duration
}

// callback is an authorization response with the right state. The handler
// waits on reply to tell the browser whether the sign-in worked.
type callback struct {
	query url.Values
	reply chan bool
}

// Run signs in and returns the grant. It listens on 127.0.0.1 only, so no
// other host can deliver a code, and it serves only /callback. A callback
// with the wrong state is answered and ignored: another local process cannot
// know the state, so its junk must not end the wait. The first callback with
// the right state ends it. The listener closes only when Run returns, after
// the exchange or when the flow ends.
//
// The wait ends with ctx, but the code exchange does not: by the time ctx
// ends the deployment may have issued the token, and a request cut short then
// leaves a live token that nobody holds. So a grant can arrive after ctx has
// ended. Run returns it as usual, and a caller that sees ctx.Err() revokes it.
func (f BrowserFlow) Run(ctx context.Context) (Grant, error) {
	if f.Client == nil || f.Open == nil {
		return Grant{}, errors.New("browser sign-in needs a client and an opener")
	}
	if f.Meta.Issuer == "" || f.Meta.AuthorizationEndpoint == "" || f.Meta.TokenEndpoint == "" {
		return Grant{}, errors.New("browser sign-in needs the deployment's discovered metadata")
	}
	// Checked before the port is opened: a page off the deployment's origin
	// is never shown to the user, and nothing listens for it.
	if !f.Client.d.SameOrigin(f.Meta.AuthorizationEndpoint) {
		return Grant{}, fmt.Errorf("%s sends the sign-in page to another address, and Brig refuses it. Ask its administrator to fix it", f.Client.d)
	}
	timeout := f.Timeout
	if timeout == 0 {
		timeout = browserWait
	}
	verifier, err := randomToken()
	if err != nil {
		return Grant{}, err
	}
	state, err := randomToken()
	if err != nil {
		return Grant{}, err
	}
	sum := sha256.Sum256([]byte(verifier))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Grant{}, fmt.Errorf("cannot listen on 127.0.0.1 for the browser: %w. Run brig login --device instead", err)
	}
	redirect := "http://" + ln.Addr().String() + "/callback"
	q := url.Values{
		"client_id":             {ClientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirect},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	setTTL(q, f.TTL)
	authURL, err := withQuery(f.Meta.AuthorizationEndpoint, q)
	if err != nil || !f.Client.d.SameOrigin(authURL) {
		ln.Close()
		return Grant{}, fmt.Errorf("the sign-in page address of %s is not valid. Ask its administrator to fix it", f.Client.d)
	}
	// Removed when Run returns, not when the opener does: open returns
	// before the browser has read the file. A callback means it has.
	page, removePage, err := writeRedirectPage(authURL)
	if err != nil {
		ln.Close()
		return Grant{}, fmt.Errorf("cannot write the temporary page that opens the sign-in: %w. Run brig login --device instead", err)
	}
	defer removePage()

	callbacks := make(chan callback)
	done := make(chan struct{})
	srv := &http.Server{
		Handler:           callbackHandler(state, callbacks, done),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(ln)
	}()
	defer func() {
		close(done)
		// Shutdown closes the port at once and lets a handler still
		// writing its page finish.
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if srv.Shutdown(sctx) != nil {
			srv.Close()
		}
		// A Shutdown that comes before Serve has started finds no listener to
		// close. Serve then closes it as it returns, so wait for that: the
		// port is closed when Run returns.
		<-served
	}()

	fmt.Fprintf(f.Out, "Opening %s in your browser.\n", authURL)
	fmt.Fprintln(f.Out, "If this host has no browser, run brig login --device instead")
	// The opener runs on its own goroutine: xdg-open can block until the
	// browser exits, and a test opener visits the URL itself, which needs
	// the loop below to be running.
	opened := make(chan error, 1)
	go func() { opened <- f.Open(page) }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return Grant{}, ctx.Err()
		case <-timer.C:
			return Grant{}, fmt.Errorf("no sign-in arrived from the browser within %s. Run brig login to try again", timeout)
		case err := <-opened:
			if err != nil {
				fmt.Fprintf(f.Out, "Cannot open a browser: %v. Open the URL above, or run brig login --device.\n", err)
			}
			opened = nil
		case cb := <-callbacks:
			// Not cancellable: see Run's comment. The client's request
			// timeout still bounds it.
			g, err := f.finish(context.WithoutCancel(ctx), cb.query, redirect, verifier)
			cb.reply <- err == nil
			return g, err
		}
	}
}

// finish checks the authorization response and redeems its code. iss is
// checked first, on an error response too (RFC 9207 section 2.4): without it
// Brig cannot tell that the response came from the deployment it asked, and
// a mix-up attacker's code would be sent to this deployment's token endpoint.
func (f BrowserFlow) finish(ctx context.Context, q url.Values, redirect, verifier string) (Grant, error) {
	d := f.Client.d
	switch iss := q["iss"]; {
	case len(iss) == 0:
		return Grant{}, fmt.Errorf("the sign-in response does not say that %s sent it (no iss parameter, RFC 9207), and Brig refuses it. Check the URL and run brig login again", d)
	case len(iss) > 1 || iss[0] != f.Meta.Issuer:
		return Grant{}, fmt.Errorf("the sign-in response says it came from %q, not %s (RFC 9207), and Brig refuses it. Check the URL and run brig login again", CleanLine(iss[0]), f.Meta.Issuer)
	}
	if e := q.Get("error"); e != "" {
		return Grant{}, fmt.Errorf("the sign-in was not approved: %s", Clean(d, oauthMessage(e, q.Get("error_description"))))
	}
	code := q.Get("code")
	if code == "" {
		return Grant{}, errors.New("the sign-in response has no code. Run brig login again")
	}
	return f.Client.requestToken(ctx, f.Meta, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {ClientID},
		"code_verifier": {verifier},
	})
}

// callbackHandler serves /callback. It hands the first response with the
// right state to Run and refuses every later one, so only one code is ever
// redeemed. The pages are fixed text: nothing the server sent is shown to the
// browser.
func callbackHandler(state string, callbacks chan<- callback, done <-chan struct{}) http.Handler {
	var mu sync.Mutex
	taken := false
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			resultPage(w, http.StatusBadRequest, "This sign-in response is not for this brig login. Brig is still waiting.")
			return
		}
		mu.Lock()
		first := !taken
		taken = true
		mu.Unlock()
		if !first {
			resultPage(w, http.StatusBadRequest, "Brig has already received a sign-in response.")
			return
		}
		cb := callback{query: q, reply: make(chan bool, 1)}
		select {
		case callbacks <- cb:
		case <-done:
			resultPage(w, http.StatusServiceUnavailable, "Sign-in failed. Return to the terminal for details.")
			return
		}
		select {
		case ok := <-cb.reply:
			if ok {
				resultPage(w, http.StatusOK, "Signed in. You can close this page and return to the terminal.")
			} else {
				resultPage(w, http.StatusBadRequest, "Sign-in failed. Return to the terminal for details.")
			}
		case <-r.Context().Done():
		}
	})
}

// resultPage writes a static page with no script and no external resource.
func resultPage(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>Brig</title><p>%s</p>", msg)
}

// writeRedirectPage writes a page that sends the browser to authURL, in a new
// directory only the user can read, and returns the page's absolute path and
// a function that removes the directory. The opener gets this path and not
// the URL: a command line is readable by every user of a Linux host, in
// /proc/<pid>/cmdline, and the URL carries the state and the PKCE challenge.
// With those, another user can finish the sign-in with an account of their
// own and deliver its code to the callback, and Brig would store a token for
// the wrong account.
func writeRedirectPage(authURL string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "brig-login-")
	if err != nil {
		return "", nil, err
	}
	remove := func() { os.RemoveAll(dir) }
	// Absolute, so the opener cannot read it as an option even when TMPDIR
	// is a relative path that starts with "-".
	path, err := filepath.Abs(filepath.Join(dir, "login.html"))
	if err == nil {
		err = os.WriteFile(path, redirectHTML(authURL), 0o600)
	}
	if err != nil {
		remove()
		return "", nil, err
	}
	return path, remove, nil
}

// redirectHTML is a page that sends the browser on to authURL with a meta
// refresh, which every browser follows from a file, so the page needs no
// script. The link is for a browser that has refreshes turned off. The URL is
// escaped, so a quote or an angle bracket in it cannot end the attribute.
func redirectHTML(authURL string) []byte {
	u := html.EscapeString(authURL)
	return []byte("<!doctype html><meta charset=utf-8><meta name=referrer content=no-referrer>" +
		`<meta http-equiv=refresh content="0;url=` + u + `"><title>Brig</title>` +
		`<p><a href="` + u + `">Continue to sign in</a>`)
}

// randomToken returns 32 random bytes, base64url without padding: 43
// characters, the RFC 7636 verifier length, used for the state too.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("cannot read random bytes for the sign-in: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// setTTL asks for a token lifetime of ttl in whole seconds, the unit the
// server takes. A ttl of 0 asks for none, so the deployment's default applies.
func setTTL(v url.Values, ttl time.Duration) {
	if ttl > 0 {
		v.Set("ttl", strconv.FormatInt(int64(ttl/time.Second), 10))
	}
}

// withQuery adds q to the query of endpoint, keeping any query it has.
func withQuery(endpoint string, q url.Values) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	merged := u.Query()
	for k, v := range q {
		merged[k] = v
	}
	u.RawQuery = merged.Encode()
	return u.String(), nil
}
