package wrap

import "github.com/brig-sh/brig/internal/verify"

// HostVerifyPolicy is the image trust policy a run of the named profile checks
// against: the shipped one with BRIG_VERIFY_REGISTRY, BRIG_VERIFY_IDENTITY,
// BRIG_VERIFY_ISSUER and BRIG_COSIGN_BIN applied, each per-agent first. brig
// doctor asks for it here so it reports the policy a run uses. When doctor built
// its own from verify.DefaultPolicy, a host that set any of these got an ok from
// doctor for an image the run then checked against another identity.
//
// An empty name is no agent, so only the global variables apply. Left to
// NewEnv, an empty name looks up BRIG__VERIFY_IDENTITY and friends first.
func HostVerifyPolicy(profileName string) verify.Policy {
	env := NewEnv(profileName, nil)
	if profileName == "" {
		env.prefix = "BRIG"
	}
	return verifyPolicy(env)
}
