package broker

import (
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// repos ignores ASCII case, as GitHub does, and paths keep it, since an
// upstream may tell /v2/Items from /v2/items.
func TestAllowFoldsCaseInReposAlone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		allow v1alpha1.RequestAllow
		rest  string
		// field is the list that refuses, or "" for none.
		field string
	}{
		{"a lower-case owner matches a mixed-case glob",
			v1alpha1.RequestAllow{Repos: []string{"Example/*"}}, "/example/lib.git/info/refs", ""},
		{"an upper-case path matches a lower-case name",
			v1alpha1.RequestAllow{Repos: []string{"example/lib"}}, "/EXAMPLE/LIB.git/x", ""},
		{"another name in another case is refused",
			v1alpha1.RequestAllow{Repos: []string{"Example/lib"}}, "/example/lib2.git/x", "repos"},
		{"a range in the glob is read in lower case",
			v1alpha1.RequestAllow{Repos: []string{"ex[A-Z]mple/*"}}, "/example/x", ""},
		// canonical refuses the path first on a route; the match must not
		// fold it either, should that ever change.
		{"the Kelvin sign is not k",
			v1alpha1.RequestAllow{Repos: []string{"Example/kit"}}, "/example/\u212ait", "repos"},
		{"paths keep their case",
			v1alpha1.RequestAllow{Paths: []string{"/v2/Items/*"}}, "/v2/items/1", "paths"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := allow(&tc.allow, "GET", tc.rest)
			switch {
			case tc.field == "" && ref != nil:
				t.Fatalf("refused by %s: %s", ref.field, ref.reason)
			case tc.field != "" && ref == nil:
				t.Fatalf("allowed, want a refusal by %s", tc.field)
			case tc.field != "" && ref.field != tc.field:
				t.Fatalf("refused by %s, want %s", ref.field, tc.field)
			}
		})
	}
}
