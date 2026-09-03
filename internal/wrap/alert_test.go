package wrap

import (
	"bufio"
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/verify"
)

// Every way verification can go wrong reaches the reader under -q.
//
// Table-driven on purpose: this is the whole of the change, and the cases are
// the outcomes the decision tables can actually produce -- the mode being off,
// a runtime that cannot pin what it checked, an image nobody claimed to
// publish, a machine with no cosign, a signature that failed, and a local copy
// that is not the digest that verified. A run that asked for silence still
// hears all of them.
func TestVerificationProblemsSurviveQuiet(t *testing.T) {
	const ours = "ghcr.io/brig-sh/claude-code:arm64"
	other := "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	for _, tc := range []struct {
		name  string
		build func(t *testing.T) *Config
		want  string
	}{
		{
			name: "checking is off",
			build: func(t *testing.T) *Config {
				c := verifyConfig(t, ours, verify.Off)
				return c
			},
			want: "BRIG_VERIFY=off",
		},
		{
			name: "the runtime cannot boot what was checked",
			build: func(t *testing.T) *Config {
				return verifyConfig(t, ours, verify.Warn)
			},
			want: "cannot boot by digest",
		},
		{
			name: "nothing on the machine can check",
			build: func(t *testing.T) *Config {
				return verifyConfig(t, ours, verify.Warn)
			},
			want: "cosign is not installed",
		},
		{
			name: "the image is nobody's we know",
			build: func(t *testing.T) *Config {
				c := digestConfig(t, "docker.io/someone/else:latest", verify.Warn,
					fakeCosign(t, testDigest, false), testDigest)
				return c
			},
			want: "is not published by brig-sh, so there is no signature of ours to check",
		},
		{
			name: "the signature failed",
			build: func(t *testing.T) *Config {
				return digestConfig(t, ours, verify.Warn, fakeCosign(t, testDigest, true), testDigest)
			},
			want: "DID NOT VERIFY",
		},
		{
			name: "the copy on disk is not what verified",
			build: func(t *testing.T) *Config {
				return digestConfig(t, ours, verify.Warn, fakeCosign(t, testDigest, false), other)
			},
			want: "NOT the sha256:",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.build(t)
			c.Verbosity = Quiet
			said := &bytes.Buffer{}
			c.Err = said

			// The answer does not matter: a refusal and a boot both have to have
			// said what was wrong before they get here.
			_ = c.verifyImage()

			if said.Len() == 0 {
				t.Fatalf("-q silenced a verification problem entirely")
			}
			if !strings.Contains(said.String(), tc.want) {
				t.Errorf("-q said %q, want it to mention %q", said.String(), tc.want)
			}
		})
	}
}

// The boot assets are the other half of the same claim -- the kernel is the
// more privileged of the two -- so they are not quieter than the image.
func TestBootAssetProblemsSurviveQuiet(t *testing.T) {
	c := verifyConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Warn)
	c.Profile.GenericBoot = true
	c.Verbosity = Quiet
	said := &bytes.Buffer{}
	c.Err = said

	if err := c.verifyBootAssets(); err != nil {
		t.Fatalf("warn mode must not refuse: %v", err)
	}
	if said.Len() == 0 {
		t.Error("-q silenced a boot-asset verification problem")
	}
}

// And there is nobody to ask is the same class: it is the check declining to
// proceed, not a note about one.
func TestNobodyToAskSurvivesQuiet(t *testing.T) {
	c, said, _ := levelled(Quiet)
	c.NoTerminal = true

	if c.confirm("Boot it anyway?") {
		t.Fatal("a run with nobody to ask must answer no")
	}
	if !strings.Contains(said.String(), "nobody to ask") {
		t.Errorf("-q silenced the refusal: %q", said.String())
	}
}

// The scoping, which is what keeps the level meaningful. An ordinary warning is
// still a warning: a level that collects "important" collects everything within
// a release, and then -q prints what it was added to suppress.
func TestAnOrdinaryWarningIsStillSuppressedByQuiet(t *testing.T) {
	c, said, _ := levelled(Quiet)
	c.warnf("the imported credential expired 24m ago")
	if said.Len() != 0 {
		t.Errorf("-q printed an ordinary warning: %q", said.String())
	}
}

// A site added to the verify path later must not reach for warnf, which -q
// would silence. The behavioural cases above cover the outcomes a test can
// drive; this covers the ones it cannot, and the ones nobody has written yet.
//
// Source-level because that is the only way to catch the site that does not
// exist. sayVerified is the one exception and it is named rather than pattern-
// matched: it reports that verification HELD, which is the one thing on this
// path a script does not need and -q is right to drop.
func TestVerifySitesDoNotReachForTheWarningLevel(t *testing.T) {
	f, err := os.Open("verify.go")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	allowed := map[string]bool{"sayVerified": true}
	fn, line := "", 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line++
		text := scanner.Text()
		if name, ok := funcName(text); ok {
			fn = name
		}
		if strings.Contains(text, "c.warnf(") && !allowed[fn] {
			t.Errorf("verify.go:%d: %s uses warnf, which -q silences. "+
				"A verification problem is an alertf: it has to reach a caller "+
				"that asked for silence. See alertf.", line, fn)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	// The allowlist must not outlive what it names, or it silently widens.
	if fn == "" {
		t.Fatal("no functions were found in verify.go; the scan proves nothing")
	}
}

// funcName reads a method declaration on Config, for the scan above.
func funcName(line string) (string, bool) {
	const prefix = "func (c *Config) "
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	name, _, ok := strings.Cut(strings.TrimPrefix(line, prefix), "(")
	return name, ok
}

// BRIG_VERIFY=off is stated exactly once, whether or not the envelope carried
// it, and at every level.
//
// Both halves are needed and neither is redundant: the VERIFY row says it on a
// run that printed the block, and the standalone line says it on one that did
// not -- a cold `brig sh`, and every run below --verbose. Saying it twice is
// how a reader learns to skip it, and saying it neither way is how a sandbox
// boots unchecked in silence.
func TestCheckingOffIsStatedExactlyOnce(t *testing.T) {
	for _, level := range []Verbosity{Quiet, Normal, Verbose} {
		for _, withEnvelope := range []bool{false, true} {
			c := verifyConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Off)
			c.Verbosity = level
			said := &bytes.Buffer{}
			c.Err = said
			if withEnvelope {
				c.PrintPreRunEnvelope(creds.Set{})
			}

			if err := c.verifyImage(); err != nil {
				t.Fatalf("level %d: off must not refuse: %v", level, err)
			}
			if got := strings.Count(said.String(), "BRIG_VERIFY=off"); got != 1 {
				t.Errorf("level %d, envelope %v: BRIG_VERIFY=off stated %d times, want 1:\n%s",
					level, withEnvelope, got, said.String())
			}
		}
	}
}
