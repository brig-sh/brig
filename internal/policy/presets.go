package policy

import (
	"embed"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// presetFiles holds the presets brig ships. Each file is one route written in
// full, and decodes strictly into a v1alpha1.Route.
//
//go:embed presets/*.yaml
var presetFiles embed.FS

// PresetNames lists the presets brig ships, sorted.
func PresetNames() []v1alpha1.Preset {
	entries, err := presetFiles.ReadDir("presets")
	if err != nil {
		return nil
	}
	var names []v1alpha1.Preset
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".yaml"); ok {
			names = append(names, v1alpha1.Preset(name))
		}
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}

// PresetRoute returns the route that a preset expands to, with the defaults
// applied. It has no credential.secretRef, which only a policy names, so it
// is not a complete route on its own.
func PresetRoute(name v1alpha1.Preset) (v1alpha1.Route, error) {
	r, err := decodePreset(name)
	if err != nil {
		return v1alpha1.Route{}, err
	}
	setDefaults(&r)
	return r, nil
}

// decodePreset reads one preset file. Each call decodes the file again, so a
// caller can change the result without reaching the next one.
func decodePreset(name v1alpha1.Preset) (v1alpha1.Route, error) {
	blob, err := presetFiles.ReadFile("presets/" + string(name) + ".yaml")
	if err != nil {
		return v1alpha1.Route{}, fmt.Errorf("preset %q is not one brig ships (%s)", name, presetList())
	}
	var r v1alpha1.Route
	if err := v1alpha1.UnmarshalStrict(blob, &r); err != nil {
		return v1alpha1.Route{}, fmt.Errorf("preset %s: %w", name, err)
	}
	// A lifecycle is the preset's to set, and Route.Validate accepts it only
	// when it was set this way.
	if r.Credential != nil {
		r.SetLifecycleFromPreset(r.Credential.Lifecycle)
	}
	return r, nil
}

func presetList() string {
	var names []string
	for _, n := range PresetNames() {
		names = append(names, string(n))
	}
	return strings.Join(names, ", ")
}

// ExpandRoute fills r from its preset, applies the defaults, and validates
// the result with Route.Validate. The result shares no map, slice or pointer
// with r.
//
// A field that r sets overrides the value of the preset. A struct is merged
// field by field, guest.env name by name, and inject.headers.remove is the
// union of both lists. Any other list is replaced whole. credential.lifecycle
// comes from the preset alone. A preset with a lifecycle keeps its upstream
// and its credential.field, since the refresh it runs is for that endpoint and
// that field.
func ExpandRoute(r v1alpha1.Route) (v1alpha1.Route, error) {
	out := *r.DeepCopy()
	if r.Preset != "" {
		base, err := decodePreset(r.Preset)
		if err != nil {
			return v1alpha1.Route{}, fmt.Errorf("route %s: %w", r.Name, err)
		}
		if out, err = overlay(base, out); err != nil {
			return v1alpha1.Route{}, fmt.Errorf("route %s: %w", r.Name, err)
		}
	}
	setDefaults(&out)
	if err := out.Validate(); err != nil {
		return v1alpha1.Route{}, err
	}
	return out, nil
}

// overlay returns base with every field that r sets taking r's value. Both
// are the caller's own copies.
func overlay(base, r v1alpha1.Route) (v1alpha1.Route, error) {
	if base.Credential != nil && base.Credential.Lifecycle != "" {
		lifecycle := base.Credential.Lifecycle
		if r.Upstream != "" && r.Upstream != base.Upstream {
			return v1alpha1.Route{}, fmt.Errorf("preset %s keeps its credential fresh (%s) against %s, "+
				"so upstream cannot be changed", r.Preset, lifecycle, base.Upstream)
		}
		if r.Credential != nil && r.Credential.Field != "" && r.Credential.Field != base.Credential.Field {
			return v1alpha1.Route{}, fmt.Errorf("preset %s keeps its credential fresh (%s) in field %s, "+
				"so credential.field cannot be changed", r.Preset, lifecycle, base.Credential.Field)
		}
	}
	out := base
	out.Name = r.Name
	out.Preset = r.Preset
	if r.Mode != "" {
		out.Mode = r.Mode
	}
	if r.Upstream != "" {
		out.Upstream = r.Upstream
	}
	if r.Credential != nil {
		c := v1alpha1.CredentialRef{}
		if base.Credential != nil {
			c = *base.Credential
		}
		if r.Credential.SecretRef.Name != "" {
			c.SecretRef.Name = r.Credential.SecretRef.Name
		}
		if r.Credential.Field != "" {
			c.Field = r.Credential.Field
		}
		out.Credential = &c
	}
	if r.Inject != nil {
		in := v1alpha1.Inject{}
		if base.Inject != nil {
			in = *base.Inject
		}
		if r.Inject.Headers != nil {
			h := v1alpha1.HeaderInject{}
			if in.Headers != nil {
				h = *in.Headers
			}
			if r.Inject.Headers.Set != nil {
				h.Set = r.Inject.Headers.Set
			}
			for _, name := range r.Inject.Headers.Remove {
				if !slices.ContainsFunc(h.Remove, func(n string) bool { return strings.EqualFold(n, name) }) {
					h.Remove = append(h.Remove, name)
				}
			}
			if r.Inject.Headers.Allow != nil {
				h.Allow = r.Inject.Headers.Allow
			}
			in.Headers = &h
		}
		if r.Inject.BasicAuth != nil {
			in.BasicAuth = r.Inject.BasicAuth
		}
		out.Inject = &in
	}
	if r.Guest != nil {
		g := v1alpha1.Guest{}
		if base.Guest != nil {
			g = *base.Guest
		}
		if len(r.Guest.Env) > 0 {
			env := map[string]string{}
			for k, v := range g.Env {
				env[k] = v
			}
			for k, v := range r.Guest.Env {
				env[k] = v
			}
			g.Env = env
		}
		if r.Guest.Git != nil {
			g.Git = r.Guest.Git
		}
		out.Guest = &g
	}
	if r.Request != nil {
		out.Request = r.Request
	}
	return out, nil
}

// setDefaults fills the fields that a route may leave out: mode, which is
// endpoint, and Authorization in inject.headers.remove. The guest's own
// Authorization carries a session token, which upstream must never see.
func setDefaults(r *v1alpha1.Route) {
	if r.Mode == "" {
		r.Mode = v1alpha1.ModeEndpoint
	}
	if r.Inject == nil {
		return
	}
	// Copied before the change, because r can share these structs with the
	// policy that the caller loaded.
	in := *r.Inject
	h := v1alpha1.HeaderInject{}
	if in.Headers != nil {
		h = *in.Headers
	}
	if !slices.ContainsFunc(h.Remove, func(name string) bool { return strings.EqualFold(name, "Authorization") }) {
		h.Remove = append([]string{"Authorization"}, h.Remove...)
	}
	in.Headers = &h
	r.Inject = &in
}

// Expand returns p with every route expanded by ExpandRoute.
func Expand(p Policy) (Policy, error) {
	if len(p.Spec.Routes) == 0 {
		return p, nil
	}
	routes := make([]v1alpha1.Route, 0, len(p.Spec.Routes))
	for _, r := range p.Spec.Routes {
		e, err := ExpandRoute(r)
		if err != nil {
			return Policy{}, err
		}
		routes = append(routes, e)
	}
	p.Spec.Routes = routes
	return p, nil
}

// Routes expands the routes of every policy in ps, in order, and refuses two
// routes that share a name, a guest.env name or a guest.git.insteadOf prefix.
//
// Every route of every bound policy applies to the run, as every egress rule
// does. A route name is the path of the route URL, a guest.env name is one
// variable in the guest, and git rewrites a prefix to one URL, so two routes
// cannot share any of the three.
func Routes(ps []Policy) ([]v1alpha1.Route, error) {
	type owner struct{ policy, route string }
	routeOwner := map[string]string{}
	envOwner := map[string]owner{}
	prefixOwner := map[string]owner{}
	var out []v1alpha1.Route
	for _, p := range ps {
		for _, r := range p.Spec.Routes {
			e, err := ExpandRoute(r)
			if err != nil {
				return nil, fmt.Errorf("policy %s: %w", p.Metadata.Name, err)
			}
			if prev, ok := routeOwner[e.Name]; ok {
				return nil, fmt.Errorf("route %s is declared by both %s and %s; a route name is "+
					"unique across the policies bound to a run", e.Name, prev, p.Metadata.Name)
			}
			routeOwner[e.Name] = p.Metadata.Name
			if e.Guest != nil {
				for _, name := range sortedKeys(e.Guest.Env) {
					if prev, ok := envOwner[name]; ok {
						return nil, fmt.Errorf("guest.env %s is set by route %s of %s and by route %s "+
							"of %s; a guest.env name is unique across the policies bound to a run",
							name, prev.route, prev.policy, e.Name, p.Metadata.Name)
					}
					envOwner[name] = owner{p.Metadata.Name, e.Name}
				}
				if e.Guest.Git != nil {
					for _, prefix := range e.Guest.Git.InsteadOf {
						if prev, ok := prefixOwner[prefix]; ok {
							return nil, fmt.Errorf("guest.git.insteadOf %s is rewritten by route %s of %s "+
								"and by route %s of %s; git sends a prefix to one route",
								prefix, prev.route, prev.policy, e.Name, p.Metadata.Name)
						}
						prefixOwner[prefix] = owner{p.Metadata.Name, e.Name}
					}
				}
			}
			out = append(out, e)
		}
	}
	return out, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
