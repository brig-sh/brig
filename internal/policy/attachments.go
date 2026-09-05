package policy

import (
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

// Attachments is the record `brig policy attach` writes and `brig policy
// detach` unwrites: which policies bind every run of a profile, and which
// bind one session by name.
//
// Sessions is keyed by profile first, then by session name: a session name
// is only unique within its profile -- `claude-code -n work` and
// `codex -n work` are different sandboxes (brig-claude-code-work and
// brig-codex-work, see internal/wrap/config.go), so a flat map keyed on
// the session name alone would let one profile's session bind, or a
// detach unbind, the other's.
type Attachments struct {
	Profiles map[string][]string            `json:"profiles,omitempty"`
	Sessions map[string]map[string][]string `json:"sessions,omitempty"`
}

// attachmentsBasename is the one file that holds every attachment, in the
// same directory LoadAll scans for policies. isPolicyFile would otherwise
// read it as one and report it broken -- see reservedBasenames in load.go.
const attachmentsBasename = "attachments.yaml"

func attachmentsPath(dir string) string {
	return filepath.Join(dir, attachmentsBasename)
}

// LoadAttachments reads the attachment record from dir. A dir with no
// attachments.yaml yet is not an error: it just means nothing has been
// attached.
//
// A record that will not parse is an error rather than an empty read, on
// purpose and unlike readIndex in internal/wrap: that one caches which
// sandbox holds which workspace, where a corrupt file costs a restart, but
// this one is what every refusal in the policy commands is checked
// against. Reading a broken record as "nothing is bound" would report a
// binding that exists as absent, which is the wrong direction to fail for
// a record whose whole job is to say what is constrained.
//
// The path is named because the parse error on its own does not name it,
// and a caller who cannot tell which file to open has nothing to act on.
func LoadAttachments(dir string) (Attachments, error) {
	path := attachmentsPath(dir)
	blob, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Attachments{}, nil
		}
		// os.ReadFile's error is a *PathError and names path already.
		return Attachments{}, err
	}
	var a Attachments
	if err := yaml.UnmarshalStrict(blob, &a); err != nil {
		return Attachments{}, fmt.Errorf("%s is not a valid attachment record: %w", path, err)
	}
	return a, nil
}

// Save writes the attachment record to dir, creating dir if necessary.
//
// Written through a temp file and renamed into place, the same convention
// editPolicy uses for a policy file and for the same reason: a direct write
// truncates attachments.yaml before the new bytes land, so a full disk or a
// kill between those two steps would otherwise leave every attachment
// unreadable, not just the one this Save was recording.
func (a Attachments) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	blob, err := yaml.Marshal(a)
	if err != nil {
		return err
	}
	path := attachmentsPath(dir)
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, ".brig-attachments-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// AttachToProfile records that name binds every run of profile, unless it
// already does.
func (a *Attachments) AttachToProfile(name, profile string) {
	if a.Profiles == nil {
		a.Profiles = map[string][]string{}
	}
	a.Profiles[profile] = addOnce(a.Profiles[profile], name)
}

// DetachFromProfile removes name from the list bound to profile, if it is
// there, and reports whether it was: a caller that wants to say what it
// actually did, or skip a write that would change nothing, needs to know
// that this was a no-op rather than infer it.
func (a *Attachments) DetachFromProfile(name, profile string) bool {
	if a.Profiles == nil {
		return false
	}
	names, removed := remove(a.Profiles[profile], name)
	a.Profiles[profile] = names
	if len(a.Profiles[profile]) == 0 {
		delete(a.Profiles, profile)
	}
	return removed
}

// AttachToSession records that name binds the session called session,
// under profile, unless it already does.
func (a *Attachments) AttachToSession(name, profile, session string) {
	if a.Sessions == nil {
		a.Sessions = map[string]map[string][]string{}
	}
	if a.Sessions[profile] == nil {
		a.Sessions[profile] = map[string][]string{}
	}
	a.Sessions[profile][session] = addOnce(a.Sessions[profile][session], name)
}

// DetachFromSession removes name from the list bound to session under
// profile, if it is there, and reports whether it was.
func (a *Attachments) DetachFromSession(name, profile, session string) bool {
	if a.Sessions[profile] == nil {
		return false
	}
	names, removed := remove(a.Sessions[profile][session], name)
	a.Sessions[profile][session] = names
	if len(a.Sessions[profile][session]) == 0 {
		delete(a.Sessions[profile], session)
	}
	if len(a.Sessions[profile]) == 0 {
		delete(a.Sessions, profile)
	}
	return removed
}

func addOnce(names []string, name string) []string {
	for _, n := range names {
		if n == name {
			return names
		}
	}
	return append(names, name)
}

// remove filters in place, reusing names' own backing array (safe here
// because every call site reassigns the result straight back into the map
// entry names came from, with nothing else holding the old slice), and
// reports whether name was actually there to remove.
func remove(names []string, name string) ([]string, bool) {
	out := names[:0]
	removed := false
	for _, n := range names {
		if n == name {
			removed = true
			continue
		}
		out = append(out, n)
	}
	return out, removed
}
