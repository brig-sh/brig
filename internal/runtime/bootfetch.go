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
// One line each end (a spinner on a terminal) and the stream behind Progress,
// the same shape the hull path takes: a first run downloads a bundle, and the
// reader is owed the fact that it is downloading rather than every layer of
// how.
func orasFetch(dir string, notice, progress io.Writer) error {
	return orasPull(dir, BootFetch{}, notice, progress)
}

// fetchNotice returns what a boot-asset download announces while it runs. A
// first fetch happens once. A replace fetches the bundle that verified over an
// older one an earlier fetch left.
func fetchNotice(fetch BootFetch) string {
	if fetch.Replace {
		return "the kernel and initrd here are an older bundle; downloading the one that verified"
	}
	return "downloading the kernel and initrd this profile boots (once)"
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
	end := announce(notice, fetchNotice(fetch), "kernel and initrd downloaded")
	cmd := exec.Command(bin, "pull", ref, "--output", dir)
	said := narrate(progress)
	cmd.Stdout, cmd.Stderr = said, said
	if err := cmd.Run(); err != nil {
		end(false)
		return said.explain(fmt.Errorf("oras pull %s: %w (if the package is private, run "+
			"`oras login ghcr.io` with a read:packages token)", ref, err))
	}
	if err := writeFetchRecord(dir, ref); err != nil {
		end(false)
		return fmt.Errorf("record the bundle fetched into %s: %w", dir, err)
	}
	end(true)
	return nil
}

// fetchRecordName is hull's provenance record, which brig writes for a bundle
// it fetched with oras.
const fetchRecordName = "provenance.json"

// writeFetchRecord records which bundle oras fetched into dir, in the shape
// hull writes for its own fetches, so wrap reads both the same way. hull on
// Linux checks the kernel and initrd against this record before it boots, by
// digest and by size, so a record without a size refuses every boot.
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
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	type file struct {
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	}
	files := map[string]file{}
	for _, name := range []string{bootKernelName(), bootInitrdName} {
		sum, n, err := hashIn(root, name)
		if err != nil {
			return err
		}
		files[name] = file{SHA256: sum, Size: n}
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
	return writeRecord(root, raw)
}

// writeRecord replaces dir's fetch record with raw through a file beside it
// and a rename, as hull writes its own. Every run reads this one file, so a
// write in place could be read half-done by a run started at the same time,
// or left half-done by a brig that died, and hull refuses a boot against
// either.
func writeRecord(root *os.Root, raw []byte) error {
	tmp := fmt.Sprintf(".%s.%d.tmp", fetchRecordName, os.Getpid())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(raw)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Rename(tmp, fetchRecordName)
	}
	if err != nil {
		_ = root.Remove(tmp)
	}
	return err
}

// fillFetchRecordSizes gives the kernel and the initrd in dir's fetch record
// the size each is missing, when the file still has the digest the record
// names. A brig older than writeFetchRecord's size wrote records without one,
// and hull on Linux reads the absent size as zero and refuses every boot from
// that directory. brig fetches again only when the files change, so such a
// record would stay.
//
// It vouches for nothing new: a file whose digest differs from the record, or
// an entry with no digest, keeps the record as it was, and hull refuses it as
// before. Only the two names hull boots are looked at, inside dir, and only
// as regular files. No record is no error. An error says the record needed a
// size and did not get it.
func fillFetchRecordSizes(dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	raw, err := root.ReadFile(fetchRecordName)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("%s is not JSON: %w", fetchRecordName, err)
	}
	var files map[string]map[string]any
	if err := json.Unmarshal(rec["files"], &files); err != nil {
		return fmt.Errorf("%s lists no files: %w", fetchRecordName, err)
	}
	changed := false
	for _, name := range []string{bootKernelName(), bootInitrdName} {
		f, ok := files[name]
		if !ok {
			continue
		}
		if size, _ := f["size"].(float64); size > 0 {
			continue
		}
		want, _ := f["sha256"].(string)
		if want == "" {
			continue
		}
		got, n, err := hashIn(root, name)
		if err != nil {
			return err
		}
		if got != want {
			continue
		}
		f["size"] = n
		changed = true
	}
	if !changed {
		return nil
	}
	out, err := json.Marshal(files)
	if err != nil {
		return err
	}
	rec["files"] = out
	if raw, err = json.Marshal(rec); err != nil {
		return err
	}
	return writeRecord(root, raw)
}

// fillBootRecord runs fillFetchRecordSizes for the bundle a run boots, in a
// directory brig chose. A BRIG_BOOT_ASSETS directory is the user's, and brig
// never writes there. A record the fill could not complete is named on
// spec.Notice, since hull's refusal then reads as if the kernel was replaced.
func fillBootRecord(spec RunSpec) {
	if !spec.GenericBoot || !spec.BootAssets.given() || spec.BootAssets.Named {
		return
	}
	dir := filepath.Dir(spec.BootAssets.Kernel)
	if err := fillFetchRecordSizes(dir); err != nil {
		noticef(spec.Notice, "the record of the boot bundle in %s is from an older brig and could not be "+
			"updated (%v). If hull refuses the kernel, delete the kernel and initrd there and run again to "+
			"fetch the bundle", dir, err)
	}
}

// hashIn returns the sha256 of the regular file name in root, in hex, and its
// size. A FIFO or a device there is refused rather than read.
func hashIn(root *os.Root, name string) (string, int64, error) {
	fi, err := root.Lstat(name)
	if err != nil {
		return "", 0, err
	}
	if !fi.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s is not a regular file", name)
	}
	f, err := root.Open(name)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// orasFetcher binds the Linux download to one run's writers, the way the hull
// adapter binds its own.
func orasFetcher(spec RunSpec) assetFetcher {
	return func(dir string) error { return orasFetch(dir, spec.Notice, spec.Progress) }
}
