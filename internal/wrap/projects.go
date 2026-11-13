package wrap

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// The project index: what brig remembers about each session of a Brigfile
// target, keyed by its sandbox name.
//
// A file of its own and not entries in sessions.json, for two reasons. An
// older release rewrites sessions.json with the fields it knows, so a field
// added there for a target is lost the first time one runs. And `brig ls`
// prunes sessions.json against the runtime's list, which on hull's plain ps
// leaves out a stopped sandbox. A project session owns a guest home, and the
// record of which home it owns must not go because a listing came back short.
// Only `brig rm`, of the session or of every sandbox, or a runtime that
// answers that the sandbox does not exist, removes an entry.
//
// The sandbox name is recomputable from the root, the target and the session
// name, so `brig stop --target` and `brig rm --target` find a session after
// its Brigfile is gone. The entry is what confirms the computed name is a
// session brig started, and it names the home.
//
// A file that is there and cannot be read as this version is never written
// over. sessions.json reads such a file as empty and the next write replaces
// it, which costs a restart there. Here it would cost the record of which
// homes belong to which project, so the writers refuse and say which file.
//
// Like sessions.json there is no lock. Two brig processes that write at the
// same moment can lose one entry, and `brig ls` then lists that session as
// unknown until its next run records it again.

// projectIndexName is the file, inside stateDir.
const projectIndexName = "projects.json"

// projectIndexVersion is the version this release writes and reads. A file of
// another version is one this release does not understand, and it neither
// reads its entries nor writes over it.
const projectIndexVersion = 1

// ProjectSession is what the project index records about one session of a
// Brigfile target.
type ProjectSession struct {
	// Sandbox is the key the entry is filed under. It is not repeated in the
	// file.
	Sandbox string `json:"-"`
	// Root is the project root: the directory that held the Brigfile, with its
	// links resolved.
	Root string `json:"root"`
	// Target is the target's name, and Session the session's, or "" for the
	// target's default session.
	Target  string `json:"target"`
	Session string `json:"session,omitempty"`
	// Home is the guest home, always <state>/homes/<sandbox>.
	Home string `json:"home"`
	// ConfigDigest is the digest of the configuration the sandbox was booted
	// with. See (*Config).ConfigDigest.
	ConfigDigest string `json:"configDigest,omitempty"`
	// Brigfile is the file the session was last started from. It is for a
	// reader of `brig ls`; nothing finds the session through it.
	Brigfile string `json:"brigfile,omitempty"`
}

// projectIndexFile is the file as written.
type projectIndexFile struct {
	Version  int                       `json:"version"`
	Sessions map[string]ProjectSession `json:"sessions"`
}

// errProjectIndex is wrapped by every refusal to write the project index
// because the file there cannot be read.
var errProjectIndex = errors.New("brig cannot read its project index")

// IsProjectSandbox reports whether name is the sandbox of a Brigfile target.
func IsProjectSandbox(name string) bool { return strings.HasPrefix(name, projectPrefix) }

// readProjects returns the sessions the index holds, and an error when a file
// is there that this release cannot read. A missing file is an empty index.
//
// An entry that is not one brig would have written is left out: a key that is
// not a project sandbox, or a home that is not that sandbox's own directory
// under homes/. rm deletes the home an entry names, so an entry naming any
// other directory is never acted on, whoever wrote it.
func readProjects() (map[string]ProjectSession, error) {
	blob, err := readStateFile(projectIndexName)
	if err != nil {
		return map[string]ProjectSession{}, fmt.Errorf("%w: %w", errProjectIndex, err)
	}
	if blob == nil {
		return map[string]ProjectSession{}, nil
	}
	var f projectIndexFile
	if err := json.Unmarshal(blob, &f); err != nil {
		return map[string]ProjectSession{}, fmt.Errorf("%w: %w", errProjectIndex, err)
	}
	if f.Version != projectIndexVersion {
		return map[string]ProjectSession{}, fmt.Errorf("%w: it is version %d, and this release reads version %d",
			errProjectIndex, f.Version, projectIndexVersion)
	}
	out := make(map[string]ProjectSession, len(f.Sessions))
	for name, ps := range f.Sessions {
		if !IsProjectSandbox(name) {
			continue
		}
		home, err := defaultWorkspace(name)
		if err != nil || ps.Home != home {
			continue
		}
		ps.Sandbox = name
		out[name] = ps
	}
	return out, nil
}

// projectIndexError names the file a writer refused to replace.
func projectIndexError(err error) error {
	path, perr := indexPath(projectIndexName)
	if perr != nil {
		return err
	}
	return fmt.Errorf("%w. brig leaves %s as it is; move it aside to start a new one", err, path)
}

// writeProjects replaces the index with sessions.
func writeProjects(sessions map[string]ProjectSession) error {
	return writeStateFile(projectIndexName, projectIndexFile{Version: projectIndexVersion, Sessions: sessions})
}

// RecordProject files ps under ps.Sandbox, replacing what was there.
//
// It refuses an entry brig would not read back: a sandbox that is not a
// project sandbox, or a home that is not that sandbox's own. It refuses a
// project whose root and brig's state directory contain one another. See
// CheckProjectState.
func RecordProject(ps ProjectSession) error {
	if !IsProjectSandbox(ps.Sandbox) {
		return fmt.Errorf("%s is not the sandbox of a Brigfile target", ps.Sandbox)
	}
	home, err := defaultWorkspace(ps.Sandbox)
	if err != nil {
		return err
	}
	if ps.Home != home {
		return fmt.Errorf("the guest home of %s is %s, not %s", ps.Sandbox, home, ps.Home)
	}
	if err := CheckProjectState(ps.Root); err != nil {
		return err
	}
	sessions, err := readProjects()
	if err != nil {
		return projectIndexError(err)
	}
	sessions[ps.Sandbox] = ps
	return writeProjects(sessions)
}

// LookupProject returns the entry filed under sandbox, and false when there is
// none or the index cannot be read.
func LookupProject(sandbox string) (ProjectSession, bool) {
	sessions, _ := readProjects()
	ps, ok := sessions[sandbox]
	return ps, ok
}

// ProjectSessions returns every entry, sorted by sandbox name. An index that
// cannot be read lists nothing.
func ProjectSessions() []ProjectSession {
	sessions, _ := readProjects()
	out := make([]ProjectSession, 0, len(sessions))
	for _, ps := range sessions {
		out = append(out, ps)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sandbox < out[j].Sandbox })
	return out
}

// ForgetProject drops the entry filed under sandbox. It reports whether there
// was one, and an error when the index cannot be read or rewritten.
//
// Call it only once the sandbox is gone: removed by brig, or reported missing
// by the runtime. The entry is what keeps the session's home from being taken
// for an orphan.
func ForgetProject(sandbox string) (bool, error) {
	sessions, err := readProjects()
	if err != nil {
		return false, projectIndexError(err)
	}
	if _, ok := sessions[sandbox]; !ok {
		return false, nil
	}
	delete(sessions, sandbox)
	return true, writeProjects(sessions)
}

// CheckProjectState refuses a project root and a state directory where one
// holds the other.
//
// The project is mounted read-write in the guest. With brig's state inside
// it, the guest could edit the index that says which home belongs to which
// session, plant a session, or edit another session's home. With the project
// inside the state directory, the guest's project is brig's own bookkeeping.
// Both paths are compared with their links resolved, and a part that does not
// exist yet is compared as written below the part that does.
func CheckProjectState(root string) error {
	state, err := stateDir()
	if err != nil {
		return err
	}
	s, r := resolvedPath(state), resolvedPath(root)
	switch {
	case pathWithin(s, r):
		return fmt.Errorf("brig's state directory %s is inside the project %s, which a sandbox of "+
			"the project can write. Set BRIG_STATE_DIR to a directory outside the project", s, r)
	case pathWithin(r, s):
		return fmt.Errorf("the project %s is inside brig's state directory %s. Run a target "+
			"of a project outside it", r, s)
	}
	return nil
}

// pathWithin reports whether path is dir or below it.
func pathWithin(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// resolvedPath is p made absolute with the links of its longest existing
// prefix resolved, and the rest appended as written.
func resolvedPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	dir, rest := abs, ""
	for {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}
