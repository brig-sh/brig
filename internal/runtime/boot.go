package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
)

// Booting an image that was never built to be a guest.
//
// An ordinary OCI image carries no kernel, so something has to supply one. The
// runtime takes the kernel and initrd as OCI annotations and never from the
// image's own metadata, so that an image cannot nominate a file on the host --
// which is what makes finding them brig's job rather than the image's.
//
// Both backends read the same two annotations: hull on its command line, and
// urunc from the container's OCI spec on Linux, where nerdctl passes them
// through with --annotation. So the pair below is the whole contract, and it
// is spelled once.
const (
	annotationBootKernel = "com.urunc.unikernel.bootKernel"
	annotationBootInitrd = "com.urunc.unikernel.bootInitrd"
)

// bootInitrdName is the same on every platform: the initrd is a cpio built for
// the guest, not for the host running brig.
const bootInitrdName = "container-initrd"

// bootKernelName is not. A Linux kernel is a bzImage on x86_64 and an Image on
// arm64, and the guest architecture follows the host: an arm64 Mac boots an
// arm64 guest, an x86_64 Linux box an x86_64 one.
func bootKernelName() string {
	if goruntime.GOARCH == "amd64" {
		return "bzImage"
	}
	return "Image"
}

// bootArtifacts are the host kernel and initrd that boot an unmodified image.
//
// The default location is where whatever ships them puts them, which differs
// per platform; BRIG_BOOT_ASSETS points at a build instead.
//
// The initrd is also where the guest agent comes from -- it is copied into the
// guest rather than taken from the image -- which is what lets brig exec into
// a stock image at all.

// assetFetcher populates dir with the boot assets.
//
// Both platforms shell out rather than linking a registry client in, which is
// the shape internal/verify already uses for cosign. On macOS the tool is hull:
// it downloads this same bundle for its own `hull run`, so it knows the
// reference and the credentials already. On Linux hull does not build -- most
// of its command surface is //go:build darwin -- so oras pulls the artifact
// directly.
//
// Deliberately two implementations rather than one shared package. What has to
// agree between them is a four-item contract -- this directory, the two file
// names, and the reference scheme -- not the mechanics of pulling an artifact
// and writing its layers. Sharing the mechanics would mean promoting hull's
// package to a public API and taking its registry client, credential helper and
// docker/containerd tree as brig dependencies, against the one dependency brig
// has today.
type assetFetcher func(dir string) error

// assetLocator reports where a runtime keeps its boot assets.
//
// brig used to answer this itself, with a path compiled in. hull then moved the
// assets under its store, so the answer depends on hull's store directory and
// on two environment variables it resolves in its own order -- none of which
// brig can see. Re-deriving that here would work until the day it did not, and
// the failure would be silent: brig would check an empty directory, download a
// second copy of the same bundle into it, and boot a kernel hull knows nothing
// about.
//
// So the runtime is asked. A runtime that has no opinion returns "" and the
// per-platform default below applies.
type assetLocator func() (string, error)

func bootArtifacts(locate assetLocator, fetch assetFetcher) (kernel, initrd string, err error) {
	explicit := os.Getenv("BRIG_BOOT_ASSETS")
	dir := explicit
	if dir == "" && locate != nil {
		// A runtime that cannot answer is not fatal: fall through to the
		// default rather than refusing to boot over a missing subcommand.
		if located, locErr := locate(); locErr == nil {
			dir = located
		}
	}
	if dir == "" {
		dir, err = defaultBootAssetsDir()
		if err != nil {
			return "", "", err
		}
	}
	kernel = filepath.Join(dir, bootKernelName())
	initrd = filepath.Join(dir, bootInitrdName)

	if !bootArtifactsPresent(kernel, initrd) {
		switch {
		case explicit != "":
			// BRIG_BOOT_ASSETS points at a build someone is iterating on.
			// Downloading a release bundle into it would replace their work,
			// so this is the one case brig refuses to fix for you.
			return "", "", fmt.Errorf("this profile boots an unmodified image, which needs %s and %s "+
				"in %s: BRIG_BOOT_ASSETS points there, and brig does not download over a directory you chose",
				bootKernelName(), bootInitrdName, explicit)
		case fetch == nil:
			return "", "", fmt.Errorf("this profile boots an unmodified image, which needs %s and %s "+
				"in %s, and this runtime cannot fetch them", bootKernelName(), bootInitrdName, dir)
		default:
			if ferr := fetch(dir); ferr != nil {
				return "", "", fmt.Errorf("this profile boots an unmodified image, which needs %s and %s; "+
					"fetching them failed: %w", bootKernelName(), bootInitrdName, ferr)
			}
		}
	}

	// The same standard as bootArtifactsPresent, not a weaker one. A stat-only
	// check would accept a zero-length kernel -- a truncated download, or a
	// half-written file from a fetch that died -- and hand it to the VMM, where
	// the failure says nothing about where it came from.
	for _, path := range []string{kernel, initrd} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return "", "", fmt.Errorf("this profile boots an unmodified image, but %s is still missing "+
				"after fetching: %w", path, statErr)
		}
		if info.IsDir() || info.Size() == 0 {
			return "", "", fmt.Errorf("this profile boots an unmodified image, but %s is empty; "+
				"delete it and retry, or point BRIG_BOOT_ASSETS at a directory holding %s and %s",
				path, bootKernelName(), bootInitrdName)
		}
	}
	return kernel, initrd, nil
}

// bootArtifactsPresent reports whether both files are already usable. A
// zero-length file counts as missing: it satisfies a bare existence check and
// then fails at boot, where the cause is much harder to see.
func bootArtifactsPresent(kernel, initrd string) bool {
	for _, path := range []string{kernel, initrd} {
		if info, err := os.Stat(path); err != nil || info.IsDir() || info.Size() == 0 {
			return false
		}
	}
	return true
}

// bootAnnotations are the annotations that carry those artifacts, in the form
// both runtimes take them.
func bootAnnotations(locate assetLocator, fetch assetFetcher) (kv []string, err error) {
	kernel, initrd, err := bootArtifacts(locate, fetch)
	if err != nil {
		return nil, err
	}
	return []string{
		annotationBootKernel + "=" + kernel,
		annotationBootInitrd + "=" + initrd,
	}, nil
}
