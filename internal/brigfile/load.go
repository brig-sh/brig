package brigfile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// ErrNotFound is what Discover returns when the project has no Brigfile.
// A caller tells it apart from a Brigfile that exists and is broken.
var ErrNotFound = errors.New("no Brigfile found")

// Discover returns the path of the Brigfile for the project startDir is in.
//
// Inside Git, the project is the worktree: the nearest directory at or above
// startDir that holds a .git entry, a directory in a main checkout or a file
// in a linked worktree. Outside Git, the project is startDir alone. Only the
// project's own directory is looked in. A Brigfile in a directory between
// startDir and the root belongs to no one, and one above the root belongs to
// a different project, so neither is picked up.
//
// git itself is not run. Discover only asks whether a .git entry exists, so
// it reads nothing from one and works the same in a checkout git would
// refuse to open.
//
// startDir is resolved through its links first, so the walk goes up the
// directories it is really in, as git's does. The lookups here read no file:
// Load opens what Discover returns through a root, and checks it there.
func Discover(startDir string) (string, error) {
	root, err := Root(startDir)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, FileName)
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%w in %s", ErrNotFound, root)
		}
		return "", err
	}
	return path, nil
}

// Root returns the project root for startDir: the directory Discover looks
// in for a Brigfile, whether or not one is there. It is apart from Discover so
// that a caller which writes a Brigfile writes it where Discover will find it.
func Root(startDir string) (string, error) {
	abs, err := filepath.Abs(startDir)
	if err != nil {
		return "", err
	}
	start, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	for dir := start; ; {
		_, err := os.Lstat(filepath.Join(dir, ".git"))
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return start, nil
		}
		dir = parent
	}
}

// Loaded is a Brigfile with every alias resolved, and the bytes it was
// resolved from.
//
// The maps and slices here are the loader's own. Target returns copies that
// share nothing with them; a caller that wants to change a profile or a
// policy takes it from there.
type Loaded struct {
	// Project is the Brigfile as written.
	Project Project
	// Root is the project root: the directory that holds the Brigfile, with
	// its links resolved. Every file reference is relative to it.
	Root string
	// Path is the Brigfile, under Root.
	Path string
	// Raw is the Brigfile's bytes.
	Raw []byte
	// Files holds each file a reference names, keyed by the path the Brigfile
	// wrote. A file two aliases name is read once and held once.
	Files map[string][]byte
	// Profiles holds each profile alias, resolved.
	Profiles map[string]ResolvedProfile
	// Policies holds each policy alias, resolved.
	Policies map[string]ResolvedPolicy
}

// Load reads the Brigfile at path, every file it names, and resolves every
// alias in it. The directory that holds path is the project root, whether
// path came from Discover or was named on the command line.
//
// Every read goes through an os.Root opened on that directory. A Brigfile is
// written by whoever wrote the repository, so a path in it is theirs to
// choose, and so is any link in the checkout: os.Root refuses a link that
// leads out of the root, at the open, with no window between a check and
// the read for the link to change in. A link that stays inside the root is
// followed. Only a regular file is read, and at most MaxFileSize of it.
//
// Load returns nil and an error at the first problem. It writes nothing.
func Load(path string) (*Loaded, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	base := filepath.Base(abs)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	where := filepath.Join(dir, base)
	raw, err := readRegular(root, base)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	p, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	l := &Loaded{
		Project:  p,
		Root:     dir,
		Path:     where,
		Raw:      raw,
		Files:    map[string][]byte{},
		Profiles: map[string]ResolvedProfile{},
		Policies: map[string]ResolvedPolicy{},
	}
	if err := l.resolve(root); err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	return l, nil
}

// read returns the bytes of one referenced file, reading it the first time
// it is asked for.
func (l *Loaded) read(root *os.Root, rel string) ([]byte, error) {
	if blob, ok := l.Files[rel]; ok {
		return blob, nil
	}
	blob, err := readRegular(root, rel)
	if err != nil {
		return nil, err
	}
	l.Files[rel] = blob
	return blob, nil
}

// readRegular reads one file under root, and refuses anything that is not a
// regular file or is larger than MaxFileSize.
//
// The type is read from the open descriptor, not from a stat of the path
// before the open, so a file swapped between the two cannot pass. A FIFO is
// opened O_NONBLOCK and refused before the first read, which is where a read
// of one would wait for a writer that never comes. internal/wrap/rootio.go
// reads the workspace the same way, for the same reasons.
func readRegular(root *os.Root, rel string) ([]byte, error) {
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is a %s, not a regular file", rel, describeType(fi.Mode()))
	}
	if fi.Size() > MaxFileSize {
		return nil, fmt.Errorf("%s is %d bytes, more than the %d MiB limit", rel, fi.Size(), MaxFileSize>>20)
	}
	// Bounded again while reading, because the size above is only what the
	// file held when it was asked; it can grow before the read ends.
	blob, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(blob) > MaxFileSize {
		return nil, fmt.Errorf("%s is more than the %d MiB limit", rel, MaxFileSize>>20)
	}
	return blob, nil
}

// describeType names a file type in words; fs.FileMode renders it as
// "p---------".
func describeType(m fs.FileMode) string {
	switch {
	case m&fs.ModeNamedPipe != 0:
		return "named pipe (FIFO)"
	case m&fs.ModeSocket != 0:
		return "socket"
	case m&fs.ModeDevice != 0:
		return "device node"
	case m&fs.ModeDir != 0:
		return "directory"
	case m&fs.ModeSymlink != 0:
		return "symlink"
	}
	return m.Type().String()
}
