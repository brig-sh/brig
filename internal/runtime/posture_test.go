package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// A record that cannot be parsed is refused rather than read as empty. Read as
// empty and written back, it would drop the network of every other sandbox, and
// each of them would restart onto the default on its next command.
func TestAnUnreadableNetworkRecordIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_GATEWAY_DIR", dir)
	path := filepath.Join(dir, "networks.json")
	if err := os.WriteFile(path, []byte("{half"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RecordBootedNet("brig-a", "isolated"); err == nil {
		t.Error("a record that cannot be parsed was written over")
	}
	if _, err := BootedNet("brig-a"); err == nil {
		t.Error("a record that cannot be parsed read as empty")
	}
	if blob, _ := os.ReadFile(path); string(blob) != "{half" {
		t.Errorf("the record was changed: %q", blob)
	}
}

// Recording, forgetting and pruning touch only the sandboxes they name.
func TestTheNetworkRecordIsKeptPerSandbox(t *testing.T) {
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	for name, net := range map[string]string{"brig-a": "isolated", "brig-b": "none", "brig-c": "shared"} {
		if err := RecordBootedNet(name, net); err != nil {
			t.Fatal(err)
		}
	}

	ForgetBootedNet("brig-a")
	PruneBootedNets([]string{"brig-b"})

	for name, want := range map[string]string{"brig-a": "", "brig-b": "none", "brig-c": ""} {
		got, err := BootedNet(name)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: recorded %q, want %q", name, got, want)
		}
	}
}
