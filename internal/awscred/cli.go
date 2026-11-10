package awscred

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/brig-sh/brig/internal/clirun"
)

// Credentials are one set of AWS keys. They are held in memory only.
type Credentials struct {
	AccessKeyID, SecretAccessKey, SessionToken string
	Expiration                                 time.Time
}

// CLI runs the aws binary.
type CLI struct {
	// Bin is the path to aws; "" means look up "aws" on PATH.
	Bin string
	// Env is the subprocess environment, exactly. An empty Env is an empty
	// environment, not the caller's: an AWS_PROFILE or AWS_ACCESS_KEY_ID in
	// the broker's own environment would otherwise decide which identity a
	// sandbox gets.
	Env []string

	// run is the seam tests replace; nil means clirun.Exec.
	run clirun.Runner
}

const (
	awsCommand  = "aws"
	ssoLoginFmt = "aws sso login --profile %s"
)

// fetchTimeout bounds each aws call. A stalled aws (an SSO browser prompt, a
// hung network) would otherwise hold the cache's single flight for its key
// and queue every request from that sandbox behind it. It is a variable only
// so a test can shorten it.
var fetchTimeout = 30 * time.Second

// InstallHint is the refusal of a host with no aws binary. Only the user can
// install it, and neither the broker nor the import can work without it.
const InstallHint = "the aws command was not found; install the AWS CLI v2: " +
	"https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html"

// Environ is the environment of an aws subprocess: environ with every AWS_
// variable dropped. An AWS_PROFILE or AWS_ACCESS_KEY_ID would decide which
// identity a sandbox gets, over the pointer that names one. The import uses
// the same rule as the broker so that the check it makes at import time sees
// what the broker will see at run time: a profile that works only because of
// a variable in the user's shell would pass the import and fail the run.
func Environ(environ []string) []string {
	env := []string{}
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "AWS_") {
			env = append(env, kv)
		}
	}
	return env
}

// ErrSSOExpired means the profile's SSO login has lapsed, which only the user
// can fix, in a browser.
type ErrSSOExpired struct{ Profile string }

func (e *ErrSSOExpired) Error() string {
	return fmt.Sprintf("the AWS SSO session for profile %s has expired; run `"+ssoLoginFmt+"`", e.Profile, e.Profile)
}

var ssoMarkers = []string{
	"Token has expired",
	"The SSO session associated with this profile has expired",
	"Error loading SSO Token",
}

// Fetch gets credentials for p. In role mode the session is named
// sessionName.
func (c CLI) Fetch(ctx context.Context, p Pointer, sessionName string) (Credentials, error) {
	if err := p.Validate(); err != nil {
		return Credentials{}, err
	}
	var args []string
	switch p.Mode {
	case ModeRole:
		args = []string{"sts", "assume-role", "--profile", p.AWSProfile, "--role-arn", p.RoleARN,
			"--role-session-name", sessionName}
		if p.SourceIdentity != "" {
			args = append(args, "--source-identity", p.SourceIdentity)
		}
		args = append(args, "--duration-seconds", strconv.Itoa(p.Session()), "--output", "json")
	default:
		args = []string{"configure", "export-credentials", "--profile", p.AWSProfile, "--format", "process"}
	}
	out, err := c.output(ctx, p.AWSProfile, args)
	if err != nil {
		return Credentials{}, err
	}
	// The same four fields in both shapes; role mode nests them.
	var doc struct {
		Credentials *struct {
			AccessKeyID     string `json:"AccessKeyId"`
			SecretAccessKey string
			SessionToken    string
			Expiration      string
		}
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string
		SessionToken    string
		Expiration      string
	}
	// The decode error is not wrapped: a malformed document is the one place
	// a message could quote part of the keys.
	if json.Unmarshal(out, &doc) != nil {
		return Credentials{}, fmt.Errorf("%s %s printed output that is not JSON", awsCommand, clirun.CmdName(args))
	}
	id, secret, token, exp := doc.AccessKeyID, doc.SecretAccessKey, doc.SessionToken, doc.Expiration
	if p.Mode == ModeRole {
		if doc.Credentials == nil {
			return Credentials{}, fmt.Errorf("%s %s printed no Credentials", awsCommand, clirun.CmdName(args))
		}
		id, secret, token, exp = doc.Credentials.AccessKeyID, doc.Credentials.SecretAccessKey, doc.Credentials.SessionToken, doc.Credentials.Expiration
	}
	if id == "" || secret == "" {
		return Credentials{}, fmt.Errorf("%s %s printed no access key", awsCommand, clirun.CmdName(args))
	}
	// A key with no expiry cannot be refreshed on a schedule or held to one,
	// and a long-lived IAM user key is not what a sandbox should be handed.
	t, err := time.Parse(time.RFC3339, exp)
	if err != nil {
		return Credentials{}, fmt.Errorf("%s %s printed no usable Expiration; long-lived keys cannot be brokered", awsCommand, clirun.CmdName(args))
	}
	return Credentials{AccessKeyID: id, SecretAccessKey: secret, SessionToken: token, Expiration: t}, nil
}

// Region returns the profile's configured region.
func (c CLI) Region(ctx context.Context, profile string) (string, error) {
	out, err := c.output(ctx, profile, []string{"configure", "get", "region", "--profile", profile})
	if err != nil {
		// `configure get` exits 1 with no output when the key is unset.
		var sso *ErrSSOExpired
		if errors.As(err, &sso) {
			return "", err
		}
		return "", fmt.Errorf("profile %s has no region: %w; set one with `aws configure set region <r> --profile %s`", profile, err, profile)
	}
	r := strings.TrimSpace(string(out))
	if r == "" {
		return "", fmt.Errorf("profile %s has no region; set one with `aws configure set region <r> --profile %s`", profile, profile)
	}
	return r, nil
}

// CallerARN returns the ARN the profile authenticates as.
func (c CLI) CallerARN(ctx context.Context, profile string) (string, error) {
	out, err := c.output(ctx, profile, []string{"sts", "get-caller-identity", "--profile", profile, "--output", "json"})
	if err != nil {
		return "", err
	}
	var doc struct{ Arn string }
	if json.Unmarshal(out, &doc) != nil || doc.Arn == "" {
		return "", errors.New("aws sts get-caller-identity printed no Arn")
	}
	return doc.Arn, nil
}

// output runs the CLI and returns stdout. A failure carries the command, the
// exit status and a clip of stderr, and never stdout, which is where the
// keys are.
func (c CLI) output(ctx context.Context, profile string, args []string) ([]byte, error) {
	bin := c.Bin
	if bin == "" {
		bin = awsCommand
	}
	stdout, stderr, err := clirun.Output(ctx, c.run, bin, args, c.Env, fetchTimeout)
	if err == nil {
		return stdout, nil
	}
	if errors.Is(err, clirun.ErrTimedOut) {
		return nil, fmt.Errorf("%s %s timed out after %v", awsCommand, clirun.CmdName(args), fetchTimeout)
	}
	for _, m := range ssoMarkers {
		if bytes.Contains(stderr, []byte(m)) {
			return nil, &ErrSSOExpired{Profile: profile}
		}
	}
	return nil, fmt.Errorf("%s %s failed: %v: %s", awsCommand, clirun.CmdName(args), err, clirun.Clip(stderr))
}
