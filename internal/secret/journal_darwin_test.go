//go:build darwin

package secret

import (
	"encoding/base64"
	"strings"
	"testing"
)

// A journal is two keychain items like any secret. The listing shows
// neither, and the line that writes the longest one still fits security's
// buffer. Neither check touches the keychain.
func TestTheKeychainListsNoJournal(t *testing.T) {
	const dump = `keychain: "/Users/x/Library/Keychains/login.keychain-db"
version: 512
class: "genp"
attributes:
    "acct"<blob>="claude-credentials"
    "svce"<blob>="sh.brig.secret"
keychain: "/Users/x/Library/Keychains/login.keychain-db"
version: 512
class: "genp"
attributes:
    "acct"<blob>="claude-credentials.journal"
    "svce"<blob>="sh.brig.secret"
keychain: "/Users/x/Library/Keychains/login.keychain-db"
version: 512
class: "genp"
attributes:
    "acct"<blob>="claude-credentials.journal.sealed"
    "svce"<blob>="sh.brig.secret"
`
	got := parseDump(dump, "sh.brig.secret")
	if len(got) != 1 || got[0].Name != "claude-credentials" {
		t.Errorf("parseDump = %+v, want claude-credentials alone", got)
	}

	k := keychain{service: "sh.brig.test"}
	long := Provenance{V: ProvenanceVersion, From: strings.Repeat("f", maxFromLen), ExpiresAt: 1 << 62}
	prefix, err := k.writePrefix(JournalName(strings.Repeat("n", maxName)), true, long)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(prefix) + len(keyPrefix) + base64.StdEncoding.EncodedLen(keyLen); n > maxLine/2 {
		t.Errorf("the key line of the longest journal is %d bytes, want well inside %d", n, maxLine)
	}
}
