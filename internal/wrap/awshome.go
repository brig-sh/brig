package wrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// awsDir is the directory an AWS SDK reads in the guest's home. Its
// credentials and config files win over the container credential provider,
// so on a run with a route that vends, nothing may put one there: the guest
// would run as that identity while the run prints the route's.
const awsDir = ".aws"

// underAWSDir reports whether rel, relative to the guest home, is .aws or
// inside it, with the case folded (see foldedUnder).
func underAWSDir(rel string) bool {
	return foldedUnder(slashClean(filepath.ToSlash(rel)), awsDir)
}

// awsHomeRefusal refuses a run with a route that vends whose profile would
// put something at ~/.aws in the guest: a volume, a files: binding, the
// onboarding file, or a host config seed, which also may not copy the host's
// own ~/.aws. It reads
// the profile only; PrepareWorkspace checks the workspace itself.
func (c *Config) awsHomeRefusal() error {
	if !c.vends() {
		return nil
	}
	var why []string
	home := slashClean(c.Profile.GuestHome)
	for _, v := range c.Profile.Volumes {
		if v.Path != "" && underAWSDir(v.Path) {
			why = append(why, fmt.Sprintf("a %s volume at %s", v.Kind, v.Path))
		}
		if v.At != "" {
			if rel, ok := strings.CutPrefix(slashClean(v.At), home+"/"); ok && underAWSDir(rel) {
				why = append(why, fmt.Sprintf("a %s volume at %s", v.Kind, v.At))
			}
		}
	}
	for _, f := range c.Profile.Files {
		if underAWSDir(f.Path) {
			why = append(why, fmt.Sprintf("the file %s from %s", f.Path, f.Ref))
		}
	}
	// seedOnboarding writes the seed after the workspace refusals have run,
	// so a seed there would boot on a fresh home.
	if ob := c.Profile.Onboarding; ob != nil && ob.File != "" && underAWSDir(ob.File) {
		why = append(why, fmt.Sprintf("the onboarding file %s", ob.File))
	}
	hostAWS := ""
	if h, err := os.UserHomeDir(); err == nil {
		hostAWS = filepath.Join(h, awsDir)
	}
	for _, s := range c.HostConfig {
		host := filepath.Clean(s.Host)
		switch {
		case underAWSDir(s.Rel):
			why = append(why, fmt.Sprintf("a copy of %s at %s", s.Host, s.Rel))
		case hostAWS != "" && foldedUnder(filepath.ToSlash(host), filepath.ToSlash(hostAWS)):
			why = append(why, fmt.Sprintf("a copy of your %s", s.Host))
		}
	}
	if len(why) == 0 {
		return nil
	}
	return fmt.Errorf("policy %s declares a route that vends AWS keys, and %s puts %s in the guest's "+
		"~/%s, which the AWS SDKs read before the route; remove it from the profile, or detach the policy",
		c.vendList(), c.Profile.Name, strings.Join(why, " and "), awsDir)
}

// awsIdentityFiles are the files in ~/.aws an AWS SDK reads an identity
// from. The rest of ~/.aws is the CLI's own cache, which the aws CLI in the
// guest writes there itself.
var awsIdentityFiles = []string{awsDir + "/credentials", awsDir + "/config"}

// awsWorkspaceRefusal refuses a run with a route that vends whose guest home
// already holds an AWS identity file, such as a home named with --home ~. It
// looks through the workspace root: a link at .aws or at the file is not
// followed, and is refused like the file. A .aws that is not a directory
// holds nothing.
func (c *Config) awsWorkspaceRefusal(r *workspaceRoot) error {
	if !c.vends() {
		return nil
	}
	if info, err := r.lstat(awsDir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return c.awsFileRefusal(r, awsDir)
	}
	for _, rel := range awsIdentityFiles {
		_, err := r.lstat(rel)
		switch {
		case absent(err):
			continue
		case err != nil:
			return err
		}
		return c.awsFileRefusal(r, rel)
	}
	return nil
}

func (c *Config) awsFileRefusal(r *workspaceRoot, rel string) error {
	return fmt.Errorf("policy %s declares a route that vends AWS keys, and the guest home %s holds %s, "+
		"which the AWS SDKs read before the route; move it out of the guest home, or run with "+
		"another --home", c.vendList(), r.dir, rel)
}
