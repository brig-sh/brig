package secret

import (
	"context"
	"errors"
	"testing"
)

// A keychain call bound to an ended context never starts security(1), so
// this test reads no keychain.
func TestKeychainCallsStopWithTheirContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	k := keychain{service: testService + "context"}.WithContext(ctx)
	if _, err := k.Read("absent"); !errors.Is(err, context.Canceled) {
		t.Errorf("Read on a canceled context = %v, want context.Canceled", err)
	}
	if _, err := k.List(); !errors.Is(err, context.Canceled) {
		t.Errorf("List on a canceled context = %v, want context.Canceled", err)
	}
}
