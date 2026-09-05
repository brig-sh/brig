package wrap

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
)

func gitConfig(t *testing.T, ws string) *Config {
	t.Helper()
	c := testConfig(t, ws, ws)
	c.GitConfig = true
	c.GitUser = "octocat"
	c.GitUserFromHost = true
	return c
}

func TestSetupGitWritesManagedFilesWithoutASecret(t *testing.T) {
	ws := t.TempDir()
	c := gitConfig(t, ws)
	var set creds.Set
	set.Add("GH_TOKEN", "ghp_secret", "")
	if err := c.SetupGit(&set); err != nil {
		t.Fatal(err)
	}

	helper, err := os.ReadFile(filepath.Join(ws, credentialHelper))
	if err != nil {
		t.Fatal(err)
	}
	// Putting these files in the workspace is only acceptable because neither
	// holds a credential: the helper reads the token from the environment.
	if bytes.Contains(helper, []byte("ghp_secret")) {
		t.Errorf("the credential helper holds a token: %s", helper)
	}
	if !bytes.Contains(helper, []byte("GH_TOKEN")) {
		t.Errorf("the helper does not read GH_TOKEN: %s", helper)
	}
	st, err := os.Stat(filepath.Join(ws, credentialHelper))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&0o111 == 0 {
		t.Errorf("the credential helper is not executable: %v", st.Mode())
	}

	managed, err := os.ReadFile(filepath.Join(ws, managedGitconfig))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(managed, []byte("ghp_secret")) {
		t.Errorf("the managed gitconfig holds a token: %s", managed)
	}
	// git treats the scp-style and ssh:// spellings as distinct strings and
	// does not derive one from the other, so both need their own insteadOf.
	for _, want := range []string{"insteadOf = git@github.com:", "insteadOf = ssh://git@github.com/"} {
		if !bytes.Contains(managed, []byte(want)) {
			t.Errorf("managed gitconfig is missing %q: %s", want, managed)
		}
	}
	// The username travels as environment, so changing it needs no rewrite.
	if !set.Has("BRIG_GIT_USER") {
		t.Error("the git username is not forwarded")
	}
}

func TestSetupGitWiresTheIncludeOnce(t *testing.T) {
	ws := t.TempDir()
	c := gitConfig(t, ws)
	var set creds.Set
	for i := 0; i < 3; i++ {
		if err := c.SetupGit(&set); err != nil {
			t.Fatal(err)
		}
	}
	blob, err := os.ReadFile(filepath.Join(ws, ".gitconfig"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(blob), "[include]"); n != 1 {
		t.Errorf("the include was appended %d times:\n%s", n, blob)
	}
}

// A .gitconfig whose last line carries no newline gets the appended section
// glued onto it, and git then reads the result as a `path` key of the
// preceding section: the string is present, so a grep concludes "already
// wired" while the include never loads. Silently, and for good.
func TestSetupGitSurvivesAMissingTrailingNewline(t *testing.T) {
	ws := t.TempDir()
	target := filepath.Join(ws, ".gitconfig")
	if err := os.WriteFile(target, []byte("[user]\n\tname = someone"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := gitConfig(t, ws)
	var set creds.Set
	if err := c.SetupGit(&set); err != nil {
		t.Fatal(err)
	}
	blob, _ := os.ReadFile(target)
	if !strings.Contains(string(blob), "name = someone\n[include]") {
		t.Errorf("the append did not get a line of its own:\n%s", blob)
	}
	// And git actually loads it, which is the thing a grep cannot tell you.
	if !includeIsLive(t, target) {
		t.Errorf("git does not see the include:\n%s", blob)
	}
}

// includeIsLive asks git whether the include actually loads, which is the
// question a grep for the path cannot answer.
func includeIsLive(t *testing.T, target string) bool {
	t.Helper()
	var out bytes.Buffer
	cmd := exec.Command("git", "config", "-f", target, "--get-all", "include.path")
	cmd.Stdout = &out
	_ = cmd.Run()
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.TrimSpace(line) == includePath {
			return true
		}
	}
	return false
}

// hosts.yml maps a host to indented keys, so an unanchored search returns
// whichever host comes first -- an Enterprise login paired with a github.com
// token, which is exactly the "Invalid username or token" this prevents.
func TestGhHostsUserReadsTheRightStanza(t *testing.T) {
	dir := t.TempDir()
	hosts := `github.enterprise.example:
    user: enterprise-login
    oauth_token: xxx
github.com:
    user: octocat
    oauth_token: yyy
`
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_CONFIG_DIR", dir)

	for _, tc := range []struct{ host, want string }{
		{"github.com", "octocat"},
		{"github.enterprise.example", "enterprise-login"},
		{"github.absent.example", ""},
	} {
		got, err := ghHostsUser(tc.host)
		if err != nil {
			t.Fatalf("ghHostsUser(%s): %v", tc.host, err)
		}
		if got != tc.want {
			t.Errorf("ghHostsUser(%s) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

func TestSetupGitIsOffByDefault(t *testing.T) {
	ws := t.TempDir()
	c := testConfig(t, ws, ws) // GitConfig stays false
	var set creds.Set
	if err := c.SetupGit(&set); err != nil {
		t.Fatal(err)
	}
	// Opting in is what creates files in your workspace; credential
	// forwarding alone needs no opt-in and writes nothing.
	if _, err := os.Stat(filepath.Join(ws, managedGitconfig)); !os.IsNotExist(err) {
		t.Error("the managed gitconfig was written without the opt-in")
	}
}
