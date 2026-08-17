package wrap

// warnExpiredSecrets says, before boot, that an imported credential has
// expired.
//
// A no-op until provenance is readable (PR 7). It replaces the warning at the
// old run.go:62, which read Profile.HostCredential and vanishes with it.
func (c *Config) warnExpiredSecrets() {}
