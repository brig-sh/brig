package main

import (
	"errors"
	"strings"
	"testing"
)

func obs(o outcome) observation {
	return observation{outcome: o, line: string(o) + " tcp x:443 detail"}
}

var (
	denied  = testCase{id: "d", want: expectDeny, mode: resolveRefused}
	control = testCase{id: "c", want: expectReach}
	gap     = testCase{id: "g", want: expectGap}
	ipv6    = testCase{id: "v6", want: expectNoRoute, mode: connectRefused}
)

func mustBe(t *testing.T, got verdict, why string, want verdict) {
	t.Helper()
	if got != want {
		t.Fatalf("verdict %q (%s), want %q", got, why, want)
	}
	if got != pass && why == "" {
		t.Fatalf("verdict %q gives no reason", got)
	}
}

func TestDeniedCaseReachedFails(t *testing.T) {
	v, why := judge(denied, obs(reached), obs(reached), true)
	mustBe(t, v, why, fail)
}

// A denied name that times out was stopped by something, but not by the
// resolver refusing it, which is what the policy does.
func TestDeniedCaseOnAnotherFailureModeFails(t *testing.T) {
	for _, o := range []outcome{connectRefused, timedOut, noRoute, resolveNXDomain, resolveTimeout, resolveFailed} {
		v, why := judge(denied, obs(o), obs(reached), true)
		mustBe(t, v, why, fail)
		if !strings.Contains(why, string(resolveRefused)) {
			t.Errorf("%s: reason %q does not name the mode the policy causes", o, why)
		}
	}
}

func TestDeniedCasePassesOnlyOnThePolicyMode(t *testing.T) {
	v, why := judge(denied, obs(resolveRefused), obs(reached), true)
	mustBe(t, v, why, pass)
}

var metadata = testCase{id: "m", want: expectDeny, mode: connectRefused, unprovenWhy: "shut with or without a policy"}

// A target the guest cannot get to without a policy is refused or dropped
// whatever the policy says. The policy's mode on it is no evidence.
func TestDeniedCaseShutWithoutAPolicyIsUnproven(t *testing.T) {
	for _, without := range []outcome{connectRefused, timedOut, noRoute, resolveFailed} {
		v, why := judge(metadata, obs(connectRefused), obs(without), true)
		mustBe(t, v, why, unproven)
		if !strings.Contains(why, string(without)+" without one") {
			t.Errorf("%s: reason %q does not name the run with no policy", without, why)
		}
	}
}

func TestDeniedCaseWithAnUnmeasuredRunWithoutAPolicyFails(t *testing.T) {
	v, why := judge(metadata, obs(connectRefused), observation{invalid: "no probe line"}, true)
	mustBe(t, v, why, fail)
	v, why = judge(metadata, obs(connectRefused), obs(clientMissing), true)
	mustBe(t, v, why, fail)
}

// Without a policy, curl through the host listener fails after the connect
// and says error-after-connect. The guest got past the step the policy stops.
func TestDeniedCaseCreditedWhenTheRunWithoutAPolicyGotPastConnect(t *testing.T) {
	v, why := judge(metadata, obs(connectRefused), obs(probeErrorAfterConnect), true)
	mustBe(t, v, why, pass)
}

// An error before any connection says nothing about how far the guest got
// with no policy, so the case has no baseline.
func TestErrorBeforeConnectWithoutAPolicyIsNoBaseline(t *testing.T) {
	c := testCase{id: "ipv4-literal", want: expectDeny, mode: connectRefused}
	v, why := judge(c, obs(connectRefused), obs(probeError), true)
	mustBe(t, v, why, fail)
	if !strings.Contains(why, "without a policy") {
		t.Errorf("reason %q does not name the run with no policy", why)
	}
}

// A denied case that connected under the policy and then failed got past
// the step the policy is meant to stop.
func TestDeniedCaseThatConnectedUnderThePolicyFails(t *testing.T) {
	v, why := judge(metadata, obs(probeErrorAfterConnect), obs(reached), true)
	mustBe(t, v, why, fail)
	if !strings.Contains(why, "connected") {
		t.Errorf("reason %q does not say it connected", why)
	}
}

// hull's gateway answers REFUSED for a name the policy denies, and NXDOMAIN
// when its own upstream lookup failed. The name cases pass on the first alone.
func TestNameCasesPassOnlyOnREFUSED(t *testing.T) {
	for _, id := range []string{"denied-name", "deny-over-allow-host"} {
		c := caseByID(t, id)
		v, why := judge(c, obs(resolveRefused), obs(reached), true)
		mustBe(t, v, why, pass)
		for _, o := range []outcome{resolveNXDomain, resolveTimeout, resolveFailed} {
			v, why := judge(c, obs(o), obs(reached), true)
			mustBe(t, v, why, fail)
		}
	}
}

// A name case is stopped at the resolver. A run with no policy that resolved
// the name got past that step, however the connection then went.
func TestNameCaseBaselinePastTheResolver(t *testing.T) {
	c := caseByID(t, "denied-name")
	for _, without := range []outcome{connectRefused, timedOut, noRoute, probeErrorAfterConnect} {
		v, why := judge(c, obs(resolveRefused), obs(without), true)
		mustBe(t, v, why, pass)
	}
	for _, without := range []outcome{resolveNXDomain, resolveTimeout} {
		v, why := judge(c, obs(resolveRefused), obs(without), true)
		mustBe(t, v, why, fail)
	}
}

// Only a case that says why its target may be shut can end unproven. Any
// other one fails, so a run cannot pass on denied cases that proved nothing.
func TestUnprovenOnlyForACaseThatSaysWhy(t *testing.T) {
	c := testCase{id: "ipv4-literal", want: expectDeny, mode: connectRefused}
	v, why := judge(c, obs(connectRefused), obs(timedOut), true)
	mustBe(t, v, why, fail)
	if !strings.Contains(why, "unproven") {
		t.Errorf("reason %q does not say the case is unproven", why)
	}
	v, why = judge(metadata, obs(connectRefused), obs(timedOut), true)
	mustBe(t, v, why, unproven)
	if !strings.Contains(why, metadata.unprovenWhy) {
		t.Errorf("reason %q does not say why the target may be shut", why)
	}
	var may []string
	for _, c := range cases {
		if c.unprovenWhy != "" {
			may = append(may, c.id)
		}
	}
	if len(may) != 1 || may[0] != "metadata" {
		t.Errorf("cases that may end unproven: %q, want metadata alone", may)
	}
}

// client-missing and error mean the probe measured nothing, and nothing
// measured is never a pass, whatever the case expected.
func TestNothingMeasuredNeverPasses(t *testing.T) {
	for _, c := range []testCase{denied, control, gap, ipv6} {
		for _, o := range []outcome{clientMissing, probeError} {
			v, why := judge(c, obs(o), obs(reached), true)
			mustBe(t, v, why, fail)
		}
		v, why := judge(c, observation{invalid: "no probe line"}, obs(reached), true)
		mustBe(t, v, why, fail)
	}
}

func TestFailedControlFails(t *testing.T) {
	for _, o := range []outcome{resolveFailed, connectRefused, timedOut, noRoute} {
		v, why := judge(control, obs(o), obs(reached), true)
		mustBe(t, v, why, fail)
	}
	v, why := judge(control, obs(reached), obs(reached), true)
	mustBe(t, v, why, pass)
}

// A guest that reaches nothing refuses every denied case in the mode it
// happens to fail in. Without a working control in the same boot, a match
// is luck.
func TestFailedControlFailsEveryDeniedCaseInTheBoot(t *testing.T) {
	v, why := judge(denied, obs(resolveRefused), obs(reached), false)
	mustBe(t, v, why, fail)
	v, why = judge(ipv6, obs(noRoute), obs(noRoute), false)
	mustBe(t, v, why, fail)
	v, why = judge(gap, obs(reached), obs(reached), false)
	mustBe(t, v, why, fail)
}

func TestIPv6ReachedUnderThePolicyFails(t *testing.T) {
	v, why := judge(ipv6, obs(reached), obs(reached), true)
	mustBe(t, v, why, fail)
}

// hull drops IPv6 with no policy too. The case records that, next to the
// run with no policy, and gives the policy no credit.
func TestIPv6BlockedWithoutAPolicyIsNoRoute(t *testing.T) {
	v, why := judge(ipv6, obs(noRoute), obs(noRoute), true)
	mustBe(t, v, why, noRouteVerdict)
	v, why = judge(ipv6, obs(timedOut), obs(timedOut), true)
	mustBe(t, v, why, noRouteVerdict)
}

// Once a backend routes IPv6, the policy is what stops the literal, and the
// case is held to the policy's mode like any other.
func TestIPv6ReachedWithoutAPolicyNeedsThePolicyMode(t *testing.T) {
	v, why := judge(ipv6, obs(noRoute), obs(reached), true)
	mustBe(t, v, why, fail)
	v, why = judge(ipv6, obs(connectRefused), obs(reached), true)
	mustBe(t, v, why, pass)
}

// An IPv6 run with no policy that measured nothing leaves the policy run
// with nothing to stand next to.
func TestIPv6WithAnUnmeasuredRunWithoutAPolicyFails(t *testing.T) {
	v, why := judge(ipv6, obs(noRoute), obs(probeError), true)
	mustBe(t, v, why, fail)
}

func TestGapThatOpensIsAKnownGap(t *testing.T) {
	v, why := judge(gap, obs(reached), obs(reached), true)
	mustBe(t, v, why, knownGap)
}

// A gap the policy closed is a change docs/policies.md has to catch up
// with, and the run fails to say so.
func TestGapThatStaysShutFails(t *testing.T) {
	v, why := judge(gap, obs(connectRefused), obs(reached), true)
	mustBe(t, v, why, fail)
}

func TestGapUnreachedWithoutAPolicyFails(t *testing.T) {
	v, why := judge(gap, obs(reached), obs(timedOut), true)
	mustBe(t, v, why, fail)
}

func TestProbeLineParsed(t *testing.T) {
	o := parseProbe("resolve-failed tcp example.net:443 lookup example.net: server misbehaving\n", 1, nil)
	if o.invalid != "" || o.outcome != resolveFailed {
		t.Fatalf("got %+v", o)
	}
	if !strings.HasPrefix(o.line, "resolve-failed tcp example.net:443") {
		t.Fatalf("line %q", o.line)
	}
	o = parseProbe("reached tcp example.com:443 connected to 1.2.3.4:443\n", 0, nil)
	if o.invalid != "" || o.outcome != reached {
		t.Fatalf("got %+v", o)
	}
}

// brig's own exit codes overlap the probe's: 1 is a general failure, 3 an
// unknown sandbox. The line on stdout is what was measured, and an exit
// status that disagrees with it means the line is not the probe's.
func TestProbeLineRequired(t *testing.T) {
	for name, tc := range map[string]struct {
		stdout string
		exit   int
		err    error
	}{
		"no output, brig exit 3":        {"", 3, nil},
		"no output, brig exit 1":        {"", 1, nil},
		"brig refusal on stdout":        {"brig: no such sandbox\n", 3, nil},
		"unknown outcome word":          {"blocked tcp x:443 detail\n", 1, nil},
		"reached with a failing exit":   {"reached tcp x:443 detail\n", 1, nil},
		"refused with exit 0":           {"connect-refused tcp x:443 detail\n", 0, nil},
		"refused with exit 3":           {"connect-refused tcp x:443 detail\n", 3, nil},
		"error with exit 1":             {"error dot x detail\n", 1, nil},
		"two lines":                     {"reached tcp x:443 a\nreached tcp y:443 b\n", 0, nil},
		"brig did not start":            {"", -1, errors.New("exec: brig: not found")},
		"line but brig did not finish":  {"reached tcp x:443 a\n", 0, errors.New("signal: killed")},
		"outcome with nothing after it": {"reached\n", 0, nil},
	} {
		o := parseProbe(tc.stdout, tc.exit, tc.err)
		if o.invalid == "" {
			t.Errorf("%s: parsed as %+v, want nothing measured", name, o)
		}
	}
}

func TestCasesCoverIssue264(t *testing.T) {
	ids := map[string]testCase{}
	for _, c := range cases {
		if _, dup := ids[c.id]; dup {
			t.Fatalf("case %s twice", c.id)
		}
		ids[c.id] = c
		if c.policy != denyPolicy && c.policy != allowPolicy {
			t.Errorf("%s: runs under %q, which is neither policy", c.id, c.policy)
		}
		if (c.want == expectDeny || c.want == expectNoRoute) && c.mode == "" {
			t.Errorf("%s: a denied case with no failure mode can never pass", c.id)
		}
	}
	for _, id := range []string{
		"allowed-name", "denied-name", "ipv4-literal", "ipv6-literal", "alternate-resolver",
		"dns-over-https", "dns-over-tls", "deny-over-allow-host", "deny-over-allow-cidr",
		"proxy-env", "host-service", "metadata", "allow/allowed-name",
	} {
		if _, ok := ids[id]; !ok {
			t.Errorf("no case %s", id)
		}
	}
	for _, p := range []string{denyPolicy, allowPolicy} {
		n := 0
		for _, c := range cases {
			if c.policy == p && c.want == expectReach {
				n++
			}
		}
		if n == 0 {
			t.Errorf("the %s boot has no allowed control", p)
		}
	}
	gaps := 0
	for _, c := range cases {
		if c.want == expectGap {
			gaps++
			if c.policy != allowPolicy {
				t.Errorf("%s: a known gap outside the default: allow boot", c.id)
			}
		}
	}
	if gaps == 0 {
		t.Error("no default: allow gap is recorded")
	}
}
