package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/awscred"
)

const (
	awsCaller = "arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_Developer_x/someone@example.com"
	awsSecret = "S3CR3T"
)

// fakeAWS writes a stand-in aws binary into a temp dir and points newAWSCLI at
// it. The script logs one call per line and answers per subcommand, so a test
// asserts on what the importer asked for rather than on a double of awscred.
// region is what `configure get region` prints.
func fakeAWS(t *testing.T, region string) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$*" >> "` + log + `"
case "$1 $2" in
"sts get-caller-identity") echo '{"Arn":"` + awsCaller + `"}' ;;
"configure get") echo '` + region + `' ;;
"sts assume-role") echo '{"Credentials":{"AccessKeyId":"ASIAFAKE","SecretAccessKey":"` + awsSecret + `","SessionToken":"tok","Expiration":"2026-10-26T15:04:05+00:00"}}' ;;
*) echo "fake aws: unexpected $*" >&2; exit 2 ;;
esac
`
	bin := filepath.Join(dir, "aws")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := newAWSCLI
	newAWSCLI = func() awscred.CLI { return awscred.CLI{Bin: bin, Env: []string{}} }
	t.Cleanup(func() { newAWSCLI = old })
	return log
}

func calls(t *testing.T, log string) string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// awsProfiles declares a profile whose one secret comes from aws, and the
// shipped-style claude-code stand-in whose source is not aws.
func awsProfiles(t *testing.T) {
	t.Helper()
	loadProfiles(t, `
name: devbox
image: ghcr.io/brig-sh/devbox:latest
guestHome: /home/devbox
binary: devbox
mem: 1024
cpus: 1
secrets:
  - name: aws-creds
    required: false
    from: aws
`, `
name: claude-code
image: ghcr.io/brig-sh/claude-code:latest
guestHome: /home/claude
binary: claude
mem: 1024
cpus: 1
secrets:
  - name: claude-credentials
    required: false
    from: file
    path: /nonexistent
`)
}

func captureImportStderr(t *testing.T) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	old := importStderr
	importStderr = &b
	t.Cleanup(func() { importStderr = old })
	return &b
}

const roleARN = "arn:aws:iam::1:role/r"

func TestImportAWSRole(t *testing.T) {
	awsProfiles(t)
	store := newAnnotating(t)
	log := fakeAWS(t, "eu-west-1")

	err := importSecrets(&bytes.Buffer{}, []string{"devbox", "aws-creds", "--aws-profile", "dev", "--role-arn", roleARN})
	if err != nil {
		t.Fatal(err)
	}
	p, err := awscred.ParsePointer(store.items["aws-creds"])
	if err != nil {
		t.Fatalf("stored doc: %v", err)
	}
	want := awscred.Pointer{Kind: "aws", AWSProfile: "dev", Mode: awscred.ModeRole, RoleARN: roleARN, SourceIdentity: "someone@example.com"}
	if p != want {
		t.Errorf("pointer = %+v, want %+v", p, want)
	}
	got := calls(t, log)
	for _, w := range []string{"sts get-caller-identity", "configure get region", "sts assume-role", "--role-session-name brig-import-check"} {
		if !strings.Contains(got, w) {
			t.Errorf("the fake aws never saw %q:\n%s", w, got)
		}
	}
	if from := store.prov["aws-creds"].From; from != "aws:dev" {
		t.Errorf("provenance = %q", from)
	}
}

func TestImportAWSSourceIdentityFlagOverrides(t *testing.T) {
	awsProfiles(t)
	store := newAnnotating(t)
	fakeAWS(t, "eu-west-1")
	err := importSecrets(&bytes.Buffer{}, []string{"devbox", "--aws-profile", "dev", "--role-arn", roleARN, "--source-identity", "ci-bot"})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := awscred.ParsePointer(store.items["aws-creds"])
	if p.SourceIdentity != "ci-bot" {
		t.Errorf("SourceIdentity = %q", p.SourceIdentity)
	}
}

func TestImportAWSSelf(t *testing.T) {
	awsProfiles(t)
	store := newAnnotating(t)
	log := fakeAWS(t, "eu-west-1")
	stderr := captureImportStderr(t)

	if err := importSecrets(&bytes.Buffer{}, []string{"devbox", "--aws-profile", "dev", "--identity", "self"}); err != nil {
		t.Fatal(err)
	}
	p, err := awscred.ParsePointer(store.items["aws-creds"])
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != awscred.ModeSelf || p.RoleARN != "" {
		t.Errorf("pointer = %+v", p)
	}
	if !strings.Contains(stderr.String(), "your own identity "+awsCaller) {
		t.Errorf("stderr = %q", stderr.String())
	}
	if strings.Contains(calls(t, log), "assume-role") {
		t.Error("self mode called assume-role")
	}
}

func TestImportAWSNeedsAnAnswer(t *testing.T) {
	awsProfiles(t)
	store := newAnnotating(t)
	fakeAWS(t, "eu-west-1")

	err := importSecrets(&bytes.Buffer{}, []string{"devbox", "--aws-profile", "dev"})
	if err == nil || !strings.Contains(err.Error(), "say how the sandbox gets AWS: --role-arn <arn> (recommended) or --identity self") {
		t.Errorf("neither: err = %v", err)
	}
	err = importSecrets(&bytes.Buffer{}, []string{"devbox", "--aws-profile", "dev", "--role-arn", roleARN, "--identity", "self"})
	if err == nil || !strings.Contains(err.Error(), "--role-arn and --identity self are two answers; pass one") {
		t.Errorf("both: err = %v", err)
	}
	if len(store.items) != 0 {
		t.Errorf("stored %v", store.items)
	}
}

func TestImportAWSNeedsAProfile(t *testing.T) {
	awsProfiles(t)
	store := newAnnotating(t)
	fakeAWS(t, "eu-west-1")
	err := importSecrets(&bytes.Buffer{}, []string{"devbox", "--role-arn", roleARN})
	if err == nil || !strings.Contains(err.Error(), "--aws-profile") {
		t.Errorf("err = %v", err)
	}
	if len(store.items) != 0 {
		t.Errorf("stored %v", store.items)
	}
}

func TestImportAWSRefusesOtherIdentity(t *testing.T) {
	awsProfiles(t)
	store := newAnnotating(t)
	fakeAWS(t, "eu-west-1")
	err := importSecrets(&bytes.Buffer{}, []string{"devbox", "--aws-profile", "dev", "--identity", "admin"})
	if err == nil || !strings.Contains(err.Error(), "self") {
		t.Errorf("err = %v", err)
	}
	if len(store.items) != 0 {
		t.Errorf("stored %v", store.items)
	}
}

func TestImportAWSFlagsNeedAWSSource(t *testing.T) {
	awsProfiles(t)
	store := newAnnotating(t)
	useHost(t, nil)
	err := importSecrets(&bytes.Buffer{}, []string{"claude-code", "--aws-profile", "dev"})
	if err == nil || !strings.Contains(err.Error(), "--aws-profile is for a secret whose source is aws") {
		t.Errorf("err = %v", err)
	}
	if len(store.items) != 0 {
		t.Errorf("stored %v", store.items)
	}
}

func TestImportAWSNoRegion(t *testing.T) {
	awsProfiles(t)
	store := newAnnotating(t)
	fakeAWS(t, "")
	err := importSecrets(&bytes.Buffer{}, []string{"devbox", "--aws-profile", "dev", "--role-arn", roleARN})
	if err == nil || !strings.Contains(err.Error(), "aws configure set region") {
		t.Errorf("err = %v", err)
	}
	if len(store.items) != 0 {
		t.Errorf("stored %v", store.items)
	}
}

func TestImportAWSStoresNoCredential(t *testing.T) {
	awsProfiles(t)
	store := newAnnotating(t)
	fakeAWS(t, "eu-west-1")
	var out bytes.Buffer
	if err := importSecrets(&out, []string{"devbox", "--aws-profile", "dev", "--role-arn", roleARN}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(store.items["aws-creds"]), awsSecret) || strings.Contains(out.String(), awsSecret) {
		t.Error("the vended secret key reached the store or the output")
	}
}

func TestImportAWSDryRunWritesNothing(t *testing.T) {
	awsProfiles(t)
	store := newAnnotating(t)
	log := fakeAWS(t, "eu-west-1")
	err := importSecrets(&bytes.Buffer{}, []string{"devbox", "--aws-profile", "dev", "--role-arn", roleARN, "--dry-run"})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.items) != 0 || len(store.calls) != 0 {
		t.Errorf("dry run wrote: %v %v", store.items, store.calls)
	}
	if !strings.Contains(calls(t, log), "assume-role") {
		t.Error("dry run skipped the checks")
	}
}

// A stub for the missing binary: the user needs the install hint, not
// "executable file not found".
func TestImportAWSMissingBinaryGivesTheInstallHint(t *testing.T) {
	awsProfiles(t)
	newAnnotating(t)
	old := newAWSCLI
	newAWSCLI = func() awscred.CLI { return awscred.CLI{Bin: filepath.Join(t.TempDir(), "no-aws"), Env: []string{}} }
	t.Cleanup(func() { newAWSCLI = old })
	err := importSecrets(&bytes.Buffer{}, []string{"devbox", "--aws-profile", "dev", "--role-arn", roleARN})
	if err == nil || !strings.Contains(err.Error(), awscred.InstallHint) {
		t.Errorf("err = %v", err)
	}
}
