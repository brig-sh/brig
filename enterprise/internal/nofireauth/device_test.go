// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package nofireauth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brig-sh/brig/enterprise/internal/nofireauth"
	"github.com/brig-sh/brig/enterprise/internal/nofireauth/fakeas"
)

// recorder is a Sleep that waits for nothing and remembers each request.
type recorder struct{ sleeps []time.Duration }

func (r *recorder) sleep(ctx context.Context, d time.Duration) error {
	r.sleeps = append(r.sleeps, d)
	return ctx.Err()
}

// deviceFake starts a fake whose own interval check never fires: the polls
// of a test arrive with no delay, so the scripted Polls drive the sequence.
// The fake still advertises interval 1, which the client must floor at 5 s.
func deviceFake(t *testing.T, o fakeas.Options) *fakeas.Server {
	t.Helper()
	o.Interval = time.Nanosecond
	return start(t, o)
}

func deviceFlow(t *testing.T, base string, rec *recorder) (nofireauth.DeviceFlow, *bytes.Buffer) {
	t.Helper()
	c := client(t, base)
	m, err := c.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	return nofireauth.DeviceFlow{Client: c, Meta: m, Out: out, Sleep: rec.sleep}, out
}

func TestDeviceFlowSignsIn(t *testing.T) {
	s := deviceFake(t, fakeas.Options{Polls: []string{"authorization_pending", "authorization_pending"}})
	rec := &recorder{}
	f, out := deviceFlow(t, s.URL, rec)
	g, err := f.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if issued := s.Issued(); len(issued) != 1 || g.AccessToken != issued[0] || g.ExpiresIn != 7*24*time.Hour {
		t.Errorf("grant = %+v, issued %d", g, len(issued))
	}
	if want := []time.Duration{5 * time.Second, 5 * time.Second, 5 * time.Second}; !slices.Equal(rec.sleeps, want) {
		t.Errorf("sleeps = %v, want %v", rec.sleeps, want)
	}
	got := out.String()
	if !strings.Contains(got, "Open "+s.URL+"/device and enter the code ") || strings.Contains(got, g.AccessToken) {
		t.Errorf("out = %q", got)
	}
	if strings.Contains(got, "said:") {
		t.Errorf("out has the error prefix: %q", got)
	}
}

func TestDeviceFlowPrintsCompleteURI(t *testing.T) {
	const path = "/device?user_code=ABCDEFGH"
	s := deviceFake(t, fakeas.Options{VerificationURIComplete: path})
	complete := s.URL + path
	f, out := deviceFlow(t, s.URL, &recorder{})
	if _, err := f.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), complete) {
		t.Errorf("out = %q, want the complete URI", out.String())
	}
}

func TestIntervalFlooredAtFive(t *testing.T) {
	s := deviceFake(t, fakeas.Options{})
	rec := &recorder{}
	f, _ := deviceFlow(t, s.URL, rec)
	if _, err := f.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{5 * time.Second}; !slices.Equal(rec.sleeps, want) {
		t.Errorf("sleeps = %v, want %v", rec.sleeps, want)
	}
}

func TestSlowDownAddsFive(t *testing.T) {
	s := deviceFake(t, fakeas.Options{Polls: []string{"slow_down", "authorization_pending"}})
	rec := &recorder{}
	f, _ := deviceFlow(t, s.URL, rec)
	if _, err := f.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 10 * time.Second}
	if !slices.Equal(rec.sleeps, want) {
		t.Errorf("sleeps = %v, want %v", rec.sleeps, want)
	}
}

func TestDeniedAndExpiredEnd(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    fakeas.Options
	}{
		{"access_denied", fakeas.Options{Polls: []string{"access_denied"}}},
		{"expired_token", fakeas.Options{Polls: []string{"expired_token"}}},
		{"denied on the page", fakeas.Options{Approval: fakeas.Deny}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := deviceFake(t, tc.o)
			rec := &recorder{}
			f, out := deviceFlow(t, s.URL, rec)
			_, err := f.Run(context.Background())
			if err == nil {
				t.Fatal("Run succeeded")
			}
			if len(s.Issued()) != 0 {
				t.Errorf("issued %d tokens", len(s.Issued()))
			}
			if len(rec.sleeps) != 1 {
				t.Errorf("sleeps = %v, want one: the flow must stop at the refusal", rec.sleeps)
			}
			if strings.Contains(out.String(), "Bearer") || strings.Contains(out.String(), "access_token") {
				t.Errorf("out shows a token: %q", out.String())
			}
		})
	}
}

func TestOffOriginVerificationURIRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    fakeas.Options
	}{
		{"verification_uri", fakeas.Options{VerificationURI: "https://evil.example/device"}},
		{"verification_uri_complete", fakeas.Options{VerificationURIComplete: "https://evil.example/device?c=1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := deviceFake(t, tc.o)
			rec := &recorder{}
			f, out := deviceFlow(t, s.URL, rec)
			_, err := f.Run(context.Background())
			if err == nil {
				t.Fatal("Run succeeded")
			}
			if len(rec.sleeps) != 0 || slices.Contains(s.Requests(), "POST /oauth/token") {
				t.Errorf("polled: sleeps %v, requests %q", rec.sleeps, s.Requests())
			}
			if strings.Contains(out.String(), "evil.example") {
				t.Errorf("printed the off-origin URI: %q", out.String())
			}
		})
	}
}

// The prompt names the origin the approval page must be on: the check of
// the verification URI binds only the text Brig prints, not where that page
// redirects the browser.
func TestDevicePromptNamesTheOrigin(t *testing.T) {
	s := deviceFake(t, fakeas.Options{})
	f, out := deviceFlow(t, s.URL, &recorder{})
	if _, err := f.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	want := "Approve only on a page at " + s.URL + ". If the page is anywhere else, stop."
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "Open ") || lines[1] != want {
		t.Errorf("out = %q, want the line %q after the code", out.String(), want)
	}
}

// hostileDevice serves metadata and a device authorization response with the
// user code and verification URIs a test chooses. "{base}" in a value is the
// server's URL. It counts the polls of its token endpoint.
func hostileDevice(t *testing.T, fields map[string]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var polls atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                                         srv.URL,
				"authorization_endpoint":                         srv.URL + "/authorize",
				"token_endpoint":                                 srv.URL + "/token",
				"device_authorization_endpoint":                  srv.URL + "/device_authorization",
				"code_challenge_methods_supported":               []string{"S256"},
				"authorization_response_iss_parameter_supported": true,
			})
		case "/device_authorization":
			resp := map[string]any{"device_code": "dc", "expires_in": 60, "interval": 5}
			for k, v := range fields {
				resp[k] = strings.ReplaceAll(v, "{base}", srv.URL)
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "/token":
			polls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"access_denied"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &polls
}

// A user code outside [A-Z0-9-]{4,16} and a verification URI with anything
// but printable ASCII and no space are refused before anything is printed and before the first poll. A
// code padded with spaces would wrap into a forged instruction of its own.
func TestDeviceResponseShapeRefused(t *testing.T) {
	forged := "ABCD" + strings.Repeat(" ", 80) + "Open https://my.nofire.ai/device and enter the code WXYZ-QRST"
	for name, fields := range map[string]map[string]string{
		"padded code":           {"user_code": forged, "verification_uri": "{base}/device"},
		"lower case code":       {"user_code": "abcd-efgh", "verification_uri": "{base}/device"},
		"short code":            {"user_code": "ABC", "verification_uri": "{base}/device"},
		"long code":             {"user_code": "ABCDEFGHJKLMNPQRS", "verification_uri": "{base}/device"},
		"code with a space":     {"user_code": "ABCD EFGH", "verification_uri": "{base}/device"},
		"non-ASCII code":        {"user_code": "ABCDÉFGH", "verification_uri": "{base}/device"},
		"URI with a space":      {"user_code": "ABCDEFGH", "verification_uri": "{base}/device and enter the code WXYZQRST"},
		"complete URI, a space": {"user_code": "ABCDEFGH", "verification_uri": "{base}/device", "verification_uri_complete": "{base}/device?c=ABCDEFGH x"},
		"full-width in path":    {"user_code": "ABCDEFGH", "verification_uri": "{base}/dｅvice"},
		"no-break space in URI": {"user_code": "ABCDEFGH", "verification_uri": "{base}/device\u00a0x"},
		"Cyrillic in query":     {"user_code": "ABCDEFGH", "verification_uri": "{base}/device", "verification_uri_complete": "{base}/device?c=ABCDEFGН"},
	} {
		t.Run(name, func(t *testing.T) {
			srv, polls := hostileDevice(t, fields)
			rec := &recorder{}
			f, out := deviceFlow(t, srv.URL, rec)
			_, err := f.Run(context.Background())
			if err == nil {
				t.Fatal("Run succeeded")
			}
			if out.Len() != 0 || len(rec.sleeps) != 0 || polls.Load() != 0 {
				t.Errorf("printed %q, slept %v, polled %d times; want none", out.String(), rec.sleeps, polls.Load())
			}
		})
	}
	for _, code := range []string{"ABCD", "WDJB-MJHT", "0123456789ABCDEF"} {
		srv, _ := hostileDevice(t, map[string]string{"user_code": code, "verification_uri": "{base}/device"})
		f, out := deviceFlow(t, srv.URL, &recorder{})
		_, _ = f.Run(context.Background())
		if !strings.Contains(out.String(), "enter the code "+code+"\n") {
			t.Errorf("code %q: out = %q, want it printed", code, out.String())
		}
	}
}

func TestNoDeviceEndpoint(t *testing.T) {
	s := deviceFake(t, fakeas.Options{NoDevice: true})
	f, _ := deviceFlow(t, s.URL, &recorder{})
	_, err := f.Run(context.Background())
	if !errors.Is(err, nofireauth.ErrNoDeviceFlow) {
		t.Fatalf("err = %v, want ErrNoDeviceFlow", err)
	}
	if !strings.Contains(err.Error(), s.URL) {
		t.Errorf("err = %q does not name the deployment %s", err, s.URL)
	}
}

func TestMaxWait(t *testing.T) {
	polls := slices.Repeat([]string{"authorization_pending"}, 50)
	s := deviceFake(t, fakeas.Options{Polls: polls})
	rec := &recorder{}
	f, _ := deviceFlow(t, s.URL, rec)
	f.MaxWait = 20 * time.Second
	_, err := f.Run(context.Background())
	if err == nil || len(s.Issued()) != 0 {
		t.Fatalf("err = %v, issued %d", err, len(s.Issued()))
	}
	var total time.Duration
	for _, d := range rec.sleeps {
		total += d
	}
	if total > 20*time.Second || len(rec.sleeps) != 4 {
		t.Errorf("sleeps = %v: waited %s past a 20s cap", rec.sleeps, total)
	}
}

// A server interval that leaves no poll within the wait is reported as that,
// not as a code the user did not approve in time.
func TestIntervalAtLeastTheWaitRefused(t *testing.T) {
	// start, not deviceFake: the fake must advertise 20 minutes.
	s := start(t, fakeas.Options{Interval: 20 * time.Minute})
	rec := &recorder{}
	f, out := deviceFlow(t, s.URL, rec)
	_, err := f.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "20m0s") || strings.Contains(err.Error(), "not approved") {
		t.Fatalf("err = %v, want one naming the 20m0s interval", err)
	}
	if len(rec.sleeps) != 0 || strings.Contains(out.String(), "enter the code") {
		t.Errorf("slept %v, printed %q; want neither", rec.sleeps, out)
	}
}

// Ctrl-C after the deployment issued the token on a poll, while its answer
// is still on the way: the poll is not cut short, and Run returns the token
// so the caller can revoke it.
func TestPollOutlivesCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newTokenProxy(t, func(w http.ResponseWriter, fake *httptest.ResponseRecorder) {
		cancel()
		relay(w, fake)
	})
	s := p.front(t, fakeas.Options{Interval: time.Nanosecond, VerificationURI: p.URL + "/device"})
	f, _ := deviceFlow(t, p.URL, &recorder{})
	g, err := f.Run(ctx)
	if ctx.Err() == nil {
		t.Fatalf("no poll was made: %v", err)
	}
	issued := s.Issued()
	if err != nil || len(issued) != 1 || g.AccessToken != issued[0] {
		t.Fatalf("token returned: %v, err = %v; issued %d: want the issued token back", g.AccessToken != "", err, len(issued))
	}
}

func TestDeviceCancel(t *testing.T) {
	s := deviceFake(t, fakeas.Options{Approval: fakeas.Page})
	ctx, cancel := context.WithCancel(context.Background())
	c := client(t, s.URL)
	m, err := c.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f := nofireauth.DeviceFlow{Client: c, Meta: m, Out: &bytes.Buffer{}, Sleep: func(ctx context.Context, d time.Duration) error {
		cancel()
		return ctx.Err()
	}}
	_, err = f.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if len(s.Issued()) != 0 {
		t.Errorf("issued %d tokens", len(s.Issued()))
	}
}

func TestDeviceSendsTTL(t *testing.T) {
	s := deviceFake(t, fakeas.Options{})
	f, _ := deviceFlow(t, s.URL, &recorder{})
	f.TTL = 36 * time.Hour
	g, err := f.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := s.RequestedTTL(); !slices.Equal(got, []int{36 * 3600}) {
		t.Errorf("ttl = %v, want [129600]", got)
	}
	if g.ExpiresIn != 36*time.Hour {
		t.Errorf("ExpiresIn = %s", g.ExpiresIn)
	}
}

func sum(ds []time.Duration) (t time.Duration) {
	for _, d := range ds {
		t += d
	}
	return t
}

func TestServerIntervalAboveFiveIsHonoured(t *testing.T) {
	// start, not deviceFake: the fake must advertise 7 s.
	s := start(t, fakeas.Options{Interval: 7 * time.Second})
	rec := &recorder{}
	f, _ := deviceFlow(t, s.URL, rec)
	if _, err := f.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{7 * time.Second}; !slices.Equal(rec.sleeps, want) {
		t.Errorf("sleeps = %v, want %v", rec.sleeps, want)
	}
}

func TestSuccessiveSlowDownsAccumulate(t *testing.T) {
	s := deviceFake(t, fakeas.Options{Polls: []string{"slow_down", "slow_down", "authorization_pending"}})
	rec := &recorder{}
	f, _ := deviceFlow(t, s.URL, rec)
	if _, err := f.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 15 * time.Second, 15 * time.Second}
	if !slices.Equal(rec.sleeps, want) {
		t.Errorf("sleeps = %v, want %v", rec.sleeps, want)
	}
}

func TestDeviceCodeExpiryCapsWait(t *testing.T) {
	polls := slices.Repeat([]string{"authorization_pending"}, 50)
	s := deviceFake(t, fakeas.Options{Polls: polls, DeviceExpiresIn: 8})
	rec := &recorder{}
	f, _ := deviceFlow(t, s.URL, rec) // MaxWait stays at its 15 minutes
	_, err := f.Run(context.Background())
	if err == nil || len(s.Issued()) != 0 {
		t.Fatalf("err = %v, issued %d", err, len(s.Issued()))
	}
	if total := sum(rec.sleeps); total > 8*time.Second || len(rec.sleeps) != 1 {
		t.Errorf("sleeps = %v: waited %s past the 8s code lifetime", rec.sleeps, total)
	}
}

func TestPollFailuresEndTheFlow(t *testing.T) {
	for _, poll := range []string{"bogus_error", "http500", "badjson"} {
		t.Run(poll, func(t *testing.T) {
			s := deviceFake(t, fakeas.Options{Polls: []string{poll}})
			rec := &recorder{}
			f, out := deviceFlow(t, s.URL, rec)
			if _, err := f.Run(context.Background()); err == nil {
				t.Fatal("Run succeeded")
			}
			if len(s.Issued()) != 0 {
				t.Errorf("issued %d tokens", len(s.Issued()))
			}
			if len(rec.sleeps) != 1 {
				t.Errorf("sleeps = %v, want one: the flow must stop at the failure", rec.sleeps)
			}
			if strings.Contains(out.String(), "Bearer") {
				t.Errorf("out shows a token: %q", out.String())
			}
		})
	}
}

func TestIncompleteDeviceResponseRefused(t *testing.T) {
	for _, field := range []string{"user_code", "device_code", "verification_uri"} {
		t.Run(field, func(t *testing.T) {
			s := deviceFake(t, fakeas.Options{DeviceOmit: []string{field}})
			rec := &recorder{}
			f, out := deviceFlow(t, s.URL, rec)
			if _, err := f.Run(context.Background()); err == nil {
				t.Fatal("Run succeeded")
			}
			if len(rec.sleeps) != 0 || slices.Contains(s.Requests(), "POST /oauth/token") {
				t.Errorf("polled: sleeps %v, requests %q", rec.sleeps, s.Requests())
			}
			if out.Len() != 0 {
				t.Errorf("printed %q", out.String())
			}
		})
	}
}

func TestDeviceAuthorizationRefusalNamesTheRightRequest(t *testing.T) {
	s := deviceFake(t, fakeas.Options{})
	f, _ := deviceFlow(t, s.URL, &recorder{})
	// A ttl under 1 s is sent as 0, which the fake refuses at the device
	// authorization endpoint.
	f.TTL = 500 * time.Millisecond
	_, err := f.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded")
	}
	if !strings.Contains(err.Error(), "device code request was refused") || strings.Contains(err.Error(), "token request") {
		t.Errorf("err = %q", err)
	}
}
