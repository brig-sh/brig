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
	// awsSessionToken is the fake's SessionToken, distinct enough that its
	// absence from the store means something.
	awsSessionToken = "F4KE-SESSI0N-T0KEN"
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
"sts assume-role") echo '{"Credentials":{"AccessKeyId":"ASIAFAKE","SecretAccessKey":"` + awsSecret + `","SessionToken":"` + awsSessionToken + `","Expiration":"2026-10-26T15:04:05+00:00"}}' ;;
*) echo "fake aws: unexpected $*" >&2; exit 2 ;;
esac
`
	installFakeAWS(t, dir, script)
	return log
}

// installFakeAWS writes script as the aws binary in dir and points newAWSCLI
// at it.
func installFakeAWS(t *testing.T, dir, script string) {
	t.Helper()
	bin := filepath.Join(dir, "aws")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := newAWSCLI
	newAWSCLI = func() awscred.CLI { return awscred.CLI{Bin: bin, Env: []string{}} }
	t.Cleanup(func() { newAWSCLI = old })
}

// realNewAWSCLI is the default seam, kept before any test replaces it.
var realNewAWSCLI = newAWSCLI

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
	// Every call names the profile: one without it would resolve whatever the
	// CLI's default is, and the check would vouch for the wrong identity.
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if !strings.Contains(line, "--profile dev") {
			t.Errorf("an aws call did not carry --profile dev: %q", line)
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
	if strings.Contains(string(store.items["aws-creds"]), awsSessionToken) ||
		strings.Contains(out.String(), awsSessionToken) {
		t.Error("the vended session token reached the store or the output")
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

// Self mode keeps no source identity, so a flag that asks for one is a
// misunderstanding to refuse and not a value to drop.
func TestImportAWSSelfRefusesASourceIdentity(t *testing.T) {
	awsProfiles(t)
	store := newAnnotating(t)
	fakeAWS(t, "eu-west-1")
	err := importSecrets(&bytes.Buffer{}, []string{"devbox", "--aws-profile", "dev", "--identity", "self", "--source-identity", "me"})
	if err == nil || !strings.Contains(err.Error(), "--source-identity") {
		t.Errorf("err = %v", err)
	}
	if len(store.items) != 0 {
		t.Errorf("stored %v", store.items)
	}
}

// The import's aws subprocess must not see the user's AWS_ variables, or its
// check would pass on an identity the broker never resolves.
func TestDefaultAWSCLIDropsAWSVariables(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
	t.Setenv("AWS_PROFILE", "someone-else")
	t.Setenv("BRIG_TEST_KEPT", "1")
	env := realNewAWSCLI().Env
	kept := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "AWS_") {
			t.Errorf("Env carries %q", kv)
		}
		kept = kept || kv == "BRIG_TEST_KEPT=1"
	}
	if !kept {
		t.Error("Env dropped a non-AWS variable")
	}
}

// The pointer is validated before anything is stored, so a malformed value is
// refused at import and not by the broker at the guest's first call.
func TestImportAWSValidatesThePointer(t *testing.T) {
	for _, args := range [][]string{
		{"--role-arn", "not-an-arn"},
		{"--role-arn", roleARN, "--source-identity", "a b"},
	} {
		awsProfiles(t)
		store := newAnnotating(t)
		fakeAWS(t, "eu-west-1")
		err := importSecrets(&bytes.Buffer{}, append([]string{"devbox", "--aws-profile", "dev"}, args...))
		if err == nil {
			t.Errorf("%v accepted", args)
		}
		if len(store.items) != 0 {
			t.Errorf("%v stored %v", args, store.items)
		}
	}
}

func TestImportAWSExpiredSSOGivesTheLoginHint(t *testing.T) {
	awsProfiles(t)
	store := newAnnotating(t)
	installFakeAWS(t, t.TempDir(), "#!/bin/sh\necho 'Error loading SSO Token: Token has expired' >&2\nexit 255\n")
	err := importSecrets(&bytes.Buffer{}, []string{"devbox", "--aws-profile", "dev", "--role-arn", roleARN})
	if err == nil || !strings.Contains(err.Error(), "aws sso login --profile dev") {
		t.Errorf("err = %v", err)
	}
	if len(store.items) != 0 {
		t.Errorf("stored %v", store.items)
	}
}

// One set of aws flags names one profile and one role. Applied to every aws
// secret of a profile, it would store the same identity under each name,
// where the user meant a different one for each.
func TestImportAWSFlagsAnswerForOneSecret(t *testing.T) {
	loadProfiles(t, `
name: devbox
image: ghcr.io/brig-sh/devbox:latest
guestHome: /home/devbox
binary: devbox
mem: 1024
cpus: 1
secrets:
  - name: aws-dev
    required: false
    from: aws
  - name: aws-ops
    required: false
    from: aws
`)
	store := newAnnotating(t)
	log := fakeAWS(t, "eu-west-1")
	for _, args := range [][]string{
		{"devbox", "--aws-profile", "dev", "--role-arn", roleARN},
		{"devbox", "aws-dev", "aws-ops", "--aws-profile", "dev", "--role-arn", roleARN},
	} {
		err := importSecrets(&bytes.Buffer{}, args)
		want := "the AWS flags answer for one secret; name it: brig secret import devbox <secret>"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: err = %v, want %q", args, err, want)
		}
	}
	if len(store.items) != 0 {
		t.Errorf("stored %v", store.items)
	}
	if got := calls(t, log); got != "" {
		t.Errorf("aws ran for a refused import:\n%s", got)
	}
}
