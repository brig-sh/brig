package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// The network each sandbox was booted on, kept on disk.
//
// A sandbox's network is fixed when it boots, so a later command that names no
// network has to be told which one the sandbox has rather than resolving the
// default and reading the difference as a change. The publications a sandbox
// offers ride on that network, so the record sits beside published.json and
// follows the same rules: keyed by sandbox name, locked for a read-modify-write,
// and dropped only when the sandbox itself is removed.
//
// A file of its own rather than a field on the session index. A release that
// does not know the field reads the index, rewrites it without the field, and
// the next command resolves the default network again. No older release reads
// or writes this file.
//
// The values are the runtime's words: shared, isolated or none.

// posturePath is the file, and flock on it guards a read-modify-write.
func posturePath() (string, error) {
	dir, err := gatewayDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "networks.json"), nil
}

// readPostures is every sandbox's recorded network, keyed by sandbox name.
//
// A missing file is an empty map. Every other failure is an error, for the
// reason readPublished gives: a read taken as empty and then written back drops
// every other sandbox's record.
func readPostures(path string) (map[string]string, error) {
	all := map[string]string{}
	blob, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return all, nil
	}
	if err != nil {
		return nil, fmt.Errorf("could not read which network these sandboxes were booted on (%s): %w",
			path, err)
	}
	if err := json.Unmarshal(blob, &all); err != nil || all == nil {
		return nil, fmt.Errorf("%s is not readable as a network record. Move it aside to "+
			"start over; each sandbox is recorded again on its next boot", path)
	}
	return all, nil
}

// writePostures replaces the file through a temporary and a rename.
func writePostures(path string, all map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	blob, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".networks-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(blob, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// BootedNet is the network this sandbox was last booted on, or "" when none is
// recorded: a sandbox booted by an older release, or one never booted.
func BootedNet(name string) (string, error) {
	path, err := posturePath()
	if err != nil {
		return "", err
	}
	all, err := readPostures(path)
	if err != nil {
		return "", err
	}
	return all[name], nil
}

// RecordBootedNet records the network this sandbox has just booted on.
//
// Called after a boot and nowhere else. A sandbox that is already running was
// not booted by this command, and recording what the command asked for would
// write down a network the runtime was never told about.
func RecordBootedNet(name, net string) error {
	path, err := posturePath()
	if err != nil {
		return err
	}
	unlock, err := flock(path)
	if err != nil {
		return err
	}
	defer unlock()
	all, err := readPostures(path)
	if err != nil {
		return err
	}
	if all[name] == net {
		return nil
	}
	all[name] = net
	return writePostures(path, all)
}

// ForgetBootedNet drops the record of a removed sandbox, so the next sandbox to
// take the name starts from the ordinary resolution.
//
// Errors are dropped, the way ForgetPublications drops its own: the removal
// worked, and a bookkeeping file is no reason to report that it did not.
func ForgetBootedNet(name string) {
	path, err := posturePath()
	if err != nil {
		return
	}
	unlock, err := flock(path)
	if err != nil {
		return
	}
	defer unlock()
	all, err := readPostures(path)
	if err != nil {
		return
	}
	if _, ok := all[name]; !ok {
		return
	}
	delete(all, name)
	_ = writePostures(path, all)
}

// PruneBootedNets drops the record of every sandbox not in keep.
//
// `brig ls` and `brig rm --all` call it with what the runtime still holds. A
// sandbox removed by an older release, or with the runtime's own CLI, leaves
// its record behind, and the next sandbox to take the name would inherit it.
func PruneBootedNets(keep []string) {
	path, err := posturePath()
	if err != nil {
		return
	}
	unlock, err := flock(path)
	if err != nil {
		return
	}
	defer unlock()
	all, err := readPostures(path)
	if err != nil || len(all) == 0 {
		return
	}
	live := map[string]bool{}
	for _, name := range keep {
		live[name] = true
	}
	pruned := false
	for name := range all {
		if !live[name] {
			delete(all, name)
			pruned = true
		}
	}
	if pruned {
		_ = writePostures(path, all)
	}
}
