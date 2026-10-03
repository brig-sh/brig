package redact

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestTextReplacesRegisteredValues(t *testing.T) {
	r := testRedactor()
	r.Var("ACME_CANARY_TOKEN")
	r.Workspace("/home/zz-canary/code/acme-canary-api")
	r.Image("ghcr.io/acme-canary/agent:1.0")

	in := "mounted /home/zz-canary/code/acme-canary-api on canary-box as zz-canary; " +
		"forwarding ACME_CANARY_TOKEN; pulled ghcr.io/acme-canary/agent:1.0; home /home/zz-canary/.cache"
	got := r.Text(in).String()
	for _, leaked := range []string{"zz-canary", "canary-box", "ACME_CANARY_TOKEN", "acme-canary"} {
		if strings.Contains(got, leaked) {
			t.Errorf("%q survived: %s", leaked, got)
		}
	}
	for _, want := range []string{"<workspace-1>", "<host>", "<user>", "<VAR-1>", "$HOME/.cache"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
}

// A value is replaced only as a whole word: "al" leaves "also" and
// /home/alice alone.
func TestTextReplacesWholeWordsOnly(t *testing.T) {
	r := New(Host{Home: "/home/al", User: "al"}, Allow{})
	got := r.Text("al also /home/al/x /home/alice").String()
	if want := "<user> also $HOME/x /home/alice"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A second scrub changes nothing, even when the user name is the word inside
// its own placeholder.
func TestTextIsStable(t *testing.T) {
	r := New(Host{User: "user", Hostname: "host"}, Allow{})
	once := r.Text("user@host logged in as user").String()
	twice := r.Text(once).String()
	if once != twice {
		t.Errorf("second pass changed %q to %q", once, twice)
	}
	if strings.Contains(once, "<<") {
		t.Errorf("nested placeholder: %q", once)
	}
}

func TestTextRemovesTokenShapes(t *testing.T) {
	r := testRedactor()
	secrets := map[string]string{
		"anthropic":       "sk-ant-api03-AbCdEfGhIjKlMnOpQrStUv",
		"openai":          "sk-proj-AbCdEfGhIjKlMnOpQrStUvWx",
		"github":          "ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123",
		"github-pat":      "github_pat_11AbCdEfGhIjKlMnOpQrSt",
		"aws":             "AKIAABCDEFGHIJKLMNOP",
		"slack":           "xoxb-1234567890-abcdefghij",
		"jwt":             "eyJhbGciOiJIUzI1.eyJzdWIiOiIxMjM0.SflKxwRJSMeKKF2QT4f",
		"bearer":          "Authorization: Bearer abcdefghijklmnop",
		"url-credentials": "https://bob:hunter22@git.example.com/x",
		"assignment":      "GITHUB_TOKEN=plainvalue123",
		"private-key":     "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----",
	}
	for name, s := range secrets {
		got := r.Text("x " + s + " y").String()
		if !strings.Contains(got, "<redacted:") {
			t.Errorf("%s: not redacted: %q", name, got)
		}
		if err := r.Check([]byte(got)); err != nil {
			t.Errorf("%s: scrubbed text fails its own check: %v (%q)", name, err, got)
		}
	}
}

// A secret assigned to a registered variable is removed.
func TestTextRemovesAValueAssignedToARegisteredVar(t *testing.T) {
	r := testRedactor()
	r.Var("ACME_CANARY_TOKEN")
	got := r.Text("ACME_CANARY_TOKEN=supersecretcanary1").String()
	if strings.Contains(got, "supersecretcanary1") || strings.Contains(got, "ACME_CANARY_TOKEN") {
		t.Errorf("got %q", got)
	}
}

// An email whose domain holds a registered name is replaced whole.
func TestTextReplacesAnEmailBeforeANameInsideIt(t *testing.T) {
	r := testRedactor()
	r.Name(KindProfile, "acme-canary")
	got := r.Text("mail zz@acme-canary.io").String()
	if strings.Contains(got, "zz@") || !strings.Contains(got, "<email-1>") {
		t.Errorf("got %q", got)
	}
}

// Near-misses stay: versions, digests, timestamps and ordinary words.
func TestTextLeavesNearMisses(t *testing.T) {
	r := testRedactor()
	for _, s := range []string{
		"brig v0.3.1-0.20260916120000-b2b2b2b2b2b2",
		"sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		"2026-09-27T10:22:33Z",
		"task-runner started",
		"see /dev/null",
	} {
		if got := r.Text(s).String(); got != s {
			t.Errorf("changed %q to %q", s, got)
		}
	}
}

func TestTextReplacesAddresses(t *testing.T) {
	r := testRedactor()
	got := r.Text("mail zz@acme-canary.io from 203.0.113.7 and 2001:db8::1 mac aa:bb:cc:dd:ee:ff; gw 198.18.0.1; lo 127.0.0.1").String()
	for _, leaked := range []string{"zz@acme-canary.io", "203.0.113.7", "2001:db8::1", "aa:bb:cc:dd:ee:ff"} {
		if strings.Contains(got, leaked) {
			t.Errorf("%q survived: %s", leaked, got)
		}
	}
	for _, kept := range []string{"198.18.0.1", "127.0.0.1"} {
		if !strings.Contains(got, kept) {
			t.Errorf("allowlisted %q was replaced: %s", kept, got)
		}
	}
}

// Check fails on raw values.
func TestCheckFailsClosed(t *testing.T) {
	r := testRedactor()
	r.Var("ACME_CANARY_TOKEN")
	for _, raw := range []string{
		"ACME_CANARY_TOKEN",
		"/home/zz-canary/x",
		"on canary-box",
		"ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123",
	} {
		err := r.Check([]byte(raw))
		var leak *LeakError
		if !errors.As(err, &leak) {
			t.Errorf("Check(%q) = %v, want a LeakError", raw, err)
		}
	}
	if err := r.Check([]byte(r.Text("ACME_CANARY_TOKEN on canary-box").String())); err != nil {
		t.Errorf("scrubbed text failed the check: %v", err)
	}
}

// Short registered values are not searched in text.
func TestShortValuesAreNotSearchedInText(t *testing.T) {
	r := testRedactor()
	r.Image("ghcr.io/acme-canary/agent:dev")
	if got := r.Text("see /dev/null").String(); got != "see /dev/null" {
		t.Errorf("a three-letter tag rewrote text: %q", got)
	}
}

// wantScrubbed asserts that Text removes each secret, the result passes
// Check, a second scrub changes nothing, and, if failRaw, Check refuses raw.
func wantScrubbed(t *testing.T, r *Redactor, raw string, failRaw bool, secrets ...string) {
	t.Helper()
	if failRaw {
		var leak *LeakError
		if err := r.Check([]byte(raw)); !errors.As(err, &leak) {
			t.Errorf("Check(%q) = %v, want a LeakError", raw, err)
		}
	}
	got := r.Text(raw).String()
	for _, s := range secrets {
		if strings.Contains(got, s) {
			t.Errorf("%q survived in %q", s, got)
		}
	}
	if err := r.Check([]byte(got)); err != nil {
		t.Errorf("scrubbed %q fails its own check: %v", got, err)
	}
	if twice := r.Text(got).String(); twice != got {
		t.Errorf("second pass changed %q to %q", got, twice)
	}
}

// An IPv6 address next to a colon is still found.
func TestTextRemovesIPv6NextToAColon(t *testing.T) {
	for _, c := range []struct{ raw, addr string }{
		{"ip:2001:db8::1 up", "2001:db8::1"},
		{"peer fe80::1: timeout", "fe80::1"},
		{"addr 2001:db8::7:", "2001:db8::7"},
		{"x :2001:db8::9 y", "2001:db8::9"},
	} {
		wantScrubbed(t, testRedactor(), c.raw, false, c.addr)
	}
}

// A candidate does not start inside a word: core::fmt is kept.
func TestTextLeavesDoubleColonPaths(t *testing.T) {
	r := testRedactor()
	for _, s := range []string{"core::fmt std::vector", "a::b::cz"} {
		if got := r.Text(s).String(); got != s {
			t.Errorf("changed %q to %q", s, got)
		}
	}
}

// Auth schemes, JSON keys, a quoted value holding '<', flags and dashed keys.
func TestTextRemovesMoreCredentialShapes(t *testing.T) {
	for _, c := range []struct {
		raw     string
		secrets []string
	}{
		{"Authorization: Basic dXNlcjpodW50ZXIyMg==", []string{"dXNlcjpodW50ZXIyMg"}},
		{"Authorization: Digest abcdefgh12345678", []string{"abcdefgh12345678"}},
		{"auth token abcdefgh12345678", []string{"abcdefgh12345678"}},
		{`{"token": "plainsecretvalue99", "password":"hunter2hunter2"}`, []string{"plainsecretvalue99", "hunter2hunter2"}},
		{`{'api_key': 'singlequoted99'}`, []string{"singlequoted99"}},
		{`PASSWORD="ab<cdsecret"`, []string{"cdsecret"}},
		{"run --token value", []string{"value"}},
		{"run --password=value", []string{"value"}},
		{"run --github-token=-dashsecret", []string{"dashsecret"}},
		{"x-api-key: abcdef123456", []string{"abcdef123456"}},
		{`PASSWORD="unterminated99`, []string{"unterminated99"}},
	} {
		wantScrubbed(t, testRedactor(), c.raw, true, c.secrets...)
	}
}

// Scrub output after a key passes Check.
func TestCheckPassesPlaceholdersAfterAKey(t *testing.T) {
	r := testRedactor()
	for _, s := range []string{
		`"forward": ["<VAR-1>"]`,
		`"forward_token_vars": ["<VAR-1>"]`,
		"KEY: <redacted:assignment>",
		`"token": "<VAR-1>"`,
		`"password": "<redacted:assignment>"`,
		"--token <redacted:cli-flag>",
		"Authorization: <redacted:assignment> <redacted:auth>",
	} {
		if err := r.Check([]byte(s)); err != nil {
			t.Errorf("Check(%q) = %v", s, err)
		}
		if got := r.Text(s).String(); got != s {
			t.Errorf("Text changed %q to %q", s, got)
		}
	}
}

// A private key cut off before its END line is still a private key.
func TestTextRemovesATruncatedPrivateKey(t *testing.T) {
	r := testRedactor()
	wantScrubbed(t, r, "log\n-----BEGIN RSA PRIVATE KEY-----\nMIIEsecretbody", true, "MIIEsecretbody")
	var leak *LeakError
	if err := r.Check([]byte("see BEGIN OPENSSH PRIVATE KEY")); !errors.As(err, &leak) {
		t.Errorf("a bare BEGIN marker passed the check: %v", err)
	}
}

// A registered value inside or next to non-placeholder angle brackets is
// still replaced.
func TestTextMatchesNextToAngleBrackets(t *testing.T) {
	for _, c := range []struct{ raw, secret string }{
		{"host <canary-box> ok", "canary-box"},
		{"prompt canary-box> ls", "canary-box"},
		{"user <zz-canary>", "zz-canary"},
	} {
		wantScrubbed(t, testRedactor(), c.raw, true, c.secret)
	}
}

// Host names, domains and emails match case-insensitively; other kinds match
// exactly.
func TestTextMatchesHostsCaseInsensitively(t *testing.T) {
	r := testRedactor()
	for _, s := range []string{"CANARY-BOX.LOCAL", "Canary-Box"} {
		if got := r.Text(s).String(); got != "<host>" {
			t.Errorf("Text(%q) = %q, want <host>", s, got)
		}
	}
	ph := r.Domain("git.acme.io")
	if got := r.Text("pull GIT.ACME.IO").String(); got != "pull "+ph {
		t.Errorf("got %q, want %q", got, "pull "+ph)
	}
	var leak *LeakError
	if err := r.Check([]byte("on Canary-Box")); !errors.As(err, &leak) {
		t.Errorf("Check passed a differently-cased host: %v", err)
	}
	r.Var("ACME_CANARY_TOKEN")
	if got := r.Text("acme_canary_token").String(); got != "acme_canary_token" {
		t.Errorf("a variable name matched case-insensitively: %q", got)
	}
}

// No part of a secret survives a character the pattern might stop at.
func TestTextRemovesWholeValues(t *testing.T) {
	for _, c := range []struct {
		raw     string
		failRaw bool
		secret  string
	}{
		{`PASSWORD="ab<cdsecret`, true, "cdsecret"},
		{`PASSWORD=ab<cdsecret`, true, "cdsecret"},
		{`--token "my secret words"`, true, "secret words"},
		{`--token 'my secret words'`, true, "secret words"},
		{`--token="my secret words"`, true, "secret words"},
		{`"password":"a\"bsecret"`, true, "bsecret"},
		{`'password':'a\'bsecret'`, true, "bsecret"},
		{`password=pa'ssword99`, true, "ssword99"},
		{`password=a,bcdefsecret`, true, "bcdefsecret"},
		{`password=a;bcdefsecret`, true, "bcdefsecret"},
		{`token abcdefgh!restsecret`, true, "restsecret"},
		{`PASSWORD=ab<VAR-1>zzsecret`, true, "zzsecret"},
	} {
		wantScrubbed(t, testRedactor(), c.raw, c.failRaw, c.secret)
	}
}

// A list of assignments still splits at a separator followed by a space.
func TestTextSplitsAssignmentLists(t *testing.T) {
	r := testRedactor()
	got := r.Text("TOKEN=abc, mode=fast; SECRET=def; next").String()
	if want := "TOKEN=<redacted:assignment>, mode=fast; SECRET=<redacted:assignment>; next"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A registered value in placeholder-like brackets is still replaced.
func TestTextMatchesInsideAFakePlaceholder(t *testing.T) {
	wantScrubbed(t, testRedactor(), "<redacted:canary-box>", true, "canary-box")
	wantScrubbed(t, testRedactor(), "<host-canary-box>", true, "canary-box")
}

// Two spellings of one email get one placeholder.
func TestTextGivesEmailSpellingsOnePlaceholder(t *testing.T) {
	r := testRedactor()
	got := r.Text("ZZ@ACME-CANARY.IO and zz@acme-canary.io").String()
	if want := "<email-1> and <email-1>"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	for _, raw := range []string{"ZZ@ACME-CANARY.IO", "zz@acme-canary.io", "Zz@Acme-Canary.io"} {
		var leak *LeakError
		if err := r.Check([]byte(raw)); !errors.As(err, &leak) {
			t.Errorf("Check(%q) = %v, want a LeakError", raw, err)
		}
	}
}

// bounded recognises every placeholder the package writes. A new Kind goes
// here and in placeholderKinds.
func TestPlaceholderShapeCoversEveryKind(t *testing.T) {
	r := testRedactor()
	kinds := []Kind{
		KindUser, KindHost, KindWorkspace, KindPath, KindProfile, KindLabel, KindSandbox,
		KindPolicy, KindVar, KindRegistry, KindImage, KindTag, KindDomain, KindIP, KindEmail,
	}
	r.mu.Lock()
	var got []string
	for _, k := range kinds {
		got = append(got, r.placeholder(k, "value-for-"+string(k)))
	}
	r.mu.Unlock()
	got = append(got, "<user>", "<host>", "<mac>")
	for _, p := range tokenPatterns {
		got = append(got, "<redacted:"+p.kind+">")
	}
	for _, ph := range got {
		if !placeholderRE.MatchString(ph) {
			t.Errorf("placeholderRE does not match %q", ph)
		}
	}
}

// A value that runs into placeholders is scanned once, not once per '<'. The
// 20 s bound allows for -race and parallel CI; a scan per '<' takes about
// 81 s here without -race.
func TestTextIsLinearInPlaceholderRuns(t *testing.T) {
	r := testRedactor()
	raw := "password=a" + strings.Repeat("<VAR-1>", 30000) + "x"
	start := time.Now()
	got := r.Text(raw).String()
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("Text on %d bytes took %v", len(raw), d)
	}
	if err := r.Check([]byte(got)); err != nil {
		t.Errorf("scrubbed text fails its own check: %v", err)
	}
}

// Text glued to a closing quote is the same value: "abc"def.
func TestTextRemovesTextGluedToAQuote(t *testing.T) {
	for _, c := range []struct{ raw, secret string }{
		{`PASSWORD="abc"defsecret`, "defsecret"},
		{`PASSWORD='abc'defsecret`, "defsecret"},
		{`password: 'a\'b'cdsecret`, "cdsecret"},
		{`PASSWORD="a\\"bsecret"`, "bsecret"},
		{`PASSWORD=""secret`, "secret"},
		{`PASSWORD="<VAR-1>"secret`, "secret"},
		{`--token "a b"cdsecret`, "cdsecret"},
		{`--token='a b'cdsecret`, "cdsecret"},
	} {
		wantScrubbed(t, testRedactor(), c.raw, true, c.secret)
	}
	// A separator after the quote still ends the value.
	r := testRedactor()
	got := r.Text(`{"token":"abc","n":1} ["--token", "x"]`).String()
	if want := `{"token":"<redacted:assignment>","n":1} ["--token", "x"]`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// An auth credential inside a JSON string ends at the closing quote.
func TestTextKeepsJSONAfterAnAuthCredential(t *testing.T) {
	r := testRedactor()
	got := r.Text(`{"auth":"Bearer abcdefghij","x":1}`).String()
	if strings.Contains(got, "abcdefghij") || !strings.HasSuffix(got, `","x":1}`) {
		t.Errorf("got %q", got)
	}
	wantScrubbed(t, r, `{"auth":"Bearer abcdefghij","x":1}`, true, "abcdefghij")
	wantScrubbed(t, r, `x "Bearer abcdefgh"z"secret"`, true, "secret")
}

// A quote reopened after a glued or unquoted part is the same value:
// abc"def ghi".
func TestTextRemovesReopenedQuotes(t *testing.T) {
	for _, c := range []struct{ raw, secret string }{
		{`PASSWORD="abc"def"ghi jkl"`, "jkl"},
		{`PASSWORD="abc"def'ghi jkl'`, "jkl"},
		{`--token "abc"def"x y"`, " y"},
		{`PASSWORD=abc"def ghi"`, "ghi"},
		{`--password="a"b"c d"`, " d"},
		{`--token abc"def ghi"`, "ghi"},
		{`--token=abc'def ghi'`, "ghi"},
	} {
		wantScrubbed(t, testRedactor(), c.raw, true, c.secret)
	}
	// A separator outside the quotes still ends the value.
	r := testRedactor()
	for raw, want := range map[string]string{
		`{"token":"abc","n":1}`:                  `{"token":"<redacted:assignment>","n":1}`,
		`TOKEN=abc, mode=fast; SECRET=def; next`: `TOKEN=<redacted:assignment>, mode=fast; SECRET=<redacted:assignment>; next`,
		`TOKEN=a"b,c", mode=fast`:                `TOKEN=<redacted:assignment>, mode=fast`,
	} {
		if got := r.Text(raw).String(); got != want {
			t.Errorf("Text(%q) = %q, want %q", raw, got, want)
		}
	}
}

// A separator ends a flag's value, quoted or not, and the result passes
// Check.
func TestTextKeepsASeparatorAfterAFlagValue(t *testing.T) {
	for raw, want := range map[string]string{
		`app --token "abc123";`:        `app --token <redacted:cli-flag>;`,
		`--password="abc123"; echo ok`: `--password=<redacted:cli-flag>; echo ok`,
		`--token 'abc123', next`:       `--token <redacted:cli-flag>, next`,
		`--token abc123; next`:         `--token <redacted:cli-flag>; next`,
		`--password=abc123, next`:      `--password=<redacted:cli-flag>, next`,
		`{"x": --token "abc123"}`:      `{"x": --token <redacted:cli-flag>}`,
		`[--token="abc123"]`:           `[--token=<redacted:cli-flag>]`,
		`--token abc;defsecret`:        `--token <redacted:cli-flag>`,
	} {
		wantScrubbed(t, testRedactor(), raw, true, "abc")
		if got := testRedactor().Text(raw).String(); got != want {
			t.Errorf("Text(%q) = %q, want %q", raw, got, want)
		}
	}
}

// A URL password runs to the last '@' before the path and may hold '<'.
func TestTextRemovesWholeURLPasswords(t *testing.T) {
	wantScrubbed(t, testRedactor(), "https://bob:p@ss@localhost:5000/x", true, "ss@", "bob")
	wantScrubbed(t, testRedactor(), "https://bob:hunter<2@registry.internal/x", true, "hunter", "bob")
}

// Check refuses an email or a MAC address.
func TestCheckRefusesEmailsAndMACs(t *testing.T) {
	r := testRedactor()
	for raw, kind := range map[string]string{
		"mail zz@acme-canary.io":  "email",
		"ether aa:bb:cc:dd:ee:ff": "mac",
	} {
		var leak *LeakError
		if err := r.Check([]byte(raw)); !errors.As(err, &leak) || leak.Kind != kind {
			t.Errorf("Check(%q) = %v, want a %s LeakError", raw, err, kind)
		}
		wantScrubbed(t, r, raw, true, "zz@", "aa:bb")
	}
}

// IPv6 candidate boundaries: nothing word-like on the right; anything but a
// letter or digit on the left, a colon included. Each row gets its own
// redactor so placeholder numbers do not depend on row order.
func TestTextIPv6NeedsBoundariesBothSides(t *testing.T) {
	for _, s := range []string{"fe80::1zz-canary", "core::fmt std::vector", "a::b::cz"} {
		r := testRedactor()
		if got := r.Text(s).String(); got != s {
			t.Errorf("changed %q to %q", s, got)
		}
		wantScrubbed(t, r, s, false)
	}
	for _, c := range []struct{ raw, want, addr string }{
		{"a 2001:db8::1,2001:db8::2 b", "a <ip-1>,<ip-2> b", "db8::"},
		{"src:2001:db8::1 up", "src:<ip-1> up", "db8::1"},
		{"ip:2001:db8::1 up", "ip:<ip-1> up", "db8::1"},
		{"peer 2001:db8::1: timeout", "peer <ip-1>: timeout", "db8::1"},
		{"fe80::1%eth0", "<ip-1>%eth0", "fe80::1"},
		{"x2001:db8::1 up", "x2001:<ip-1> up", "db8::1"},
		{"eth0:fe80::1", "eth0:<ip-1>", "fe80::1"},
		{"ipv6:2001:db8::1", "ipv6:<ip-1>", "db8::1"},
		{"if1:fe80::abcd", "if1:<ip-1>", "fe80::abcd"},
		{"Z2001:db8::1", "Z2001:<ip-1>", "db8::1"},
	} {
		r := testRedactor()
		got := r.Text(c.raw).String()
		if got != c.want {
			t.Errorf("Text(%q) = %q, want %q", c.raw, got, c.want)
		}
		if again := r.Text(got).String(); again != got {
			t.Errorf("Text is not idempotent on %q: %q", got, again)
		}
		wantScrubbed(t, r, c.raw, false, c.addr)
	}
}
