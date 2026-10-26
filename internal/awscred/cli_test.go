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
		"--role-session-name", "brig-x", "--source-identity", "me@example.com", "--duration-seconds", "3600", "--output", "json"}
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
	out, _, err := execRun(context.Background(), "/usr/bin/env", nil, nil)
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
