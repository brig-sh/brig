package wrap

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/brig-sh/brig/internal/creds"
)

// The two managed files brig writes into the workspace when guest git is
// enabled. Neither holds a secret: the token travels as environment and the
// helper reads it there. That is what makes putting them in the workspace
// acceptable at all.
const (
	credentialHelper = ".brig-git-credential"
	managedGitconfig = ".gitconfig.brig"
	includePath      = "~/" + managedGitconfig
)

// resolveGitIdentity works out who the guest commits as, and which username
// pairs with the forwarded token.
//
// Identity is read from the host's git config *as seen from the invoking
// directory*, so per-directory includeIf rules carry into the guest. It is
// forwarded as environment rather than written into the managed gitconfig:
// that file is global to the guest, identity varies per invocation, and env
// takes precedence over config.
//
// Signing config is deliberately not forwarded. A host gpg.ssh.program or
// gpg-agent points at a signer that exists only on the host and that the
// guest can neither execute nor reach, so forwarding commit.gpgsign would
// make every guest commit fail rather than degrade. Guest commits are
// unsigned.
func (c *Config) resolveGitIdentity() {
	c.GitName = c.env.String("GIT_NAME", c.gitConfigValue("user.name"))
	c.GitEmail = c.env.String("GIT_EMAIL", c.gitConfigValue("user.email"))

	if user, ok := c.hostGitUser(); ok {
		c.GitUserFromHost = true
		c.GitUser = c.env.String("GIT_USER", user)
		return
	}
	// GitHub requires exactly x-access-token for a GitHub App installation
	// token and generally ignores the username for a PAT, so that is the
	// fallback -- but it is not universal, which is why setup_git reports it
	// rather than letting a later "Invalid username or token" look like a
	// broken sandbox.
	c.GitUser = c.env.String("GIT_USER", "x-access-token")
}

func (c *Config) gitConfigValue(key string) string {
	cmd := exec.Command("git", "config", "--get", key)
	cmd.Dir = c.Cwd
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

// hostGitUser finds the GitHub login to pair with the token: github.user
// first, then the gh CLI's own record.
func (c *Config) hostGitUser() (string, bool) {
	u := c.gitConfigValue("github.user")
	if u == "" {
		u = ghHostsUser(c.primaryGitHost())
	}
	// A GitHub login never contains whitespace; ignore anything that does.
	// user.name is a display name, and is the usual thing to get wrong here.
	if u == "" || strings.ContainsAny(u, " \t") {
		return "", false
	}
	return u, true
}

func (c *Config) primaryGitHost() string {
	if len(c.GitHosts) == 0 {
		return "github.com"
	}
	return c.GitHosts[0]
}

// ghHostsUser reads the login gh recorded for one host.
//
// hosts.yml maps a host to indented keys, so an unanchored search returns
// whichever host comes first -- an Enterprise login paired with a github.com
// token, which is exactly the "Invalid username or token" this exists to
// prevent. Only the stanza for the host being configured is read.
func ghHostsUser(host string) string {
	dir := os.Getenv("GH_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config", "gh")
	}
	f, err := os.Open(filepath.Join(dir, "hosts.yml"))
	if err != nil {
		return ""
	}
	defer f.Close()

	inHost := false
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := scan.Text()
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") &&
			!strings.HasPrefix(strings.TrimSpace(line), "#") {
			inHost = strings.TrimSpace(line) == host+":"
			continue
		}
		if !inHost {
			continue
		}
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && k == "user" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// SetupGit writes the managed git files into the workspace and adds the
// plumbing variables that make them work.
//
// The workspace is a host directory mounted as the guest home, so this is
// written host-side. Both files are regenerated on every run so an upgrade
// takes effect, and are wired in by an [include] appended once to .gitconfig
// -- your own .gitconfig is never rewritten.
func (c *Config) SetupGit(set *creds.Set) error {
	if !c.GitConfig {
		return nil
	}
	if err := os.MkdirAll(c.Workspace, 0o755); err != nil {
		return err
	}
	if !c.GitUserFromHost {
		if _, overridden := c.env.Get("GIT_USER"); !overridden {
			c.warnf("github.user is not set, falling back to %q. If git in the sandbox "+
				"reports \"Invalid username or token\" while your token is valid, set it "+
				"once on the host: `git config --global github.user <your-github-login>`",
				c.GitUser)
		}
	}

	// The username travels as environment, not baked into the helper, so
	// changing it takes effect without regenerating anything. URUNC_GIT_USER
	// goes too, for a workspace still carrying the Homebrew wrapper's helper.
	set.AddPlumbing("BRIG_GIT_USER", c.GitUser)
	set.AddPlumbing("URUNC_GIT_USER", c.GitUser)

	helper := filepath.Join(c.Workspace, credentialHelper)
	if err := writeIfChanged(helper, credentialHelperScript, 0o755); err != nil {
		return err
	}
	if err := writeIfChanged(filepath.Join(c.Workspace, managedGitconfig),
		c.managedGitconfig(), 0o644); err != nil {
		return err
	}
	return c.wireInclude()
}

// credentialHelperScript answers git's credential query from GH_TOKEN in the
// guest environment, so no token is ever written to disk. A silent no-op when
// GH_TOKEN is absent, which lets git report its own error rather than failing
// on an empty password.
const credentialHelperScript = `#!/bin/sh
# Managed by brig -- regenerated every run, local edits are lost.
[ "$1" = get ] || exit 0
[ -n "${GH_TOKEN:-}" ] || exit 0
printf 'username=%s\n' "${BRIG_GIT_USER:-${URUNC_GIT_USER:-x-access-token}}"
printf 'password=%s\n' "$GH_TOKEN"
`

func (c *Config) managedGitconfig() string {
	var b strings.Builder
	b.WriteString("# Managed by brig -- regenerated every run, local edits are lost.\n")
	b.WriteString("# Sends SSH GitHub remotes over HTTPS instead, authenticated by GH_TOKEN.\n")
	for _, host := range c.GitHosts {
		// Both SSH spellings need their own insteadOf: git treats the
		// scp-style `git@host:` and the `ssh://git@host/` form as distinct
		// strings and does not derive one from the other.
		fmt.Fprintf(&b, "\n[url \"https://%s/\"]\n\tinsteadOf = git@%s:\n\tinsteadOf = ssh://git@%s/\n",
			host, host, host)
		fmt.Fprintf(&b, "[credential \"https://%s\"]\n\thelper = %s/%s\n",
			host, c.Profile.GuestHome, credentialHelper)
	}
	return b.String()
}

// wireInclude appends the [include] to the workspace .gitconfig, once.
//
// It asks git whether the include is in effect rather than grepping for the
// path. A .gitconfig whose last line carries no newline gets the appended
// section glued onto it, and git then reads the result as a `path` key of the
// preceding section: the string is present, so a grep concludes "already
// wired" while the include never loads. Silently, and for good.
func (c *Config) wireInclude() error {
	target := filepath.Join(c.Workspace, ".gitconfig")

	cmd := exec.Command("git", "config", "-f", target, "--get-all", "include.path")
	var out bytes.Buffer
	cmd.Stdout = &out
	_ = cmd.Run()
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.TrimSpace(line) == includePath {
			return nil
		}
	}

	existing, _ := os.ReadFile(target)
	if bytes.Contains(existing, []byte(managedGitconfig)) {
		c.warnf("%s names %s but git does not load it -- an earlier append landed on a "+
			"line with no trailing newline. Re-adding it properly; check the line above "+
			"it, it may have picked up a stray '[include]'.", target, managedGitconfig)
	}

	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	// Give the appended section a line of its own, whatever came before.
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		if _, err := f.WriteString("\n"); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(f, "[include]\n\tpath = %s\n", includePath)
	return err
}
