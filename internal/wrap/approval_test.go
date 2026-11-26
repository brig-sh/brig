package wrap

import (
	"encoding/json"
	"slices"
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

// The git URLs a route takes over in the guest decide which traffic carries
// the secret, so a change to them alone is a new digest and a named change.
func TestTheApprovalSubjectNamesAChangeToWhatARouteTakesOver(t *testing.T) {
	digestHost(t)
	forge := func(prefix string) policy.Policy {
		return policy.Policy{Metadata: v1alpha1.ObjectMeta{Name: "forge"}, Spec: v1alpha1.PolicySpec{
			Routes: []v1alpha1.Route{{Name: "forge", Upstream: "https://git.example.com",
				Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: "forge-token"}},
				Inject:     &v1alpha1.Inject{BasicAuth: &v1alpha1.BasicAuth{Username: "x-access-token"}},
				Guest:      &v1alpha1.Guest{Git: &v1alpha1.GitGuest{InsteadOf: []string{prefix}}}}}}}
	}
	base := subjectOf(t, approvalConfig(t, forge("https://git.example.com/")))
	now := subjectOf(t, approvalConfig(t, forge("https://github.com/")))
	if ApprovalDigest(now) == ApprovalDigest(base) {
		t.Error("the digest did not change")
	}
	const want = "route forge changed the git URLs it takes over or the variables it sets in the guest"
	if got := strings.Join(ApprovalChanges(base, now), "\n"); !strings.Contains(got, want) {
		t.Errorf("the changes do not name %q:\n%s", want, got)
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

// fixedSubject is a subject built by hand, with no MCP server, so its digest
// depends on nothing but the encoding of ApprovalSubject.
func fixedSubject() ApprovalSubject {
	return ApprovalSubject{
		Version: approvalVersion, Root: "/src/a", Target: "dev", Sandbox: "brig-dev",
		Brigfile:  ApprovalDocument{Kind: "brigfile", Source: "/src/a/Brigfile", SHA256: "aa"},
		Documents: []ApprovalDocument{},
		Profile:   ApprovalProfile{Alias: "claude", Source: "file:.brig/claude.yaml", SHA256: "bb"},
		Policies:  []ApprovalPolicy{{Name: "gh", From: "brigfile gh (inline)", SHA256: "cc"}},
		Egress:    ApprovalEgress{Allow: []policy.Rule{}, Deny: []policy.Rule{}},
		Routes: []PlanRoute{{Name: "api", Policy: "brigfile gh (inline)", Preset: "github-token",
			Mode: "endpoint", Upstream: "https://api.github.com", Secrets: []string{"gh-token"}}},
		KeptOnHost:  []string{"gh-token"},
		Image:       ApprovalImage{Ref: "example.com/repo:1", Pull: "missing"},
		Limits:      PlanLimits{Mem: 1024, CPUs: 1},
		Network:     ApprovalNet{Effective: "isolated"},
		Mounts:      []PlanMount{{Host: "/home/u/ws", Guest: "/root", Mode: "read-write"}},
		Settings:    []ApprovalSetting{},
		Credentials: []ApprovalCredential{},
		Forwarded:   []string{},
		Ports:       []string{},
		Runtime:     ApprovalRuntime{Kind: "hull"},
		Binary:      "x",
		Args:        []string{"--x"},
	}
}

// An approval given before MCP servers were covered stays valid for a run
// that has none: the subject of such a run encodes to the bytes it did, so
// its digest is the one pinned here from the release before.
func TestApprovalDigestUnchangedWithoutMCP(t *testing.T) {
	const want = "sha256:c2b1fa7e8b466148caf612dc0b72864cd6e7bce5f244edd075fb94ae1ed06df1"
	if got := ApprovalDigest(fixedSubject()); got != want {
		t.Errorf("the digest of a subject with no MCP server is %s, want %s", got, want)
	}
	digestHost(t)
	blob, err := json.Marshal(subjectOf(t, approvalConfig(t, routePolicy("gh", "https://api.github.com", "gh-token"))))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), `"mcp"`) {
		t.Errorf("the subject of a run with no MCP server has an mcp field:\n%s", blob)
	}
}

// mcpPolicy is a policy named nofire with one http MCP server, nofire, at
// url, reading the secret nofire-mcp and allowing tools. A repository policy
// cannot set url (see internal/brigfile), but a policy of yours attached to
// the target's built-in profile can, and the approval covers it either way.
func mcpPolicy(url string, tools ...string) policy.Policy {
	return policy.Policy{Metadata: v1alpha1.ObjectMeta{Name: "nofire"}, Spec: v1alpha1.PolicySpec{
		MCP: []v1alpha1.MCPServer{{Name: "nofire", Transport: v1alpha1.MCPTransportHTTP, URL: url,
			Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: "nofire-mcp"}},
			Inject: &v1alpha1.Inject{Headers: &v1alpha1.HeaderInject{
				Set: []v1alpha1.Header{{Name: "Authorization", Value: "Bearer {value}"}}}},
			Tools: &v1alpha1.MCPTools{Allow: tools}}}}}
}

// approvalChangeNamed fails unless going from was to now changes the digest
// and the changes hold want.
func approvalChangeNamed(t *testing.T, was, now ApprovalSubject, want string) {
	t.Helper()
	if ApprovalDigest(was) == ApprovalDigest(now) {
		t.Fatal("the digest did not change")
	}
	if got := strings.Join(ApprovalChanges(was, now), "\n"); !strings.Contains(got, want) {
		t.Errorf("the changes do not name %q:\n%s", want, got)
	}
}

// An MCP server sends your secret to its URL. Pointing it elsewhere asks
// again, and the question names the server and both URLs. A run with MCP
// servers and no route keeps their secret on the host, and the subject says
// so.
func TestApprovalAsksAgainWhenMCPURLChanges(t *testing.T) {
	digestHost(t)
	was := subjectOf(t, approvalConfig(t, mcpPolicy("https://mcp.nofire.ai/mcp", "search")))
	if again := subjectOf(t, approvalConfig(t, mcpPolicy("https://mcp.nofire.ai/mcp", "search"))); ApprovalDigest(again) != ApprovalDigest(was) {
		t.Fatal("the same configuration has two digests")
	}
	if len(was.MCP) != 1 || was.MCP[0].URL != "https://mcp.nofire.ai/mcp" ||
		!slices.Equal(was.MCP[0].Secrets, []string{"nofire-mcp"}) {
		t.Fatalf("the subject's MCP servers are %+v", was.MCP)
	}
	if !slices.Equal(was.KeptOnHost, []string{"nofire-mcp"}) {
		t.Errorf("the subject keeps %v on the host, want nofire-mcp", was.KeptOnHost)
	}
	now := subjectOf(t, approvalConfig(t, mcpPolicy("https://collector.example/mcp", "search")))
	approvalChangeNamed(t, was, now,
		"mcp nofire sends to https://collector.example/mcp, and sent to https://mcp.nofire.ai/mcp")
}

// A tool the approval did not allow is a new permission.
func TestApprovalAsksAgainWhenMCPToolsChange(t *testing.T) {
	digestHost(t)
	was := subjectOf(t, approvalConfig(t, mcpPolicy("https://mcp.nofire.ai/mcp", "search")))
	now := subjectOf(t, approvalConfig(t, mcpPolicy("https://mcp.nofire.ai/mcp", "*")))
	approvalChangeNamed(t, was, now, "mcp nofire allows all tools (*), and allowed tools search")
}

// A server the approval did not see asks again, named.
func TestApprovalAsksAgainWhenMCPAdded(t *testing.T) {
	digestHost(t)
	was := subjectOf(t, approvalConfig(t, routePolicy("gh", "https://api.github.com", "gh-token")))
	now := subjectOf(t, approvalConfig(t, routePolicy("gh", "https://api.github.com", "gh-token"),
		mcpPolicy("https://mcp.nofire.ai/mcp", "search")))
	approvalChangeNamed(t, was, now, "mcp nofire is new")
}
