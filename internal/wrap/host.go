package wrap

// MacOSVersion reports the host's macOS product version like "15.6", or "" off
// macOS.
//
// It is the very reading Load seeds Config.MacOSVersion with -- the same
// kern.osproductversion sysctl, no second way to ask -- exported so brig doctor
// can name the host without building a Config for the sake of one field. The
// per-platform macOSVersion behind it is where the sysctl and the "" off-macOS
// answer are documented.
func MacOSVersion() string { return macOSVersion() }
