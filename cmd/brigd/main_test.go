package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A cold boot through the daemon reaches the runtime's gateway directory: the
// boot stops any isolated gateway left under the sandbox's name, and the
// session and network records go under ~/.brig too. None of these tests set a
// directory, so without this they wrote into the real ~/.brig of whoever runs
// them and could stop a real gateway of the same name.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "brigd-home-")
	if err != nil {
		panic(err)
	}
	for name, value := range map[string]string{
		"HOME":              home,
		"BRIG_GATEWAY_DIR":  filepath.Join(home, ".brig"),
		"BRIG_GATEWAY_SOCK": "",
		"BRIG_STATE_DIR":    "",
	} {
		if err := os.Setenv(name, value); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}

// The daemon refuses BRIG_TEMPLATE_DIR as the CLI does. Otherwise a daemon
// started with it in its service environment serves the built-ins in place
// of the profiles it named.
func TestLoadProfilesRefusesTheRemovedTemplateDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("BRIG_PROFILE_DIR", "")
	t.Setenv("BRIG_TEMPLATE_DIR", t.TempDir())
	if err := loadProfiles(); err == nil || !strings.Contains(err.Error(), "BRIG_TEMPLATE_DIR") {
		t.Errorf("loadProfiles must refuse BRIG_TEMPLATE_DIR by name: %v", err)
	}
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	if err := loadProfiles(); err != nil {
		t.Errorf("beside BRIG_PROFILE_DIR it decides nothing: %v", err)
	}
}
