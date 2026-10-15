package secret

import (
	"strings"
	"testing"
)

// A journal is stored under a name the backends take and no user can give:
// ValidName refuses it, so `brig secret create` cannot make one and List,
// which skips names outside the grammar, never shows one.
func TestJournalNamesAreStoredButNeverListed(t *testing.T) {
	journal := JournalName("claude-credentials")
	if err := validStoreName(journal); err != nil {
		t.Errorf("the backends refuse the journal name %q: %v", journal, err)
	}
	if ValidName(journal) == nil {
		t.Errorf("ValidName takes %q, so List would show it and create could make it", journal)
	}
	if err := validStoreName(JournalName(strings.Repeat("a", maxName))); err != nil {
		t.Errorf("the journal of the longest name is refused: %v", err)
	}
	for _, name := range []string{
		JournalName(journal),
		JournalName("1-starts-with-a-digit"),
		JournalName(""),
		"claude-credentials.sealed",
		"claude-credentials.journal.sealed",
		"claude-credentials.journalx",
	} {
		if validStoreName(name) == nil {
			t.Errorf("the backends take %q", name)
		}
	}
}
