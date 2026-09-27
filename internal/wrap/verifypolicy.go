package wrap

import (
	"fmt"

	"github.com/brig-sh/brig/internal/verify"
)

// HostVerifyPolicy is the image trust policy a run of the named profile checks
// against: the shipped one with BRIG_VERIFY_REGISTRY, BRIG_VERIFY_IDENTITY,
// BRIG_VERIFY_ISSUER and BRIG_COSIGN_BIN applied, each per-agent first. brig
// doctor asks for it here so it reports the policy a run uses. When doctor built
// its own from verify.DefaultPolicy, a host that set any of these got an ok from
// doctor for an image the run then checked against another identity.
func HostVerifyPolicy(profileName string) verify.Policy {
	return verifyPolicy(hostEnv(profileName))
}

// HostVerifyMode is BRIG_VERIFY as a run of the named profile reads it,
// per-agent first, with the name of the variable it came from. doctor read
// only the global BRIG_VERIFY, so a host that set BRIG_<AGENT>_VERIFY=require
// saw warn in doctor and a refusal in the run. The name is BRIG_VERIFY when
// nothing is set, and an error quotes the variable the user wrote.
func HostVerifyMode(profileName string) (mode verify.Mode, from string, err error) {
	from, v, ok := hostEnv(profileName).getNamed("VERIFY")
	if !ok {
		from = "BRIG_VERIFY"
	}
	if mode, err = verify.ParseModeStrict(v); err != nil {
		return mode, from, fmt.Errorf("%s=%q is not a mode: use off, warn, or require", from, v)
	}
	return mode, from, nil
}

// hostEnv is the lookup Load builds for a profile, read from the process
// environment. An empty name is no agent, so only the global variables apply.
// Left to NewEnv, an empty name looks up BRIG__VERIFY_IDENTITY and friends
// first.
func hostEnv(profileName string) Env {
	env := NewEnv(profileName, nil)
	if profileName == "" {
		env.prefix = "BRIG"
	}
	return env
}
