package wrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
)

// gcpEndpoint binds claude-code to a gcp route with one service.
const gcpEndpoint = `apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: {name: brokered}
spec:
  routes:
    - name: gcp
      preset: gcp
      credential: {secretRef: {name: gcp-creds}}
      services:
        - name: storage
`

// GOOGLE_APPLICATION_CREDENTIALS, CLOUDSDK_CONFIG and the rest hand gcloud or
// a Google client library an identity besides the route's, so a run with a
// gcp route that also hands the guest one is refused before the hull is
// asked anything. The preset's own gcloud variables are not refused, and a
// run without a gcp route may forward any of them.
func TestAGCPRunThatForwardsGoogleIdentityVariablesIsRefused(t *testing.T) {
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	shortStateDir(t)
	t.Setenv("BRIG_FORWARD_ENV", "GOOGLE_APPLICATION_CREDENTIALS")
	rec := &hullRecorder{}
	cfg := routedConfig(t, gcpEndpoint, rec)
	shortStateDir(t)
	cfg.Env = append(cfg.Env, profile.EnvBinding{Name: "CLOUDSDK_CONFIG", Value: "/elsewhere"})
	err := cfg.routeRefusal()
	if err == nil {
		t.Fatal("a gcp run that forwards GOOGLE_APPLICATION_CREDENTIALS and CLOUDSDK_CONFIG was not refused")
	}
	for _, want := range []string{"GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_CONFIG", "brokered (route gcp)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %s: %v", want, err)
		}
	}
	if rec.linkLocalProbes != 0 {
		t.Errorf("a refused run probed the hull %d times", rec.linkLocalProbes)
	}
	t.Setenv("BRIG_FORWARD_ENV", "")

	// The list docs/security.md states, written out here so a name dropped
	// from gcpShadowVars fails. The prefixes are checked with a name each.
	for _, name := range []string{
		"GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_CONFIG", "CLOUDSDK_CORE_ACCOUNT",
		"CLOUDSDK_ACTIVE_CONFIG_NAME", "CLOUDSDK_AUTH_ACCESS_TOKEN_FILE", "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE",
		"CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT", "GCE_METADATA_HOST", "GCE_METADATA_IP",
	} {
		one := routedConfig(t, gcpEndpoint, &hullRecorder{})
		one.Env = append(one.Env, profile.EnvBinding{Name: name, Value: "x"})
		if err := one.gcpShadowRefusal(); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: gcpShadowRefusal = %v, want a refusal naming it", name, err)
		}
	}

	// The token file the guest points gcloud at is the route's, so its
	// refusal says where to set it instead.
	file := routedConfig(t, gcpEndpoint, &hullRecorder{})
	file.Env = append(file.Env, profile.EnvBinding{Name: "CLOUDSDK_AUTH_ACCESS_TOKEN_FILE", Value: "/root/tok"})
	if err := file.gcpShadowRefusal(); err == nil || !strings.Contains(err.Error(), "The guest sets CLOUDSDK_AUTH_ACCESS_TOKEN_FILE itself") {
		t.Errorf("gcpShadowRefusal = %v, want a refusal that says the guest sets the token file", err)
	}
	one := routedConfig(t, gcpEndpoint, &hullRecorder{})
	one.Env = append(one.Env, profile.EnvBinding{Name: "CLOUDSDK_CONFIG", Value: "/x"})
	if err := one.gcpShadowRefusal(); err == nil || strings.Contains(err.Error(), "The guest sets") {
		t.Errorf("gcpShadowRefusal = %v, want a refusal without the token file note", err)
	}

	for _, name := range []string{
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_STORAGE", "CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE",
		"CLOUDSDK_CORE_PROJECT", "CLOUDSDK_AUTHX", "GOOGLE_CLOUD_PROJECT",
	} {
		one := routedConfig(t, gcpEndpoint, &hullRecorder{})
		one.Env = append(one.Env, profile.EnvBinding{Name: name, Value: "x"})
		if err := one.gcpShadowRefusal(); err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}

	plain := routedConfig(t, claudeEndpoint, &hullRecorder{})
	plain.Env = append(plain.Env, profile.EnvBinding{Name: "GOOGLE_APPLICATION_CREDENTIALS", Value: "/k.json"})
	if err := plain.gcpShadowRefusal(); err != nil {
		t.Errorf("a run with no gcp route was refused for GOOGLE_APPLICATION_CREDENTIALS: %v", err)
	}
}

// A profile that puts anything at ~/.config/gcloud in the guest, or at
// ~/.config, which holds it, is refused on a run with a gcp route: a volume,
// a files: binding, a seed into either, or a seed copied from the host's own
// ~/.config/gcloud or from a directory that holds it. A name that only starts
// with gcloud is not gcloud, and a run with no gcp route is not refused.
func TestAGCPRunRefusesGCloudPutInTheGuestHome(t *testing.T) {
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	shortStateDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	cases := map[string]func(c *Config){
		"volume": func(c *Config) {
			c.Profile.Volumes = append(c.Profile.Volumes, profile.Volume{Kind: "hostmount", Path: ".config/gcloud"})
		},
		"volume on .config": func(c *Config) {
			c.Profile.Volumes = append(c.Profile.Volumes, profile.Volume{Kind: "volume", Path: ".config"})
		},
		"tmpfs at": func(c *Config) {
			c.Profile.Volumes = append(c.Profile.Volumes, profile.Volume{Kind: "tmpfs", Path: "x", At: c.Profile.GuestHome + "/.config/gcloud/configurations"})
		},
		"file": func(c *Config) {
			c.Profile.Files = append(c.Profile.Files, profile.FileBinding{Ref: "secrets.gcp", Path: ".config/gcloud/application_default_credentials.json"})
		},
		"seed into": func(c *Config) {
			c.HostConfig = append(c.HostConfig, hostSeed{Host: "/elsewhere", Rel: ".config/gcloud"})
		},
		"seed of .config": func(c *Config) {
			c.HostConfig = append(c.HostConfig, hostSeed{Host: filepath.Join(home, ".config"), Rel: ".config"})
		},
		"host ~/.config/gcloud": func(c *Config) {
			c.HostConfig = append(c.HostConfig, hostSeed{Host: filepath.Join(home, ".config", "gcloud", "configurations"), Rel: "cfg"})
		},
		"host ~": func(c *Config) { c.HostConfig = append(c.HostConfig, hostSeed{Host: home, Rel: "copy"}) },
	}
	for name, add := range cases {
		cfg := routedConfig(t, gcpEndpoint, &hullRecorder{})
		add(cfg)
		if err := cfg.routeRefusal(); err == nil || !strings.Contains(err.Error(), "~/.config/gcloud") {
			t.Errorf("%s: routeRefusal = %v, want the ~/.config/gcloud refusal", name, err)
		}
		plain := routedConfig(t, claudeEndpoint, &hullRecorder{})
		add(plain)
		if err := plain.gcpHomeRefusal(); err != nil {
			t.Errorf("%s: a run with no gcp route was refused: %v", name, err)
		}
	}
	for name, add := range map[string]func(c *Config){
		"gcloudx": func(c *Config) {
			c.Profile.Volumes = append(c.Profile.Volumes, profile.Volume{Kind: "hostmount", Path: ".config/gcloudx"})
		},
		"other .config": func(c *Config) {
			c.HostConfig = append(c.HostConfig, hostSeed{Host: filepath.Join(home, ".config", "gh"), Rel: ".config/gh"})
		},
	} {
		near := routedConfig(t, gcpEndpoint, &hullRecorder{})
		add(near)
		if err := near.gcpHomeRefusal(); err != nil {
			t.Errorf("%s was taken for gcloud: %v", name, err)
		}
	}
}

// A guest home that already holds a gcloud identity file, such as `--home ~`,
// is refused on a run with a gcp route, before brig writes anything into it.
// A .config/gcloud that is a planted link is refused as well, and its target
// is not touched. What gcloud in the guest writes there itself on its first
// run holds no identity, and is allowed, so a kept home boots a second time.
func TestAGCPRunRefusesAGuestHomeWithAGCloudIdentity(t *testing.T) {
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	shortStateDir(t)
	for _, rel := range append([]string{"link", "link inside"}, gcloudIdentityFiles...) {
		cfg := routedConfig(t, gcpEndpoint, &hullRecorder{})
		outside := filepath.Join(t.TempDir(), "credentials.db")
		if err := os.WriteFile(outside, []byte("sqlite"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(cfg.Workspace, ".config"), 0o700); err != nil {
			t.Fatal(err)
		}
		switch rel {
		case "link":
			if err := os.Symlink(filepath.Dir(outside), filepath.Join(cfg.Workspace, gcloudDir)); err != nil {
				t.Fatal(err)
			}
		case "link inside":
			// A link that stays in the home is not refused by the root, so
			// this one is refused by the check of .config/gcloud alone.
			if err := os.MkdirAll(filepath.Join(cfg.Workspace, "elsewhere"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../elsewhere", filepath.Join(cfg.Workspace, gcloudDir)); err != nil {
				t.Fatal(err)
			}
		default:
			if err := os.MkdirAll(filepath.Dir(filepath.Join(cfg.Workspace, rel)), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfg.Workspace, rel), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		err := cfg.PrepareWorkspace()
		if err == nil || !strings.Contains(err.Error(), ".config/gcloud") || !strings.Contains(err.Error(), "gcp route") {
			t.Fatalf("%s: PrepareWorkspace = %v, want the .config/gcloud refusal", rel, err)
		}
		if _, err := os.Stat(filepath.Join(cfg.Workspace, markerFile)); !os.IsNotExist(err) {
			t.Errorf("%s: the refused run wrote its marker: %v", rel, err)
		}
		if b, _ := os.ReadFile(outside); string(b) != "sqlite" {
			t.Errorf("%s: the file behind the link changed: %q", rel, b)
		}
	}

	// What gcloud wrote to a guest home on a real first run with the route.
	kept := routedConfig(t, gcpEndpoint, &hullRecorder{})
	for _, f := range []string{"active_config", "configurations/config_default", "default_configs.db", "gce",
		".last_survey_prompt.yaml", "logs/2026.11.09/12.00.00.000000.log"} {
		p := filepath.Join(kept.Workspace, gcloudDir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := kept.PrepareWorkspace(); err != nil {
		t.Errorf("a guest home with only what gcloud writes for itself was refused: %v", err)
	}

	plain := routedConfig(t, claudeEndpoint, &hullRecorder{})
	p := filepath.Join(plain.Workspace, gcloudDir, "credentials.db")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := plain.PrepareWorkspace(); err != nil {
		t.Errorf("a run with no gcp route was refused for its ~/.config/gcloud: %v", err)
	}
}
