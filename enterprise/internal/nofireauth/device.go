// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package nofireauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"
	// minInterval floors the poll interval (AUTH001 section 8.2): a server
	// that advertises less, or nothing, does not get polled faster.
	minInterval = 5 * time.Second
	// deviceWait is how long Brig polls when the flow sets no MaxWait.
	deviceWait = 15 * time.Minute
)

// userCode is the shape Brig accepts for a user code. AUTH001 R4 has the
// server send 8 characters from an unambiguous alphabet; this is wider, so a
// deployment may group them with a hyphen, but it has no space.
var userCode = regexp.MustCompile(`^[A-Z0-9-]{4,16}$`)

// ErrNoDeviceFlow is returned when the deployment offers no device grant,
// for example because its administrator turned it off. Run wraps it with the
// deployment's name.
var ErrNoDeviceFlow = errors.New("the deployment does not offer sign-in with a code")

// DeviceFlow signs in with the device authorization grant (RFC 8628,
// AUTH001 section 5.3). It never opens a browser: the user opens the
// verification URI on any device.
type DeviceFlow struct {
	Client *Client
	Meta   Metadata
	// TTL is the token lifetime to ask for; 0 asks for none and takes the
	// deployment's default.
	TTL time.Duration
	// Out gets the lines for the user. It must be set.
	Out io.Writer
	// Sleep waits for d or until ctx ends. nil is a real timer; a test
	// records the waits instead.
	Sleep func(ctx context.Context, d time.Duration) error
	// MaxWait bounds the wait for the user's approval; 0 is 15 minutes.
	MaxWait time.Duration
}

type deviceResponse struct {
	DeviceCode              string          `json:"device_code"`
	UserCode                string          `json:"user_code"`
	VerificationURI         string          `json:"verification_uri"`
	VerificationURIComplete string          `json:"verification_uri_complete"`
	ExpiresIn               json.RawMessage `json:"expires_in"`
	Interval                json.RawMessage `json:"interval"`
}

// Run asks for a device code, prints where to enter it, and polls until the
// user decides, the code expires or the wait ends. The verification URIs
// are checked before anything is printed, so a hostile server cannot point
// the user at a page of its own choosing, and before the first poll.
//
// The waits end with ctx, but a poll in flight does not, for the reason
// BrowserFlow.Run gives: a grant can arrive after ctx has ended, and a caller
// that sees ctx.Err() revokes it.
func (f DeviceFlow) Run(ctx context.Context) (Grant, error) {
	if f.Client == nil {
		return Grant{}, errors.New("device sign-in needs a client")
	}
	d := f.Client.d
	if f.Meta.Issuer == "" || f.Meta.TokenEndpoint == "" {
		return Grant{}, errors.New("device sign-in needs the deployment's discovered metadata")
	}
	if f.Meta.DeviceAuthorizationEndpoint == "" {
		return Grant{}, fmt.Errorf("%w: %s. Run brig login --browser on a host with a browser", ErrNoDeviceFlow, d)
	}
	sleep := f.Sleep
	if sleep == nil {
		sleep = realSleep
	}
	limit := f.MaxWait
	if limit == 0 {
		limit = deviceWait
	}

	dr, err := f.authorize(ctx)
	if err != nil {
		return Grant{}, err
	}
	if secs, ok := wholeSeconds(dr.ExpiresIn); ok && secs > 0 && time.Duration(secs)*time.Second < limit {
		// The code is dead after this, so polling longer only waits for
		// an expired_token.
		limit = time.Duration(secs) * time.Second
	}
	interval := minInterval
	if secs, ok := wholeSeconds(dr.Interval); ok && time.Duration(secs)*time.Second > interval {
		interval = time.Duration(secs) * time.Second
	}
	if interval >= limit {
		// No poll fits in the wait. Said before the code is printed, and
		// not as "not approved within", which would blame the user for
		// the server's interval.
		return Grant{}, fmt.Errorf("%s asks brig to check for approval every %s, which does not fit in the %s that brig waits. Run brig login --browser instead", d, interval, limit)
	}

	fmt.Fprintf(f.Out, "Open %s and enter the code %s\n", CleanLine(dr.VerificationURI), CleanLine(dr.UserCode))
	if dr.VerificationURIComplete != "" {
		fmt.Fprintf(f.Out, "Or open %s\n", CleanLine(dr.VerificationURIComplete))
	}
	// The same-origin check binds only the text printed above, not where the
	// page sends the browser. A hostile deployment can redirect its page to
	// the real NOFire AI page with a code it requested there, and collect a
	// token for the user's account. The user is the only one who can see
	// where the page ended up, so Brig names the origin to look for. Both
	// URIs are on it; the deployment's form is the normalised one.
	fmt.Fprintf(f.Out, "Approve only on a page at %s://%s. If the page is anywhere else, stop.\n", d.u.Scheme, d.u.Host)

	form := url.Values{
		"grant_type":  {deviceGrantType},
		"device_code": {dr.DeviceCode},
		"client_id":   {ClientID},
	}
	var waited time.Duration
	for {
		// The wait comes first: the user cannot have approved yet. A wait
		// past the cap is not started, so Brig never outlives it.
		if waited+interval > limit {
			return Grant{}, fmt.Errorf("the code was not approved within %s. Run brig login --device again", limit)
		}
		if err := sleep(ctx, interval); err != nil {
			return Grant{}, err
		}
		waited += interval

		g, err := f.Client.requestToken(context.WithoutCancel(ctx), f.Meta, form)
		if err == nil {
			return g, nil
		}
		var oe *oauthError
		if !errors.As(err, &oe) {
			return Grant{}, err
		}
		switch oe.code {
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		case "access_denied":
			return Grant{}, fmt.Errorf("the sign-in was denied on %s. If that was a mistake, run brig login --device again", d)
		case "expired_token":
			return Grant{}, errors.New("the code expired before it was approved; run brig login --device again")
		default:
			return Grant{}, err
		}
	}
}

// authorize requests the device and user codes and checks them.
func (f DeviceFlow) authorize(ctx context.Context) (deviceResponse, error) {
	d := f.Client.d
	form := url.Values{"client_id": {ClientID}}
	setTTL(form, f.TTL)
	endpoint := f.Meta.DeviceAuthorizationEndpoint
	if !d.SameOrigin(endpoint) {
		return deviceResponse{}, fmt.Errorf("%s sends the device sign-in to another address, and Brig refuses it. Ask its administrator to fix it", d)
	}
	status, body, err := f.Client.postForm(ctx, endpoint, "device sign-in address", "request a device code", form)
	if err != nil {
		return deviceResponse{}, err
	}
	if status != http.StatusOK {
		var e tokenResponse
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			// Not an oauthError: its text says the token request was
			// refused, and this was the device authorization request.
			return deviceResponse{}, fmt.Errorf("the device code request was refused: %s", Clean(d, oauthMessage(e.Error, e.ErrorDescription)))
		}
		return deviceResponse{}, fmt.Errorf("%s answered the device code request with status %d. Run brig login --device again later", d, status)
	}
	var r deviceResponse
	// The error says nothing of the body: it holds the device code.
	if err := json.Unmarshal(body, &r); err != nil {
		return deviceResponse{}, fmt.Errorf("%s sent a device code response that is not JSON. Check the URL, or ask its administrator", d)
	}
	if r.DeviceCode == "" || r.UserCode == "" || r.VerificationURI == "" {
		return deviceResponse{}, fmt.Errorf("%s sent a device code response without a device code, a user code or a page address. Ask its administrator to fix it", d)
	}
	for _, u := range []string{r.VerificationURI, r.VerificationURIComplete} {
		if u == "" {
			continue
		}
		// Printable ASCII with no space only. A space lets the URI run on
		// into words of the server's choosing, such as a second
		// instruction, and a lookalike or full-width character makes the
		// printed URI read as another than the one the browser opens.
		if strings.IndexFunc(u, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 {
			return deviceResponse{}, fmt.Errorf("the page address for the code from %s has a space or a character that is not ASCII, and Brig refuses it. Ask its administrator to fix it", d)
		}
		// What is printed is what was checked: a URI that cleaning would
		// change is refused, not shown in another form.
		if !d.SameOrigin(u) || CleanLine(u) != u {
			return deviceResponse{}, fmt.Errorf("the page address for the code is not on %s, and Brig refuses it. A deployment must show its own page. Ask its administrator to fix it", d)
		}
	}
	// A code of any other shape is refused, not cleaned: printable text
	// padded with spaces wraps on the terminal into a forged line of its own.
	if !userCode.MatchString(r.UserCode) {
		return deviceResponse{}, fmt.Errorf("the code from %s is not 4 to 16 capital letters, digits or hyphens, and Brig refuses to show it. Ask its administrator to fix it", d)
	}
	return r, nil
}

// wholeSeconds reads a JSON integer; ok is false for anything else.
func wholeSeconds(raw json.RawMessage) (int64, bool) {
	n, err := strconv.ParseInt(string(raw), 10, 32)
	return n, err == nil
}

func realSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
