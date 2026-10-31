package wrap

import (
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
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

// The boot is a separate call site from an exec, so it is checked on its own:
// a pane the image opens off the boot's environment needs the variable too.
func TestTheBootCarriesTheProject(t *testing.T) {
	rt := &livenessRuntime{}
	c := livenessConfig(t, rt)
	c.GuestProject = "/work/api"
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if got := values(rt.spec.Env, "BRIG_PROJECT"); len(got) != 1 || got[0] != "/work/api" {
		t.Errorf("the boot carried BRIG_PROJECT %q, want exactly [/work/api]", got)
	}
}

// A route's variables reach the guest by a path of their own, after the set,
// so a route naming BRIG_PROJECT must not replace the mount brig made.
func TestARouteDoesNotReplaceTheProject(t *testing.T) {
	c := livenessConfig(t, &livenessRuntime{})
	c.GuestProject = "/work/api"
	c.routeVars = []runtime.Var{{Name: "BRIG_PROJECT", Value: "/elsewhere"}}
	spec := c.execSpec(creds.Set{}, []string{"true"}, false)
	if got := values(spec.Env, "BRIG_PROJECT"); len(got) != 1 || got[0] != "/work/api" {
		t.Errorf("the exec carried BRIG_PROJECT %q, want exactly [/work/api]", got)
	}
}

// A binding or route that names BRIG_PROJECT is dropped where the run is
// built, with a warning, so no report names a variable the guest does not get.
// The route came from a bound policy, so it is copied, not edited.
func TestTheProjectVariableIsDroppedFromBindingsAndRoutes(t *testing.T) {
	bindings := []profile.EnvBinding{
		{Name: "GH_TOKEN", Ref: "env.GH_TOKEN"},
		{Name: "BRIG_PROJECT", Value: "/elsewhere"},
	}
	route := v1alpha1.Route{Name: "inventory", Guest: &v1alpha1.Guest{Env: map[string]string{
		"INVENTORY_TOKEN": v1alpha1.PlaceholderSessionToken,
		"BRIG_PROJECT":    v1alpha1.PlaceholderSessionToken,
	}}}

	kept, routes, warnings := withoutProjectVar(bindings, []v1alpha1.Route{route})

	if names := bindingNames(kept); len(names) != 1 || names[0] != "GH_TOKEN" {
		t.Errorf("kept bindings %v, want [GH_TOKEN]", names)
	}
	if _, ok := routes[0].Guest.Env["BRIG_PROJECT"]; ok {
		t.Errorf("the route still sets BRIG_PROJECT: %v", routes[0].Guest.Env)
	}
	if _, ok := route.Guest.Env["BRIG_PROJECT"]; !ok {
		t.Errorf("dropping BRIG_PROJECT edited the policy's own route: %v", route.Guest.Env)
	}
	c := &Config{routes: routes}
	for _, rc := range c.routeCredentials() {
		if rc.Name == "BRIG_PROJECT" {
			t.Errorf("the report names BRIG_PROJECT as a route credential: %v", c.routeCredentials())
		}
	}
	if len(warnings) != 2 {
		t.Fatalf("got %d warnings, want one for the binding and one for the route: %q", len(warnings), warnings)
	}
	for _, w := range warnings {
		if !strings.Contains(w, "BRIG_PROJECT") {
			t.Errorf("warning %q does not name BRIG_PROJECT", w)
		}
	}
	if !strings.Contains(warnings[1], "inventory") {
		t.Errorf("warning %q does not name the route", warnings[1])
	}
}
