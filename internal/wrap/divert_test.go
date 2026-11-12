package wrap

import (
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// A host route is reached at its own name under brig.internal, so the
// runtime diverts brig.internal and not the host of its upstream.
func TestHostRouteIsNotDiverted(t *testing.T) {
	shortStateDir(t)
	c := &Config{
		Runtime: &hullRecorder{},
		VMName:  "brig-host-route",
		routes: []v1alpha1.Route{{
			Name: "github-api", Mode: v1alpha1.ModeHost, Upstream: "https://api.github.com",
		}},
	}
	d := c.diverts()
	for _, r := range d.Routes {
		if r.Host == "api.github.com" {
			t.Errorf("Divert.Routes holds %+v, want no divert of the upstream", r)
		}
	}
	if !d.BrigInternal {
		t.Errorf("Divert = %+v, want brig.internal diverted", d)
	}
}
