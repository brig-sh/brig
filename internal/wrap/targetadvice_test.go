package wrap

import (
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/profile"
)

// A repository profile's name registers nothing of yours with the broker:
// its workload is named after the target's sandbox.
func TestATargetRegistersWithTheBrokerUnderItsSandbox(t *testing.T) {
	digestHost(t)
	c := approvalConfig(t)
	if got := c.brokerProfile(); got != c.VMName || got == "claude-code" {
		t.Errorf("the broker profile of a target is %q", got)
	}
	p, err := Load(repoClaude(t), Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.brokerProfile(); got != "claude-code" {
		t.Errorf("the broker profile of a profile run is %q", got)
	}
}

// The temporary-home notice of a target names the rm of the target, not
// `brig rm claude-code`, which would remove your own session.
func TestTheTemporaryHomeNoticeOfATargetNamesItsRm(t *testing.T) {
	digestHost(t)
	c := approvalConfig(t)
	c.target.RmCommand = "brig rm --target dev"
	c.Project = "/src/a"
	var said strings.Builder
	c.Err = &said
	c.ephemeralNotice()
	if got := said.String(); !strings.Contains(got, "brig rm --target dev") || strings.Contains(got, "claude-code") {
		t.Errorf("the notice says:\n%s", got)
	}
}

// A route of a target whose secret is not in your store is never advised
// as an import, even on the claude-code brig ships: import reads your
// login, and the route the repository wrote decides where the secret goes.
// create names the one secret, and the plan names the route.
func TestAMissingRouteSecretOfATargetIsNotAdvisedAsAnImport(t *testing.T) {
	digestHost(t)
	p, _, ok := profile.BuiltIn("claude-code")
	if !ok {
		t.Fatal("no built-in claude-code")
	}
	p.Policy = nil
	id := ProjectIdentity("/src/a", "dev", "", "claude-code")
	tg := repoTarget()
	tg.Root, tg.Brigfile, tg.BuiltIn, tg.ProfileSource = "/src/a", "/src/a/Brigfile", true, "builtin:claude-code"
	collector := routePolicy("collector", "https://collector.example", "claude-credentials")
	c, err := Load(p, Options{Identity: &id, Target: tg,
		Policies: &PolicySet{Docs: []policy.Policy{collector}, Origins: []string{"brigfile collector"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.OpenStore = func() (creds.SecretReader, error) { return listingStore{}, nil }
	err = c.routeStoreRefusal(c.checkRouteStore())
	if err == nil {
		t.Fatal("a missing route secret was not refused")
	}
	if strings.Contains(err.Error(), "secret import") || !strings.Contains(err.Error(), "brig secret create claude-credentials") {
		t.Errorf("the advice for a target's route secret: %v", err)
	}
}
