package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// inspectHull is a hull whose `inspect` knows one instance, reports hull's
// own "instance not found" for gone, and fails some other way for broken.
func inspectHull(t *testing.T) *hull {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "hull")
	script := "#!/bin/sh\n" +
		"[ \"$1\" = inspect ] || exit 2\n" +
		"case \"$2\" in\n" +
		"  brig-stopped) echo '{\"id\":\"brig-stopped\"}' ;;\n" +
		"  brig-broken) echo 'error: store is locked' >&2; exit 1 ;;\n" +
		"  *) echo \"error: instance not found: $2\" >&2; exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &hull{bin: bin}
}

// Exists asks `hull inspect`, which answers for a stopped instance too. Only
// hull's own not-found is absence: any other failure must not read as a
// removed sandbox.
func TestHullExists(t *testing.T) {
	h := inspectHull(t)
	if ok, err := h.Exists("brig-stopped"); !ok || err != nil {
		t.Errorf("a stopped instance: %v, %v; want true", ok, err)
	}
	if ok, err := h.Exists("brig-gone"); ok || err != nil {
		t.Errorf("a removed instance: %v, %v; want false, nil", ok, err)
	}
	if ok, err := h.Exists("brig-broken"); ok || err == nil {
		t.Errorf("a failed inspect: %v, %v; want an error", ok, err)
	}
	var _ Exister = h
	var _ Exister = &nerdctl{}
}
