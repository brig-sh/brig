package main

import (
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/runtime"
)

// nestedDoctorRuntime is doctorRuntime with an answer to the nested question.
type nestedDoctorRuntime struct {
	doctorRuntime
	s runtime.NestedSupport
}

func (r nestedDoctorRuntime) NestedVirt() runtime.NestedSupport { return r.s }

// withNestedAnswer makes the detected runtime answer the nested question with s,
// keeping the healthy host's stand-in binary so the runtime row still passes.
func withNestedAnswer(t *testing.T, s runtime.NestedSupport) {
	t.Helper()
	rt, err := detectRuntime()
	if err != nil {
		t.Fatal(err)
	}
	base := rt.(doctorRuntime)
	swap(t, &detectRuntime, func() (runtime.Runtime, error) {
		return nestedDoctorRuntime{doctorRuntime: base, s: s}, nil
	})
}

// A host that can nest says so, naming the backend that gives it.
func TestDoctorNestedSupported(t *testing.T) {
	healthyHost(t)
	withNestedAnswer(t, runtime.NestedSupport{Supported: true, Backend: "hvi",
		Detail: "Hypervisor.framework reports EL2"})
	c := findCheck(t, runDoctor(nil, nil), "nested")
	if c.State != statePass {
		t.Errorf("a host that supports nested virtualization is %q, want ok", c.State)
	}
	want := "supported (hull hvi backend; Hypervisor.framework reports EL2)"
	if c.Finding != want {
		t.Errorf("finding = %q, want %q", c.Finding, want)
	}
}

// Most hosts never run a kvm profile, so neither "no" nor a hull that cannot
// answer is a failure: never !!, never a fix line, never a non-zero exit. The same rule
// the virtual row follows.
func TestDoctorNestedRowNeverGatesExit(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    runtime.NestedSupport
		want string
	}{
		{"unsupported", runtime.NestedSupport{Backend: "hvi", Detail: "Hypervisor.framework reports no EL2"},
			"not supported on this host: Hypervisor.framework reports no EL2"},
		{"outdated", runtime.NestedSupport{Outdated: true,
			Detail: "this hull (0.1.0-rc29) predates nested virtualization; upgrade hull"},
			"this hull (0.1.0-rc29) predates nested virtualization; upgrade hull"},
		{"no answer", runtime.NestedSupport{Detail: "hull capabilities --json: exit status 1"},
			"not supported on this host: hull capabilities --json: exit status 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			healthyHost(t)
			withNestedAnswer(t, tc.s)
			checks := runDoctor(nil, nil)
			c := findCheck(t, checks, "nested")
			if c.State != stateInfo || c.Fix != "" || c.err != nil {
				t.Errorf("%s: the nested row is %q with fix %q and err %v, want -- and nothing to act on",
					tc.name, c.State, c.Fix, c.err)
			}
			if c.Finding != tc.want {
				t.Errorf("finding = %q, want %q", c.Finding, tc.want)
			}
			if err := doctorExit(checks); err != nil {
				t.Errorf("the nested row gated the exit: %v", err)
			}
		})
	}
}

// A runtime with no answer to give is reported as having none. A "no" would
// answer a question nobody put to it. doctorRuntime is that runtime: it
// answers nothing beyond Kind and Bin.
func TestDoctorNestedOnARuntimeWithoutAnAnswer(t *testing.T) {
	healthyHost(t)
	c := findCheck(t, runDoctor(nil, nil), "nested")
	if c.State != stateInfo || !strings.Contains(c.Finding, "does not pass /dev/kvm through") {
		t.Errorf("nested row is %q/%q", c.State, c.Finding)
	}
}

// Without a runtime there is nothing to ask, and the row says it was not
// reached. Failing it would only repeat the runtime row in other words.
func TestDoctorNestedNotReachedWithoutARuntime(t *testing.T) {
	healthyHost(t)
	swap(t, &detectRuntime, func() (runtime.Runtime, error) { return nil, runtime.ErrNoRuntime })
	c := findCheck(t, runDoctor(nil, nil), "nested")
	if c.State != stateInfo || c.Finding != "not reached" {
		t.Errorf("nested after a missing runtime is %q/%q, want -- / not reached", c.State, c.Finding)
	}
}

// Boot order: the nested question is about the runtime, so it sits after the
// runtime and its boot assets and before the signature tooling.
func TestDoctorNestedSitsAfterBoot(t *testing.T) {
	healthyHost(t)
	checks := runDoctor(nil, nil)
	at := map[string]int{}
	for i, c := range checks {
		at[c.Name] = i
	}
	if !(at["boot"] < at["nested"] && at["nested"] < at["verify"]) {
		t.Errorf("nested is not between boot and verify: %v", at)
	}
}
