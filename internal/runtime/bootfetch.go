package runtime

import (
	"fmt"
	"io"
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
// BootAssetsRef is the bundle this host would boot, exported so the layer that
// owns BRIG_VERIFY can name what it is checking. The fetch happens deep inside
// a runtime adapter, but whether to trust what is fetched is a policy question
// and belongs where the other one is answered.
func BootAssetsRef() string { return bootAssetsRef() }

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
//
// One line each end and the stream behind Progress, the same shape the hull
// path takes: a first run downloads a bundle, and the reader is owed the fact
// that it is downloading rather than every layer of how.
func orasFetch(dir string, notice, progress io.Writer) error {
	bin, err := lookPath("oras")
	if err != nil {
		return fmt.Errorf("oras is not installed, so the kernel and initrd cannot be downloaded. "+
			"Install it from https://oras.land, or fetch %s into %s yourself and point "+
			"BRIG_BOOT_ASSETS there", bootAssetsRef(), dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create boot asset directory %s: %w", dir, err)
	}
	noticef(notice, "downloading the kernel and initrd this profile boots (once)...")
	cmd := exec.Command(bin, "pull", bootAssetsRef(), "--output", dir)
	said := narrate(progress)
	cmd.Stdout, cmd.Stderr = said, said
	if err := cmd.Run(); err != nil {
		return said.explain(fmt.Errorf("oras pull %s: %w (if the package is private, run "+
			"`oras login ghcr.io` with a read:packages token)", bootAssetsRef(), err))
	}
	noticef(notice, "kernel and initrd downloaded")
	return nil
}

// orasFetcher binds the Linux download to one run's writers, the way the hull
// adapter binds its own.
func orasFetcher(spec RunSpec) assetFetcher {
	return func(dir string) error { return orasFetch(dir, spec.Notice, spec.Progress) }
}
