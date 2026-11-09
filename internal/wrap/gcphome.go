package wrap

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// gcloudDir is the directory gcloud reads its logins from in the guest's
// home, and the directory of the application default credentials the Google
// client libraries read. A login there is a Google identity besides the gcp
// route's, which the run did not name: gcloud uses it whenever the guest has
// not pointed it at the route's token file, and a client library, which
// never reads that file, uses it to reach Google directly wherever egress
// allows. So on a run with a gcp route nothing the host hands the guest may
// put one there. A guest that gets one in some other way still reaches the
// listed services only through the broker, which takes the session token
// alone, and under deny egress no Google host directly.
const gcloudDir = ".config/gcloud"

// gcpShadowVars are the variables that hand gcloud or a Google client library
// an identity, pick one, or move where it reads one from: the identity
// variables the broker drops when it runs gcloud (gcpcred.droppedEnv),
// CLOUDSDK_CONFIG, which moves ~/.config/gcloud, and GCE_METADATA_*, which
// points a client at another metadata server. A name that ends in _ is a
// prefix. The preset's own variables, CLOUDSDK_API_ENDPOINT_OVERRIDES_* and
// CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE, are not on it.
//
// CLOUDSDK_AUTH_ACCESS_TOKEN_FILE is on it too, although the guest sets it to
// the route's token: from the profile it can as well name a token that a
// files: binding puts in the guest, which gcloud would use in place of the
// route while the run names the route's identity.
var gcpShadowVars = []string{
	"GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_CONFIG", "CLOUDSDK_CORE_ACCOUNT",
	"CLOUDSDK_ACTIVE_CONFIG_NAME", "CLOUDSDK_AUTH_", "GCE_METADATA_",
}

// gcpTokenFileVar is the variable the guest sets itself to the file it writes
// the route's session token to.
const gcpTokenFileVar = "CLOUDSDK_AUTH_ACCESS_TOKEN_FILE"

// gcpShadow reports whether name is on gcpShadowVars.
func gcpShadow(name string) bool {
	return slices.ContainsFunc(gcpShadowVars, func(v string) bool {
		if strings.HasSuffix(v, "_") {
			return strings.HasPrefix(name, v)
		}
		return name == v
	})
}

// gcpRoute reports whether this run has a gcp route.
func (c *Config) gcpRoute() bool {
	return slices.ContainsFunc(c.routes, isGCPRoute)
}

func isGCPRoute(r v1alpha1.Route) bool { return r.Preset == v1alpha1.PresetGCP }

// gcpList names each gcp route, with the policy that declares it.
func (c *Config) gcpList() string {
	var names []string
	for _, r := range c.routes {
		if isGCPRoute(r) {
			names = append(names, fmt.Sprintf("%s (route %s)", c.routePolicy[r.Name], r.Name))
		}
	}
	return strings.Join(names, ", ")
}

// gcpShadowRefusal refuses a run with a gcp route when the guest would also
// get a variable from gcpShadowVars, from the profile or BRIG_FORWARD_ENV.
func (c *Config) gcpShadowRefusal() error {
	if !c.gcpRoute() {
		return nil
	}
	var shadow []string
	for _, name := range bindingNames(c.Env) {
		if gcpShadow(name) && !slices.Contains(shadow, name) {
			shadow = append(shadow, name)
		}
	}
	if len(shadow) == 0 {
		return nil
	}
	msg := fmt.Sprintf("policy %s declares a gcp route, and %s also hands the guest %s, "+
		"which can give gcloud or a Google client library an identity besides the route's; remove it from the "+
		"profile and BRIG_FORWARD_ENV, or detach the policy", c.gcpList(), c.Profile.Name, strings.Join(shadow, ", "))
	if slices.Contains(shadow, gcpTokenFileVar) {
		msg += ". The guest sets " + gcpTokenFileVar + " itself, to the file it writes $BRIG_GCP_TOKEN to"
	}
	return errors.New(msg)
}

// onGCloudDir reports whether rel, relative to the guest home, is
// .config/gcloud, inside it, or .config, which holds it.
func onGCloudDir(rel string) bool {
	rel = slashClean(filepath.ToSlash(rel))
	return rel == gcloudDir || strings.HasPrefix(rel, gcloudDir+"/") || rel == filepath.Dir(gcloudDir)
}

// gcpHomeRefusal refuses a run with a gcp route whose profile would put
// something at ~/.config/gcloud in the guest, or at ~/.config, which holds
// it: a volume, a files: binding, or a host config seed. A seed may not copy
// the host's own ~/.config/gcloud either, or a directory that holds it. It
// reads the profile only; PrepareWorkspace checks the workspace itself.
func (c *Config) gcpHomeRefusal() error {
	if !c.gcpRoute() {
		return nil
	}
	var why []string
	home := slashClean(c.Profile.GuestHome)
	for _, v := range c.Profile.Volumes {
		if v.Path != "" && onGCloudDir(v.Path) {
			why = append(why, fmt.Sprintf("a %s volume at %s", v.Kind, v.Path))
		}
		if v.At != "" {
			if rel, ok := strings.CutPrefix(slashClean(v.At), home+"/"); ok && onGCloudDir(rel) {
				why = append(why, fmt.Sprintf("a %s volume at %s", v.Kind, v.At))
			}
		}
	}
	for _, f := range c.Profile.Files {
		if onGCloudDir(f.Path) {
			why = append(why, fmt.Sprintf("the file %s from %s", f.Path, f.Ref))
		}
	}
	hostGCloud := ""
	if h, err := os.UserHomeDir(); err == nil {
		hostGCloud = filepath.Join(h, filepath.FromSlash(gcloudDir))
	}
	for _, s := range c.HostConfig {
		switch {
		case onGCloudDir(s.Rel):
			why = append(why, fmt.Sprintf("a copy of %s at %s", s.Host, s.Rel))
		case hostGCloud != "" && overlaps(filepath.Clean(s.Host), hostGCloud):
			why = append(why, fmt.Sprintf("a copy of your %s", s.Host))
		}
	}
	if len(why) == 0 {
		return nil
	}
	return fmt.Errorf("policy %s declares a gcp route, and %s puts %s in the guest's ~/%s, "+
		"where gcloud and the Google client libraries read an identity besides the route's; remove it from "+
		"the profile, or detach the policy", c.gcpList(), c.Profile.Name, strings.Join(why, " and "), gcloudDir)
}

// overlaps reports whether one of the clean host paths a and b is the other
// or holds it.
func overlaps(a, b string) bool {
	sep := string(filepath.Separator)
	return a == b || strings.HasPrefix(a, b+sep) || strings.HasPrefix(b, a+sep)
}

// gcloudIdentityFiles are the files in ~/.config/gcloud that hold an
// identity: gcloud's stored logins and tokens, and the application default
// credentials. The rest is what gcloud in the guest writes there itself on
// its first run, such as active_config, configurations/ and logs/.
var gcloudIdentityFiles = []string{
	gcloudDir + "/credentials.db",
	gcloudDir + "/access_tokens.db",
	gcloudDir + "/application_default_credentials.json",
	gcloudDir + "/legacy_credentials",
}

// gcpWorkspaceRefusal refuses a run with a gcp route whose guest home already
// holds a gcloud identity file, such as a home named with --home ~. It looks
// through the workspace root: a link at .config/gcloud or at the file is not
// followed, and is refused like the file. A .config that leads out of the
// home is refused by the root.
func (c *Config) gcpWorkspaceRefusal(r *workspaceRoot) error {
	if !c.gcpRoute() {
		return nil
	}
	info, err := r.lstat(gcloudDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case info.Mode()&os.ModeSymlink != 0:
		return c.gcloudFileRefusal(r, gcloudDir)
	}
	for _, rel := range gcloudIdentityFiles {
		_, err := r.lstat(rel)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return err
		}
		return c.gcloudFileRefusal(r, rel)
	}
	return nil
}

func (c *Config) gcloudFileRefusal(r *workspaceRoot, rel string) error {
	return fmt.Errorf("policy %s declares a gcp route, and the guest home %s holds %s, "+
		"an identity besides the route's that gcloud and the Google client libraries read; move it out of "+
		"the guest home, or run with another --home", c.gcpList(), r.dir, rel)
}
