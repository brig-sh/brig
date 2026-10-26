package awscred

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
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

	// run is the seam tests replace; nil means execRun.
	run func(ctx context.Context, bin string, args, env []string) (stdout, stderr []byte, err error)
}

const (
	maxOutput   = 64 << 10 // far above any STS response
	maxStderr   = 400      // what an error repeats of the CLI's stderr
	awsCommand  = "aws"
	ssoLoginFmt = "aws sso login --profile %s"
)

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
		args = append(args, "--duration-seconds", "3600", "--output", "json")
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
		return Credentials{}, fmt.Errorf("%s %s printed output that is not JSON", awsCommand, cmdName(args))
	}
	id, secret, token, exp := doc.AccessKeyID, doc.SecretAccessKey, doc.SessionToken, doc.Expiration
	if p.Mode == ModeRole {
		if doc.Credentials == nil {
			return Credentials{}, fmt.Errorf("%s %s printed no Credentials", awsCommand, cmdName(args))
		}
		id, secret, token, exp = doc.Credentials.AccessKeyID, doc.Credentials.SecretAccessKey, doc.Credentials.SessionToken, doc.Credentials.Expiration
	}
	if id == "" || secret == "" {
		return Credentials{}, fmt.Errorf("%s %s printed no access key", awsCommand, cmdName(args))
	}
	// A key with no expiry cannot be refreshed on a schedule or held to one,
	// and a long-lived IAM user key is not what a sandbox should be handed.
	t, err := time.Parse(time.RFC3339, exp)
	if err != nil {
		return Credentials{}, fmt.Errorf("%s %s printed no usable Expiration; long-lived keys cannot be brokered", awsCommand, cmdName(args))
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
	run := c.run
	if run == nil {
		run = execRun
	}
	bin := c.Bin
	if bin == "" {
		bin = awsCommand
	}
	stdout, stderr, err := run(ctx, bin, args, c.Env)
	if err == nil {
		return stdout, nil
	}
	for _, m := range ssoMarkers {
		if bytes.Contains(stderr, []byte(m)) {
			return nil, &ErrSSOExpired{Profile: profile}
		}
	}
	msg := strings.TrimSpace(string(stderr))
	if len(msg) > maxStderr {
		msg = msg[:maxStderr] + "..."
	}
	return nil, fmt.Errorf("%s %s failed: %v: %s", awsCommand, cmdName(args), err, msg)
}

// cmdName is the aws subcommand for an error: "sts assume-role".
func cmdName(args []string) string {
	if len(args) >= 2 {
		return args[0] + " " + args[1]
	}
	return strings.Join(args, " ")
}

// execRun is the real runner. Both streams are capped so a misbehaving binary
// cannot fill the broker's memory.
func execRun(ctx context.Context, bin string, args, env []string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	if env == nil {
		// A nil Env makes os/exec inherit the parent's.
		env = []string{}
	}
	cmd.Env = env
	out, errb := &capped{limit: maxOutput}, &capped{limit: maxOutput}
	cmd.Stdout, cmd.Stderr = out, errb
	err := cmd.Run()
	if out.over {
		// The cap first, as the cause: the signal the process died of is
		// not what the reader needs.
		return nil, errb.buf.Bytes(), fmt.Errorf("stdout is over %d bytes", maxOutput)
	}
	return out.buf.Bytes(), errb.buf.Bytes(), err
}

// capped is an io.Writer that stops at a limit; the write error tears the
// command down instead of reading it to the end.
type capped struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); len(p) > room {
		c.buf.Write(p[:room])
		c.over = true
		return 0, errors.New("output over the limit")
	}
	return c.buf.Write(p)
}
