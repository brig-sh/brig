// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package nofireauth_test

import (
	"testing"

	"github.com/brig-sh/brig/enterprise/internal/nofireauth"
)

func TestHeadless(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		goos string
		want bool
	}{
		{"ssh connection", map[string]string{"SSH_CONNECTION": "1.2.3.4 1 5.6.7.8 22"}, "darwin", true},
		{"ssh tty", map[string]string{"SSH_TTY": "/dev/pts/0"}, "darwin", true},
		{"ssh with a display", map[string]string{"SSH_TTY": "/dev/pts/0", "DISPLAY": ":0"}, "linux", true},
		{"linux, no display", nil, "linux", true},
		{"linux, x11", map[string]string{"DISPLAY": ":0"}, "linux", false},
		{"linux, wayland", map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, "linux", false},
		{"darwin, nothing set", nil, "darwin", false},
		{"windows, nothing set", nil, "windows", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nofireauth.Headless(func(k string) string { return tc.env[k] }, tc.goos)
			if got != tc.want {
				t.Errorf("Headless = %v, want %v", got, tc.want)
			}
		})
	}
}
