package main

import (
	"fmt"
	"strings"
)

// outcome is the first word of a netprobe line. The runner keeps its own copy
// of the set, since netprobe is a separate command, and a word outside it is
// never taken for a measurement.
type outcome string

const (
	reached                outcome = "reached"
	resolveRefused         outcome = "resolve-refused"
	resolveNXDomain        outcome = "resolve-nxdomain"
	resolveTimeout         outcome = "resolve-timeout"
	resolveFailed          outcome = "resolve-failed"
	connectRefused         outcome = "connect-refused"
	timedOut               outcome = "timeout"
	noRoute                outcome = "no-route"
	clientMissing          outcome = "client-missing"
	probeError             outcome = "error"
	probeErrorAfterConnect outcome = "error-after-connect"
)

// resolveOutcomes are the ways a lookup fails. A case stopped there never
// got as far as a connection.
var resolveOutcomes = map[outcome]bool{
	resolveRefused: true, resolveNXDomain: true, resolveTimeout: true, resolveFailed: true,
}

// observation is one probe run in one boot.
type observation struct {
	outcome outcome
	line    string
	// invalid says why the run measured nothing. Empty when outcome is a
	// measurement.
	invalid string
}

type verdict string

const (
	pass     verdict = "pass"
	fail     verdict = "FAIL"
	knownGap verdict = "known gap"
	// noRouteVerdict is the IPv6 case when the run with no policy did not
	// reach the target either, so the policy is not what stopped it.
	noRouteVerdict verdict = "no route"
	// unproven is a denied case whose target the run with no policy did not
	// get to either. The policy run failed in the right mode, but a target
	// that is shut without a policy gives the policy no credit. Only a case
	// that says why its target may be shut can end unproven, and it does not
	// fail the run. It is no evidence for the policy.
	unproven verdict = "unproven"
)

// probeExit is the exit status netprobe gives each outcome, a copy of the
// table in test/netprobe. brig passes the guest's status through, and a line
// whose status disagrees came from something other than the probe.
var probeExit = map[outcome]int{
	reached:                0,
	resolveRefused:         1,
	resolveNXDomain:        1,
	resolveTimeout:         1,
	resolveFailed:          1,
	connectRefused:         1,
	timedOut:               1,
	noRoute:                1,
	clientMissing:          3,
	probeError:             3,
	probeErrorAfterConnect: 3,
}

// parseProbe reads what one `brig sh ... netprobe` printed. brig's own exit
// codes overlap the probe's, so the line on stdout decides what was measured
// and the exit status only has to agree with it.
func parseProbe(stdout string, exit int, runErr error) observation {
	line := strings.TrimSpace(stdout)
	bad := func(format string, a ...any) observation {
		return observation{line: line, invalid: fmt.Sprintf(format, a...)}
	}
	if runErr != nil {
		return bad("brig did not finish: %v", runErr)
	}
	if line == "" {
		return bad("no probe line, brig exit %d", exit)
	}
	if strings.Contains(line, "\n") {
		return bad("more than one line, brig exit %d", exit)
	}
	word, rest, _ := strings.Cut(line, " ")
	o := outcome(word)
	want, known := probeExit[o]
	if !known {
		return bad("%q is not a probe outcome, brig exit %d", word, exit)
	}
	if strings.TrimSpace(rest) == "" {
		return bad("probe line %q names no target", line)
	}
	if exit != want {
		return bad("probe said %s, which exits %d, and brig exited %d", o, want, exit)
	}
	return observation{outcome: o, line: line}
}

// measured says why o is not a measurement, or "" when it is.
func measured(o observation) string {
	switch {
	case o.invalid != "":
		return o.invalid
	case o.outcome == clientMissing:
		return "the guest has no client for this case"
	case o.outcome == probeError:
		return "the probe hit an error"
	case o.outcome == probeErrorAfterConnect:
		return "the probe connected, then hit an error"
	}
	return ""
}

// pastStep reports whether a run with no policy that ended in o got past the
// step where the policy stops a case whose mode is mode. A name the policy
// stops at the resolver is past that step once it resolved, however the
// connection then went. Any other case is past it only once it connected.
// An error before any connection says neither.
func pastStep(mode, o outcome) bool {
	if resolveOutcomes[mode] {
		return !resolveOutcomes[o] && o != probeError && o != clientMissing
	}
	return o == reached || o == probeErrorAfterConnect
}

// judge applies the verdict rules to one case. withPolicy is the case in its
// own boot, noPolicy the same command in the boot with no policy, and
// controlsOK whether every allowed control in the policy boot reached its
// target, first and last.
func judge(c testCase, withPolicy, noPolicy observation, controlsOK bool) (verdict, string) {
	if c.want != expectReach && c.want != expectGap && withPolicy.invalid == "" &&
		withPolicy.outcome == probeErrorAfterConnect {
		return fail, "connected to a target the policy denies, then hit an error"
	}
	if why := measured(withPolicy); why != "" {
		return fail, "nothing measured: " + why
	}
	got := withPolicy.outcome
	if c.want == expectReach {
		if got != reached {
			return fail, fmt.Sprintf("an allowed control got %s", got)
		}
		return pass, ""
	}
	if !controlsOK {
		return fail, "an allowed control in this boot failed, so a failure here proves nothing"
	}
	switch c.want {
	case expectDeny:
		if got == reached {
			return fail, "reached a target the policy denies"
		}
		if got != c.mode {
			return fail, fmt.Sprintf("the policy causes %s, got %s", c.mode, got)
		}
		// error-after-connect is not a measurement on its own, but without a
		// policy it means the probe got past the step the policy stops: curl
		// through the host listener says "Proxy CONNECT aborted", and a TLS
		// failure comes after the connect. An error before any connection
		// says nothing either way.
		switch {
		case noPolicy.invalid != "":
			return fail, "nothing measured without a policy: " + noPolicy.invalid
		case noPolicy.outcome == clientMissing, noPolicy.outcome == probeError:
			return fail, "nothing measured without a policy: " + measured(noPolicy)
		case !pastStep(c.mode, noPolicy.outcome):
			why := fmt.Sprintf("%s with the policy and %s without one", got, noPolicy.outcome)
			if c.unprovenWhy == "" {
				return fail, "unproven, and only a case that says why its target may be shut can be: " + why
			}
			return unproven, why + ". " + c.unprovenWhy
		}
		return pass, ""
	case expectGap:
		if why := measured(noPolicy); why != "" {
			return fail, "nothing measured without a policy: " + why
		}
		if noPolicy.outcome != reached {
			return fail, fmt.Sprintf("without a policy it got %s, so the gap was never tested", noPolicy.outcome)
		}
		if got != reached {
			return fail, fmt.Sprintf("the gap did not open (%s): update docs/policies.md and this case", got)
		}
		return knownGap, "default: allow leaves this open"
	case expectNoRoute:
		if got == reached {
			return fail, "reached a target the policy denies"
		}
		if why := measured(noPolicy); why != "" {
			return fail, "nothing measured without a policy: " + why
		}
		if noPolicy.outcome != reached {
			return noRouteVerdict, fmt.Sprintf("%s with the policy and %s without one", got, noPolicy.outcome)
		}
		if got != c.mode {
			return fail, fmt.Sprintf("reached without a policy, so the policy causes %s, got %s", c.mode, got)
		}
		return pass, ""
	}
	return fail, fmt.Sprintf("case %s expects nothing this suite knows", c.id)
}
