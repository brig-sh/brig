package wrap

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/policy"
)

// approvalConfig loads the dev session of /src/a on a repository profile,
// with the policies given, as the CLI would.
func approvalConfig(t *testing.T, docs ...policy.Policy) *Config {
	t.Helper()
	id := ProjectIdentity("/src/a", "dev", "", "")
	tg := repoTarget()
	tg.Root, tg.Brigfile = "/src/a", "/src/a/Brigfile"
	set := &PolicySet{}
	for _, d := range docs {
		set.Docs = append(set.Docs, d)
		set.Origins = append(set.Origins, "brigfile "+d.Metadata.Name+" (file:.brig/"+d.Metadata.Name+".yaml)")
	}
	c, err := Load(repoClaude(t), Options{Identity: &id, Target: tg, Policies: set}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func routePolicy(name, upstream, secret string) policy.Policy {
	return policy.Policy{Metadata: v1alpha1.ObjectMeta{Name: name}, Spec: v1alpha1.PolicySpec{
		Routes: []v1alpha1.Route{{Name: "api", Preset: "github-token", Upstream: upstream,
			Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: secret}}}}}}
}

func subjectOf(t *testing.T, c *Config) ApprovalSubject {
	t.Helper()
	s, ok := c.ApprovalSubject(ApprovalInput{Brigfile: []byte("apiVersion: brig.sh/v1alpha1\n"),
		Args: []string{"--x"}})
	if !ok {
		t.Fatal("a run of a target has no subject")
	}
	return s
}

// One configuration has one digest, run after run. A route's upstream and
// its secret are each a change, and the list names them.
func TestTheApprovalSubjectNamesARouteChange(t *testing.T) {
	digestHost(t)
	base := subjectOf(t, approvalConfig(t, routePolicy("gh", "https://api.github.com", "gh-token")))
	if again := subjectOf(t, approvalConfig(t, routePolicy("gh", "https://api.github.com", "gh-token"))); ApprovalDigest(again) != ApprovalDigest(base) {
		t.Fatal("the same configuration has two digests")
	}
	for _, c := range []struct {
		name string
		pol  policy.Policy
		want string
	}{
		{"upstream", routePolicy("gh", "https://collector.example", "gh-token"),
			"route api sends to https://collector.example, and sent to https://api.github.com"},
		{"secret", routePolicy("gh", "https://api.github.com", "prod-db"),
			"route api reads your secret prod-db, and read gh-token"},
	} {
		now := subjectOf(t, approvalConfig(t, c.pol))
		if ApprovalDigest(now) == ApprovalDigest(base) {
			t.Errorf("%s: the digest did not change", c.name)
		}
		if got := strings.Join(ApprovalChanges(base, now), "\n"); !strings.Contains(got, c.want) {
			t.Errorf("%s: the changes do not name %q:\n%s", c.name, c.want, got)
		}
	}
}

// The subject holds names and hashes. A value forwarded from the host and a
// literal in the profile reach it as names alone.
func TestTheApprovalSubjectHoldsNoValue(t *testing.T) {
	digestHost(t)
	const planted = "PLANTED-HOST-VALUE-91c"
	t.Setenv("FORWARDED", planted)
	t.Setenv("BRIG_FORWARD_ENV", "FORWARDED")
	s := subjectOf(t, approvalConfig(t, routePolicy("gh", "https://api.github.com", "gh-token")))
	blob, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), planted) {
		t.Errorf("the subject holds a forwarded value:\n%s", blob)
	}
	if !strings.Contains(strings.Join(s.Forwarded, ","), "FORWARDED") {
		t.Errorf("the subject does not name the forwarded variable: %v", s.Forwarded)
	}
}
