package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/awscred"
	"github.com/brig-sh/brig/internal/gcpcred"
)

const (
	gcpSA      = "agent@proj.iam.gserviceaccount.com"
	gcpAccount = "someone@example.com"
	// gcpToken is the fake's access token. It starts ya29 like a real one,
	// so its absence from the store and the output means something.
	gcpToken = "ya29.F4KE-ACCESS-T0KEN"
)

// fakeGCloud writes a stand-in gcloud binary into a temp dir, starts a fake
// tokeninfo that reports email, and points newGCloudCLI at both. The script
// logs one call per line, so a test asserts on what the importer asked for.
func fakeGCloud(t *testing.T, email string) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$*" >> "` + log + `"
case "$1 $2" in
"auth print-access-token") echo '{"token":"` + gcpToken + `"}' ;;
"config get-value") echo '` + gcpAccount + `' ;;
*) echo "fake gcloud: unexpected $*" >&2; exit 2 ;;
esac
`
	installFakeGCloud(t, dir, script, email)
	return log
}

// installFakeGCloud writes script as the gcloud binary in dir and points
// newGCloudCLI at it and at a tokeninfo that reports email.
func installFakeGCloud(t *testing.T, dir, script, email string) {
	t.Helper()
	bin := filepath.Join(dir, "gcloud")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"expires_in":"3599","email":"`+email+`"}`)
	}))
	t.Cleanup(srv.Close)
	old := newGCloudCLI
	newGCloudCLI = func() gcpcred.CLI {
		return gcpcred.CLI{Bin: bin, Env: []string{}, TokenInfoURL: srv.URL, Client: srv.Client()}
	}
	t.Cleanup(func() { newGCloudCLI = old })
}

// gcloudProfiles declares a profile whose one secret comes from gcloud, one
// with an aws and a gcloud secret, and a stand-in whose source is neither.
func gcloudProfiles(t *testing.T) {
	t.Helper()
	loadProfiles(t, `
name: gcpbox
image: ghcr.io/brig-sh/devbox:latest
guestHome: /home/devbox
binary: devbox
mem: 1024
cpus: 1
secrets:
  - name: gcp-creds
    required: false
    from: gcloud
`, `
name: cloudbox
image: ghcr.io/brig-sh/devbox:latest
guestHome: /home/devbox
binary: devbox
mem: 1024
cpus: 1
secrets:
  - name: aws-creds
    required: false
    from: aws
  - name: gcp-creds
    required: false
    from: gcloud
`, `
name: envbox
image: ghcr.io/brig-sh/devbox:latest
guestHome: /home/devbox
binary: devbox
mem: 1024
cpus: 1
secrets:
  - name: env-token
    required: false
    from: env
    var: BRIG_TEST_GCP_UNSET
`)
}

func TestGCloudImportStoresAPointerNotAToken(t *testing.T) {
	gcloudProfiles(t)
	store := newAnnotating(t)
	log := fakeGCloud(t, gcpSA)
	stderr := captureImportStderr(t)
	var out bytes.Buffer

	err := importSecrets(&out, []string{"gcpbox", "--gcloud-configuration", "default", "--impersonate-service-account", gcpSA})
	if err != nil {
		t.Fatal(err)
	}
	p, err := gcpcred.ParsePointer(store.items["gcp-creds"])
	if err != nil {
		t.Fatalf("stored doc: %v", err)
	}
	want := gcpcred.Pointer{Kind: "gcloud", Configuration: "default", Mode: gcpcred.ModeImpersonate, ServiceAccount: gcpSA}
	if p != want {
		t.Errorf("pointer = %+v, want %+v", p, want)
	}
	// The check minted a token; it is dropped, never stored or printed.
	for what, b := range map[string]string{"store": string(store.items["gcp-creds"]), "stdout": out.String(), "stderr": stderr.String()} {
		if strings.Contains(b, "ya29") {
			t.Errorf("the minted token reached the %s: %q", what, b)
		}
	}
	if from := store.prov["gcp-creds"].From; from != "gcloud:default" {
		t.Errorf("provenance = %q", from)
	}
	wantMsg := "gcp: the sandbox will act as " + gcpSA + " (impersonated by " + gcpAccount + ")"
	if !strings.Contains(stderr.String(), wantMsg) {
		t.Errorf("stderr = %q, want %q", stderr.String(), wantMsg)
	}
	// Every call names the configuration: one without it would resolve the
	// active configuration, and the check would vouch for the wrong login.
	got := calls(t, log)
	if !strings.Contains(got, "auth print-access-token") || !strings.Contains(got, "--impersonate-service-account "+gcpSA) {
		t.Errorf("the fake gcloud did not mint as the service account:\n%s", got)
	}
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if !strings.Contains(line, "--configuration default") {
			t.Errorf("a gcloud call did not carry --configuration default: %q", line)
		}
	}
}

func TestGCloudImportNeedsAMode(t *testing.T) {
	gcloudProfiles(t)
	store := newAnnotating(t)
	log := fakeGCloud(t, gcpSA)
	err := importSecrets(&bytes.Buffer{}, []string{"gcpbox", "--gcloud-configuration", "default"})
	want := "say how the sandbox gets GCP: --impersonate-service-account <email> (recommended) or --identity self"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want %q", err, want)
	}
	if len(store.items) != 0 {
		t.Errorf("stored %v", store.items)
	}
	if got := calls(t, log); got != "" {
		t.Errorf("gcloud ran for a refused import:\n%s", got)
	}
}

func TestGCloudImportRefusesTwoModes(t *testing.T) {
	gcloudProfiles(t)
	store := newAnnotating(t)
	fakeGCloud(t, gcpSA)
	err := importSecrets(&bytes.Buffer{}, []string{"gcpbox", "--gcloud-configuration", "default",
		"--impersonate-service-account", gcpSA, "--identity", "self"})
	want := "--impersonate-service-account and --identity self are two answers; pass one"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want %q", err, want)
	}
	if len(store.items) != 0 {
		t.Errorf("stored %v", store.items)
	}
}

func TestGCloudImportNeedsAConfiguration(t *testing.T) {
	gcloudProfiles(t)
	store := newAnnotating(t)
	fakeGCloud(t, gcpSA)
	for _, args := range [][]string{
		{"gcpbox", "--impersonate-service-account", gcpSA},
		{"gcpbox", "--identity", "self"},
	} {
		err := importSecrets(&bytes.Buffer{}, args)
		want := "--gcloud-configuration is required: name the local gcloud configuration the sandbox's tokens come from"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: err = %v, want %q", args, err, want)
		}
	}
	if len(store.items) != 0 {
		t.Errorf("stored %v", store.items)
	}
}

func TestGCloudImportSelfIsDeliberate(t *testing.T) {
	gcloudProfiles(t)
	store := newAnnotating(t)
	log := fakeGCloud(t, gcpAccount)
	stderr := captureImportStderr(t)
	if err := importSecrets(&bytes.Buffer{}, []string{"gcpbox", "--gcloud-configuration", "default", "--identity", "self"}); err != nil {
		t.Fatal(err)
	}
	p, err := gcpcred.ParsePointer(store.items["gcp-creds"])
	if err != nil {
		t.Fatal(err)
	}
	// The account is stored, so a later mint is of this login and not of
	// whichever account the configuration names by then.
	want := gcpcred.Pointer{Kind: "gcloud", Configuration: "default", Mode: gcpcred.ModeSelf, Account: gcpAccount}
	if p != want {
		t.Errorf("pointer = %+v, want %+v", p, want)
	}
	if !strings.Contains(stderr.String(), "gcp: the sandbox will get your own identity "+gcpAccount) {
		t.Errorf("stderr = %q", stderr.String())
	}
	got := calls(t, log)
	if strings.Contains(got, "--impersonate-service-account") {
		t.Error("self mode impersonated")
	}
	// The account is read first, and the check mints as that account.
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "config get-value account") ||
		!strings.HasPrefix(lines[1], "auth print-access-token "+gcpAccount+" ") {
		t.Errorf("gcloud calls:\n%s", got)
	}
}

// A configuration with no account has no login to pin. The import is refused
// before any mint, and nothing is stored.
func TestGCloudImportSelfNeedsAnAccount(t *testing.T) {
	gcloudProfiles(t)
	store := newAnnotating(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	installFakeGCloud(t, dir, `#!/bin/sh
echo "$*" >> "`+log+`"
case "$1 $2" in
"auth print-access-token") echo '{"token":"`+gcpToken+`"}' ;;
"config get-value") echo '' ;;
*) echo "fake gcloud: unexpected $*" >&2; exit 2 ;;
esac
`, gcpAccount)
	err := importSecrets(&bytes.Buffer{}, []string{"gcpbox", "--gcloud-configuration", "default", "--identity", "self"})
	want := "the gcloud configuration default has no account; run gcloud auth login --configuration default"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want %q", err, want)
	}
	if len(store.items) != 0 {
		t.Errorf("stored %v", store.items)
	}
	if got := calls(t, log); strings.Contains(got, "print-access-token") {
		t.Errorf("gcloud minted for a configuration with no account:\n%s", got)
	}
}

// A self token whose tokeninfo names no email does not prove it is the
// account the pointer records, so the import is refused and stores nothing.
func TestGCloudImportSelfWithNoEmail(t *testing.T) {
	gcloudProfiles(t)
	store := newAnnotating(t)
	fakeGCloud(t, "")
	err := importSecrets(&bytes.Buffer{}, []string{"gcpbox", "--gcloud-configuration", "default", "--identity", "self"})
	if err == nil || !strings.Contains(err.Error(), gcpAccount) {
		t.Errorf("err = %v, want a refusal naming %s", err, gcpAccount)
	}
	if err != nil && strings.Contains(err.Error(), "ya29") {
		t.Errorf("error carries the token: %v", err)
	}
	if len(store.items) != 0 {
		t.Errorf("stored %v", store.items)
	}
}

func TestGCloudImportRefusesWhenTheMintFails(t *testing.T) {
	gcloudProfiles(t)
	store := newAnnotating(t)
	installFakeGCloud(t, t.TempDir(), "#!/bin/sh\necho 'ERROR: (gcloud.auth.print-access-token) Permission iam.serviceAccounts.getAccessToken denied' >&2\nexit 1\n", gcpSA)
	err := importSecrets(&bytes.Buffer{}, []string{"gcpbox", "--gcloud-configuration", "default", "--impersonate-service-account", gcpSA})
	if err == nil || !strings.Contains(err.Error(), "serviceAccountTokenCreator") {
		t.Errorf("err = %v", err)
	}
	if len(store.items) != 0 {
		t.Errorf("stored %v", store.items)
	}
}

// brig-broker reads gcloud logins from $HOME/.config/gcloud, so an import
// under another CLOUDSDK_CONFIG would vouch for a login the sandbox never
// gets. It is refused before gcloud runs, and nothing is stored.
func TestGCloudImportRefusesWhileCloudSDKConfigIsSet(t *testing.T) {
	gcloudProfiles(t)
	store := newAnnotating(t)
	log := fakeGCloud(t, gcpSA)
	t.Setenv("CLOUDSDK_CONFIG", t.TempDir())
	for _, args := range [][]string{
		{"gcpbox", "--gcloud-configuration", "default", "--impersonate-service-account", gcpSA},
		{"gcpbox", "--gcloud-configuration", "default", "--identity", "self", "--dry-run"},
	} {
		err := importSecrets(&bytes.Buffer{}, args)
		if err == nil || !strings.Contains(err.Error(), "CLOUDSDK_CONFIG is set") ||
			!strings.Contains(err.Error(), "$HOME/.config/gcloud") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
	if len(store.items) != 0 {
		t.Errorf("stored %v", store.items)
	}
	if got := calls(t, log); got != "" {
		t.Errorf("gcloud ran for a refused import:\n%s", got)
	}
}

// A stub for the missing binary: the user needs the install hint, not
// "executable file not found".
func TestGCloudImportMissingBinaryGivesTheInstallHint(t *testing.T) {
	gcloudProfiles(t)
	store := newAnnotating(t)
	old := newGCloudCLI
	newGCloudCLI = func() gcpcred.CLI {
		return gcpcred.CLI{Bin: filepath.Join(t.TempDir(), "no-gcloud"), Env: []string{}}
	}
	t.Cleanup(func() { newGCloudCLI = old })
	err := importSecrets(&bytes.Buffer{}, []string{"gcpbox", "--gcloud-configuration", "default", "--impersonate-service-account", gcpSA})
	if err == nil || !strings.Contains(err.Error(), gcpcred.InstallHint) {
		t.Errorf("err = %v", err)
	}
	if len(store.items) != 0 {
		t.Errorf("stored %v", store.items)
	}
}

// The pointer is validated before gcloud runs, so a value that would reach
// gcloud's argv as a flag is refused at import.
func TestGCloudImportValidatesThePointer(t *testing.T) {
	for _, args := range [][]string{
		{"--gcloud-configuration", "--verbosity=debug", "--identity", "self"},
		{"--gcloud-configuration", "default", "--impersonate-service-account", "not-a-service-account"},
	} {
		gcloudProfiles(t)
		store := newAnnotating(t)
		log := fakeGCloud(t, gcpSA)
		err := importSecrets(&bytes.Buffer{}, append([]string{"gcpbox"}, args...))
		if err == nil {
			t.Errorf("%v accepted", args)
		}
		if len(store.items) != 0 {
			t.Errorf("%v stored %v", args, store.items)
		}
		if got := calls(t, log); got != "" {
			t.Errorf("%v: gcloud ran:\n%s", args, got)
		}
	}
}

// --identity is shared by the aws and gcloud sources. On a secret with
// neither, it must still be refused rather than dropped without a word.
func TestIdentityOnASecretWithNoCloudSourceIsRefused(t *testing.T) {
	gcloudProfiles(t)
	store := newAnnotating(t)
	useHost(t, nil)
	err := importSecrets(&bytes.Buffer{}, []string{"envbox", "--identity", "self"})
	if err == nil || !strings.Contains(err.Error(), "--identity is for a secret whose source is aws") {
		t.Errorf("err = %v", err)
	}
	if len(store.items) != 0 {
		t.Errorf("stored %v", store.items)
	}
}

// One --identity on a line that selects an aws and a gcloud secret would
// answer for both, where the user may have meant it for one.
func TestIdentityOnAnAWSAndAGCloudSecretNeedsAName(t *testing.T) {
	gcloudProfiles(t)
	store := newAnnotating(t)
	fakeAWS(t, "eu-west-1")
	fakeGCloud(t, gcpAccount)
	err := importSecrets(&bytes.Buffer{}, []string{"cloudbox", "--identity", "self"})
	if err == nil || !strings.Contains(err.Error(), "name it: brig secret import cloudbox <secret>") {
		t.Errorf("err = %v", err)
	}
	if len(store.items) != 0 {
		t.Errorf("stored %v", store.items)
	}
}

func TestGCloudAndAWSFlagsOnOneLineAreRefused(t *testing.T) {
	gcloudProfiles(t)
	store := newAnnotating(t)
	awsLog := fakeAWS(t, "eu-west-1")
	gcpLog := fakeGCloud(t, gcpSA)
	for _, args := range [][]string{
		{"cloudbox", "--aws-profile", "dev", "--gcloud-configuration", "default"},
		{"cloudbox", "gcp-creds", "--role-arn", roleARN, "--impersonate-service-account", gcpSA},
		{"cloudbox", "--source-identity", "me", "--gcloud-configuration", "default"},
		{"cloudbox", "--session-duration", "30m", "--impersonate-service-account", gcpSA},
	} {
		err := importSecrets(&bytes.Buffer{}, args)
		want := "the aws and gcloud flags fill different secrets; import each on its own line"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: err = %v, want %q", args, err, want)
		}
	}
	if len(store.items) != 0 {
		t.Errorf("stored %v", store.items)
	}
	if calls(t, awsLog) != "" || calls(t, gcpLog) != "" {
		t.Error("a cloud CLI ran for a refused import")
	}
}

func TestGCloudFlagsOnASecretWithoutAGCloudSource(t *testing.T) {
	gcloudProfiles(t)
	store := newAnnotating(t)
	useHost(t, nil)
	for flag, args := range map[string][]string{
		"--gcloud-configuration":        {"envbox", "--gcloud-configuration", "default"},
		"--impersonate-service-account": {"envbox", "--impersonate-service-account", gcpSA},
	} {
		err := importSecrets(&bytes.Buffer{}, args)
		want := flag + " is for a secret whose source is gcloud, and \"env-token\" has none"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: err = %v, want %q", args, err, want)
		}
	}
	if len(store.items) != 0 {
		t.Errorf("stored %v", store.items)
	}
}

// One set of gcloud flags names one configuration and one service account.
// Applied to two gcloud secrets, it would store the same identity under both.
func TestGCloudFlagsAnswerForOneSecret(t *testing.T) {
	loadProfiles(t, `
name: gcpbox
image: ghcr.io/brig-sh/devbox:latest
guestHome: /home/devbox
binary: devbox
mem: 1024
cpus: 1
secrets:
  - name: gcp-dev
    required: false
    from: gcloud
  - name: gcp-ops
    required: false
    from: gcloud
`)
	store := newAnnotating(t)
	log := fakeGCloud(t, gcpSA)
	err := importSecrets(&bytes.Buffer{}, []string{"gcpbox", "--gcloud-configuration", "default", "--impersonate-service-account", gcpSA})
	want := "the gcloud flags answer for one secret; name it: brig secret import gcpbox <secret> --gcloud-configuration"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want %q", err, want)
	}
	if len(store.items) != 0 {
		t.Errorf("stored %v", store.items)
	}
	if got := calls(t, log); got != "" {
		t.Errorf("gcloud ran for a refused import:\n%s", got)
	}
}

// --identity became shared with the gcloud source; an aws self import must
// behave as it did before.
func TestIdentitySelfStillWorksForAWS(t *testing.T) {
	gcloudProfiles(t)
	store := newAnnotating(t)
	awsLog := fakeAWS(t, "eu-west-1")
	gcpLog := fakeGCloud(t, gcpAccount)
	stderr := captureImportStderr(t)
	if err := importSecrets(&bytes.Buffer{}, []string{"cloudbox", "aws-creds", "--aws-profile", "dev", "--identity", "self"}); err != nil {
		t.Fatal(err)
	}
	p, err := awscred.ParsePointer(store.items["aws-creds"])
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != awscred.ModeSelf {
		t.Errorf("pointer = %+v", p)
	}
	if _, ok := store.items["gcp-creds"]; ok {
		t.Error("an aws import stored the gcloud secret")
	}
	if !strings.Contains(stderr.String(), "aws: the sandbox will get your own identity "+awsCaller) {
		t.Errorf("stderr = %q", stderr.String())
	}
	if calls(t, awsLog) == "" || calls(t, gcpLog) != "" {
		t.Errorf("aws calls %q, gcloud calls %q", calls(t, awsLog), calls(t, gcpLog))
	}
}

// The import's gcloud subprocess gets gcpcred.Environ's environment, the
// broker's rule, so its check does not pass on an identity the user's shell
// chose and the broker never sees.
func TestDefaultGCloudCLIDropsIdentityVariables(t *testing.T) {
	t.Setenv("CLOUDSDK_CORE_ACCOUNT", "someone-else@example.com")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/tmp/key.json")
	t.Setenv("BRIG_TEST_KEPT", "1")
	env := realNewGCloudCLI().Env
	kept := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "CLOUDSDK_CORE_ACCOUNT=") || strings.HasPrefix(kv, "GOOGLE_APPLICATION_CREDENTIALS=") {
			t.Errorf("Env carries %q", kv)
		}
		kept = kept || kv == "BRIG_TEST_KEPT=1"
	}
	if !kept {
		t.Error("Env dropped an unrelated variable")
	}
}

// realNewGCloudCLI is the default seam, kept before any test replaces it.
var realNewGCloudCLI = newGCloudCLI

// The gcloud flags are read off the line, so a missing, contradictory or
// misplaced one is a usage error, and nothing is stored or run.
func TestGCloudImportFlagMistakesExitTwo(t *testing.T) {
	gcloudProfiles(t)
	store := newAnnotating(t)
	log := fakeGCloud(t, gcpSA)
	useHost(t, nil)
	for _, args := range [][]string{
		{"gcpbox", "--gcloud-configuration", "default"},
		{"gcpbox", "--gcloud-configuration", "default", "--impersonate-service-account", gcpSA, "--identity", "self"},
		{"gcpbox", "--impersonate-service-account", gcpSA},
		{"envbox", "--gcloud-configuration", "default"},
		{"cloudbox", "--aws-profile", "dev", "--gcloud-configuration", "default"},
		{"cloudbox", "--identity", "self"},
	} {
		err := importSecrets(&bytes.Buffer{}, args)
		if got := exitCode(err); got != exitUsage {
			t.Errorf("%v: exit %d, want %d: %v", args, got, exitUsage, err)
		}
	}
	if len(store.items) != 0 {
		t.Errorf("stored %v", store.items)
	}
	if got := calls(t, log); got != "" {
		t.Errorf("gcloud ran for a refused import:\n%s", got)
	}
}

// CLOUDSDK_CONFIG is the host's setting, not something on the line, so its
// refusal is a general failure.
func TestGCloudImportCloudSDKConfigIsNotAUsageError(t *testing.T) {
	gcloudProfiles(t)
	newAnnotating(t)
	fakeGCloud(t, gcpSA)
	t.Setenv("CLOUDSDK_CONFIG", t.TempDir())
	err := importSecrets(&bytes.Buffer{}, []string{"gcpbox", "--gcloud-configuration", "default", "--impersonate-service-account", gcpSA})
	if got := exitCode(err); got != exitFailure {
		t.Errorf("exit %d, want %d: %v", got, exitFailure, err)
	}
}
