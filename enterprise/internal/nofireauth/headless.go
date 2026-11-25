// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package nofireauth

// Headless reports whether this host probably has no browser the user can
// see: an SSH session, or Linux without a display. brig login then defaults
// to the device flow, because a browser opened on a remote host would be
// one the user cannot reach.
func Headless(getenv func(string) string, goos string) bool {
	if getenv("SSH_CONNECTION") != "" || getenv("SSH_TTY") != "" {
		return true
	}
	return goos == "linux" && getenv("DISPLAY") == "" && getenv("WAYLAND_DISPLAY") == ""
}
