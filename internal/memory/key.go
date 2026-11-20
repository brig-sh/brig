package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/brig-sh/brig/internal/memory/memkey"
)

const projectPrefix = memkey.ProjectPrefix

// ValidName checks a memory name. The stem of the index file is refused in
// any case: on a case-insensitive host, memory.md and MEMORY.md are one
// directory entry, and the index rebuild would replace the memory.
func ValidName(name string) error {
	if err := memkey.ValidChars(name); err != nil {
		return err
	}
	if strings.EqualFold(name+suffix, indexFile) {
		return fmt.Errorf("%w: %q is reserved for the index", ErrInvalidName, name)
	}
	return nil
}

// ValidStoreName checks the name of a store a caller picks; the rules are
// in memkey so that profile loading need not link this package.
func ValidStoreName(name string) error { return memkey.ValidStoreName(name) }

// Project is the setting that asks for the store of the project folder.
const Project = memkey.Project

// ParseSetting reads a memory setting; see memkey.ParseSetting.
func ParseSetting(v string) (string, error) { return memkey.ParseSetting(v) }

// ProjectKey is the store key for the project at absPath; see
// memkey.ProjectKey.
func ProjectKey(absPath string) string { return memkey.ProjectKey(absPath) }

// validKey accepts what a store directory under the root may be called: a
// named store or a ProjectKey. Anything else, in particular a key with a
// separator, is refused before any path is built from it.
func validKey(key string) error {
	if rest, ok := strings.CutPrefix(key, projectPrefix); ok {
		if len(rest) == 16 && strings.Trim(rest, "0123456789abcdef") == "" {
			return nil
		}
		return fmt.Errorf("%w: %q is not a project key", ErrInvalidName, key)
	}
	return ValidStoreName(key)
}

// StoreInfo describes one store under the root.
type StoreInfo struct {
	Key      string
	Project  string // the project's path; empty for a named store
	Memories int
}

// List reports the stores under root, sorted by key. A missing root has none.
// A directory that is a link, or whose name is not a store key, is not a
// store.
func List(root string) ([]StoreInfo, error) {
	r, err := os.OpenRoot(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer r.Close()
	d, err := r.Open(".")
	if err != nil {
		return nil, err
	}
	ents, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		return nil, err
	}
	var out []StoreInfo
	for _, e := range ents {
		if !e.IsDir() || validKey(e.Name()) != nil {
			continue
		}
		sub, err := r.OpenRoot(e.Name())
		if err != nil {
			continue
		}
		info := StoreInfo{Key: e.Name()}
		if b, err := readLimited(sub, "project", 4096); err == nil {
			info.Project = strings.TrimSpace(string(b))
		}
		info.Memories = countParsed(sub)
		_ = sub.Close()
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Remove deletes the store at root/key and everything in it.
func Remove(root, key string) error {
	if err := validKey(key); err != nil {
		return err
	}
	r, err := os.OpenRoot(root)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	defer r.Close()
	fi, err := r.Lstat(key)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// Brig makes no link here, so refuse one rather than guess.
	if !fi.IsDir() {
		return fmt.Errorf("refusing to remove %s: it is not a directory", filepath.Join(root, key))
	}
	mu := lockFor(fi)
	mu.Lock()
	defer mu.Unlock()
	return r.RemoveAll(key)
}
