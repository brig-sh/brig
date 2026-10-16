// Package routemode holds the route modes a build of brig serves besides
// endpoint, which every build serves.
//
// A mode is registered from an init function of the code that serves it. A
// build with no such code serves endpoint alone. A route in a mode the build
// does not serve still parses, so a policy reads the same in every build.
// brig policy check and every run refuse it, the run with exit 7.
//
// Each layer of brig that serves a mode has its own hook beside this
// registry: the route mode table in internal/runtime, the run in
// internal/wrap, and the divert handler in cmd/brig-broker.
package routemode

import (
	"fmt"
	"slices"
	"sync"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// Mode is one route mode a build registers.
type Mode struct {
	// Name is the value of a route's mode field.
	Name v1alpha1.RouteMode
	// Presets are the presets that support the mode. A route in the mode
	// names one of them.
	Presets []v1alpha1.Preset
	// Check refuses a route in the mode, or is nil. base is the route of
	// the preset, written is the route as its policy writes it, and
	// expanded is written over base. Check runs before the defaults and
	// the validation every route gets, which follow when it passes.
	Check func(base, written, expanded v1alpha1.Route) error
}

var (
	mu    sync.RWMutex
	modes = map[v1alpha1.RouteMode]Mode{}
)

// Register adds m to the modes this build serves, and returns a function that
// removes it again, for a test. It panics on endpoint, on a name that is not
// a policy name, and on a mode registered twice.
func Register(m Mode) (unregister func()) {
	if m.Name == "" || m.Name == v1alpha1.ModeEndpoint {
		panic(fmt.Sprintf("routemode: %q is not a mode to register", m.Name))
	}
	if err := v1alpha1.CheckName(string(m.Name)); err != nil {
		panic(fmt.Sprintf("routemode: %v", err))
	}
	mu.Lock()
	defer mu.Unlock()
	if _, ok := modes[m.Name]; ok {
		panic(fmt.Sprintf("routemode: %s is registered twice", m.Name))
	}
	modes[m.Name] = m
	return func() {
		mu.Lock()
		defer mu.Unlock()
		delete(modes, m.Name)
	}
}

// Lookup returns the registered mode name.
func Lookup(name v1alpha1.RouteMode) (Mode, bool) {
	mu.RLock()
	defer mu.RUnlock()
	m, ok := modes[name]
	return m, ok
}

// Names returns the registered modes, sorted.
func Names() []v1alpha1.RouteMode {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]v1alpha1.RouteMode, 0, len(modes))
	for name := range modes {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// Served returns whether this build serves mode name. A route that leaves
// its mode out is in mode endpoint.
func Served(name v1alpha1.RouteMode) bool {
	if name == "" || name == v1alpha1.ModeEndpoint {
		return true
	}
	_, ok := Lookup(name)
	return ok
}

// NotInBuild is the reason a route in mode name is refused by a build that
// does not serve it.
func NotInBuild(name v1alpha1.RouteMode) string {
	return fmt.Sprintf("route mode %s is not part of this build of brig", name)
}
