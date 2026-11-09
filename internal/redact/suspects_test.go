package redact

import (
	"encoding/base64"
	"math/rand/v2"
	"strings"
	"testing"
)

// A credential of a shape no rule knows passes the masking. Suspects is what
// points at it: a random value printed bare, with no name in front, is found
// on its line.
func TestSuspectsFindsAnUnknownShape(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	missed := 0
	for i := 0; i < 200; i++ {
		b := make([]byte, 24+rng.IntN(24))
		for j := range b {
			b[j] = byte(rng.Uint32())
		}
		v := base64.RawURLEncoding.EncodeToString(b)
		text := String("connecting\nsession " + v + " accepted\n")
		if !strings.Contains(text, v) {
			continue // a rule masked it; nothing for Suspects to find
		}
		got := Suspects(text)
		if len(got) != 1 || got[0].Line != 2 || got[0].Start != v[:4] {
			t.Logf("missed %s", v)
			missed++
		}
	}
	// A random value can lack digits or an upper-case letter.
	if missed > 6 {
		t.Errorf("Suspects missed %d of 200 random values", missed)
	}
}

// The text a report is made of is not flagged: hashes, UUIDs, paths with
// random lower-case directories, kernel lines, identifiers, and masks.
func TestSuspectsLeavesOrdinaryTextAlone(t *testing.T) {
	for _, line := range []string{
		"sha256:de5ba1cecf90b306c7b3576194fce8c421cbbfc26556a53752e7992d76b4170d",
		"742b49f8-21b6-4f6b-8e3c-582486c5c544",
		"/var/folders/bt/djgr0q5d4pqdzymqbk890lmr0000gn/T/TestReportFileIsPrivate134726287/001",
		"/Users/pmoust/.hull/store/instances/brig-plum/boot-initrd",
		"[17538.451910] Out of memory: Killed process 21272 (node) total-vm:13653136kB, anon-rss:1052680kB",
		"func TestRedactMasksPlantedValues(t *testing.T)",
		"GH_TOKEN=[masked: env-secret] [masked: brig-token]",
		"virtio-net: gvisor-tap gateway relay via /Users/stelioschar/.brig/sandbox-brig-claude-code.sock.qemu",
		"go build -ldflags '-s -w -X github.com/brig-sh/brig/internal/buildinfo.gitCommit=761ecac37ee6d676ec12e6f876e6ae6e11aca442'",
	} {
		if got := Suspects(line); len(got) != 0 {
			t.Errorf("Suspects(%q) = %+v, want none", line, got)
		}
	}
}
