package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
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
// owns BRIG_VERIFY can name what it is checking. The fetch happens in a runtime
// adapter, through BootResolver, but whether to trust what is fetched is a
// policy question and belongs where the other one is answered.
func BootAssetsRef() string { return bootAssetsRef() }

func bootAssetsRef() string {
	if r := bootAssetsRefOverride(); r != "" {
		return r
	}
	return fmt.Sprintf("%s:%s-%s", bootAssetsRepo, goruntime.GOOS, goruntime.GOARCH)
}

// bootAssetsRefOverride is BRIG_BOOT_ASSETS_REF, or empty when it is unset.
// The hull path needs to tell an override from the default, and bootAssetsRef
// hides that by filling the default in.
func bootAssetsRefOverride() string { return os.Getenv("BRIG_BOOT_ASSETS_REF") }

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
	return orasPull(dir, BootFetch{}, notice, progress)
}

// fetchNotice is the line a boot-asset download says before it starts. A
// first fetch happens once. A replace fetches the bundle that verified over an
// older one an earlier fetch left.
func fetchNotice(fetch BootFetch) string {
	if fetch.Replace {
		return "the kernel and initrd here are an older bundle; downloading the one that verified..."
	}
	return "downloading the kernel and initrd this profile boots (once)..."
}

// orasPull is orasFetch for the bundle fetch names: fetch.Ref, pinned to the
// digest brig verified, or the tag when it is empty. oras replaces files that
// are there.
func orasPull(dir string, fetch BootFetch, notice, progress io.Writer) error {
	ref := fetch.Ref
	if ref == "" {
		ref = bootAssetsRef()
	}
	bin, err := lookPath("oras")
	if err != nil {
		return fmt.Errorf("oras is not installed, so the kernel and initrd cannot be downloaded. "+
			"Install it from https://oras.land, or fetch %s into %s yourself and point "+
			"BRIG_BOOT_ASSETS there", ref, dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create boot asset directory %s: %w", dir, err)
	}
	noticef(notice, "%s", fetchNotice(fetch))
	cmd := exec.Command(bin, "pull", ref, "--output", dir)
	said := narrate(progress)
	cmd.Stdout, cmd.Stderr = said, said
	if err := cmd.Run(); err != nil {
		return said.explain(fmt.Errorf("oras pull %s: %w (if the package is private, run "+
			"`oras login ghcr.io` with a read:packages token)", ref, err))
	}
	if err := writeFetchRecord(dir, ref); err != nil {
		return fmt.Errorf("record the bundle fetched into %s: %w", dir, err)
	}
	noticef(notice, "kernel and initrd downloaded")
	return nil
}

// fetchRecordName is hull's provenance record, which brig writes for a bundle
// it fetched with oras.
const fetchRecordName = "provenance.json"

// writeFetchRecord records which bundle oras fetched into dir, in the shape
// hull writes for its own fetches, so wrap reads both the same way.
//
// Without it, files a later publish of the tag left behind are
// indistinguishable from files somebody changed: both differ from the bundle
// that verified, and only the record says which bundle they were. A pull by
// tag records nothing it could vouch for, so an older record is removed rather
// than left describing other files.
func writeFetchRecord(dir, ref string) error {
	_, digest, pinned := strings.Cut(ref, "@")
	if !pinned {
		if err := os.Remove(filepath.Join(dir, fetchRecordName)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	type file struct {
		SHA256 string `json:"sha256"`
	}
	files := map[string]file{}
	for _, name := range []string{bootKernelName(), bootInitrdName} {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		_ = f.Close()
		if err != nil {
			return err
		}
		files[name] = file{SHA256: hex.EncodeToString(h.Sum(nil))}
	}
	raw, err := json.Marshal(struct {
		Ref            string          `json:"ref"`
		Digest         string          `json:"digest"`
		VerifiedDigest string          `json:"verifiedDigest"`
		Files          map[string]file `json:"files"`
	}{ref, digest, digest, files})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, fetchRecordName), raw, 0o644)
}

// orasFetcher binds the Linux download to one run's writers, the way the hull
// adapter binds its own.
func orasFetcher(spec RunSpec) assetFetcher {
	return func(dir string) error { return orasFetch(dir, spec.Notice, spec.Progress) }
}
