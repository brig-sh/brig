package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// An unparsable record is an error, not an empty map that a write would
// save over every other sandbox's record.
func TestAnUnreadableBootRecordIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_GATEWAY_DIR", dir)
	path := filepath.Join(dir, "boots.json")
	if err := os.WriteFile(path, []byte("{half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RecordBoot("brig-a", BootRecord{Result: BootFailed, At: time.Now()}); err == nil {
		t.Error("a record that cannot be parsed was written over")
	}
	if _, _, err := LastBoot("brig-a"); err == nil {
		t.Error("a record that cannot be parsed read as empty")
	}
	if blob, _ := os.ReadFile(path); string(blob) != "{half" {
		t.Errorf("the record was changed: %q", blob)
	}
}

// Recording, forgetting and pruning touch only the sandboxes they name.
func TestTheBootRecordIsKeptPerSandbox(t *testing.T) {
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	at := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	for name, result := range map[string]string{"brig-a": BootOK, "brig-b": BootFailed, "brig-c": BootOK} {
		if err := RecordBoot(name, BootRecord{Result: result, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	ForgetBoot("brig-a")
	PruneBoots([]string{"brig-b"})

	if _, ok, _ := LastBoot("brig-a"); ok {
		t.Error("a forgotten sandbox still has a record")
	}
	if _, ok, _ := LastBoot("brig-c"); ok {
		t.Error("a pruned sandbox still has a record")
	}
	got, ok, err := LastBoot("brig-b")
	if err != nil || !ok {
		t.Fatalf("the kept sandbox lost its record: ok=%v err=%v", ok, err)
	}
	if got.Result != BootFailed || !got.At.Equal(at) {
		t.Errorf("got %+v, want failed at %v", got, at)
	}
}

// A later boot replaces the earlier outcome.
func TestALaterBootReplacesTheRecord(t *testing.T) {
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	_ = RecordBoot("brig-a", BootRecord{Result: BootFailed, At: time.Now()})
	_ = RecordBoot("brig-a", BootRecord{Result: BootOK, At: time.Now()})
	if got, _, _ := LastBoot("brig-a"); got.Result != BootOK {
		t.Errorf("result %q, want ok", got.Result)
	}
}

// The lock serialises concurrent recorders; none is lost.
func TestConcurrentBootRecordsAreAllKept(t *testing.T) {
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := RecordBoot(fmt.Sprintf("brig-%d", i), BootRecord{Result: BootOK, At: time.Now()}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for i := range 20 {
		if _, ok, _ := LastBoot(fmt.Sprintf("brig-%d", i)); !ok {
			t.Errorf("brig-%d lost its record", i)
		}
	}
}

// The record keeps the image and digest, and stores At in UTC.
func TestTheBootRecordKeepsTheImageAndDigest(t *testing.T) {
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	digest := "sha256:" + strings.Repeat("ab", 32)
	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.FixedZone("x", 3600))
	want := BootRecord{Result: BootFailed, At: at, Image: "ghcr.io/brig-sh/claude-code:1", Digest: digest}
	if err := RecordBoot("brig-a", want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LastBoot("brig-a")
	if err != nil || !ok {
		t.Fatalf("no record: ok=%v err=%v", ok, err)
	}
	if got.Image != want.Image || got.Digest != want.Digest || got.Result != want.Result || !got.At.Equal(at) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if got.At.Location() != time.UTC {
		t.Errorf("at %v is not stored in UTC", got.At)
	}
}

// A boot with no image or digest writes neither key.
func TestABootWithoutADigestOmitsIt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_GATEWAY_DIR", dir)
	if err := RecordBoot("brig-a", BootRecord{Result: BootOK, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(dir, "boots.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"digest"`, `"image"`} {
		if strings.Contains(string(blob), key) {
			t.Errorf("boots.json carries %s for a boot that had none:\n%s", key, blob)
		}
	}
}
