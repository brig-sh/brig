package wrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
)

// A profile that puts anything at ~/.aws in the guest is refused on a run
// with a route that vends: a volume, a files: binding, a seed into .aws, or
// a seed copied from the host's own ~/.aws. A name that only starts with
// .aws is not .aws, and a run with no route that vends is not refused.
func TestAVendingRunRefusesAWSPutInTheGuestHome(t *testing.T) {
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	shortStateDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	cases := map[string]func(c *Config){
		"volume": func(c *Config) {
			c.Profile.Volumes = append(c.Profile.Volumes, profile.Volume{Kind: "hostmount", Path: ".aws"})
		},
		"tmpfs at": func(c *Config) {
			c.Profile.Volumes = append(c.Profile.Volumes, profile.Volume{Kind: "tmpfs", Path: "x", At: c.Profile.GuestHome + "/.aws/sso"})
		},
		"file": func(c *Config) {
			c.Profile.Files = append(c.Profile.Files, profile.FileBinding{Ref: "secrets.aws", Path: ".aws/credentials"})
		},
		"seed into": func(c *Config) { c.HostConfig = append(c.HostConfig, hostSeed{Host: "/elsewhere", Rel: ".aws"}) },
		"host ~/.aws": func(c *Config) {
			c.HostConfig = append(c.HostConfig, hostSeed{Host: filepath.Join(home, ".aws", "sso"), Rel: "cfg"})
		},
	}
	for name, add := range cases {
		cfg := routedConfig(t, awsEndpoint, &hullRecorder{})
		add(cfg)
		if err := cfg.routeRefusal(); err == nil || !strings.Contains(err.Error(), "~/.aws") {
			t.Errorf("%s: routeRefusal = %v, want the ~/.aws refusal", name, err)
		}
		plain := routedConfig(t, claudeEndpoint, &hullRecorder{})
		add(plain)
		if err := plain.awsHomeRefusal(); err != nil {
			t.Errorf("%s: a run with no route that vends was refused: %v", name, err)
		}
	}
	near := routedConfig(t, awsEndpoint, &hullRecorder{})
	near.Profile.Volumes = append(near.Profile.Volumes, profile.Volume{Kind: "hostmount", Path: ".awsome"})
	if err := near.awsHomeRefusal(); err != nil {
		t.Errorf(".awsome was taken for .aws: %v", err)
	}
}

// A guest home that already holds ~/.aws/credentials or ~/.aws/config, such
// as `--home ~`, is refused on a run with a route that vends, before brig
// writes anything into it. A .aws that is a planted link is refused as well,
// and its target is not touched. The CLI's own cache, which the aws CLI in
// the guest writes to ~/.aws/cli, is not an identity and is allowed.
func TestAVendingRunRefusesAGuestHomeWithAWS(t *testing.T) {
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	shortStateDir(t)
	for _, planted := range []bool{false, true} {
		cfg := routedConfig(t, awsEndpoint, &hullRecorder{})
		if err := os.MkdirAll(cfg.Workspace, 0o755); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "creds")
		if err := os.WriteFile(outside, []byte("[default]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if planted {
			if err := os.Symlink(filepath.Dir(outside), filepath.Join(cfg.Workspace, ".aws")); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.MkdirAll(filepath.Join(cfg.Workspace, ".aws"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfg.Workspace, ".aws", "config"), []byte("[default]\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		err := cfg.PrepareWorkspace()
		if err == nil || !strings.Contains(err.Error(), ".aws") {
			t.Fatalf("planted=%v: PrepareWorkspace = %v, want the .aws refusal", planted, err)
		}
		if _, err := os.Stat(filepath.Join(cfg.Workspace, markerFile)); !os.IsNotExist(err) {
			t.Errorf("planted=%v: the refused run wrote its marker: %v", planted, err)
		}
		if b, _ := os.ReadFile(outside); string(b) != "[default]\n" {
			t.Errorf("planted=%v: the file behind the link changed: %q", planted, b)
		}
	}

	cache := routedConfig(t, awsEndpoint, &hullRecorder{})
	if err := os.MkdirAll(filepath.Join(cache.Workspace, ".aws", "cli", "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := cache.PrepareWorkspace(); err != nil {
		t.Errorf("a guest home with only the CLI's cache in .aws was refused: %v", err)
	}

	plain := routedConfig(t, claudeEndpoint, &hullRecorder{})
	if err := os.MkdirAll(filepath.Join(plain.Workspace, ".aws"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := plain.PrepareWorkspace(); err != nil {
		t.Errorf("a run with no route that vends was refused for its ~/.aws: %v", err)
	}
}

// A .aws that is a regular file holds no identity file, so a vending run on
// that home is not refused for it.
func TestAVendingRunAllowsAGuestHomeWhoseAWSIsAFile(t *testing.T) {
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	shortStateDir(t)
	cfg := routedConfig(t, awsEndpoint, &hullRecorder{})
	if err := os.MkdirAll(cfg.Workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Workspace, ".aws"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cfg.PrepareWorkspace(); err != nil {
		t.Errorf("a guest home whose .aws is a file was refused: %v", err)
	}
}
