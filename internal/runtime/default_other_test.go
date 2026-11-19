//go:build !darwin

package runtime

import (
	"errors"
	"testing"
)

// The default follows what the host carries: a Linux host with hull boots on
// hull, and one without it stays on nerdctl rather than failing every run
// with "no hull on PATH".
func TestDefaultKindOnLinuxFollowsHull(t *testing.T) {
	orig := lookHull
	t.Cleanup(func() { lookHull = orig })

	lookHull = func() error { return nil }
	if got := defaultKind(); got != "hull" {
		t.Errorf("with hull on PATH the default is %q, want hull", got)
	}
	lookHull = func() error { return errors.New("not found") }
	if got := defaultKind(); got != "nerdctl" {
		t.Errorf("without hull the default is %q, want nerdctl", got)
	}
}
