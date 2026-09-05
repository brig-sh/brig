package runtime

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestHullVersionPinsDigest pins the version at which hull resolves a digest
// reference against its own store. Before 0.1.0-rc23 a repo@sha256:... boot
// missed the cache and re-pulled every time, and failed outright under
// --pull=never with the bytes on disk, so brig verified and booted the tag
// there instead. From rc23 the store answers a digest, and brig pins it.
//
// Anything brig cannot read as a version is a build from source, which is a
// maintainer's, and it pins: a wrong guess there costs a re-pull, never a
// weaker check, because a digest the store does not hold is fetched from the
// registry and is the verified bytes either way.
func TestHullVersionPinsDigest(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"hull version 0.1.0-rc21\n", false},
		{"hull version 0.1.0-rc9\n", false},
		{"hull version 0.1.0-rc22\n", false},
		{"hull version 0.1.0-rc23\n", true},
		{"hull version 0.1.0-rc30\n", true},
		{"hull version 0.1.0\n", true},
		{"hull version 0.2.0-rc1\n", true},
		{"hull version 1.0.0\n", true},
		{"hull version \n", true},
		{"hull version dev\n", true},
		{"", true},
		{"garbage\n", true},
	}
	for _, tc := range cases {
		if got := hullVersionPinsDigest(tc.out); got != tc.want {
			t.Errorf("hullVersionPinsDigest(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
}

// stubHull writes a hull that answers --version with the given line and does
// nothing else, which is all PinsDigest needs from the binary.
func stubHull(t *testing.T, versionLine string) Runtime {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hull")
	script := "#!/bin/sh\ncase \"$1\" in --version) echo \"" + versionLine + "\";; esac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	rt, err := newHull(path)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

// TestHullPinsDigestAsksTheBinary: the answer comes from the hull on this
// machine, not from a build-time constant, because the hull brig drives is
// whatever is on PATH or in BRIG_RUNTIME_BIN and may be older than brig.
func TestHullPinsDigestAsksTheBinary(t *testing.T) {
	if stubHull(t, "hull version 0.1.0-rc21").PinsDigest() {
		t.Fatal("rc21 cannot resolve a digest against its store, and was reported as pinning")
	}
	if !stubHull(t, "hull version 0.1.0-rc23").PinsDigest() {
		t.Fatal("rc23 resolves a digest against its store, and was reported as not pinning")
	}
}

// TestRunArgsBootsThePinnedDigest: when the verify path resolved a digest, hull
// boots repo@digest rather than the tag, so the bytes that boot are the bytes
// cosign checked. With no digest the tag boots as before.
func TestRunArgsBootsThePinnedDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	pinned := RunSpec{Name: "s", Image: "ghcr.io/brig-sh/x:latest", Digest: digest}
	args, _, err := runArgs(pinned, "vz", "shared", "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(args, "ghcr.io/brig-sh/x@"+digest) {
		t.Fatalf("the pinned digest did not reach hull run: %q", args)
	}
	if slices.Contains(args, "ghcr.io/brig-sh/x:latest") {
		t.Fatalf("the tag was passed alongside the digest: %q", args)
	}

	unpinned := RunSpec{Name: "s", Image: "ghcr.io/brig-sh/x:latest"}
	args, _, err = runArgs(unpinned, "vz", "shared", "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(args, "ghcr.io/brig-sh/x:latest") {
		t.Fatalf("with no digest the tag should boot: %q", args)
	}
}

// TestHullLocalDigestIsUnknown documents a limit rather than a feature: hull
// exposes no way to read the full digest, let alone the index digest, that its
// store holds for a reference, so brig cannot compare the copy on disk against
// the registry there. It answers "" for "cannot say", which the verify path
// treats as no local copy and raises no mismatch over. The boot is still pinned.
func TestHullLocalDigestIsUnknown(t *testing.T) {
	got, err := stubHull(t, "hull version 0.1.0-rc23").LocalDigest("ghcr.io/brig-sh/x:latest")
	if err != nil || got != "" {
		t.Fatalf("LocalDigest = %q, %v; want \"\", nil", got, err)
	}
}
