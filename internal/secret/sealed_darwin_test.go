//go:build darwin

package secret

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// rawItem is the line security holds for account, exactly as stored.
func rawItem(t *testing.T, k *testKeychain, account string) string {
	t.Helper()
	out, err := exec.Command(securityBin, "find-generic-password",
		"-s", k.service, "-a", account, "-w").Output()
	if err != nil {
		t.Fatalf("find-generic-password %s: %v", account, err)
	}
	return strings.TrimRight(string(out), "\n")
}

// keyOf is the key the item for name holds, or a failed test.
func keyOf(t *testing.T, k *testKeychain, name string) []byte {
	t.Helper()
	raw := rawItem(t, k, name)
	rest, ok := strings.CutPrefix(raw, keyPrefix)
	if !ok {
		t.Fatalf("the item does not start with the key marker: %q", raw[:min(len(raw), 12)])
	}
	key, err := base64.StdEncoding.DecodeString(rest)
	if err != nil {
		t.Fatalf("the key is not base64: %v", err)
	}
	return key
}

// hasSealed reports whether the sealed item for name is in the keychain.
func hasSealed(k *testKeychain, name string) bool {
	return exec.Command(securityBin, "find-generic-password",
		"-s", k.service, "-a", sealedAccount(name)).Run() == nil
}

// A value far past the old command-line ceiling round-trips, because the
// value no longer rides the command line at all.
func TestLargeValueRoundTrips(t *testing.T) {
	k := testStore(t)
	value := bytes.Repeat([]byte("0123456789abcdef"), 1024) // 16 KB
	if err := k.Create("big", value); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := k.Read("big")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, value) {
		t.Errorf("read back %d bytes, want %d", len(got), len(value))
	}
}

// A secret is two items: the key under the name, and the ciphertext under
// the sealed account. Neither holds the value, and the key item is the
// same size for every secret.
func TestASecretIsAKeyItemAndASealedItem(t *testing.T) {
	k := testStore(t)
	value := []byte("keychain-canary-value")
	if err := k.Create("two", value); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := len(keyOf(t, k, "two")); got != keyLen {
		t.Errorf("the key item holds %d bytes, want %d", got, keyLen)
	}
	sealed := rawItem(t, k, sealedAccount("two"))
	blob, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		t.Fatalf("the sealed item is not base64: %v", err)
	}
	if bytes.Contains(blob, value) || strings.Contains(sealed, base64.StdEncoding.EncodeToString(value)) {
		t.Fatal("the sealed item carries the value")
	}
	if !bytes.HasPrefix(blob, []byte(sealedMagic)) {
		t.Errorf("the sealed item does not start with the format marker")
	}
}

// The sealed write is the one that goes through argv, and what it puts
// there is ciphertext: neither the value nor its base64 appears in the
// arguments, and the key never does.
func TestArgvCarriesOnlyCiphertext(t *testing.T) {
	k := testStore(t)
	value := []byte("argv-canary-value")
	key, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := seal("x", key, value)
	if err != nil {
		t.Fatal(err)
	}
	args := k.sealedArgs("x", blob)
	joined := strings.Join(args, " ")
	for what, needle := range map[string]string{
		"the value":        string(value),
		"the value base64": base64.StdEncoding.EncodeToString(value),
		"the key base64":   base64.StdEncoding.EncodeToString(key),
	} {
		if strings.Contains(joined, needle) {
			t.Errorf("%s reached argv: %q", what, joined)
		}
	}
	if !strings.Contains(joined, base64.StdEncoding.EncodeToString(blob)) {
		t.Error("the ciphertext is not on argv, so the sealed write goes somewhere else")
	}
	if args[len(args)-2] != "-w" {
		t.Errorf("-w is not the last option: %q", args)
	}
}

// A brig from before this change reads an item as base64 of the value. The
// key item must not decode that way, or an older brig would hand a guest the
// key bytes as its credential. It refuses instead, with the error it already
// has for an item it did not write.
func TestKeyItemIsNotReadableAsAValueByAnOlderBrig(t *testing.T) {
	k := testStore(t)
	if err := k.Create("marked", []byte("v")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := base64.StdEncoding.DecodeString(rawItem(t, k, "marked")); err == nil {
		t.Error("the key item decodes as base64, so an older brig would read the key as the value")
	}
}

// The sealed item is not a secret of its own: List never shows it.
func TestListHidesTheSealedItem(t *testing.T) {
	k := testStore(t)
	if err := k.Create("shown", []byte("v")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	list, err := k.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "shown" {
		t.Errorf("List = %+v, want shown alone", list)
	}
}

// Delete takes both items, and reports absence once for both.
func TestDeleteRemovesBothItems(t *testing.T) {
	k := testStore(t)
	if err := k.Create("gone", []byte("v")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := k.Delete("gone"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if hasSealed(k, "gone") {
		t.Error("the sealed item survived Delete")
	}
	if err := k.Delete("gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete = %v, want ErrNotFound", err)
	}
}

// A sealed item left behind by an interrupted create is swept up by the
// next Delete of that name, and the caller still learns the secret is not
// there.
func TestDeleteSweepsAnOrphanedSealedItem(t *testing.T) {
	k := testStore(t)
	plant(t, k.service, sealedAccount("orphan"), "c3RhbGU=")
	if err := k.Delete("orphan"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete = %v, want ErrNotFound", err)
	}
	if hasSealed(k, "orphan") {
		t.Error("the orphaned sealed item survived")
	}
}

// An update replaces the sealed item under the same key and keeps reading
// back. The key never changes on an update, so the sealed item's
// replacement is the one step that changes the value.
func TestUpdateReplacesTheSealedItemUnderTheSameKey(t *testing.T) {
	k := testStore(t)
	if err := k.Create("rot", []byte("first")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	key := keyOf(t, k, "rot")
	if err := k.Update("rot", []byte("second")); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := k.Read("rot")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("read back %q, want %q", got, "second")
	}
	if !bytes.Equal(keyOf(t, k, "rot"), key) {
		t.Error("Update replaced the key")
	}
}

// Sixteen concurrent updates of one secret end with a secret that opens and
// holds one of the values written. security's -U replaces the sealed item
// whole, and every writer seals under the same key, so whichever landed
// last still opens. An individual update is allowed to fail: security's own
// -U reports a duplicate when two of them collide.
func TestConcurrentUpdatesLeaveAReadableSecret(t *testing.T) {
	k := testStore(t)
	if err := k.Create("busy", []byte("v0")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	const n = 16
	errs := make(chan error, n)
	for i := range n {
		go func() {
			errs <- k.keychain.Update("busy", bytes.Repeat([]byte{'a' + byte(i)}, 4096))
		}()
	}
	for range n {
		<-errs
	}
	got, err := k.Read("busy")
	if err != nil {
		t.Fatalf("after concurrent updates, Read: %v", err)
	}
	if len(got) != 4096 || strings.Trim(string(got), string(got[0])) != "" {
		t.Errorf("read back a value no writer wrote: %d bytes", len(got))
	}
}

// legacyItem stores value the way brig did before this change: the base64
// value itself in the keychain item. Reads must keep working on a store
// that has these.
func legacyItem(t *testing.T, k *testKeychain, name string, value []byte) {
	t.Helper()
	k.cleanup(name)
	plant(t, k.service, name, base64.StdEncoding.EncodeToString(value))
}

func TestReadsAnItemWrittenBeforeSealing(t *testing.T) {
	k := testStore(t)
	legacyItem(t, k, "old", []byte("legacy-value"))
	got, err := k.Read("old")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "legacy-value" {
		t.Errorf("read back %q", got)
	}
}

// Updating a pre-sealing item moves it: after the update the item holds a
// key and the value is in the sealed item.
func TestUpdateMovesAnOldItemIntoTheSealedLayout(t *testing.T) {
	k := testStore(t)
	legacyItem(t, k, "moved", []byte("legacy-value"))
	if err := k.Update("moved", []byte("new-value")); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !strings.HasPrefix(rawItem(t, k, "moved"), keyPrefix) {
		t.Error("the item still holds the value rather than a key")
	}
	if !hasSealed(k, "moved") {
		t.Error("no sealed item after the update")
	}
	got, err := k.Read("moved")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "new-value" {
		t.Errorf("read back %q", got)
	}
}

// A key without its sealed item is a state brig has to explain rather than
// report as "no such secret": the secret exists, and half of it is gone.
func TestReadExplainsAMissingSealedItem(t *testing.T) {
	k := testStore(t)
	if err := k.Create("half", []byte("v")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := exec.Command(securityBin, "delete-generic-password",
		"-s", k.service, "-a", sealedAccount("half")).Run(); err != nil {
		t.Fatal(err)
	}
	_, err := k.Read("half")
	if err == nil {
		t.Fatal("Read succeeded with the sealed item gone")
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a missing sealed item reported as ErrNotFound, which would let import overwrite the key")
	}
	if !strings.Contains(err.Error(), "sealed") {
		t.Errorf("error = %v, want it to name the sealed item", err)
	}
}

// A sealed item that does not open with the stored key was replaced outside
// brig, and the error says so rather than reporting a decode failure.
func TestReadExplainsASealedItemThatDoesNotOpen(t *testing.T) {
	k := testStore(t)
	if err := k.Create("one", []byte("value-one")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := k.Create("two", []byte("value-two")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	other := rawItem(t, k, sealedAccount("two"))
	plantUpdate(t, k.service, sealedAccount("one"), other)
	_, err := k.Read("one")
	if err == nil {
		t.Fatal("Read succeeded on a sealed item encrypted for another secret")
	}
	if !strings.Contains(err.Error(), "outside brig") {
		t.Errorf("error = %v, want it to say the item was changed outside brig", err)
	}
}

// A sealed item that is not in brig's format at all is named as such.
func TestReadNamesASealedItemThatIsNotOurs(t *testing.T) {
	k := testStore(t)
	if err := k.Create("notours", []byte("v")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	plantUpdate(t, k.service, sealedAccount("notours"), base64.StdEncoding.EncodeToString([]byte("just some bytes")))
	_, err := k.Read("notours")
	if err == nil {
		t.Fatal("Read succeeded on a sealed item that is not brig's")
	}
	if !strings.Contains(err.Error(), "not a brig sealed value") {
		t.Errorf("error = %v, want it to say the item is not a brig sealed value", err)
	}
}

// A create whose sealed write fails leaves no key behind: a caller told the
// create failed expects nothing to be there.
func TestCreateRollsBackTheKeyWhenTheSealedWriteFails(t *testing.T) {
	k := testStore(t)
	k.beforeSeal = func() error { return errors.New("injected: the sealed write failed") }
	if err := k.Create("nosealed", []byte("v")); err == nil {
		t.Fatal("Create succeeded without writing the sealed item")
	}
	k.beforeSeal = nil
	if _, err := k.Read("nosealed"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the failed create left a key behind: %v", err)
	}
}

// The key marker and the key are short by construction, so the command line
// security -i reads has no ceiling a value can reach: provenance at its
// longest and the key together stay well inside the buffer.
func TestKeyLineNeverNearsTheBuffer(t *testing.T) {
	k := keychain{service: "sh.brig.test"}
	long := Provenance{V: ProvenanceVersion, From: strings.Repeat("x", maxFromLen), ExpiresAt: 1755436980000}
	prefix, err := k.writePrefix(strings.Repeat("n", maxName), true, long)
	if err != nil {
		t.Fatal(err)
	}
	line := len(prefix) + len(keyPrefix) + base64.StdEncoding.EncodedLen(keyLen)
	if line > maxLine/2 {
		t.Errorf("the longest key line is %d bytes, too close to the %d-byte buffer", line, maxLine)
	}
}

// plantUpdate replaces an item's value directly, the way plant writes one,
// to stand in for something else on the host changing it.
func plantUpdate(t *testing.T, service, account, value string) {
	t.Helper()
	cmd := exec.Command(securityBin, "add-generic-password",
		"-s", service, "-a", account, "-U", "-w", value)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("replacing %s: %v\n%s", account, err, out)
	}
}

// A secret with one item damaged or missing is reported as ErrDamaged, not
// as absent: it exists, and half of it is unusable. That is what lets an
// import overwrite it rather than stop on the read.
func TestAHalfPresentSecretReadsAsDamaged(t *testing.T) {
	k := testStore(t)
	if err := k.Create("half", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(securityBin, "delete-generic-password",
		"-s", k.service, "-a", sealedAccount("half")).Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Read("half"); !errors.Is(err, ErrDamaged) {
		t.Errorf("Read = %v, want ErrDamaged", err)
	}
	if err := k.Update("half", []byte("again")); err != nil {
		t.Fatalf("Update did not repair it: %v", err)
	}
	if got, _ := k.Read("half"); string(got) != "again" {
		t.Errorf("read back %q", got)
	}
}

// A key item that carries the marker but no key is damaged too, and an
// update replaces it with a fresh key rather than refusing.
func TestUpdateReplacesAMarkedItemThatHoldsNoKey(t *testing.T) {
	k := testStore(t)
	plant(t, k.service, "badkey", keyPrefix+"not-a-key")
	k.cleanup("badkey")
	if _, err := k.Read("badkey"); !errors.Is(err, ErrDamaged) {
		t.Errorf("Read = %v, want ErrDamaged", err)
	}
	if err := k.Update("badkey", []byte("fresh")); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got, _ := k.Read("badkey"); string(got) != "fresh" {
		t.Errorf("read back %q", got)
	}
}
