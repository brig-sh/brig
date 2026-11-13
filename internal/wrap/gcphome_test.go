package wrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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
	for _, want := range []string{"GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_CONFIG", "gcp (policy brokered)"} {
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
	if err := file.gcpShadowRefusal(); err == nil || !strings.Contains(err.Error(), "the guest sets CLOUDSDK_AUTH_ACCESS_TOKEN_FILE itself") {
		t.Errorf("gcpShadowRefusal = %v, want a refusal that says the guest sets the token file", err)
	}
	one := routedConfig(t, gcpEndpoint, &hullRecorder{})
	one.Env = append(one.Env, profile.EnvBinding{Name: "CLOUDSDK_CONFIG", Value: "/x"})
	if err := one.gcpShadowRefusal(); err == nil || strings.Contains(err.Error(), "the guest sets") {
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
// a files: binding, the onboarding file, a seed into either, or a seed copied
// from the host's own ~/.config/gcloud or from a directory that holds it, in
// any case. A name that only starts
// with gcloud is not gcloud, and a run with no gcp route is not refused.
func TestAGCPRunRefusesGCloudPutInTheGuestHome(t *testing.T) {
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	shortStateDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, d := range []string{".config/gcloud", ".config/gh"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(c *Config){
		"volume": func(c *Config) {
			c.Profile.Volumes = append(c.Profile.Volumes, profile.Volume{Kind: "hostmount", Path: ".config/gcloud"})
		},
		"volume on .config": func(c *Config) {
			c.Profile.Volumes = append(c.Profile.Volumes, profile.Volume{Kind: "volume", Path: ".config"})
		},
		"file": func(c *Config) {
			c.Profile.Files = append(c.Profile.Files, profile.FileBinding{Ref: "secrets.gcp", Path: ".config/gcloud/application_default_credentials.json"})
		},
		// macOS file systems are case-insensitive, so these land in
		// ~/.config/gcloud too.
		"volume, mixed case": func(c *Config) {
			c.Profile.Volumes = append(c.Profile.Volumes, profile.Volume{Kind: "hostmount", Path: ".Config/GCloud"})
		},
		"file, mixed case": func(c *Config) {
			c.Profile.Files = append(c.Profile.Files, profile.FileBinding{Ref: "secrets.gcp", Path: ".Config/gcloud/application_default_credentials.json"})
		},
		"onboarding, mixed case": func(c *Config) {
			c.Profile.Onboarding = &profile.Onboarding{File: ".CONFIG/gcloud/application_default_credentials.json",
				Seed: map[string]any{"type": "authorized_user"}}
		},
		"seed into, mixed case": func(c *Config) {
			c.HostConfig = append(c.HostConfig, hostSeed{Host: "/elsewhere", Rel: ".config/GCLOUD"})
		},
		"host ~/.Config/gcloud": func(c *Config) {
			c.HostConfig = append(c.HostConfig, hostSeed{Host: filepath.Join(home, ".Config", "gcloud"), Rel: "cfg"})
		},
		// macOS also folds some letters that are not ASCII: .conﬁg, with
		// the ligature ﬁ, is .config there.
		"file, ligature": func(c *Config) {
			c.Profile.Files = append(c.Profile.Files, profile.FileBinding{Ref: "secrets.gcp", Path: ".conﬁg/gcloud/application_default_credentials.json"})
		},
		"onboarding, ligature": func(c *Config) {
			c.Profile.Onboarding = &profile.Onboarding{File: ".conﬁg/gcloud/application_default_credentials.json",
				Seed: map[string]any{"type": "authorized_user"}}
		},
		"seed into, ligature": func(c *Config) {
			c.HostConfig = append(c.HostConfig, hostSeed{Host: "/elsewhere", Rel: ".conﬁg"})
		},
		"host ~/.conﬁg": func(c *Config) {
			c.HostConfig = append(c.HostConfig, hostSeed{Host: filepath.Join(home, ".conﬁg"), Rel: "cfg"})
		},
		// A seed into the home itself copies the host directory's
		// .config/gcloud along with the rest. projectPaths is not checked
		// for .., so a profile can name one.
		"seed into the home": func(c *Config) {
			host := filepath.Join(t.TempDir(), "evil")
			if err := os.MkdirAll(filepath.Join(host, gcloudDir), 0o700); err != nil {
				t.Fatal(err)
			}
			seeds := hostProjections(profile.Profile{HostConfigDir: filepath.Join(host, "home"), ProjectPaths: []string{".."}}, true)
			if len(seeds) != 1 || seeds[0].Rel != "." {
				t.Fatalf("hostProjections = %+v, want one seed into the home", seeds)
			}
			c.HostConfig = append(c.HostConfig, seeds...)
		},
		"onboarding": func(c *Config) {
			c.Profile.Onboarding = &profile.Onboarding{File: ".config/gcloud/application_default_credentials.json",
				Seed: map[string]any{"type": "authorized_user"}}
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
		// The seed copies through links, so the path it resolves to counts.
		"host ~/.config/gcloud by its real path": func(c *Config) {
			c.HostConfig = append(c.HostConfig, hostSeed{Host: filepath.Join(realHome, ".config", "gcloud"), Rel: "cfg"})
		},
		"host ~/.config/gcloud through a link": func(c *Config) {
			link := filepath.Join(t.TempDir(), "link")
			if err := os.Symlink(filepath.Join(home, ".config", "gcloud"), link); err != nil {
				t.Fatal(err)
			}
			c.HostConfig = append(c.HostConfig, hostSeed{Host: link, Rel: "cfg"})
		},
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
		"a name that is not ASCII": func(c *Config) {
			c.Profile.Volumes = append(c.Profile.Volumes, profile.Volume{Kind: "hostmount", Path: "café"})
			c.HostConfig = append(c.HostConfig, hostSeed{Host: "/elsewhere", Rel: ".config/ghé"})
		},
		"a name with no ASCII": func(c *Config) {
			c.Profile.Files = append(c.Profile.Files, profile.FileBinding{Ref: "secrets.gcp", Path: "日本語"})
			c.Profile.Volumes = append(c.Profile.Volumes, profile.Volume{Kind: "hostmount", Path: "é/ü/x"})
			c.HostConfig = append(c.HostConfig, hostSeed{Host: filepath.Join(home, "Проекты"), Rel: "cfg"})
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

// HOME moves ~/.config/gcloud as CLOUDSDK_CONFIG does, and the checks of the
// profile and of the workspace look only at the guest home's, so a gcp run
// whose profile moves HOME is refused. A HOME set to the guest home itself is
// not, and a run without a gcp route may set any.
func TestAGCPRunThatMovesHOMEIsRefused(t *testing.T) {
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	shortStateDir(t)
	for _, b := range []profile.EnvBinding{
		{Name: "HOME", Value: "/root/alt"},
		{Name: "HOME", Ref: "env.ALT_HOME"},
		{Name: "HOME", Refs: []string{"env.ALT_HOME", "secrets.alt-home"}},
	} {
		cfg := routedConfig(t, gcpEndpoint, &hullRecorder{})
		cfg.Env = append(cfg.Env, b)
		err := cfg.gcpShadowRefusal()
		if err == nil || !strings.Contains(err.Error(), "HOME must be the guest home") {
			t.Errorf("%+v: gcpShadowRefusal = %v, want the HOME refusal", b, err)
		}
	}

	same := routedConfig(t, gcpEndpoint, &hullRecorder{})
	same.Env = append(same.Env, profile.EnvBinding{Name: "HOME", Value: same.Profile.GuestHome + "/"})
	if err := same.gcpShadowRefusal(); err != nil {
		t.Errorf("a HOME set to the guest home was refused: %v", err)
	}

	plain := routedConfig(t, claudeEndpoint, &hullRecorder{})
	plain.Env = append(plain.Env, profile.EnvBinding{Name: "HOME", Value: "/root/alt"})
	if err := plain.gcpShadowRefusal(); err != nil {
		t.Errorf("a run with no gcp route was refused for HOME: %v", err)
	}
}

// gcloud creates credentials.db and access_tokens.db with their tables and no
// rows whenever it opens its store, so a kept home where gcloud ran with no
// login boots again. A store with a row is refused, and so is one that cannot
// be read as empty: one with a write-ahead log beside it, one that is not
// SQLite, or one too large to read. A .config that is a regular file holds
// nothing.
func TestAGCPRunRefusesOnlyAGCloudStoreWithARow(t *testing.T) {
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	shortStateDir(t)
	empty, err := os.ReadFile(filepath.Join("testdata", "gcloud-store-empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := os.ReadFile(filepath.Join("testdata", "gcloud-store-row.db"))
	if err != nil {
		t.Fatal(err)
	}
	view, err := os.ReadFile(filepath.Join("testdata", "gcloud-store-view.db"))
	if err != nil {
		t.Fatal(err)
	}
	put := func(cfg *Config, rel string, blob []byte) {
		t.Helper()
		p := filepath.Join(cfg.Workspace, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, blob, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	kept := routedConfig(t, gcpEndpoint, &hullRecorder{})
	for _, rel := range gcloudStores {
		put(kept, rel, empty)
	}
	put(kept, gcloudDir+"/empty-but-zero.db", nil)
	if err := kept.PrepareWorkspace(); err != nil {
		t.Errorf("a guest home with gcloud's empty stores was refused: %v", err)
	}
	zero := routedConfig(t, gcpEndpoint, &hullRecorder{})
	put(zero, gcloudStores[0], nil)
	if err := zero.PrepareWorkspace(); err != nil {
		t.Errorf("a guest home with an empty file as gcloud's store was refused: %v", err)
	}

	for name, files := range map[string]map[string][]byte{
		"a row":                    {gcloudStores[0]: row},
		"a row in the other store": {gcloudStores[0]: empty, gcloudStores[1]: row},
		"a log":                    {gcloudStores[1]: empty, gcloudStores[1] + "-wal": []byte("x")},
		"a journal":                {gcloudStores[1]: empty, gcloudStores[1] + "-journal": []byte("x")},
		"a view":                   {gcloudStores[0]: view},
		"not sqlite":               {gcloudStores[0]: []byte("x")},
		"too large":                {gcloudStores[0]: append(append([]byte{}, empty...), make([]byte, maxStoreSize)...)},
	} {
		cfg := routedConfig(t, gcpEndpoint, &hullRecorder{})
		for rel, blob := range files {
			put(cfg, rel, blob)
		}
		err := cfg.PrepareWorkspace()
		if err == nil || !strings.Contains(err.Error(), ".config/gcloud") || !strings.Contains(err.Error(), "gcp route") {
			t.Errorf("%s: PrepareWorkspace = %v, want the .config/gcloud refusal", name, err)
		}
		if _, err := os.Stat(filepath.Join(cfg.Workspace, markerFile)); !os.IsNotExist(err) {
			t.Errorf("%s: the refused run wrote its marker: %v", name, err)
		}
	}

	file := routedConfig(t, gcpEndpoint, &hullRecorder{})
	put(file, ".config", []byte("x"))
	if err := file.PrepareWorkspace(); err != nil {
		t.Errorf("a guest home whose .config is a file was refused: %v", err)
	}
}

// Each check of emptySQLite, against the empty store gcloud writes. A view or
// a trigger in the schema is refused although every page is empty, since
// either can answer gcloud's query with a row that no page holds.
func TestEmptySQLiteReadsPageHeaders(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	empty := read("gcloud-store-empty.db")
	if !emptySQLite(empty) {
		t.Fatal("gcloud's empty store was not read as empty")
	}
	for _, name := range []string{"gcloud-store-row.db", "gcloud-store-view.db", "gcloud-store-trigger.db",
		"gcloud-store-forged.db"} {
		if emptySQLite(read(name)) {
			t.Errorf("%s: read as empty", name)
		}
	}
	edit := func(f func(b []byte) []byte) []byte { return f(append([]byte{}, empty...)) }
	for name, blob := range map[string][]byte{
		"a cell on the table page":   edit(func(b []byte) []byte { b[4096+4] = 1; return b }),
		"an interior page":           edit(func(b []byte) []byte { b[4096] = 0x05; return b }),
		"a free page":                edit(func(b []byte) []byte { b[39] = 1; return b }),
		"a short last page":          empty[:len(empty)-1],
		"a bad page size":            edit(func(b []byte) []byte { b[16], b[17] = 0x03, 0x00; return b }),
		"another header":             edit(func(b []byte) []byte { b[0] = 's'; return b }),
		"too short":                  empty[:50],
		"UTF-16 text":                edit(func(b []byte) []byte { b[59] = 2; return b }),
		"a schema root past the end": edit(func(b []byte) []byte { return b[:len(b)-4096] }),
	} {
		if emptySQLite(blob) {
			t.Errorf("%s: read as empty", name)
		}
	}
}

// A record is guest-written data: a serial type or length near 2^64 is
// refused, not turned into a slice bound that panics the host.
func TestSQLiteRecordRefusesHugeLengths(t *testing.T) {
	huge := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	for name, rec := range map[string][]byte{
		"a text near 2^63":      append(append([]byte{10}, huge...), 'x'),
		"a header past the end": {0x7f, 0x01},
		"a cut varint":          {0x05, 0x81, 0x81},
	} {
		if _, ok := sqliteRecord(rec); ok {
			t.Errorf("%s: decoded", name)
		}
	}
}

// A store that is a link is refused although its target is an empty store
// inside the home: the target can change after the check reads it.
func TestAGCPRunRefusesALinkedGCloudStore(t *testing.T) {
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	shortStateDir(t)
	empty, err := os.ReadFile(filepath.Join("testdata", "gcloud-store-empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := routedConfig(t, gcpEndpoint, &hullRecorder{})
	dir := filepath.Join(cfg.Workspace, filepath.FromSlash(gcloudDir))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Workspace, "empty.db"), empty, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../empty.db", filepath.Join(dir, "credentials.db")); err != nil {
		t.Fatal(err)
	}
	err = cfg.PrepareWorkspace()
	if err == nil || !strings.Contains(err.Error(), "credentials.db") {
		t.Fatalf("PrepareWorkspace = %v, want the credentials.db refusal", err)
	}
}

// The statement SQLite builds an entry from must create what the entry's
// type claims, and only SQLite's own key index may have none.
func TestSchemaSQLMatchesTheType(t *testing.T) {
	text := func(s string) sqliteValue { return sqliteValue{text: []byte(s)} }
	for _, c := range []struct {
		kind, name string
		sql        sqliteValue
		want       bool
	}{
		{"table", "credentials", text(`CREATE TABLE "credentials" (account_id TEXT PRIMARY KEY, value BLOB)`), true},
		{"index", "i", text("create unique index i on t(a)"), true},
		{"index", "sqlite_autoindex_credentials_1", sqliteValue{}, true},
		{"table", "credentials", text("CREATE VIEW credentials AS SELECT 1, 2"), false},
		{"table", "credentials", text("CREATE VIRTUAL TABLE credentials USING x"), false},
		{"table", "credentials", text("CREATE /* x */ TABLE credentials (a)"), false},
		{"index", "i", text("CREATE TRIGGER i AFTER INSERT ON t BEGIN SELECT 1; END"), false},
		{"index", "i", sqliteValue{}, false},
		{"table", "credentials", sqliteValue{}, false},
		{"table", "credentials", sqliteValue{isInt: true}, false},
	} {
		if got := schemaSQL(c.kind, c.name, c.sql); got != c.want {
			t.Errorf("schemaSQL(%s, %s, %q) = %v, want %v", c.kind, c.name, c.sql.text, got, c.want)
		}
	}
}

// A tmpfs carries nothing from the host, and over ~/.config/gcloud it keeps a
// login made in the guest off the home share, so one at its own path is
// allowed. A files: binding under it is still refused on its own path. One
// mounted there with at: needs no check of the route's: the profile does not
// validate (see TestATmpfsAtTheGuestGCloudDoesNotValidate).
func TestAGCPRunAllowsATmpfsOverGCloud(t *testing.T) {
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	shortStateDir(t)
	for name, v := range map[string]func(c *Config) profile.Volume{
		"at its path": func(*Config) profile.Volume { return profile.Volume{Kind: "tmpfs", Path: gcloudDir} },
		"on .config":  func(*Config) profile.Volume { return profile.Volume{Kind: "tmpfs", Path: ".config"} },
	} {
		cfg := routedConfig(t, gcpEndpoint, &hullRecorder{})
		cfg.Profile.Volumes = append(cfg.Profile.Volumes, v(cfg))
		if err := cfg.gcpHomeRefusal(); err != nil {
			t.Errorf("a tmpfs %s was refused: %v", name, err)
		}
		cfg.Profile.Files = append(cfg.Profile.Files, profile.FileBinding{Ref: "secrets.gcp",
			Path: gcloudDir + "/application_default_credentials.json"})
		if err := cfg.gcpHomeRefusal(); err == nil || !strings.Contains(err.Error(), "the file "+gcloudDir) {
			t.Errorf("a files: binding under a tmpfs %s: gcpHomeRefusal = %v, want its refusal", name, err)
		}
	}
}

// A gcloud configuration whose [auth] section sets a property, such as
// credential_file_override or impersonate_service_account, hands gcloud an
// identity from elsewhere in the home, so a guest home that holds one is
// refused, and so is the legacy properties file gcloud turns into one. A
// configuration that sets other sections, or an [auth] with only comments, is
// allowed. One brig cannot read, a link or one too large, is refused.
func TestAGCPRunRefusesAGCloudConfigurationThatSetsAuth(t *testing.T) {
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	shortStateDir(t)
	put := func(cfg *Config, rel, text string) {
		t.Helper()
		p := filepath.Join(cfg.Workspace, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const cfgDefault = gcloudConfigDir + "/config_default"
	for name, setup := range map[string]func(cfg *Config){
		"credential_file_override": func(cfg *Config) {
			put(cfg, cfgDefault, "[core]\naccount = a@example.com\n\n[auth]\ncredential_file_override = /home/x/key.json\n")
		},
		"impersonate_service_account": func(cfg *Config) {
			put(cfg, gcloudConfigDir+"/config_other", "[auth]\nimpersonate_service_account = sa@example.iam.gserviceaccount.com\n")
		},
		"another spelling": func(cfg *Config) { put(cfg, cfgDefault, "[ Auth ]\naccess_token_file=/home/x/t\n") },
		"carriage returns": func(cfg *Config) {
			put(cfg, cfgDefault, "[core]\rproject = p\r[auth]\rcredential_file_override = /home/x/key.json\r")
		},
		"a header behind a separator": func(cfg *Config) {
			put(cfg, cfgDefault, "[core]\nproject = p\n\x1c[auth]\ncredential_file_override = /home/x/key.json\n")
		},
		"a header behind a no-break space": func(cfg *Config) {
			put(cfg, cfgDefault, "[core]\n\u00a0[auth]\ncredential_file_override = /home/x/key.json\n")
		},
		"a DEFAULT section": func(cfg *Config) {
			put(cfg, cfgDefault, "[DEFAULT]\ncredential_file_override = /home/x/key.json\n[core]\nproject = p\n")
		},
		"legacy properties": func(cfg *Config) {
			put(cfg, gcloudLegacyProperties, "[auth]\ncredential_file_override = /home/x/key.json\n")
		},
		"too large": func(cfg *Config) { put(cfg, cfgDefault, "#"+strings.Repeat("x", maxConfigSize)) },
		// configparser takes a line for a header only when it matches
		// \[(.+)\], so these lines are options and [auth] goes on.
		"a [ line with no ]": func(cfg *Config) {
			put(cfg, cfgDefault, "[auth]\n[x = 1\nimpersonate_service_account = sa@example.iam.gserviceaccount.com\n")
		},
		"an empty header": func(cfg *Config) {
			put(cfg, cfgDefault, "[auth]\n[]=1\ncredential_file_override = /home/x/key.json\n")
		},
		"DEFAULT behind a [ line": func(cfg *Config) {
			put(cfg, cfgDefault, "[DEFAULT]\n[y: 2\ncredential_file_override = /home/x/key.json\n[core]\nproject = p\n")
		},
		// On macOS gcloud opens config_default as either of these.
		"another case": func(cfg *Config) {
			put(cfg, gcloudConfigDir+"/Config_default", "[auth]\ncredential_file_override = /home/x/key.json\n")
		},
		"a ligature": func(cfg *Config) {
			put(cfg, gcloudConfigDir+"/conﬁg_default", "[auth]\ncredential_file_override = /home/x/key.json\n")
		},
		"a linked configuration": func(cfg *Config) {
			put(cfg, "elsewhere/config", "[core]\n")
			if err := os.MkdirAll(filepath.Join(cfg.Workspace, gcloudConfigDir), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../../../elsewhere/config", filepath.Join(cfg.Workspace, cfgDefault)); err != nil {
				t.Fatal(err)
			}
		},
		"a linked configurations": func(cfg *Config) {
			put(cfg, "elsewhere/config_default", "[core]\n")
			if err := os.MkdirAll(filepath.Join(cfg.Workspace, gcloudDir), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../../elsewhere", filepath.Join(cfg.Workspace, gcloudConfigDir)); err != nil {
				t.Fatal(err)
			}
		},
		"too many": func(cfg *Config) {
			for i := range maxGCloudConfigs + 1 {
				put(cfg, fmt.Sprintf("%s/config_%d", gcloudConfigDir, i), "[core]\n")
			}
		},
	} {
		cfg := routedConfig(t, gcpEndpoint, &hullRecorder{})
		setup(cfg)
		err := cfg.PrepareWorkspace()
		if err == nil || !strings.Contains(err.Error(), ".config/gcloud") || !strings.Contains(err.Error(), "gcp route") {
			t.Errorf("%s: PrepareWorkspace = %v, want the .config/gcloud refusal", name, err)
		}
		if _, err := os.Stat(filepath.Join(cfg.Workspace, markerFile)); !os.IsNotExist(err) {
			t.Errorf("%s: the refused run wrote its marker: %v", name, err)
		}
	}

	for name, text := range map[string]string{
		"core only":          "[core]\naccount = a@example.com\nproject = p\n",
		"auth with comments": "[auth]\n# credential_file_override = /home/x/key.json\n; nothing\n\n[core]\nproject = p\n",
		"empty":              "",
		"a [ line in core":   "[core]\n[x = 1\nproject = p\n",
	} {
		cfg := routedConfig(t, gcpEndpoint, &hullRecorder{})
		put(cfg, cfgDefault, text)
		put(cfg, gcloudConfigDir+"/not_a_config", "[auth]\nx = y\n")
		if err := cfg.PrepareWorkspace(); err != nil {
			t.Errorf("%s: a configuration that sets no auth property was refused: %v", name, err)
		}
	}
}

// gcpHomeRefusal does not look at a tmpfs's at:, because profile validation
// refuses an at: in the guest home, ~/.config/gcloud included. If that rule
// is ever relaxed, the route's check needs an at: arm back.
func TestATmpfsAtTheGuestGCloudDoesNotValidate(t *testing.T) {
	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("no claude-code profile")
	}
	p.Volumes = append(p.Volumes, profile.Volume{Kind: "tmpfs", Path: "x", At: p.GuestHome + "/.config/gcloud"})
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "overlaps it") {
		t.Errorf("Validate = %v, want the at: refusal", err)
	}
}

// The guest is live on the attach path and can swap configurations between
// the check's lstat and its open. A FIFO must not block brig on the host; it
// holds no configuration gcloud reads, so it passes as a FIFO there from the
// start does. A link to a directory the guest can fill must not be listed in
// its place, so it is refused.
func TestAGCPRunRefusesGCloudConfigurationsSwappedBeforeTheOpen(t *testing.T) {
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	shortStateDir(t)
	for name, tc := range map[string]struct {
		swap    func(dir string) error
		refused bool
	}{
		"a fifo": {swap: func(dir string) error { return syscall.Mkfifo(dir, 0o600) }},
		"a link": {refused: true, swap: func(dir string) error {
			elsewhere := filepath.Join(filepath.Dir(dir), "elsewhere")
			if err := os.MkdirAll(elsewhere, 0o700); err != nil {
				return err
			}
			return os.Symlink("elsewhere", dir)
		}},
	} {
		cfg := routedConfig(t, gcpEndpoint, &hullRecorder{})
		dir := filepath.Join(cfg.Workspace, filepath.FromSlash(gcloudConfigDir))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		saved := gcloudConfigBeforeOpen
		gcloudConfigBeforeOpen = func() {
			if err := os.Remove(dir); err != nil {
				t.Error(err)
			}
			if err := tc.swap(dir); err != nil {
				t.Error(err)
			}
		}
		done := make(chan error, 1)
		go func() { done <- cfg.PrepareWorkspace() }()
		select {
		case err := <-done:
			if tc.refused && (err == nil || !strings.Contains(err.Error(), ".config/gcloud")) {
				t.Errorf("%s swapped in: PrepareWorkspace = %v, want the .config/gcloud refusal", name, err)
			}
			if !tc.refused && err != nil {
				t.Errorf("%s swapped in: PrepareWorkspace = %v, want no refusal", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s swapped in: the check blocked", name)
		}
		gcloudConfigBeforeOpen = saved
	}
}

// The guest is live on the attach path and can remove or replace a store
// between the check's lstat and its open: gone counts as empty, and one that
// cannot be opened as a row, never as an error that aborts the run.
func TestEmptyStoreFoldsAnOpenErrorIntoItsTwoAnswers(t *testing.T) {
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	shortStateDir(t)
	cfg := routedConfig(t, gcpEndpoint, &hullRecorder{})
	p := filepath.Join(cfg.Workspace, filepath.FromSlash(gcloudStores[0]))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := cfg.openWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if empty, err := emptyStore(r, gcloudStores[0]); err != nil || !empty {
		t.Errorf("a store that is gone: emptyStore = %v, %v, want empty", empty, err)
	}
	if err := os.WriteFile(p, nil, 0o000); err != nil {
		t.Fatal(err)
	}
	if empty, err := emptyStore(r, gcloudStores[0]); err != nil || empty {
		t.Errorf("a store that cannot be opened: emptyStore = %v, %v, want a row", empty, err)
	}
}
