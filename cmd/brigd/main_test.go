package main

import (
	"os"
	"path/filepath"
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
