package runtime

import (
	"fmt"
	"os"
	"os/exec"
	goruntime "runtime"
)

// bootAssetsRepo is where the published bundles live. The platform is in the
// tag rather than in a multi-platform index: an OCI artifact has an empty
// config, so the descriptors in an index come out carrying no platform and a
// matching client resolves nothing.
const bootAssetsRepo = "ghcr.io/nofireai/hull-assets"

// bootAssetsRef is the bundle for this host.
//
// The tag names the guest platform. On Linux that is also the host's -- an
// amd64 box boots an amd64 guest -- so GOOS-GOARCH is the right key.
// BRIG_BOOT_ASSETS_REF overrides it whole, which is how a version gets pinned
// or a mirror gets used.
func bootAssetsRef() string {
	if r := os.Getenv("BRIG_BOOT_ASSETS_REF"); r != "" {
		return r
	}
	return fmt.Sprintf("%s:%s-%s", bootAssetsRepo, goruntime.GOOS, goruntime.GOARCH)
}

// lookPath is a variable so a test can pretend a tool is or is not installed.
// internal/verify does the same for cosign.
var lookPath = exec.LookPath

// orasFetch downloads the bundle into dir with oras.
//
// This is the Linux path. There is no hull to ask there, and linking a registry
// client in would cost brig its single-dependency go.mod, so brig shells out to
// oras exactly as it shells out to cosign for signatures: use the tool when it
// is present, say so plainly when it is not.
func orasFetch(dir string) error {
	bin, err := lookPath("oras")
	if err != nil {
		return fmt.Errorf("oras is not installed, so the kernel and initrd cannot be downloaded. "+
			"Install it from https://oras.land, or fetch %s into %s yourself and point "+
			"BRIG_BOOT_ASSETS there", bootAssetsRef(), dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create boot asset directory %s: %w", dir, err)
	}
	cmd := exec.Command(bin, "pull", bootAssetsRef(), "--output", dir)
	// Progress belongs on stderr: brig's stdout is the workload's.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("oras pull %s: %w (if the package is private, run "+
			"`oras login ghcr.io` with a read:packages token)", bootAssetsRef(), err)
	}
	return nil
}
