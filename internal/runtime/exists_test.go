package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestHullExistsHonorsInspectionDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if exists, err := inspectHull(t).exists(ctx, "brig-stopped"); exists || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exists = %t, %v; want false, deadline exceeded", exists, err)
	}
}

// Exists asks `hull inspect`, which answers for a stopped instance too. It
// preserves errors other than hull's ambiguous not-found response; legacy
// posture recovery cannot use the latter as proof of absence.
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
