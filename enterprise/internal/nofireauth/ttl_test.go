// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package nofireauth

import (
	"testing"
	"time"
)

func TestParseTTL(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"12h": 12 * time.Hour,
		"30d": 720 * time.Hour,
		"1h":  time.Hour,
	} {
		got, err := ParseTTL(in)
		if err != nil || got != want {
			t.Errorf("ParseTTL(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"0d", "0h", "-1h", "1w", "90m", "1.5d", "", "h", "d", "+1h", " 1h", "99999999999999999999d", "9999999999d"} {
		if got, err := ParseTTL(in); err == nil {
			t.Errorf("ParseTTL(%q) = %v, want an error", in, got)
		}
	}
}
