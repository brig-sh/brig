package runtime

// On macOS the container is a microVM and hull is what boots it.
func defaultKind() string { return "hull" }
