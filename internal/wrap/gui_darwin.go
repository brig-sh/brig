package wrap

import "os/exec"

// focusWindow brings a graphical sandbox's window to the front. The GUI
// runner is a regular NSApplication, so it shows up as a process that can be
// activated. Best-effort: a missing accessibility permission is not a reason
// to fail a run that otherwise worked.
func focusWindow() {
	_ = exec.Command("osascript", "-e",
		`tell application "System Events" to set frontmost of (first process whose name contains "vz-runner") to true`,
	).Run()
}
