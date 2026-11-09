package gcpcred

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	sa      = "agent@proj.iam.gserviceaccount.com"
	account = "someone@example.com"
)

var (
	impersonate = Pointer{Kind: "gcloud", Configuration: "default", Mode: ModeImpersonate, ServiceAccount: sa}
	self        = Pointer{Kind: "gcloud", Configuration: "default", Mode: ModeSelf, Account: account}
)

type call struct {
	bin  string
	args []string
	env  []string
}

func seam(calls *[]call, stdout, stderr string, err error) func(context.Context, string, []string, []string) ([]byte, []byte, error) {
	return func(_ context.Context, bin string, args, env []string) ([]byte, []byte, error) {
		if calls != nil {
			*calls = append(*calls, call{bin, args, env})
		}
		return []byte(stdout), []byte(stderr), err
	}
}

// tokenInfo is a fake tokeninfo endpoint that answers body to every request.
func tokenInfo(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func cliFor(srv *httptest.Server, run func(context.Context, string, []string, []string) ([]byte, []byte, error)) CLI {
	return CLI{TokenInfoURL: srv.URL, Client: srv.Client(), run: run}
}

func TestMintArgvPerMode(t *testing.T) {
	var calls []call
	cli := cliFor(tokenInfo(t, 200, `{"expires_in":"3000","email":"`+sa+`"}`), seam(&calls, `{"token":"ya29.fake"}`, "", nil))
	cli.Env = Environ([]string{"PATH=/bin"})
	if _, err := cli.Mint(context.Background(), impersonate); err != nil {
		t.Fatal(err)
	}
	cli = cliFor(tokenInfo(t, 200, `{"expires_in":"3000","email":"`+account+`"}`), seam(&calls, `{"token":"ya29.fake"}`, "", nil))
	cli.Env = Environ([]string{"PATH=/bin"})
	if _, err := cli.Mint(context.Background(), self); err != nil {
		t.Fatal(err)
	}
	wantImp := []string{"auth", "print-access-token", "--configuration", "default",
		"--impersonate-service-account", sa, "--format=json", "--no-log-http", "--verbosity=error"}
	// The account is gcloud's positional ACCOUNT argument, so the token is
	// the pointer's account even when the configuration now names another.
	wantSelf := []string{"auth", "print-access-token", account, "--configuration", "default",
		"--format=json", "--no-log-http", "--verbosity=error"}
	if !slices.Equal(calls[0].args, wantImp) {
		t.Errorf("impersonate argv = %v", calls[0].args)
	}
	if !slices.Equal(calls[1].args, wantSelf) {
		t.Errorf("self argv = %v", calls[1].args)
	}
	if calls[0].bin != "gcloud" {
		t.Errorf("bin = %q, want gcloud", calls[0].bin)
	}
	for _, c := range calls {
		if !slices.Contains(c.env, "CLOUDSDK_CORE_DISABLE_PROMPTS=1") {
			t.Errorf("env %v lacks CLOUDSDK_CORE_DISABLE_PROMPTS=1", c.env)
		}
	}
}

func TestTokenInfoRefusesARedirect(t *testing.T) {
	var hit atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Add(1)
		io.WriteString(w, `{"expires_in":"3000"}`)
	}))
	t.Cleanup(other.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	cli := cliFor(srv, seam(nil, `{"token":"ya29.fake"}`, "", nil))
	_, err := cli.Mint(context.Background(), self)
	if err == nil {
		t.Fatal("a redirect was followed")
	}
	if hit.Load() != 0 {
		t.Errorf("the redirect target received %d requests", hit.Load())
	}
	if strings.Contains(err.Error(), "ya29.fake") {
		t.Errorf("error carries the token: %v", err)
	}
}

func TestTokenInfoCapsTheResponse(t *testing.T) {
	big := `{"expires_in":"3000","pad":"` + strings.Repeat("a", 1<<20) + `"}`
	srv := tokenInfo(t, 200, big)
	cli := cliFor(srv, seam(nil, `{"token":"ya29.fake"}`, "", nil))
	if _, err := cli.Mint(context.Background(), self); err == nil {
		t.Fatal("a 1 MiB tokeninfo response was accepted")
	}
}

func TestMintReadsTheExpiryFromTokenInfo(t *testing.T) {
	var method, ctype, body, query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, ctype, query = r.Method, r.Header.Get("Content-Type"), r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		io.WriteString(w, `{"expires_in":"3395","email":"`+sa+`"}`)
	}))
	t.Cleanup(srv.Close)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	cli := cliFor(srv, seam(nil, `{"token":"ya29.fake"}`, "", nil))
	cli.now = func() time.Time { return now }
	tok, err := cli.Mint(context.Background(), impersonate)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Value != "ya29.fake" || !tok.Expiry.Equal(now.Add(3395*time.Second)) || tok.Email != sa {
		t.Errorf("token = {Expiry:%v Email:%q}, value ok = %v", tok.Expiry, tok.Email, tok.Value == "ya29.fake")
	}
	if method != http.MethodPost {
		t.Errorf("method = %s", method)
	}
	if ctype != "application/x-www-form-urlencoded" {
		t.Errorf("content type = %q", ctype)
	}
	if body != "access_token=ya29.fake" {
		t.Errorf("body = %q", body)
	}
	if query != "" {
		t.Errorf("the token reached the query string: %q", query)
	}
}

func TestMintRefusesAnotherIdentity(t *testing.T) {
	srv := tokenInfo(t, 200, `{"expires_in":"3000","email":"other@p.iam.gserviceaccount.com"}`)
	cli := cliFor(srv, seam(nil, `{"token":"ya29.fake"}`, "", nil))
	_, err := cli.Mint(context.Background(), impersonate)
	if err == nil {
		t.Fatal("a token for another identity was accepted")
	}
	if strings.Contains(err.Error(), "ya29.fake") {
		t.Errorf("error carries the token: %v", err)
	}
}

// The configuration's account can change after the import, with gcloud
// config set account or a gcloud auth login as another user. A self pointer
// then must not hand on the other identity's token.
func TestMintSelfRefusesAnotherAccount(t *testing.T) {
	srv := tokenInfo(t, 200, `{"expires_in":"3000","email":"other@example.com"}`)
	cli := cliFor(srv, seam(nil, `{"token":"ya29.fake"}`, "", nil))
	tok, err := cli.Mint(context.Background(), self)
	if err == nil {
		t.Fatal("a token for another account was accepted")
	}
	if tok.Value != "" {
		t.Error("a token was returned with the error")
	}
	for _, want := range []string{account, "default"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "ya29.fake") {
		t.Errorf("error carries the token: %v", err)
	}
	// The case of an address is not the identity.
	srv = tokenInfo(t, 200, `{"expires_in":"3000","email":"SomeOne@Example.com"}`)
	if _, err := cliFor(srv, seam(nil, `{"token":"ya29.fake"}`, "", nil)).Mint(context.Background(), self); err != nil {
		t.Errorf("the same account in another case was refused: %v", err)
	}
}

// A token gcloud prints for a user login carries the userinfo.email scope, so
// tokeninfo names its account. With no email the account is unproven, and a
// self token is the user's whole identity, so it is refused.
func TestMintSelfNeedsAnEmail(t *testing.T) {
	srv := tokenInfo(t, 200, `{"expires_in":"3000"}`)
	cli := cliFor(srv, seam(nil, `{"token":"ya29.fake"}`, "", nil))
	_, err := cli.Mint(context.Background(), self)
	if err == nil {
		t.Fatal("a self token with no email was accepted")
	}
	if !strings.Contains(err.Error(), account) || strings.Contains(err.Error(), "ya29.fake") {
		t.Errorf("err = %v", err)
	}
}

// gcloud mints an impersonated token with the userinfo.email scope, so
// tokeninfo names the service account. With no email the identity is
// unproven, and the token is refused.
func TestMintImpersonateNeedsAnEmail(t *testing.T) {
	srv := tokenInfo(t, 200, `{"expires_in":"3000"}`)
	cli := cliFor(srv, seam(nil, `{"token":"ya29.fake"}`, "", nil))
	tok, err := cli.Mint(context.Background(), impersonate)
	if err == nil {
		t.Fatal("an impersonated token with no email was accepted")
	}
	if tok.Value != "" {
		t.Error("a token was returned with the error")
	}
	if !strings.Contains(err.Error(), sa) || strings.Contains(err.Error(), "ya29.fake") {
		t.Errorf("err = %v", err)
	}
}

func TestMintNeedsAnExpiry(t *testing.T) {
	for _, body := range []string{`{"email":"` + sa + `"}`, `{"expires_in":"0"}`, `{"expires_in":"-5"}`, `{"expires_in":"soon"}`} {
		srv := tokenInfo(t, 200, body)
		cli := cliFor(srv, seam(nil, `{"token":"ya29.fake"}`, "", nil))
		if _, err := cli.Mint(context.Background(), impersonate); err == nil {
			t.Errorf("tokeninfo %s: accepted with no usable expiry", body)
		}
	}
}

func TestMintIgnoresStderrOnSuccess(t *testing.T) {
	srv := tokenInfo(t, 200, `{"expires_in":"3000","email":"`+account+`"}`)
	cli := cliFor(srv, seam(nil, `{"token":"ya29.fake"}`,
		"Updates are available for some Google Cloud CLI components.", nil))
	if _, err := cli.Mint(context.Background(), self); err != nil {
		t.Fatal(err)
	}
}

func TestMintNeverEchoesStdout(t *testing.T) {
	srv := tokenInfo(t, 200, `{"expires_in":"3000"}`)
	for _, out := range []string{`ya29.SECRET not json`, `{"tok":"ya29.SECRET"}`, `{"token":"ya29.SECRET"`} {
		cli := cliFor(srv, seam(nil, out, "", nil))
		_, err := cli.Mint(context.Background(), self)
		if err == nil {
			t.Errorf("stdout %q: accepted", out)
			continue
		}
		if strings.Contains(err.Error(), "ya29.SECRET") {
			t.Errorf("stdout %q: error carries it: %v", out, err)
		}
	}
	// A failing gcloud's stdout is dropped as well.
	cli := cliFor(srv, seam(nil, `{"token":"ya29.SECRET"}`, "boom", errors.New("exit status 1")))
	if _, err := cli.Mint(context.Background(), self); err == nil || strings.Contains(err.Error(), "ya29.SECRET") {
		t.Errorf("failed run: err = %v", err)
	}
}

func TestMintTokenInfoFailure(t *testing.T) {
	srv := tokenInfo(t, 400, `{"error":"invalid_token","error_description":"ya29.fake"}`)
	cli := cliFor(srv, seam(nil, `{"token":"ya29.fake"}`, "", nil))
	_, err := cli.Mint(context.Background(), self)
	if err == nil || strings.Contains(err.Error(), "ya29.fake") {
		t.Errorf("400: err = %v", err)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	cli = cliFor(closed, seam(nil, `{"token":"ya29.fake"}`, "", nil))
	_, err = cli.Mint(context.Background(), self)
	if err == nil || strings.Contains(err.Error(), "ya29.fake") {
		t.Errorf("closed server: err = %v", err)
	}
}

func TestMintNamesTheConfiguration(t *testing.T) {
	srv := tokenInfo(t, 200, `{"expires_in":"3000"}`)
	cli := cliFor(srv, seam(nil, "", "ERROR: (gcloud) Configuration [nope] does not exist.", errors.New("exit status 1")))
	_, err := cli.Mint(context.Background(), self)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}

	cli = cliFor(srv, seam(nil, "", strings.Repeat("x", 1000), errors.New("exit status 1")))
	_, err = cli.Mint(context.Background(), self)
	if err == nil {
		t.Fatal("a failed run succeeded")
	}
	if strings.Contains(err.Error(), strings.Repeat("x", maxStderr+1)) {
		t.Errorf("stderr not capped: %d bytes of error", len(err.Error()))
	}
}

func TestMintLapsedLogin(t *testing.T) {
	srv := tokenInfo(t, 200, `{"expires_in":"3000"}`)
	for _, stderr := range []string{
		"ERROR: (gcloud.auth.print-access-token) There was a problem refreshing your current auth tokens: Reauthentication failed. cannot prompt during non-interactive execution.",
		"ERROR: (gcloud.auth.print-access-token) Your current active account does not have any valid credentials\nPlease run:\n\n  $ gcloud auth login\n\nto obtain new credentials.",
	} {
		// A self pointer mints only as its account, so the fix names it. An
		// impersonate pointer mints as whoever the configuration holds.
		for _, tc := range []struct {
			p    Pointer
			want string
		}{
			{self, "run `gcloud auth login " + account + " --configuration default`"},
			{impersonate, "run `gcloud auth login --configuration default`"},
		} {
			cli := cliFor(srv, seam(nil, "", stderr, errors.New("exit status 1")))
			_, err := cli.Mint(context.Background(), tc.p)
			var lapsed *ErrLoginExpired
			if !errors.As(err, &lapsed) {
				t.Errorf("mode %s, stderr %q: err = %v, want ErrLoginExpired", tc.p.Mode, stderr, err)
				continue
			}
			if lapsed.Configuration != "default" || lapsed.Account != tc.p.Account {
				t.Errorf("mode %s: configuration %q account %q", tc.p.Mode, lapsed.Configuration, lapsed.Account)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("mode %s: error %q lacks %q", tc.p.Mode, err, tc.want)
			}
		}
	}

	// The same prefix covers a network failure, which a login does not fix.
	cli := cliFor(srv, seam(nil, "", "ERROR: (gcloud.auth.print-access-token) There was a problem refreshing your current auth tokens: connection reset", errors.New("exit status 1")))
	_, err := cli.Mint(context.Background(), self)
	var lapsed *ErrLoginExpired
	if err == nil || errors.As(err, &lapsed) {
		t.Errorf("network failure: err = %v, want a plain error", err)
	}
}

func TestMintCannotImpersonate(t *testing.T) {
	srv := tokenInfo(t, 200, `{"expires_in":"3000"}`)
	cli := cliFor(srv, seam(nil, "", "ERROR: (gcloud.auth.print-access-token) PERMISSION_DENIED: Permission 'iam.serviceAccounts.getAccessToken' denied on resource (or it may not exist).", errors.New("exit status 1")))
	_, err := cli.Mint(context.Background(), impersonate)
	var denied *ErrCannotImpersonate
	if !errors.As(err, &denied) {
		t.Fatalf("err = %v, want ErrCannotImpersonate", err)
	}
	if denied.ServiceAccount != sa || !strings.Contains(err.Error(), sa) || !strings.Contains(err.Error(), "roles/iam.serviceAccountTokenCreator") {
		t.Errorf("error = %v", err)
	}
}

func TestMintTimesOut(t *testing.T) {
	old := fetchTimeout
	fetchTimeout = 50 * time.Millisecond
	t.Cleanup(func() { fetchTimeout = old })
	srv := tokenInfo(t, 200, `{"expires_in":"3000"}`)
	cli := cliFor(srv, func(ctx context.Context, _ string, _, _ []string) ([]byte, []byte, error) {
		<-ctx.Done()
		return []byte(`{"token":"ya29.fake"}`), nil, ctx.Err()
	})
	_, err := cli.Mint(context.Background(), self)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "ya29.fake") {
		t.Errorf("error carries the token: %v", err)
	}
}

func TestMintRefusesAnInvalidPointer(t *testing.T) {
	var calls []call
	srv := tokenInfo(t, 200, `{"expires_in":"3000"}`)
	cli := cliFor(srv, seam(&calls, `{"token":"ya29.fake"}`, "", nil))
	if _, err := cli.Mint(context.Background(), Pointer{Kind: "gcloud", Configuration: "--x", Mode: ModeSelf}); err == nil {
		t.Fatal("an invalid pointer was used")
	}
	if len(calls) != 0 {
		t.Errorf("gcloud ran for an invalid pointer: %v", calls)
	}
}

func TestEnvironDropsIdentityVariables(t *testing.T) {
	in := []string{
		"CLOUDSDK_AUTH_ACCESS_TOKEN_FILE=/t",
		"CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT=x@p.iam.gserviceaccount.com",
		"CLOUDSDK_CORE_ACCOUNT=me@example.com",
		"CLOUDSDK_ACTIVE_CONFIG_NAME=other",
		"CLOUDSDK_CORE_LOG_HTTP=true",
		"CLOUDSDK_CORE_VERBOSITY=debug",
		"GOOGLE_APPLICATION_CREDENTIALS=/k.json",
		"CLOUDSDK_CONFIG=/x",
		"CLOUDSDK_PYTHON=/y",
		"PATH=/z",
	}
	// CLOUDSDK_CONFIG goes too: the broker's environment never carries it,
	// so an import that kept it would check a login the broker cannot see.
	want := []string{"CLOUDSDK_PYTHON=/y", "PATH=/z", "CLOUDSDK_CORE_DISABLE_PROMPTS=1"}
	if got := Environ(in); !slices.Equal(got, want) {
		t.Errorf("Environ = %v, want %v", got, want)
	}
	// A value already set is replaced, not doubled.
	got := Environ([]string{"CLOUDSDK_CORE_DISABLE_PROMPTS=0", "PATH=/z"})
	if !slices.Equal(got, []string{"PATH=/z", "CLOUDSDK_CORE_DISABLE_PROMPTS=1"}) {
		t.Errorf("Environ = %v", got)
	}
}

// Account names the login a configuration mints with, for the import to say
// who impersonates. The configuration is on the argv, so the active one is
// never what answers.
func TestAccountReadsTheConfiguration(t *testing.T) {
	var calls []call
	cli := CLI{run: seam(&calls, "someone@example.com\n", "", nil)}
	got, err := cli.Account(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if got != "someone@example.com" {
		t.Errorf("Account = %q", got)
	}
	want := []string{"config", "get-value", "account", "--configuration", "default", "--verbosity=error"}
	if !slices.Equal(calls[0].args, want) {
		t.Errorf("argv = %v, want %v", calls[0].args, want)
	}
}

// A configuration name that gcloud would read as a flag never reaches argv.
func TestAccountRefusesAnInvalidConfiguration(t *testing.T) {
	var calls []call
	cli := CLI{run: seam(&calls, "", "", nil)}
	if _, err := cli.Account(context.Background(), "--verbosity=debug"); err == nil {
		t.Error("accepted a configuration that is a flag")
	}
	if len(calls) != 0 {
		t.Errorf("gcloud ran: %v", calls)
	}
}
