//go:build !darwin

package runtime

// Everywhere else brig sits on containerd through nerdctl.
func defaultKind() string { return "nerdctl" }
