package verify

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// isolateDocker points DOCKER_CONFIG and HOME at empty directories, so no test
// reads the Docker config of the machine it runs on. It returns the
// DOCKER_CONFIG directory.
func isolateDocker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dir)
	t.Setenv("HOME", t.TempDir())
	return dir
}

func writeDockerConfig(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// timedOutResolve is Verify for a reference whose resolve cosign did not answer.
func timedOutResolve(t *testing.T, ref string) Result {
	t.Helper()
	digestStub(t, deprecation+"\n", timedOut, nil)
	return DefaultPolicy().Verify(ref, "")
}

// Docker Desktop not running, with credsStore set to it, is the reported
// cause: cosign asks the helper for ghcr.io credentials and the helper never
// answers. The timeout names the helper, where it is set, and the way round it.
func TestATimeoutNamesTheCredentialHelper(t *testing.T) {
	for _, tc := range []struct {
		what, ref, body string
		want            []string
		not             []string
		defaultPath     bool
	}{
		{
			what: "credsStore", ref: "ghcr.io/brig-sh/claude-code:arm64",
			body: `{"credsStore":"desktop"}`,
			want: []string{"docker-credential-desktop", "credsStore", "DOCKER_CONFIG"},
		},
		{
			what: "credHelpers for the host", ref: "ghcr.io/brig-sh/claude-code:arm64",
			body: `{"credsStore":"desktop","credHelpers":{"ghcr.io":"osxkeychain"}}`,
			want: []string{"docker-credential-osxkeychain", "credHelpers", "DOCKER_CONFIG"},
			not:  []string{"docker-credential-desktop"},
		},
		{
			// The host is matched in the spelling Docker keys it on.
			what: "credHelpers for another spelling of the host", ref: "GHCR.io:443/brig-sh/claude-code:arm64",
			body: `{"credHelpers":{"ghcr.io":"osxkeychain"}}`,
			want: []string{"docker-credential-osxkeychain", "credHelpers"},
		},
		{
			what: "the default path", ref: "ghcr.io/brig-sh/claude-code:arm64",
			body: `{"credsStore":"desktop"}`, defaultPath: true,
			want: []string{"docker-credential-desktop", "credsStore", "DOCKER_CONFIG"},
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			dir := isolateDocker(t)
			if tc.defaultPath {
				t.Setenv("DOCKER_CONFIG", "")
				dir = filepath.Join(os.Getenv("HOME"), ".docker")
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			path := writeDockerConfig(t, dir, tc.body)
			got := timedOutResolve(t, tc.ref)
			msg := got.Message()
			for _, w := range append(tc.want, path, "did not answer") {
				if !strings.Contains(msg, w) {
					t.Errorf("the message does not name %q: %q", w, msg)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(msg, n) {
					t.Errorf("the message names %q: %q", n, msg)
				}
			}
		})
	}
}

// The file cosign's helper is named in can also hold registry credentials, in
// auths. The hint reads two fields and prints nothing else from it.
func TestTheTimeoutHintNeverEchoesDockerAuths(t *testing.T) {
	dir := isolateDocker(t)
	writeDockerConfig(t, dir, `{"credsStore":"desktop","auths":{"ghcr.io":{"auth":"c2VjcmV0LXRva2Vu"}}}`)
	msg := timedOutResolve(t, "ghcr.io/brig-sh/claude-code:arm64").Message()
	if !strings.Contains(msg, "docker-credential-desktop") {
		t.Errorf("the message does not name the helper: %q", msg)
	}
	for _, leak := range []string{"c2VjcmV0LXRva2Vu", "secret-token", "auths"} {
		if strings.Contains(msg, leak) {
			t.Errorf("the message echoes %q from the Docker config: %q", leak, msg)
		}
	}
}

// With no helper to name, the timeout names none, and nothing from a file it
// cannot make sense of reaches the message.
func TestATimeoutWithNoUsableDockerConfigNamesNoHelper(t *testing.T) {
	for _, tc := range []struct{ what, body string }{
		{"no config.json", ""},
		{"malformed JSON", `{"credsStore": "desk`},
		{"credHelpers for another host", `{"credHelpers":{"quay.io":"ecr-login"}}`},
		{"an empty credsStore", `{"credsStore":""}`},
	} {
		t.Run(tc.what, func(t *testing.T) {
			dir := isolateDocker(t)
			if tc.body != "" {
				writeDockerConfig(t, dir, tc.body)
			}
			msg := timedOutResolve(t, "ghcr.io/brig-sh/claude-code:arm64").Message()
			if !strings.Contains(msg, "did not answer") {
				t.Errorf("the message does not say cosign did not answer: %q", msg)
			}
			for _, n := range []string{"docker-credential", "quay.io", "ecr-login", "desk", "config.json"} {
				if strings.Contains(msg, n) {
					t.Errorf("the message names %q: %q", n, msg)
				}
			}
		})
	}
}

// The hint is for a cosign that hung. A cosign that failed for another reason
// said why, and a credential helper in the Docker config is beside the point.
func TestANonTimeoutFailureNamesNoCredentialHelper(t *testing.T) {
	dir := isolateDocker(t)
	writeDockerConfig(t, dir, `{"credsStore":"desktop"}`)
	p := DefaultPolicy()

	digestStub(t, deprecation+"\n"+denied+"\n", errExit1, nil)
	resolve := p.Verify("ghcr.io/brig-sh/claude-code:arm64", "")
	stub(t, true, deprecation+"\n"+denied+"\n", errExit1)
	check := p.Image("ghcr.io/brig-sh/claude-code:arm64")

	for what, got := range map[string]Result{"resolve": resolve, "signature check": check} {
		if got.TimedOut {
			t.Errorf("%s: a failure that did not time out is marked TimedOut", what)
		}
		for _, s := range []string{got.Detail, got.Message()} {
			if strings.Contains(s, "docker-credential") || strings.Contains(s, "DOCKER_CONFIG") {
				t.Errorf("%s: names a credential helper it did not wait on: %q", what, s)
			}
		}
	}
}

// The hint is built after cosign has already cost the user 30s, and it must
// not add a hang of its own. A config.json that is a FIFO blocks open(2) until
// a writer appears, so it is not read at all, and the timeout names no helper.
func TestATimeoutDoesNotHangOnAConfigThatIsNotAFile(t *testing.T) {
	dir := isolateDocker(t)
	path := filepath.Join(dir, "config.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot make a FIFO here: %v", err)
	}
	// A reader stuck in open(2) is released by a writer opening the FIFO, so a
	// failing run does not leave a blocked thread behind for the tests after it.
	t.Cleanup(func() {
		if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
		}
	})

	done := make(chan string, 1)
	go func() {
		_, helper, _ := credentialHelper("ghcr.io/brig-sh/claude-code:arm64")
		done <- helper
	}()
	select {
	case helper := <-done:
		if helper != "" {
			t.Errorf("a FIFO config named helper %q", helper)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("credentialHelper blocked on a config.json that is a FIFO")
	}
}
