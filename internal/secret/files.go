package secret

// This file is the store of a broker on a Kubernetes node: a directory with
// one file per secret, the way the kubelet mounts a Secret. The kubelet makes
// the directory root-owned and 0755, and each entry in it is a symlink
// through `..data` to a file in a timestamped directory, so the mode of the
// directory says nothing about who can read a value. The files are checked
// instead: each must be a regular file that is not world-readable. A value is
// the file's bytes verbatim, a trailing newline included.
//
// The store is read-only. A write to a mounted Secret would fail or land
// where nothing reads it, and either would hide a mistake, so every write
// returns ErrReadOnly.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// maxFileValue bounds the bytes Read takes from one file. A mounted Secret
// is at most 1 MiB, so a larger file is not one.
const maxFileValue = 1 << 20

// worldReadable is the mode bit that lets any user on the node read a file.
const worldReadable = 0o004

// Files is a read-only Store over a directory with one file per secret, such
// as a Kubernetes Secret the kubelet mounted.
type Files struct {
	// dir is the directory the store reads.
	dir string
	// skipped are the entry names OpenFiles found and does not serve.
	skipped []string
}

var _ Store = (*Files)(nil)

// OpenFiles opens the store over dir, which must be a directory; a symlink to
// one is followed.
//
// An entry whose name starts with a dot is the kubelet's own bookkeeping
// (`..data`, `..<timestamp>`) and is skipped without a word. A name outside
// the secret-name grammar, such as `ca.crt`, is skipped and reported by
// Skipped, whatever its stat says. A name inside the grammar whose stat fails,
// a dangling symlink for one, fails the open: it is a secret the operator
// meant to mount and cannot be read. One that is not a regular file is
// skipped. A regular file that anyone on the node may read fails the open,
// because a world-readable file on a shared node leaks the credential. An
// empty directory opens.
func OpenFiles(dir string) (*Files, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("cannot open the secret directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("cannot open the secret directory %s: it is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("cannot read the secret directory %s: %w", dir, err)
	}
	f := &Files{dir: dir}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if ValidName(name) != nil {
			f.skipped = append(f.skipped, name)
			continue
		}
		path := filepath.Join(dir, name)
		fi, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("cannot read the secret %s: %w", path, err)
		}
		if !fi.Mode().IsRegular() {
			f.skipped = append(f.skipped, name)
			continue
		}
		if err := checkReadable(path, fi); err != nil {
			return nil, err
		}
	}
	slices.Sort(f.skipped)
	return f, nil
}

// checkReadable refuses a secret file that any user on the node can read.
// There is no ownership rule: the broker on a node runs as root, or the
// Secret's fsGroup makes the file readable to it.
func checkReadable(path string, fi os.FileInfo) error {
	if fi.Mode().Perm()&worldReadable != 0 {
		return fmt.Errorf("refusing the secret %s: its mode is %#o, and any user on the node can read it. "+
			"Mount the Secret with defaultMode: 0440", path, fi.Mode().Perm())
	}
	return nil
}

// Kind names the backend, for reporting.
func (f *Files) Kind() string { return "files" }

// Read returns the value of name verbatim, at most 1 MiB, or ErrNotFound when
// there is no such file. A file that is not a regular file, or that anyone on
// the node may read, is refused with an error naming it.
func (f *Files) Read(name string) ([]byte, error) {
	if err := validStoreName(name); err != nil {
		return nil, err
	}
	path := filepath.Join(f.dir, name)
	// The name is in the secret-name grammar, so it is one path element.
	// The reads still go through a root on the directory, so no name and no
	// link can reach a file outside it.
	root, err := os.OpenRoot(f.dir)
	if err != nil {
		return nil, fmt.Errorf("cannot open the secret directory %s: %w", f.dir, err)
	}
	defer func() { _ = root.Close() }()
	fi, err := root.Stat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read the secret %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing the secret %s: it is not a regular file", path)
	}
	if err := checkReadable(path, fi); err != nil {
		return nil, err
	}
	file, err := root.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read the secret %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	value, err := io.ReadAll(io.LimitReader(file, maxFileValue+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read the secret %s: %w", path, err)
	}
	if len(value) > maxFileValue {
		return nil, fmt.Errorf("refusing the secret %s: it is larger than %d bytes", path, maxFileValue)
	}
	return value, nil
}

// Create returns ErrReadOnly: the directory is mounted read-only.
func (f *Files) Create(name string, _ []byte) error { return f.refuse(name) }

// Update returns ErrReadOnly: the directory is mounted read-only.
func (f *Files) Update(name string, _ []byte) error { return f.refuse(name) }

// Delete returns ErrReadOnly: the directory is mounted read-only.
func (f *Files) Delete(name string) error { return f.refuse(name) }

// List returns the regular files with valid names, sorted by name, each with
// its modification time and no provenance. Values are never read.
func (f *Files) List() ([]Secret, error) {
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return nil, fmt.Errorf("cannot read the secret directory %s: %w", f.dir, err)
	}
	var out []Secret
	for _, e := range entries {
		name := e.Name()
		if ValidName(name) != nil {
			continue
		}
		fi, err := os.Stat(filepath.Join(f.dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("cannot read the secret %s: %w", filepath.Join(f.dir, name), err)
		}
		if fi.Mode().IsRegular() {
			out = append(out, Secret{Name: name, Modified: fi.ModTime()})
		}
	}
	return out, nil
}

// Skipped returns the sorted names of the entries the store does not serve:
// names outside the secret-name grammar, and names inside it that are not
// regular files. Names starting with a dot are never listed.
func (f *Files) Skipped() []string {
	return append([]string(nil), f.skipped...)
}

// refuse is every write: the name is checked first, so a bad name reads as
// one, and then the write is refused.
func (f *Files) refuse(name string) error {
	if err := validStoreName(name); err != nil {
		return err
	}
	return ErrReadOnly
}
