package memory

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unicode"
)

const (
	indexFile   = "MEMORY.md"
	projectFile = "project"
	suffix      = ".md"
	// frontmatterSlack is room in a file for the frontmatter around a
	// maximal body.
	frontmatterSlack = 2 * 1024
	// frontmatterMax is the longest frontmatter Brig writes: the markers,
	// and a name and a description at their caps. Index reads no further,
	// so listing a store never reads its bodies.
	frontmatterMax = len("---\nname: \ndescription: \n---\n\n") + MaxName + MaxDescription
)

// Files is a Store kept as one Markdown file per memory in a directory.
type Files struct {
	root *os.Root
	dir  string
	key  string
	mu   *sync.Mutex
}

var _ Store = (*Files)(nil)

// dirID identifies a directory by device and inode, not by the spelling of
// the path that reached it.
type dirID struct{ dev, ino uint64 }

// locks serialises writers to one store directory. Two Files on the same
// directory share a mutex, so a rebuild of MEMORY.md never interleaves with
// another Put. The key is the directory's device and inode: two spellings of
// one path would otherwise get two mutexes, let both writers past the
// MaxMemories check and lose index updates.
var locks sync.Map // dirID -> *sync.Mutex

func lockFor(fi fs.FileInfo) *sync.Mutex {
	var id dirID
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		id = dirID{uint64(st.Dev), uint64(st.Ino)}
	}
	mu, _ := locks.LoadOrStore(id, new(sync.Mutex))
	return mu.(*sync.Mutex)
}

// OpenFiles opens the store root/key, creating it 0700. It refuses a store
// directory that is a link, that another user owns, or that group or other
// can reach, because the guest influences what is written into it and a
// directory someone else controls could be pointed anywhere.
func OpenFiles(root, key string) (*Files, error) {
	return openStore(root, key, true)
}

// OpenExisting opens the store root/key as OpenFiles does, and returns
// ErrNotFound rather than create it. A registered store that is gone was
// removed, and recreating it empty would hand the agent a store that silently
// lost its memories.
func OpenExisting(root, key string) (*Files, error) {
	return openStore(root, key, false)
}

func openStore(root, key string, create bool) (*Files, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	if create {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return nil, err
		}
	}
	parent, err := os.OpenRoot(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: no store root %s", ErrNotFound, root)
	}
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	if create {
		if err := parent.Mkdir(key, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	dir := filepath.Join(root, key)
	fi, err := parent.Lstat(key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: no store %s", ErrNotFound, dir)
	}
	if err != nil {
		return nil, err
	}
	if err := checkStoreDir(fi, dir); err != nil {
		return nil, err
	}
	r, err := parent.OpenRoot(key)
	if err != nil {
		return nil, err
	}
	// Look again at what was opened: the directory could have been swapped
	// between the Lstat and the open.
	opened, err := r.Stat(".")
	if err == nil {
		err = checkStoreDir(opened, dir)
	}
	if err == nil && !os.SameFile(fi, opened) {
		err = fmt.Errorf("refusing %s: it changed while it was opened", dir)
	}
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	return &Files{root: r, dir: dir, key: key, mu: lockFor(opened)}, nil
}

// visibleRune reports whether r is a character a reader sees: a letter,
// mark, number, punctuation, symbol or space, and none of the ones in those
// classes that render as nothing. An allowlist, because the characters that
// hide text are many and Unicode adds more: controls and format characters
// (zero-width joiners and spaces, bidi overrides, tags) fall outside it.
// Marks are in, because Thai, Devanagari, Khmer and Hebrew with niqqud
// cannot be written without them. The marks and letters that show nothing
// are refused by name: the emoji and Mongolian variation selectors, the
// combining grapheme joiner, the Hangul fillers, the Khmer inherent vowels
// and the Braille blank.
func visibleRune(r rune) bool {
	switch r {
	case '\u115f', '\u1160', '\u3164', '\uffa0', // Hangul fillers
		'\u034f', '\u2800', '\u17b4', '\u17b5', '\u2028', '\u2029':
		return false
	}
	if (r >= '\ufe00' && r <= '\ufe0f') || (r >= '\U000e0100' && r <= '\U000e01ef') ||
		(r >= '\u180b' && r <= '\u180d') || r == '\u180f' {
		return false
	}
	return unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Zs)
}

func checkStoreDir(fi fs.FileInfo, dir string) error {
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("refusing %s: it is a symlink", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("refusing %s: it is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("refusing %s: it is owned by another user", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("refusing %s: its mode is %#o; run chmod 700 on it", dir, fi.Mode().Perm())
	}
	return nil
}

// Close releases the directory handle.
func (f *Files) Close() error { return f.root.Close() }

// SetProject records the project path a project store belongs to, so List
// can name it. Only the absolute folder the store's key was made from is
// recorded: a store listed under another folder would have a user remove
// the wrong one.
func (f *Files) SetProject(absPath string) error {
	if !strings.HasPrefix(f.key, projectPrefix) {
		return fmt.Errorf("%w: %s is a named store, which has no project", ErrInvalidName, f.key)
	}
	if !filepath.IsAbs(absPath) {
		return fmt.Errorf("%w: the project %q is not an absolute path", ErrInvalidName, absPath)
	}
	if ProjectKey(absPath) != f.key {
		return fmt.Errorf("%w: the project %s is not the folder of %s", ErrInvalidName, absPath, f.key)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writeFile(projectFile, []byte(filepath.Clean(absPath)+"\n"))
}

func (f *Files) Index(ctx context.Context) ([]Entry, error) {
	return f.entries(ctx)
}

func (f *Files) Get(ctx context.Context, name string) (Memory, error) {
	if err := ctx.Err(); err != nil {
		return Memory{}, err
	}
	if err := ValidName(name); err != nil {
		return Memory{}, err
	}
	return readMemory(f.root, name)
}

func (f *Files) Put(ctx context.Context, m Memory) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidName(m.Name); err != nil {
		return err
	}
	if len(m.Body) > MaxBody {
		return fmt.Errorf("%w: body is %d bytes, the limit is %d", ErrTooLarge, len(m.Body), MaxBody)
	}
	if len(m.Description) > MaxDescription {
		return fmt.Errorf("%w: description is %d bytes, the limit is %d", ErrTooLarge, len(m.Description), MaxDescription)
	}
	// One line, or a description could close the frontmatter and forge
	// fields after it.
	if strings.ContainsAny(m.Description, "\r\n") {
		return fmt.Errorf("%w: the description must be a single line", ErrInvalidName)
	}
	// Visible text, or a description could carry words the user reading
	// the instructions it lands in never sees.
	for _, r := range m.Description {
		if !visibleRune(r) {
			return fmt.Errorf("%w: the description holds %U, which is not visible text", ErrInvalidName, r)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	file := m.Name + suffix
	fi, err := f.root.Lstat(file)
	switch {
	case err == nil:
		// Brig makes no links in a store, and a rename would replace one
		// without touching its target. Refuse it, so the planted file is
		// noticed.
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("refusing %s: it is not a regular file", filepath.Join(f.dir, file))
		}
	case errors.Is(err, fs.ErrNotExist):
		n, err := f.count()
		if err != nil {
			return err
		}
		if n >= MaxMemories {
			return fmt.Errorf("%w: the limit is %d memories", ErrStoreFull, MaxMemories)
		}
	default:
		return err
	}
	if err := f.writeFile(file, encode(m)); err != nil {
		return err
	}
	return f.rebuildIndex()
}

func (f *Files) Delete(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidName(name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	file := name + suffix
	if _, err := f.root.Lstat(file); errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	// Remove unlinks the name, so a planted link is dropped, never followed.
	if err := f.root.Remove(file); err != nil {
		return err
	}
	return f.rebuildIndex()
}

// Search reads one memory at a time and drops it before the next, so a search
// holds one body in the broker, never the whole store.
func (f *Files) Search(ctx context.Context, query string) ([]Entry, error) {
	names, err := f.names()
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(query)
	var out []Entry
	for _, n := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m, err := readMemory(f.root, n)
		if err != nil {
			continue
		}
		if containsFold(m.Name, q) || containsFold(m.Description, q) || containsFold(m.Body, q) {
			// The name and description are slices of the whole file, and
			// would keep its body alive in the result.
			out = append(out, Entry{strings.Clone(m.Name), strings.Clone(m.Description)})
		}
	}
	return out, nil
}

// containsFold reports whether s holds the lower-cased query q, ignoring
// case.
func containsFold(s, q string) bool {
	return strings.Contains(strings.ToLower(s), q)
}

// names lists the files in the store that are named like a memory, sorted.
func (f *Files) names() ([]string, error) {
	d, err := f.root.Open(".")
	if err != nil {
		return nil, err
	}
	defer d.Close()
	ents, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), suffix)
		if !ok || ValidName(name) != nil || e.Type()&fs.ModeSymlink != 0 || e.IsDir() {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func (f *Files) count() (int, error) {
	n, err := f.names()
	return len(n), err
}

// entries reads the name and description of every memory that parses,
// sorted by name, from the frontmatter alone. A file that does not parse,
// such as a hand-written one, is skipped so one bad file does not hide the
// rest.
func (f *Files) entries(ctx context.Context) ([]Entry, error) {
	names, err := f.names()
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, n := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e, err := readEntry(f.root, n); err == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

// rebuildIndex rewrites MEMORY.md from the memory files, so a hand edit or a
// deleted index heals on the next write. Callers hold f.mu.
func (f *Files) rebuildIndex() error {
	ents, err := f.entries(context.Background())
	if err != nil {
		return err
	}
	var b bytes.Buffer
	for _, e := range ents {
		fmt.Fprintf(&b, "- %s: %s\n", e.Name, e.Description)
	}
	return f.writeFile(indexFile, b.Bytes())
}

// writeFile writes name through a temp file in the store and a rename, so a
// reader sees the old file or the new one and never half of one. Callers hold
// f.mu.
func (f *Files) writeFile(name string, data []byte) error {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := ".tmp-" + hex.EncodeToString(rnd[:])
	w, err := f.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = f.root.Rename(tmp, name)
	}
	if err != nil {
		_ = f.root.Remove(tmp)
	}
	return err
}

func encode(m Memory) []byte {
	return []byte("---\nname: " + m.Name + "\ndescription: " + m.Description + "\n---\n\n" + m.Body)
}

// decode parses exactly the form encode writes.
func decode(b []byte) (Memory, bool) {
	s, ok := strings.CutPrefix(string(b), "---\nname: ")
	if !ok {
		return Memory{}, false
	}
	name, s, ok := strings.Cut(s, "\ndescription: ")
	if !ok {
		return Memory{}, false
	}
	desc, body, ok := strings.Cut(s, "\n---\n\n")
	if !ok || strings.Contains(name, "\n") || strings.Contains(desc, "\n") {
		return Memory{}, false
	}
	return Memory{Name: name, Description: desc, Body: body}, true
}

// readLimited reads a regular file inside r, refusing a link and anything
// bigger than max bytes.
func readLimited(r *os.Root, name string, max int64) ([]byte, error) {
	return readHead(r, name, max, max)
}

// readHead reads at most n bytes of a regular file inside r, refusing a
// link and a file bigger than max bytes.
func readHead(r *os.Root, name string, n, max int64) ([]byte, error) {
	fi, err := r.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing %s: it is not a regular file", name)
	}
	fh, err := r.Open(name)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	opened, err := fh.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(fi, opened) {
		return nil, fmt.Errorf("refusing %s: it changed while it was opened", name)
	}
	if opened.Size() > max {
		return nil, fmt.Errorf("%w: %s is over %d bytes", ErrTooLarge, name, max)
	}
	b, err := io.ReadAll(io.LimitReader(fh, min(n, max+1)))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%w: %s is over %d bytes", ErrTooLarge, name, max)
	}
	return b, nil
}

// readEntry reads the name and description of a memory from its
// frontmatter, and leaves the body unread. A file Get would refuse as too
// large is refused here too, so the index lists nothing memory_read cannot
// read.
func readEntry(r *os.Root, name string) (Entry, error) {
	b, err := readHead(r, name+suffix, int64(frontmatterMax), MaxBody+frontmatterSlack)
	if err != nil {
		return Entry{}, err
	}
	// decode needs the end of the frontmatter, so the body is whatever of
	// it fit in the bytes read.
	m, ok := decode(b)
	if !ok || m.Name != name {
		return Entry{}, fmt.Errorf("%s%s is not in the form brig writes", name, suffix)
	}
	return Entry{m.Name, m.Description}, nil
}

func readMemory(r *os.Root, name string) (Memory, error) {
	b, err := readLimited(r, name+suffix, MaxBody+frontmatterSlack)
	if errors.Is(err, fs.ErrNotExist) {
		return Memory{}, ErrNotFound
	}
	if err != nil {
		return Memory{}, err
	}
	m, ok := decode(b)
	if !ok || m.Name != name {
		return Memory{}, fmt.Errorf("%s%s is not in the form brig writes", name, suffix)
	}
	return m, nil
}

// countParsed counts the memories in a store root, for List.
func countParsed(r *os.Root) int {
	d, err := r.Open(".")
	if err != nil {
		return 0
	}
	ents, _ := d.ReadDir(-1)
	_ = d.Close()
	n := 0
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), suffix)
		if !ok || ValidName(name) != nil {
			continue
		}
		if _, err := readMemory(r, name); err == nil {
			n++
		}
	}
	return n
}
