package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNerdctlExistsUsesAllContainers(t *testing.T) {
	for _, tt := range []struct {
		name, list string
		status     int
		wantExists bool
		wantErr    bool
	}{
		{name: "absent", list: "brig-other\tUp 1 minute"},
		{name: "stopped", list: "brig-legacy\tExited (0)", wantExists: true},
		{name: "list failed", status: 1, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			n := &nerdctl{bin: networkInspectFixture(t, "", "", 0, tt.list, tt.status)}
			got, err := n.Exists("brig-legacy")
			if got != tt.wantExists || (err != nil) != tt.wantErr {
				t.Fatalf("Exists = %t, %v; want %t, error %t", got, err, tt.wantExists, tt.wantErr)
			}
		})
	}
}

func TestNerdctlExistsHonorsInspectionDeadline(t *testing.T) {
	// A canceled list says nothing about absence. Legacy recovery must keep
	// its subprocess deadline when it shares the ordinary Exists path.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	n := &nerdctl{bin: networkInspectFixture(t, "", "", 0, "", 0)}
	if got, err := n.exists(ctx, "brig-legacy"); got || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exists = %t, %v; want false, deadline exceeded", got, err)
	}
}
