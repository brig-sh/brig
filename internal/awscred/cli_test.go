package awscred

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/clirun"
)

const roleDoc = `{"Credentials":{"AccessKeyId":"ASIAX","SecretAccessKey":"sek","SessionToken":"tok","Expiration":"2026-10-26T15:04:05+00:00"}}`
const selfDoc = `{"Version":1,"AccessKeyId":"ASIAX","SecretAccessKey":"sek","SessionToken":"tok","Expiration":"2026-10-26T15:04:05+00:00"}`

type call struct {
	bin  string
	args []string
	env  []string
}

func seam(calls *[]call, stdout, stderr string, err error) func(context.Context, string, []string, []string) ([]byte, []byte, error) {
	return func(_ context.Context, bin string, args, env []string) ([]byte, []byte, error) {
		*calls = append(*calls, call{bin, args, env})
		return []byte(stdout), []byte(stderr), err
	}
}

func wantCreds(t *testing.T, c Credentials) {
	t.Helper()
	want := time.Date(2026, 10, 26, 15, 4, 5, 0, time.UTC)
	if c.AccessKeyID != "ASIAX" || c.SecretAccessKey != "sek" || c.SessionToken != "tok" || !c.Expiration.Equal(want) {
		t.Errorf("credentials = %+v", c)
	}
}

func TestFetchRoleArgv(t *testing.T) {
	var calls []call
	cli := CLI{run: seam(&calls, roleDoc, "", nil)}
	p := Pointer{Kind: "aws", AWSProfile: "dev", Mode: ModeRole, RoleARN: "arn:aws:iam::000000000000:role/r", SourceIdentity: "me@example.com"}
	c, err := cli.Fetch(context.Background(), p, "brig-x")
	if err != nil {
		t.Fatal(err)
	}
	wantCreds(t, c)
	want := []string{"sts", "assume-role", "--profile", "dev", "--role-arn", "arn:aws:iam::000000000000:role/r",
		"--role-session-name", "brig-x", "--source-identity", "me@example.com", "--duration-seconds", "1800", "--output", "json"}
	if !slices.Equal(calls[0].args, want) {
		t.Errorf("argv = %v", calls[0].args)
	}
	p.SourceIdentity = ""
	calls = nil
	if _, err := cli.Fetch(context.Background(), p, "brig-x"); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(calls[0].args, "--source-identity") {
		t.Errorf("--source-identity passed with none set: %v", calls[0].args)
	}
}

func TestFetchAsksForThePointersSession(t *testing.T) {
	var calls []call
	cli := CLI{run: seam(&calls, roleDoc, "", nil)}
	p := Pointer{Kind: "aws", AWSProfile: "dev", Mode: ModeRole, RoleARN: "arn:aws:iam::000000000000:role/r", SessionSeconds: 2700}
	if _, err := cli.Fetch(context.Background(), p, "brig-x"); err != nil {
		t.Fatal(err)
	}
	i := slices.Index(calls[0].args, "--duration-seconds")
	if i < 0 || i+1 >= len(calls[0].args) || calls[0].args[i+1] != "2700" {
		t.Errorf("argv does not ask for 2700 seconds: %v", calls[0].args)
	}
}

func TestFetchPassesSessionPolicies(t *testing.T) {
	var calls []call
	cli := CLI{run: seam(&calls, roleDoc, "", nil)}
	p := Pointer{Kind: "aws", AWSProfile: "dev", Mode: ModeRole, RoleARN: "arn:aws:iam::000000000000:role/r",
		PolicyARNs: []string{"arn:aws:iam::0:policy/a", "arn:aws:iam::0:policy/b"}}
	if _, err := cli.Fetch(context.Background(), p, "brig-x"); err != nil {
		t.Fatal(err)
	}
	want := []string{"--policy-arns", "arn=arn:aws:iam::0:policy/a", "arn=arn:aws:iam::0:policy/b"}
	i := slices.Index(calls[0].args, "--policy-arns")
	if i < 0 || i+len(want) > len(calls[0].args) || !slices.Equal(calls[0].args[i:i+len(want)], want) {
		t.Errorf("argv does not pass the session policies as %v: %v", want, calls[0].args)
	}
}

func TestFetchSelfArgv(t *testing.T) {
	var calls []call
	cli := CLI{run: seam(&calls, selfDoc, "", nil)}
	c, err := cli.Fetch(context.Background(), Pointer{Kind: "aws", AWSProfile: "dev", Mode: ModeSelf}, "brig-x")
	if err != nil {
		t.Fatal(err)
	}
	wantCreds(t, c)
	want := []string{"configure", "export-credentials", "--profile", "dev", "--format", "process"}
	if !slices.Equal(calls[0].args, want) {
		t.Errorf("argv = %v", calls[0].args)
	}
}

func TestFetchEnvIsExactlyCLIEnv(t *testing.T) {
	t.Setenv("AWS_PROFILE", "leak")
	var calls []call
	env := []string{"HOME=/h", "PATH=/p"}
	cli := CLI{Env: env, run: seam(&calls, selfDoc, "", nil)}
	if _, err := cli.Fetch(context.Background(), Pointer{Kind: "aws", AWSProfile: "p", Mode: ModeSelf}, "s"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls[0].env, env) {
		t.Errorf("env = %v", calls[0].env)
	}
}

// An empty Env must not fall back to the broker's own environment, which
// os/exec does for a nil slice.
func TestExecEmptyEnvIsEmpty(t *testing.T) {
	t.Setenv("AWS_PROFILE", "leak")
	out, _, err := clirun.Exec(context.Background(), "/usr/bin/env", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "AWS_PROFILE") {
		t.Errorf("environment inherited: %s", out)
	}
}

func TestFetchSSOExpired(t *testing.T) {
	var calls []call
	cli := CLI{run: seam(&calls, "", "Error loading SSO Token: Token for dev does not exist", errors.New("exit status 255"))}
	_, err := cli.Fetch(context.Background(), Pointer{Kind: "aws", AWSProfile: "dev", Mode: ModeSelf}, "s")
	var sso *ErrSSOExpired
	if !errors.As(err, &sso) || sso.Profile != "dev" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "aws sso login --profile dev") {
		t.Errorf("message lacks the fix: %v", err)
	}
}

func TestFetchErrorCarriesNoStdout(t *testing.T) {
	var calls []call
	cli := CLI{run: seam(&calls, `{"Credentials":{"SecretAccessKey":"S3CR3T"}}`, strings.Repeat("e", 10<<10), errors.New("exit status 1"))}
	_, err := cli.Fetch(context.Background(), Pointer{Kind: "aws", AWSProfile: "p", Mode: ModeRole, RoleARN: "arn:aws:iam::0:role/r"}, "s")
	if err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(err.Error(), "S3CR3T") || len(err.Error()) > 512 {
		t.Errorf("error leaks or is long (%d bytes)", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "aws sts assume-role") {
		t.Errorf("error does not name the command: %v", err)
	}
}

func TestFetchBadOutputCarriesNoOutput(t *testing.T) {
	var calls []call
	cli := CLI{run: seam(&calls, `{"AccessKeyId":"A","SecretAccessKey":"S3CR3T","Expiration":"tomorrow"}`, "", nil)}
	_, err := cli.Fetch(context.Background(), Pointer{Kind: "aws", AWSProfile: "p", Mode: ModeSelf}, "s")
	if err == nil || strings.Contains(err.Error(), "S3CR3T") {
		t.Errorf("err = %v", err)
	}
}

// Long-lived IAM user keys come back with no Expiration. They cannot be
// refreshed on a schedule, and a sandbox must not be handed them.
func TestFetchRefusesNoExpiration(t *testing.T) {
	var calls []call
	cli := CLI{run: seam(&calls, `{"Version":1,"AccessKeyId":"AKIAX","SecretAccessKey":"S3CR3T"}`, "", nil)}
	c, err := cli.Fetch(context.Background(), Pointer{Kind: "aws", AWSProfile: "p", Mode: ModeSelf}, "s")
	if err == nil || !strings.Contains(err.Error(), "printed no usable Expiration; long-lived keys cannot be brokered") {
		t.Fatalf("err = %v, want the long-lived key refusal", err)
	}
	if c != (Credentials{}) {
		t.Errorf("returned %+v with the refusal", c)
	}
	if strings.Contains(err.Error(), "S3CR3T") {
		t.Errorf("the refusal carries the secret key: %v", err)
	}
}

func TestRegionEmpty(t *testing.T) {
	var calls []call
	cli := CLI{run: seam(&calls, "\n", "", nil)}
	_, err := cli.Region(context.Background(), "dev")
	if err == nil || !strings.Contains(err.Error(), "aws configure set region") {
		t.Errorf("err = %v", err)
	}
	cli = CLI{run: seam(&calls, "eu-west-1\n", "", nil)}
	if r, err := cli.Region(context.Background(), "dev"); err != nil || r != "eu-west-1" {
		t.Errorf("got %q, %v", r, err)
	}
}

func TestCallerARN(t *testing.T) {
	var calls []call
	cli := CLI{run: seam(&calls, `{"Arn":"arn:aws:sts::1:assumed-role/R/me@example.com"}`, "", nil)}
	got, err := cli.CallerARN(context.Background(), "p")
	if err != nil || got != "arn:aws:sts::1:assumed-role/R/me@example.com" {
		t.Errorf("got %q, %v", got, err)
	}
	want := []string{"sts", "get-caller-identity", "--profile", "p", "--output", "json"}
	if !slices.Equal(calls[0].args, want) {
		t.Errorf("argv = %v", calls[0].args)
	}
}

func TestExecFakeAWS(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	bin, err := filepath.Abs("testdata/fake-aws")
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "argv")
	cli := CLI{Bin: bin, Env: []string{"PATH=/usr/bin:/bin", "FAKE_AWS_LOG=" + log}}
	p := Pointer{Kind: "aws", AWSProfile: "p", Mode: ModeRole, RoleARN: "arn:aws:iam::0:role/r"}
	c, err := cli.Fetch(context.Background(), p, "brig-x")
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessKeyID != "ASIAFAKE" || c.SecretAccessKey != "fake-secret-value" || c.SessionToken != "fake-token-value" {
		t.Errorf("credentials = %+v", c)
	}
	argv, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{c.SecretAccessKey, c.SessionToken} {
		if strings.Contains(string(argv), secret) {
			t.Errorf("argv carries a credential: %s", argv)
		}
	}
	if r, err := cli.Region(context.Background(), "p"); err != nil || r != "eu-west-1" {
		t.Errorf("region %q, %v", r, err)
	}
}

func TestFetchTimesOut(t *testing.T) {
	old := fetchTimeout
	fetchTimeout = 50 * time.Millisecond
	t.Cleanup(func() { fetchTimeout = old })
	cli := CLI{run: func(ctx context.Context, _ string, _ []string, _ []string) ([]byte, []byte, error) {
		<-ctx.Done()
		return []byte("SECRET-STDOUT"), nil, ctx.Err()
	}}
	start := time.Now()
	_, err := cli.Fetch(context.Background(), Pointer{Kind: "aws", AWSProfile: "p", Mode: ModeSelf}, "s")
	if err == nil {
		t.Fatal("no error from a call that never returned")
	}
	if time.Since(start) > time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	if !strings.Contains(err.Error(), "timed out after 50ms") {
		t.Errorf("error does not name the timeout: %v", err)
	}
	if strings.Contains(err.Error(), "SECRET-STDOUT") {
		t.Errorf("error carries stdout: %v", err)
	}
}

// CommandContext kills aws, not what aws started. A credential_process
// helper or an SSO browser flow can keep the pipes open, and Run waits for
// them to close, so without a WaitDelay the timeout would not release the
// single flight.
func TestExecTimeoutWithChildHoldingPipes(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	bin := filepath.Join(t.TempDir(), "aws")
	script := "#!/bin/sh\nsleep 30 &\nsleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := fetchTimeout
	fetchTimeout = time.Second // long enough that the shell has forked its child
	t.Cleanup(func() { fetchTimeout = old })
	cli := CLI{Bin: bin, Env: []string{"PATH=/usr/bin:/bin"}}
	start := time.Now()
	_, err := cli.Fetch(context.Background(), Pointer{Kind: "aws", AWSProfile: "p", Mode: ModeSelf}, "s")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v: the timeout did not release the call", d)
	}
}

func TestEnvironDropsAWSVariables(t *testing.T) {
	got := Environ([]string{"PATH=/bin", "AWS_PROFILE=someone-else", "HOME=/h", "AWS_ACCESS_KEY_ID=x", "NOT_AWS_X=1"})
	want := []string{"PATH=/bin", "HOME=/h", "NOT_AWS_X=1"}
	if !slices.Equal(got, want) {
		t.Errorf("Environ = %v, want %v", got, want)
	}
	if got := Environ(nil); got == nil || len(got) != 0 {
		t.Errorf("Environ(nil) = %#v, want an empty non-nil slice", got)
	}
}

// scopedDoc is a get-scoped-credentials answer, with an access key id of its
// own so a test can tell which command answered.
func scopedDoc(exp time.Time) string {
	return `{"Credentials":{"AccessKeyId":"ASIASCOPED","SecretAccessKey":"scoped-sek","SessionToken":"scoped-tok","Expiration":"` +
		exp.Format(time.RFC3339) + `"},"SessionInfo":{"Arn":"arn:aws:sts::111122223333:assumed-role/R/me"},"ScopedPoliciesCount":1}`
}

// reply is one answer of a scripted aws.
type reply struct {
	stdout, stderr string
	err            error
}

// script answers each call with the next reply, and the last one after that.
func script(calls *[]call, replies ...reply) func(context.Context, string, []string, []string) ([]byte, []byte, error) {
	return func(_ context.Context, bin string, args, env []string) ([]byte, []byte, error) {
		*calls = append(*calls, call{bin, slices.Clone(args), env})
		r := replies[min(len(*calls), len(replies))-1]
		return []byte(r.stdout), []byte(r.stderr), r.err
	}
}

var scopedExp = time.Date(2026, 10, 26, 12, 30, 0, 0, time.UTC)

// scopedCLI is a CLI whose clock reads half an hour before scopedExp.
func scopedCLI(calls *[]call, replies ...reply) CLI {
	return CLI{run: script(calls, replies...), now: func() time.Time { return scopedExp.Add(-30 * time.Minute) }}
}

func scopedPointer() Pointer {
	return Pointer{Kind: "aws", AWSProfile: "dev", Mode: ModeScoped,
		ScopePolicy: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:ListAllMyBuckets","Resource":"*"}]}`}
}

// The observed answer of aws when the request asks for more than the
// calling session has left.
const insufficientLifetime = "An error occurred (InsufficientSessionLifetime) when calling the GetScopedCredentials operation: " +
	"The requested DurationSeconds exceeds the remaining lifetime of the calling session. Remaining session lifetime is 3075 seconds."

// wantOnlyScoped fails the test if any call was not get-scoped-credentials
// with the pointer's policy. export-credentials would hand out the profile's
// whole keys, and assume-role a session the scope policy does not bound.
func wantOnlyScoped(t *testing.T, calls []call, p Pointer) {
	t.Helper()
	if len(calls) == 0 {
		t.Fatal("aws never ran")
	}
	for _, c := range calls {
		joined := strings.Join(c.args, " ")
		if len(c.args) < 2 || c.args[0] != "sts" || c.args[1] != "get-scoped-credentials" {
			t.Errorf("a scoped fetch ran %q", joined)
		}
		if strings.Contains(joined, "export-credentials") || strings.Contains(joined, "assume-role") {
			t.Errorf("a scoped fetch ran %q", joined)
		}
		i := slices.Index(c.args, "--policy")
		if i < 0 || i+1 >= len(c.args) || c.args[i+1] != p.ScopePolicy {
			t.Errorf("a scoped call did not carry the pointer's policy: %q", joined)
		}
	}
}

func TestFetchScopedNeverExportsCredentials(t *testing.T) {
	var calls []call
	cli := scopedCLI(&calls, reply{stdout: scopedDoc(scopedExp)})
	p := scopedPointer()
	c, err := cli.Fetch(context.Background(), p, "brig-x")
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessKeyID != "ASIASCOPED" || c.SecretAccessKey != "scoped-sek" || c.SessionToken != "scoped-tok" || !c.Expiration.Equal(scopedExp) {
		t.Errorf("credentials = %+v", c)
	}
	wantOnlyScoped(t, calls, p)
}

// No failure of the scoped call falls back to another command, and none
// returns keys.
func TestFetchScopedFailureReturnsNoKeys(t *testing.T) {
	old := fetchTimeout
	fetchTimeout = 50 * time.Millisecond
	t.Cleanup(func() { fetchTimeout = old })
	exit := errors.New("exit status 254")
	for name, r := range map[string]reply{
		"access denied":    {stderr: "An error occurred (AccessDenied) when calling the GetScopedCredentials operation: not authorized", err: exit},
		"malformed":        {stderr: "An error occurred (MalformedPolicyDocument) when calling the GetScopedCredentials operation: Syntax error at position (-1,-1)", err: exit},
		"packed too big":   {stderr: "An error occurred (PackedPolicyTooLarge) when calling the GetScopedCredentials operation", err: exit},
		"timeout":          {err: context.DeadlineExceeded},
		"not json":         {stdout: "ASIASCOPED scoped-sek"},
		"no Credentials":   {stdout: `{"SessionInfo":{}}`},
		"the flat shape":   {stdout: selfDoc},
		"no expiration":    {stdout: `{"Credentials":{"AccessKeyId":"ASIASCOPED","SecretAccessKey":"scoped-sek","SessionToken":"scoped-tok"}}`},
		"retry also fails": {stderr: insufficientLifetime, err: exit},
	} {
		t.Run(name, func(t *testing.T) {
			var calls []call
			cli := scopedCLI(&calls, r)
			if name == "timeout" {
				cli.run = func(ctx context.Context, bin string, args, env []string) ([]byte, []byte, error) {
					calls = append(calls, call{bin, slices.Clone(args), env})
					<-ctx.Done()
					return []byte(selfDoc), nil, ctx.Err()
				}
			}
			p := scopedPointer()
			c, err := cli.Fetch(context.Background(), p, "brig-x")
			if err == nil {
				t.Error("no error")
			}
			if c != (Credentials{}) {
				t.Errorf("returned keys with the error: %+v", c)
			}
			if err != nil && (strings.Contains(err.Error(), "sek") || strings.Contains(err.Error(), "tok")) {
				t.Errorf("the error carries a key: %v", err)
			}
			wantOnlyScoped(t, calls, p)
		})
	}
}

// The one retry drops --duration-seconds and nothing else. A retry without
// --policy would be the profile's whole identity.
func TestFetchScopedRetryKeepsThePolicy(t *testing.T) {
	var calls []call
	exit := errors.New("exit status 254")
	cli := scopedCLI(&calls, reply{stderr: insufficientLifetime, err: exit}, reply{stdout: scopedDoc(scopedExp.Add(-10 * time.Minute))})
	p := scopedPointer()
	c, err := cli.Fetch(context.Background(), p, "brig-x")
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessKeyID != "ASIASCOPED" {
		t.Errorf("credentials = %+v", c)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	var want []string
	for i := 0; i < len(calls[0].args); i++ {
		if calls[0].args[i] == "--duration-seconds" {
			i++
			continue
		}
		want = append(want, calls[0].args[i])
	}
	if !slices.Equal(calls[1].args, want) || slices.Contains(calls[1].args, "--duration-seconds") {
		t.Errorf("retry argv = %v, want %v", calls[1].args, want)
	}
	wantOnlyScoped(t, calls, p)

	// A second InsufficientSessionLifetime is an error after exactly two
	// calls.
	calls = nil
	cli = scopedCLI(&calls, reply{stderr: insufficientLifetime, err: exit})
	if c, err := cli.Fetch(context.Background(), p, "brig-x"); err == nil || c != (Credentials{}) {
		t.Errorf("got %+v, %v", c, err)
	} else if !strings.Contains(err.Error(), "InsufficientSessionLifetime") {
		t.Errorf("the error lost the reason: %v", err)
	}
	if len(calls) != 2 {
		t.Errorf("calls = %d, want 2", len(calls))
	}
}

// The retry asks for keys that end with the calling session. If that
// session was renewed between the two calls, they could outlive the bound
// the secret names, so the full request runs once more and its answer is the
// one returned.
func TestFetchScopedRetryDoesNotOutliveTheSession(t *testing.T) {
	var calls []call
	exit := errors.New("exit status 254")
	cli := scopedCLI(&calls,
		reply{stderr: insufficientLifetime, err: exit},
		reply{stdout: strings.Replace(scopedDoc(scopedExp.Add(12*time.Hour)), "ASIASCOPED", "ASIALONG", 1)},
		reply{stdout: scopedDoc(scopedExp)})
	p := scopedPointer()
	c, err := cli.Fetch(context.Background(), p, "brig-x")
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessKeyID != "ASIASCOPED" || !c.Expiration.Equal(scopedExp) {
		t.Errorf("credentials = %+v, want the third call's", c)
	}
	if len(calls) != 3 || !slices.Equal(calls[2].args, calls[0].args) {
		t.Errorf("calls = %v, want the full request again", calls)
	}
	wantOnlyScoped(t, calls, p)

	// When that third call fails, the long-lived keys of the retry are not
	// the answer either.
	calls = nil
	cli = scopedCLI(&calls,
		reply{stderr: insufficientLifetime, err: exit},
		reply{stdout: strings.Replace(scopedDoc(scopedExp.Add(12*time.Hour)), "ASIASCOPED", "ASIALONG", 1)},
		reply{stderr: "An error occurred (AccessDenied)", err: exit})
	c, err = cli.Fetch(context.Background(), p, "brig-x")
	if err == nil || c != (Credentials{}) {
		t.Errorf("got %+v, %v; want no keys and an error", c, err)
	}
	if len(calls) != 3 {
		t.Errorf("calls = %d, want 3", len(calls))
	}
	wantOnlyScoped(t, calls, p)
}

func TestFetchScopedArgv(t *testing.T) {
	var calls []call
	cli := scopedCLI(&calls, reply{stdout: scopedDoc(scopedExp)})
	p := scopedPointer()
	p.SessionSeconds = 2700
	if _, err := cli.Fetch(context.Background(), p, SessionName("brig-devbox-api")); err != nil {
		t.Fatal(err)
	}
	want := []string{"sts", "get-scoped-credentials", "--profile", "dev", "--policy", p.ScopePolicy,
		"--scoped-session-description", "brig-devbox-api", "--duration-seconds", "2700", "--output", "json"}
	if !slices.Equal(calls[0].args, want) {
		t.Errorf("argv = %v\nwant %v", calls[0].args, want)
	}
	// STS wants a description of at least two characters.
	calls = nil
	if _, err := cli.Fetch(context.Background(), scopedPointer(), "x"); err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(calls[0].args, "--scoped-session-description"); calls[0].args[i+1] != "brig-x" {
		t.Errorf("a one-character name went out as %q", calls[0].args[i+1])
	}
}

// An aws older than 2.41.2 has no such command. The user can fix that, so
// the error says how, and no other command runs in its place.
func TestFetchScopedNamesTheCLIUpgrade(t *testing.T) {
	for _, stderr := range []string{
		"aws: [ERROR]: An error occurred (ParamValidation): argument operation: Found invalid choice 'get-scoped-credentials'",
		"aws: error: argument operation: Invalid choice, valid choices are:\n\nassume-role | get-caller-identity\nget-session-token",
	} {
		var calls []call
		cli := scopedCLI(&calls, reply{stderr: stderr, err: errors.New("exit status 252")})
		c, err := cli.Fetch(context.Background(), scopedPointer(), "brig-x")
		var old *ErrCLITooOld
		if !errors.As(err, &old) {
			t.Fatalf("err = %v, want *ErrCLITooOld", err)
		}
		if !strings.Contains(err.Error(), "2.41.2") || c != (Credentials{}) {
			t.Errorf("got %+v, %v", c, err)
		}
		if len(calls) != 1 {
			t.Errorf("calls = %d, want 1", len(calls))
		}
	}
}

// A profile with no region reaches the STS global endpoint, which refuses
// the call. The error names the region setting, not the raw InvalidAction.
func TestFetchScopedNamesTheGlobalEndpoint(t *testing.T) {
	var calls []call
	cli := scopedCLI(&calls, reply{stderr: "aws: [ERROR]: An error occurred (InvalidAction) when calling the GetScopedCredentials operation: " +
		"Operation not supported on STS global endpoint", err: errors.New("exit status 254")})
	_, err := cli.Fetch(context.Background(), scopedPointer(), "brig-x")
	var global *ErrGlobalEndpoint
	if !errors.As(err, &global) || global.Profile != "dev" {
		t.Fatalf("err = %v, want *ErrGlobalEndpoint for dev", err)
	}
	if !strings.Contains(err.Error(), "aws configure set region <r> --profile dev") {
		t.Errorf("err = %v, want the fix", err)
	}
}

// Fetch validates the pointer before the switch, so a mode it does not know
// never reaches it. fetchArgs is the switch itself, and a mode it does not
// handle must run nothing: falling through to export-credentials would hand
// out the profile's whole keys.
func TestFetchArgsDefaultRunsNothing(t *testing.T) {
	for _, m := range []Mode{"", "admin", "Scoped", "SELF"} {
		args, err := fetchArgs(Pointer{Kind: "aws", AWSProfile: "dev", Mode: m}, "brig-x")
		if err == nil || args != nil {
			t.Errorf("mode %q: args %v, err %v", m, args, err)
		}
	}
}

func TestExecFakeAWSScoped(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	bin, err := filepath.Abs("testdata/fake-aws")
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "argv")
	cli := CLI{Bin: bin, Env: []string{"PATH=/usr/bin:/bin", "FAKE_AWS_LOG=" + log},
		now: func() time.Time { return time.Date(2026, 10, 26, 14, 30, 0, 0, time.UTC) }}
	p := scopedPointer()
	c, err := cli.Fetch(context.Background(), p, "brig-x")
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessKeyID != "ASIASCOPED" || c.SecretAccessKey != "fake-secret-value" || c.SessionToken != "fake-token-value" {
		t.Errorf("credentials = %+v", c)
	}
	argv, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(argv)), "\n")
	if len(lines) < 2 || lines[0] != "sts" || lines[1] != "get-scoped-credentials" || !slices.Contains(lines, p.ScopePolicy) {
		t.Errorf("argv = %q", lines)
	}
	for _, secret := range []string{c.SecretAccessKey, c.SessionToken} {
		if strings.Contains(string(argv), secret) {
			t.Errorf("argv carries a credential: %s", argv)
		}
	}
}
