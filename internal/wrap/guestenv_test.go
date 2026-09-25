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
