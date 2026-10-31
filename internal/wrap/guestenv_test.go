package wrap

import (
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
)

// homes returns every HOME in env, in order.
func homes(env []runtime.Var) []string {
	var out []string
	for _, v := range env {
		if v.Name == "HOME" {
			out = append(out, v.Value)
		}
	}
	return out
}

// An exec through urunc leaves HOME unset, where runc would fill it in from
// /etc/passwd, so brig sets it on the boot itself.
func TestTheBootCarriesTheGuestHome(t *testing.T) {
	rt := &livenessRuntime{}
	c := livenessConfig(t, rt)
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if got := homes(rt.spec.Env); len(got) != 1 || got[0] != "/home/liveness" {
		t.Errorf("the boot carried HOME %q, want exactly [/home/liveness]", got)
	}
}

func TestAnExecCarriesTheGuestHome(t *testing.T) {
	c := livenessConfig(t, &livenessRuntime{})
	set := creds.Set{Vars: []runtime.Var{{Name: "GH_TOKEN", Value: "x", Secret: true}}}
	spec := c.execSpec(set, []string{"true"}, false)
	if got := homes(spec.Env); len(got) != 1 || got[0] != "/home/liveness" {
		t.Errorf("the exec carried HOME %q, want exactly [/home/liveness]", got)
	}
	if len(spec.Env) != 2 {
		t.Errorf("the exec carried %d variables, want HOME and GH_TOKEN: %v", len(spec.Env), spec.Env)
	}
	if len(set.Vars) != 1 {
		t.Errorf("adding HOME changed the caller's set: %v", set.Vars)
	}
}

func TestAHomeTheSetCarriesIsKept(t *testing.T) {
	c := livenessConfig(t, &livenessRuntime{})
	set := creds.Set{Vars: []runtime.Var{{Name: "HOME", Value: "/somewhere"}}}
	spec := c.execSpec(set, []string{"true"}, false)
	if got := homes(spec.Env); len(got) != 1 || got[0] != "/somewhere" {
		t.Errorf("the exec carried HOME %q, want exactly [/somewhere]", got)
	}
}

// values returns every value of the variable name in env, in order.
func values(env []runtime.Var, name string) []string {
	var out []string
	for _, v := range env {
		if v.Name == name {
			out = append(out, v.Value)
		}
	}
	return out
}

// An SSH session into the guest starts in HOME with nothing that says where
// the project is mounted. BRIG_PROJECT is what a shell hook reads to go there.
func TestAnExecCarriesTheProject(t *testing.T) {
	c := livenessConfig(t, &livenessRuntime{})
	c.GuestProject = "/work/api"
	spec := c.execSpec(creds.Set{}, []string{"true"}, false)
	if got := values(spec.Env, "BRIG_PROJECT"); len(got) != 1 || got[0] != "/work/api" {
		t.Errorf("the exec carried BRIG_PROJECT %q, want exactly [/work/api]", got)
	}
}

// Without a project the variable is absent, so a hook that reads it can tell
// "a project is mounted here" from "nothing is". A value the set carries does
// not stand in for one: it would name a directory brig never mounted.
func TestNoProjectCarriesNoProjectVariable(t *testing.T) {
	c := livenessConfig(t, &livenessRuntime{})
	set := creds.Set{Vars: []runtime.Var{{Name: "BRIG_PROJECT", Value: "/elsewhere"}}}
	spec := c.execSpec(set, []string{"true"}, false)
	if got := values(spec.Env, "BRIG_PROJECT"); len(got) != 0 {
		t.Errorf("a run with no project carried BRIG_PROJECT %q, want none", got)
	}
}

// brig owns the name: a value the set carries is replaced by the mount brig
// made, not sent beside it.
func TestTheProjectReplacesOneTheSetCarries(t *testing.T) {
	c := livenessConfig(t, &livenessRuntime{})
	c.GuestProject = "/work/api"
	set := creds.Set{Vars: []runtime.Var{{Name: "BRIG_PROJECT", Value: "/elsewhere"}}}
	spec := c.execSpec(set, []string{"true"}, false)
	if got := values(spec.Env, "BRIG_PROJECT"); len(got) != 1 || got[0] != "/work/api" {
		t.Errorf("the exec carried BRIG_PROJECT %q, want exactly [/work/api]", got)
	}
	if len(set.Vars) != 1 || set.Vars[0].Value != "/elsewhere" {
		t.Errorf("replacing BRIG_PROJECT changed the caller's set: %v", set.Vars)
	}
}
