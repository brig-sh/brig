package cli

import (
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
)

// Brig Community has no brig login. The verb is answered with a hint rather
// than as an unknown word, and it touches nothing: no keychain, no network.
// The keychain is left alone by construction, since cmd/brig does not link
// the login store (TestRunPathCannotOpenTheLoginStore).
func TestCommunityLoginAndLogoutAreAHint(t *testing.T) {
	for verb, does := range map[string]string{"login": "log in to NOFire AI", "logout": "log out of NOFire AI"} {
		for _, args := range [][]string{{verb}, {verb, "https://my.nofire.ai", "--device"}, {verb, "--help"}} {
			want := "brig " + verb + " is part of Brig Enterprise, and this is Brig Community. Install Brig Enterprise to " + does
			out, err := captureStdout(t, func() error { return run(args) })
			if err == nil || err.Error() != want {
				t.Errorf("brig %s: err = %v, want %q", strings.Join(args, " "), err, want)
			}
			if exitCode(err) != exitFailure {
				t.Errorf("brig %s exits %d, want %d", strings.Join(args, " "), exitCode(err), exitFailure)
			}
			if out != "" {
				t.Errorf("brig %s printed %q, want nothing on stdout", strings.Join(args, " "), out)
			}
		}
	}
}

// Help and completion offer only commands that work, so Brig Community does
// not list brig login or brig logout.
func TestCommunityHelpAndCompletionDoNotOfferLogin(t *testing.T) {
	out, err := captureStdout(t, func() error { return run([]string{"help"}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "brig login") || strings.Contains(out, "brig logout") {
		t.Errorf("brig help lists login or logout:\n%s", out)
	}
	if _, got := complete([]string{"log"}); slices.Contains(got, "login") || slices.Contains(got, "logout") {
		t.Errorf("completion offers %v", got)
	}
}

// registerForTest records o as Main would, and undoes it when the test ends.
func registerForTest(t *testing.T, o Options) {
	t.Helper()
	oldVerbs, oldUsages, oldText, oldEdition := slices.Clone(verbs), maps.Clone(verbUsages), usageText, edition
	t.Cleanup(func() {
		verbs, verbUsages, usageText, edition = oldVerbs, oldUsages, oldText, oldEdition
		commands = map[string]Command{}
	})
	register(o)
}

// A command a build adds is run, helped and completed like the verbs every
// build has, and brig version names the build.
func TestARegisteredCommandIsRunHelpedAndCompleted(t *testing.T) {
	var got []string
	registerForTest(t, Options{Edition: "enterprise", Commands: []Command{{
		Verb:     "login",
		Synopsis: "  brig login [URL]                               log in\n",
		Usage:    "brig login -- log in\n",
		Complete: func(rest []string, cur string) []string { return []string{"--device", "--ttl"} },
		Run: func(out io.Writer, args []string) error {
			got = args
			_, err := io.WriteString(out, "ran\n")
			return err
		},
	}}})

	out, err := captureStdout(t, func() error { return run([]string{"login", "https://x.example", "--device"}) })
	if err != nil || out != "ran\n" || !slices.Equal(got, []string{"https://x.example", "--device"}) {
		t.Errorf("brig login: out %q, err %v, args %q", out, err, got)
	}
	out, _ = captureStdout(t, func() error { return run([]string{"help"}) })
	if !strings.Contains(out, "  brig login [URL]                               log in\n"+usageAnchor) {
		t.Errorf("brig help does not list login above telemetry:\n%s", out)
	}
	for _, args := range [][]string{{"login", "--help"}, {"help", "login"}} {
		out, _ = captureStdout(t, func() error { return run(args) })
		if out != "brig login -- log in\n" {
			t.Errorf("brig %s printed %q, want the command's usage", strings.Join(args, " "), out)
		}
	}
	if _, got := complete([]string{"log"}); !slices.Contains(got, "login") {
		t.Errorf("completion offers %v, want login", got)
	}
	if dir, got := complete([]string{"login", "--t"}); dir != dirNames || !slices.Equal(got, []string{"--ttl"}) {
		t.Errorf("login --t completes to %s %v, want --ttl", dir, got)
	}
	out, _ = captureStdout(t, func() error { return run([]string{"version"}) })
	if !strings.HasSuffix(out, ", enterprise)\n") {
		t.Errorf("brig version printed %q, want the enterprise edition", out)
	}
}

// The added verbs' lines go above a line of usage, so that line must stay.
func TestUsageHasTheAnchorForAddedVerbs(t *testing.T) {
	if !strings.Contains(usage, "\n"+usageAnchor) {
		t.Errorf("usage has no line %q for added verbs to go above", usageAnchor)
	}
}
