package gcpcred

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/brig-sh/brig/internal/clirun"
)

// Token is one Google access token. It is held in memory only.
type Token struct {
	Value  string
	Expiry time.Time
	// Email is the identity tokeninfo reports for the token: the service
	// account in impersonate mode, the account of the login in self mode.
	Email string
}

// CLI runs the gcloud binary and asks tokeninfo when its token expires.
type CLI struct {
	// Bin is the path to gcloud; "" means look up "gcloud" on PATH.
	Bin string
	// Env is the subprocess environment, exactly. An empty Env is an empty
	// environment, not the caller's: a CLOUDSDK_CORE_ACCOUNT or
	// GOOGLE_APPLICATION_CREDENTIALS in the broker's own environment would
	// otherwise decide which identity a sandbox gets. Build it with Environ.
	Env []string
	// TokenInfoURL is the tokeninfo endpoint; "" means Google's.
	TokenInfoURL string
	// Client makes the tokeninfo request; nil means a client with a 10s
	// timeout. Its redirects are refused either way.
	Client *http.Client

	// run is the seam tests replace; nil means clirun.Exec.
	run clirun.Runner
	// now is the clock tests replace; nil means time.Now.
	now func() time.Time
}

const (
	maxOutput        = 64 << 10 // far above any tokeninfo response
	gcloudCommand    = "gcloud"
	tokenInfoURL     = "https://oauth2.googleapis.com/tokeninfo"
	tokenInfoTimeout = 10 * time.Second
)

// fetchTimeout bounds each gcloud call. A stalled gcloud (a hung network, a
// prompt it should not have shown) would otherwise hold the cache's single
// flight for its key and queue every request from that sandbox behind it. It
// is a variable only so a test can shorten it.
var fetchTimeout = 30 * time.Second

// InstallHint is the refusal of a host with no gcloud binary. Only the user
// can install it, and neither the broker nor the import can work without it.
const InstallHint = "the gcloud command was not found; install the Google Cloud CLI: https://cloud.google.com/sdk/docs/install"

// droppedEnv are the variables that choose an identity, a configuration or a
// log level over the pointer and the flags Mint passes. CLOUDSDK_AUTH_* is
// dropped as a prefix: every auth property there (an access token file, an
// impersonated account, a credential file override) replaces the login the
// pointer names. Log level and HTTP logging are dropped because they put
// tokens on the stderr that errors quote. Other CLOUDSDK_* variables, such
// as CLOUDSDK_PYTHON or CLOUDSDK_API_ENDPOINT_OVERRIDES_*, are kept: the
// broker's gcloud never sees one, because injector.brokerEnv starts the broker
// with an allowlist that names none. Widening that allowlist needs this list
// widened too.
var droppedEnv = map[string]bool{
	"CLOUDSDK_CORE_ACCOUNT":          true,
	"CLOUDSDK_ACTIVE_CONFIG_NAME":    true,
	"CLOUDSDK_CORE_LOG_HTTP":         true,
	"CLOUDSDK_CORE_VERBOSITY":        true,
	"CLOUDSDK_CORE_DISABLE_PROMPTS":  true,
	"GOOGLE_APPLICATION_CREDENTIALS": true,
	// brig-broker's environment carries no CLOUDSDK_CONFIG, so its gcloud
	// reads $HOME/.config/gcloud. The import drops it for the same reason it
	// drops the rest: its check must read the logins the broker reads.
	"CLOUDSDK_CONFIG": true,
}

// Environ is the environment of a gcloud subprocess: environ without the
// variables that choose an identity or turn on HTTP logging, and with
// prompts disabled, so a lapsed login fails at once instead of waiting on a
// terminal nobody is at. CLOUDSDK_CONFIG is dropped too, so both sides read
// the logins under $HOME/.config/gcloud. The import uses the same rule as the
// broker so that the check it makes at import time sees what the broker will
// see at run time.
func Environ(environ []string) []string {
	env := []string{}
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if droppedEnv[name] || strings.HasPrefix(name, "CLOUDSDK_AUTH_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "CLOUDSDK_CORE_DISABLE_PROMPTS=1")
}

// ErrLoginExpired means the configuration's gcloud login has lapsed, which
// only the user can fix, in a browser. Account is the pointer's account in
// self mode, and "" in impersonate mode. A self pointer mints only as its
// account, so the fix names it: a login to the configuration alone can sign
// in another account, and the pointer would still be refused.
type ErrLoginExpired struct{ Configuration, Account string }

func (e *ErrLoginExpired) Error() string {
	if e.Account != "" {
		return fmt.Sprintf("the gcloud login of %s in configuration %s has expired; run `gcloud auth login %s --configuration %s`",
			e.Account, e.Configuration, e.Account, e.Configuration)
	}
	return fmt.Sprintf("the gcloud login of configuration %s has expired; run `gcloud auth login --configuration %s`",
		e.Configuration, e.Configuration)
}

// ErrCannotImpersonate means the user's account may not mint tokens for the
// service account, which only an IAM grant fixes.
type ErrCannotImpersonate struct{ ServiceAccount string }

func (e *ErrCannotImpersonate) Error() string {
	return fmt.Sprintf("your gcloud account cannot impersonate %s; it needs roles/iam.serviceAccountTokenCreator on that service account",
		e.ServiceAccount)
}

// lapsedLogin reports whether gcloud's stderr says the login must be redone.
// "There was a problem refreshing your current auth tokens" alone is not
// enough: gcloud prints it for a network failure too, and telling the user
// to log in again would send them the wrong way.
func lapsedLogin(stderr []byte) bool {
	if bytes.Contains(stderr, []byte("Reauthentication failed")) {
		return true
	}
	_, after, ok := bytes.Cut(stderr, []byte("Please run:"))
	return ok && bytes.Contains(after, []byte("gcloud auth login"))
}

// Mint gets a token for p.
func (c CLI) Mint(ctx context.Context, p Pointer) (Token, error) {
	if err := p.Validate(); err != nil {
		return Token{}, err
	}
	args := []string{"auth", "print-access-token"}
	if p.Mode == ModeSelf {
		// ACCOUNT is gcloud's positional argument. Naming it mints for the
		// login the import recorded, not for whichever account the
		// configuration names now.
		args = append(args, p.Account)
	}
	args = append(args, "--configuration", p.Configuration)
	if p.Mode == ModeImpersonate {
		args = append(args, "--impersonate-service-account", p.ServiceAccount)
	}
	// A configuration with log_http or debug verbosity writes HTTP bodies,
	// tokens included, to the stderr that errors quote. The flags override
	// the configuration.
	args = append(args, "--format=json", "--no-log-http", "--verbosity=error")
	out, err := c.output(ctx, p, args)
	if err != nil {
		return Token{}, err
	}
	var doc struct {
		Token string `json:"token"`
	}
	// The decode error is not wrapped: a malformed document is the one place
	// a message could quote part of the token.
	if json.Unmarshal(out, &doc) != nil {
		return Token{}, fmt.Errorf("%s %s printed output that is not JSON", gcloudCommand, clirun.CmdName(args))
	}
	if doc.Token == "" {
		return Token{}, fmt.Errorf("%s %s printed no token", gcloudCommand, clirun.CmdName(args))
	}
	// gcloud does not print the expiry (checked with gcloud 591), and the
	// cache needs it to refresh the token before it lapses. tokeninfo also
	// says whose token it is.
	life, email, err := c.tokenInfo(ctx, doc.Token)
	if err != nil {
		return Token{}, err
	}
	// A token for an identity other than the one the pointer names is not
	// handed on. gcloud mints an impersonated token with the userinfo.email
	// scope, so tokeninfo names the service account (checked with gcloud 591);
	// no email leaves the identity unproved.
	if p.Mode == ModeImpersonate && (email == "" || !strings.EqualFold(email, p.ServiceAccount)) {
		got := email
		if got == "" {
			got = "no identity"
		}
		return Token{}, fmt.Errorf("gcloud minted a token for %s, not the service account %s", got, p.ServiceAccount)
	}
	// A self token is the user's whole identity, so it must be proved to be
	// the recorded account. A user login's token carries the userinfo.email
	// scope, so tokeninfo names it; no email leaves the account unproved.
	if p.Mode == ModeSelf && (email == "" || !strings.EqualFold(email, p.Account)) {
		got := email
		if got == "" {
			got = "no account"
		}
		return Token{}, fmt.Errorf("the gcloud configuration %s minted a token for %s, not %s, the account it was imported with; import the secret again to use another account",
			p.Configuration, got, p.Account)
	}
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	return Token{Value: doc.Token, Expiry: now().Add(life), Email: email}, nil
}

// Account is the account configuration logs in as, or "" when it has none.
// The import names it beside the service account it impersonates, so the user
// sees which of their logins the sandbox's access rests on, and records it in
// a self pointer. An account is an email address, not a credential.
func (c CLI) Account(ctx context.Context, configuration string) (string, error) {
	// The name goes on argv, so it must not read as a flag.
	if !configurationRE.MatchString(configuration) {
		return "", fmt.Errorf("the gcloud configuration name %q is not usable: use a lowercase letter first, then lowercase letters, digits and -", configuration)
	}
	args := []string{"config", "get-value", "account", "--configuration", configuration, "--verbosity=error"}
	out, err := c.output(ctx, Pointer{Configuration: configuration}, args)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// tokenInfo returns the remaining life of token and the identity it belongs
// to. No error it returns carries the token or the response body.
func (c CLI) tokenInfo(ctx context.Context, token string) (time.Duration, string, error) {
	endpoint := c.TokenInfoURL
	if endpoint == "" {
		endpoint = tokenInfoURL
	}
	client := http.Client{Timeout: tokenInfoTimeout}
	if c.Client != nil {
		client = *c.Client
	}
	// The token is in the body; a redirect could carry it to another host.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("tokeninfo redirects are not followed")
	}
	// The token goes in a form body, never the query string, which servers
	// and proxies log.
	form := url.Values{"access_token": {token}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form))
	if err != nil {
		return 0, "", fmt.Errorf("tokeninfo request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		// A *url.Error names the endpoint, which holds no token.
		return 0, "", fmt.Errorf("tokeninfo: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOutput+1))
	if err != nil {
		return 0, "", fmt.Errorf("tokeninfo: reading the response: %w", err)
	}
	if len(body) > maxOutput {
		return 0, "", fmt.Errorf("tokeninfo answered over %d bytes", maxOutput)
	}
	// The body of a refusal is not repeated: an error description may echo
	// the token it was sent.
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("tokeninfo refused the token: %s", resp.Status)
	}
	var doc struct {
		ExpiresIn json.RawMessage `json:"expires_in"`
		Email     string          `json:"email"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return 0, "", errors.New("tokeninfo answered with a document that is not JSON")
	}
	// Google sends expires_in as a string of seconds; a number is accepted
	// too.
	raw := strings.Trim(string(doc.ExpiresIn), `"`)
	secs, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || secs <= 0 {
		return 0, "", errors.New("tokeninfo gave no usable expires_in; a token with no expiry cannot be refreshed on a schedule")
	}
	return time.Duration(secs) * time.Second, doc.Email, nil
}

// output runs the CLI and returns stdout. A failure carries the command, the
// exit status and a clip of stderr, and never stdout, which is where the
// token is.
func (c CLI) output(ctx context.Context, p Pointer, args []string) ([]byte, error) {
	bin := c.Bin
	if bin == "" {
		bin = gcloudCommand
	}
	// Stderr is ignored on success: gcloud writes notices there, such as that
	// updates are available.
	stdout, stderr, err := clirun.Output(ctx, c.run, bin, args, c.Env, fetchTimeout)
	if err == nil {
		return stdout, nil
	}
	if errors.Is(err, clirun.ErrTimedOut) {
		return nil, fmt.Errorf("%s %s timed out after %v", gcloudCommand, clirun.CmdName(args), fetchTimeout)
	}
	if lapsedLogin(stderr) {
		return nil, &ErrLoginExpired{Configuration: p.Configuration, Account: p.Account}
	}
	if p.Mode == ModeImpersonate && bytes.Contains(stderr, []byte("iam.serviceAccounts.getAccessToken")) {
		return nil, &ErrCannotImpersonate{ServiceAccount: p.ServiceAccount}
	}
	return nil, fmt.Errorf("%s %s failed: %v: %s", gcloudCommand, clirun.CmdName(args), err, clirun.Clip(stderr))
}
