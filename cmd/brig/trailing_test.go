package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The group used to drop a trailing word: `brig agent ls extra` printed the
// listing and forgot extra, `brig agent edit a b` edited a and let b fall on
// the floor, `brig agent import a b` did the same. Every other verb in brig
// names a stray token rather than swallowing it, and now so does every verb
// here.
//
// The point of a table over the whole group -- rather than a case each for the
// three in the report -- is that none of the ten is missed: a verb added later
// with the old drop-the-tail habit shows up as a row with no error.
func TestEveryAgentSubcommandRefusesAWordTooMany(t *testing.T) {
	for _, args := range [][]string{
		{"agent", "ls", "extra"},
		{"agent", "show", "claude-code", "extra"},
		{"agent", "new", "mine", "extra", "--from", "claude-code"},
		{"agent", "edit", "mine", "extra"},
		{"agent", "rm", "mine", "extra"},
		{"agent", "import", "mine.yaml", "extra"},
		{"agent", "export", "claude-code", "dest", "extra"},
		// The deprecated spellings map onto the verbs above and must refuse the
		// same way -- a retired word that quietly dropped the tail is still a
		// word that drops the tail.
		{"agent", "list", "extra"},
		{"agent", "save", "claude-code", "dest", "extra"},
		{"agent", "load", "mine.yaml", "extra"},
	} {
		t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
		if err := run(args); err == nil {
			t.Errorf("brig %s dropped the trailing word instead of refusing it",
				strings.Join(args, " "))
		}
	}
}

// The verb named in the report, in full: it is a usage error (exit 2, so a
// script tells "you typed it wrong" from "it ran and failed"), it names the
// word at fault, and it says so in `brig ls`'s own words rather than a second
// phrasing invented here.
func TestAgentLsRefusesATrailingArgumentAsUsage(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	out, err := captureStdout(t, func() error { return run([]string{"agent", "ls", "extra"}) })
	if err == nil {
		t.Fatal("brig agent ls extra was accepted")
	}
	if got := exitCode(err); got != 2 {
		t.Errorf("brig agent ls extra exits %d, want 2 (the usage class)", got)
	}
	if !strings.Contains(err.Error(), `"extra"`) {
		t.Errorf("the error does not name the stray word: %v", err)
	}
	if !strings.Contains(err.Error(), "unexpected argument") ||
		!strings.Contains(err.Error(), "brig agent ls") {
		t.Errorf("the refusal is not in `brig ls`'s voice: %v", err)
	}
	// Refused, not printed and then refused: the listing never ran.
	if out != "" {
		t.Errorf("brig agent ls extra printed the listing anyway:\n%s", out)
	}
}

// The two from the report that acted on the first word and dropped the second:
// each now reports the second rather than swallowing it, and does so as a usage
// error naming the verb the reader typed.
func TestAgentEditAndImportRefuseTheSecondWord(t *testing.T) {
	for _, c := range []struct {
		args    []string
		command string
	}{
		{[]string{"agent", "edit", "a", "b"}, "brig agent edit"},
		{[]string{"agent", "import", "a", "b"}, "brig agent import"},
	} {
		t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
		err := run(c.args)
		if err == nil {
			t.Errorf("brig %s dropped %q", strings.Join(c.args, " "), "b")
			continue
		}
		if got := exitCode(err); got != 2 {
			t.Errorf("brig %s exits %d, want 2", strings.Join(c.args, " "), got)
		}
		if !strings.Contains(err.Error(), `"b"`) {
			t.Errorf("brig %s does not name the dropped word: %v", strings.Join(c.args, " "), err)
		}
		if !strings.Contains(err.Error(), c.command) {
			t.Errorf("brig %s does not name the verb typed: %v", strings.Join(c.args, " "), err)
		}
	}
}

// A retired spelling behaves exactly like the verb it maps onto -- the refusal
// of a stray word included -- and still prints its one deprecation notice. Both
// halves matter: a spelling that stopped refusing would drop the tail again,
// and one that stopped notifying would never teach the reader the new word.
func TestDeprecatedAgentSpellingRefusesTailAndStillNotifies(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	var err error
	notice := captureStderr(t, func() {
		_, err = captureStdout(t, func() error { return run([]string{"agent", "list", "extra"}) })
	})
	if err == nil {
		t.Fatal("brig agent list extra was accepted")
	}
	if got := exitCode(err); got != 2 {
		t.Errorf("brig agent list extra exits %d, want 2", got)
	}
	if !strings.Contains(err.Error(), `"extra"`) {
		t.Errorf("the error does not name the stray word: %v", err)
	}
	// It refuses in the new verb's words, which is where the notice just sent
	// the reader.
	if !strings.Contains(err.Error(), "brig agent ls") {
		t.Errorf("the refusal does not name the current spelling: %v", err)
	}
	if !strings.Contains(notice, "brig: `") || !strings.Contains(notice, "brig agent ls") {
		t.Errorf("brig agent list printed no deprecation notice:\n%s", notice)
	}
}

// The other side of the change: the right number of operands still works and
// still exits 0. A refusal that fired one word too early would be as much a bug
// as the drop it replaces.
func TestAgentAcceptsTheRightNumberOfArguments(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	blob := "name: mine\nimage: i\nguestHome: /home/mine\nbinary: m\nmem: 1\ncpus: 1\n"
	if err := os.WriteFile(filepath.Join(dir, "mine.yaml"), []byte(blob), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "round.yaml")
	if err := os.WriteFile(file, []byte(strings.Replace(blob, "name: mine", "name: round", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	stubEditor(t, "true")

	for _, args := range [][]string{
		{"agent", "ls"},             // no operand
		{"agent", "edit", "mine"},   // one operand
		{"agent", "import", file},   // one operand
		{"agent", "show", "mine"},   // one operand
		{"agent", "export", "mine"}, // one operand, prints
	} {
		if _, err := captureStdout(t, func() error { return run(args) }); err != nil {
			t.Errorf("brig %s was refused: %v", strings.Join(args, " "), err)
		}
	}
}

// The policy listing and brig version read their verb and stopped there, so a
// trailing word was dropped and the command still exited 0 -- and a reader who
// typed `brig policy ls --help` got the listing, not help, with a status that
// said it worked.
//
// The table is over every spelling that reaches listPolicies, not just the two
// in the report: `brig policy list` and the top-level `brig policies` are the
// same listing behind a retired word, and a retired word that quietly drops the
// tail is still a word that drops the tail.
//
// --help is not in this table. Asking for help is not a stray word, and the
// group answers it with the group's usage and exit 0 -- see
// TestPolicyListingAnswersHelpLikeTheRestOfTheGroup.
func TestPolicyListingAndVersionRefuseAWordTooMany(t *testing.T) {
	for _, c := range []struct {
		args  []string
		names string // the command the refusal must name
	}{
		{[]string{"policy", "ls", "extra"}, "brig policy ls"},
		// The retired spellings refuse in the current verb's words, which is
		// where their notice has just sent the reader.
		{[]string{"policy", "list", "extra"}, "brig policy ls"},
		{[]string{"policies", "extra"}, "brig policy ls"},
		{[]string{"version", "extra"}, "brig version"},
		{[]string{"--version", "extra"}, "brig --version"},
	} {
		t.Setenv("BRIG_POLICY_DIR", t.TempDir())
		t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
		typed := strings.Join(c.args, " ")
		var err error
		// Through captureStderr as well, so a retired spelling's notice does
		// not land in the test's own output.
		captureStderr(t, func() {
			_, err = captureStdout(t, func() error { return run(c.args) })
		})
		if err == nil {
			t.Errorf("brig %s dropped the trailing word instead of refusing it", typed)
			continue
		}
		if got := exitCode(err); got != 2 {
			t.Errorf("brig %s exits %d, want 2 (the usage class)", typed, got)
		}
		stray := c.args[len(c.args)-1]
		if !strings.Contains(err.Error(), `"`+stray+`"`) {
			t.Errorf("brig %s does not name the stray word: %v", typed, err)
		}
		if !strings.Contains(err.Error(), "unexpected argument") ||
			!strings.Contains(err.Error(), "`"+c.names+"`") {
			t.Errorf("brig %s does not refuse in %s's words: %v", typed, c.names, err)
		}
	}
}

// Nothing is printed before the refusal. The bug was not only the exit code:
// the listing had already run, so a reader saw output that looked like an
// answer to what they typed.
func TestPolicyListingRefusesBeforeItPrints(t *testing.T) {
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	out, err := captureStdout(t, func() error { return run([]string{"policy", "ls", "extra"}) })
	if err == nil {
		t.Fatal("brig policy ls extra was accepted")
	}
	if out != "" {
		t.Errorf("brig policy ls extra printed the listing before refusing:\n%s", out)
	}
}

// The retired spellings still say which word replaces them. Both halves matter:
// a spelling that stopped refusing would drop the tail again, and one that
// stopped notifying would never teach the reader the current word.
func TestRetiredPolicyListingSpellingsRefuseTailAndStillNotify(t *testing.T) {
	for _, c := range []struct {
		args   []string
		notice string
	}{
		{[]string{"policy", "list", "extra"}, "brig policy list"},
		{[]string{"policies", "extra"}, "brig policies"},
	} {
		t.Setenv("BRIG_POLICY_DIR", t.TempDir())
		t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
		typed := strings.Join(c.args, " ")
		var err error
		notice := captureStderr(t, func() {
			_, err = captureStdout(t, func() error { return run(c.args) })
		})
		if err == nil {
			t.Errorf("brig %s was accepted", typed)
			continue
		}
		if !strings.Contains(notice, c.notice) || !strings.Contains(notice, "brig policy ls") {
			t.Errorf("brig %s printed no deprecation notice:\n%s", typed, notice)
		}
	}
}

// The other side of the change: the right number of operands still works and
// still exits 0. A refusal one word too early would be as much a bug as the
// drop it replaces.
func TestPolicyListingAndVersionStillAcceptNoArguments(t *testing.T) {
	for _, args := range [][]string{
		{"policy", "ls"},
		{"policy", "list"},
		{"policies"},
		{"version"},
		{"--version"},
	} {
		t.Setenv("BRIG_POLICY_DIR", t.TempDir())
		t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
		var err error
		captureStderr(t, func() {
			_, err = captureStdout(t, func() error { return run(args) })
		})
		if err != nil {
			t.Errorf("brig %s was refused: %v", strings.Join(args, " "), err)
		}
	}
}

// The other half of the report's --help complaint. `brig policy ls --help`
// printed the listing, which is not help, and exited 0 saying it worked; a
// usage error would not have been help either. policyCmd already translates a
// verb's flag.ErrHelp into the group's usage and exit 0 -- policy.go says so
// where it does it -- and ls is not an exception to that, so it is asserted
// against a sibling rather than in isolation: whatever `brig policy show
// --help` answers, ls and both retired spellings answer the same.
func TestPolicyListingAnswersHelpLikeTheRestOfTheGroup(t *testing.T) {
	for _, args := range [][]string{
		{"policy", "ls", "--help"},
		{"policy", "ls", "-h"},
		{"policy", "list", "--help"},
		{"policies", "--help"},
	} {
		t.Setenv("BRIG_POLICY_DIR", t.TempDir())
		t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
		typed := strings.Join(args, " ")
		var err error
		var out string
		captureStderr(t, func() {
			out, err = captureStdout(t, func() error { return run(args) })
		})
		if err != nil {
			t.Errorf("brig %s was refused instead of answered with help: %v", typed, err)
			continue
		}
		if !strings.Contains(out, "brig policy -- manage egress policies") {
			t.Errorf("brig %s did not print the policy usage:\n%s", typed, out)
		}
		// The listing is what it used to print instead of help. Nothing of it
		// may survive alongside the usage.
		if strings.Contains(out, "no policies yet") {
			t.Errorf("brig %s printed the listing as well as the help:\n%s", typed, out)
		}
	}
}

// The sibling the rule is borrowed from, so a change that stopped translating
// flag.ErrHelp for the group would fail here rather than only where ls asserts
// it.
func TestPolicySiblingsStillAnswerHelp(t *testing.T) {
	for _, verb := range []string{"show", "create", "rm", "attach"} {
		t.Setenv("BRIG_POLICY_DIR", t.TempDir())
		t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
		var err error
		var out string
		captureStderr(t, func() {
			out, err = captureStdout(t, func() error { return run([]string{"policy", verb, "--help"}) })
		})
		if err != nil {
			t.Errorf("brig policy %s --help was refused: %v", verb, err)
		}
		if !strings.Contains(out, "brig policy -- manage egress policies") {
			t.Errorf("brig policy %s --help did not print the policy usage:\n%s", verb, out)
		}
	}
}
