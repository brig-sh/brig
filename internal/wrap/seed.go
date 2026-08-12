package wrap

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// seedHostConfig copies the user's own agent configuration into the workspace,
// entry by entry, so an opted-in sandbox starts with the skills and plugins
// they already have.
//
// Entry by entry rather than directory by directory, for two reasons. It tops
// up: a skill added on the host after this workspace was created appears on
// the next run, without a separate sync step. And it never clobbers: anything
// the sandbox already has under that name is left exactly as it is, so a
// plugin installed inside the guest, or a skill edited there, survives.
//
// The copy is one-way by construction. Nothing here reads back from the
// workspace into ~/.claude, so whatever happens inside the sandbox cannot
// reach the host originals.
//
// Freshly copied entries then have their absolute host paths repointed at the
// guest, because a copy alone is not enough: agent config records where its
// own pieces live. See rewriteRoot.
func (c *Config) seedHostConfig() error {
	for _, s := range c.HostSeed {
		dst := filepath.Join(c.Workspace, s.Rel)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return fmt.Errorf("cannot create %s: %w", dst, err)
		}
		entries, err := os.ReadDir(s.Host)
		if err != nil {
			// It was there when the config resolved. Gone now is not worth
			// failing a run over.
			continue
		}
		copied := 0
		for _, e := range entries {
			target := filepath.Join(dst, e.Name())
			if _, err := os.Lstat(target); err == nil {
				continue
			}
			if err := copyTree(filepath.Join(s.Host, e.Name()), target); err != nil {
				return fmt.Errorf("cannot seed %s: %w", filepath.Join(s.Rel, e.Name()), err)
			}
			if err := rewriteRoot(target, s.HostRoot, s.GuestRoot); err != nil {
				return fmt.Errorf("cannot seed %s: %w", filepath.Join(s.Rel, e.Name()), err)
			}
			copied++
		}
		if copied > 0 {
			c.sayf("seeded %d item(s) into %s from %s", copied, s.Rel, s.Host)
		}
	}
	return nil
}

// copyTree copies src to dst recursively. dst must not exist: callers check
// that first, and creating it here exclusively keeps a race from overwriting
// something the sandbox put there in the meantime.
//
// Symlinks are recreated as symlinks rather than followed. Following them
// would copy a target from outside the seeded directory into the workspace,
// which is more than the user asked for; recreating means the guest resolves
// the link in its own namespace, and a link pointing at a host path simply
// dangles there, visibly.
func copyTree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case info.IsDir():
		if err := os.Mkdir(dst, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		// Directories are created before their contents, so a read-only mode
		// has to be applied afterwards or the contents cannot be written.
		return os.Chmod(dst, info.Mode().Perm())
	case info.Mode().IsRegular():
		return copyFile(src, dst, info.Mode().Perm())
	default:
		// Sockets, devices and fifos: nothing an agent configuration needs,
		// and nothing worth carrying into a guest.
		return nil
	}
}

// maxRewrite caps the JSON we are willing to read into memory to repoint. The
// manifests this exists for are a few KB; anything far larger is not one.
const maxRewrite = 8 << 20

// rewriteRoot repoints absolute host paths in seeded JSON at the guest.
//
// Only .json files, and never inside a .git directory. That is deliberately
// narrow: the seeded tree contains git checkouts, and a blind search and
// replace across it would happily corrupt a pack file that merely happens to
// contain the same bytes. The manifests that actually carry paths --
// installed_plugins.json, known_marketplaces.json -- are plain JSON sitting at
// the top of the tree.
//
// The rewrite is a prefix swap and nothing more, which is exact here because
// the copy mirrors the host layout: everything under HostRoot lands under
// GuestRoot at the same relative path.
func rewriteRoot(root, from, to string) error {
	if from == "" || to == "" || from == to {
		return nil
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || filepath.Ext(p) != ".json" {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxRewrite {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if !bytes.Contains(b, []byte(from)) {
			return nil
		}
		out := bytes.ReplaceAll(b, []byte(from), []byte(to))
		mode := info.Mode().Perm()
		if mode&0o200 == 0 {
			// Copied from a read-only original; make it writable for the
			// rewrite and put the mode back afterwards.
			if err := os.Chmod(p, mode|0o200); err != nil {
				return err
			}
			defer os.Chmod(p, mode)
		}
		return os.WriteFile(p, out, mode)
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
