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
		if err == nil || !strings.Contains(err.Error(), "HOME moves ~/.config/gcloud") {
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
