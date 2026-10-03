package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Each sandbox's last boot outcome, image and digest, in boots.json.
// brig doctor bundle includes a console log only when the last boot failed.
//
// Digest is empty when verification was off or the runtime cannot pin one.
// A file of its own because older releases rewrite sessions.json without
// fields they don't know. Keyed by sandbox name, flock'd for
// read-modify-write, dropped with the sandbox.

// BootOK and BootFailed are the two outcomes. A boot that errors before the
// readiness wait records nothing.
const (
	BootOK     = "ok"
	BootFailed = "failed"
)

// BootRecord is one sandbox's last boot.
type BootRecord struct {
	Result string    `json:"result"`
	At     time.Time `json:"at"`
	Image  string    `json:"image,omitempty"`
	Digest string    `json:"digest,omitempty"`
}

func bootsPath() (string, error) {
	dir, err := gatewayDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "boots.json"), nil
}

// readBoots returns every sandbox's record. A missing file is an empty map;
// an unparsable one is an error, so a write cannot drop other records.
func readBoots(path string) (map[string]BootRecord, error) {
	all := map[string]BootRecord{}
	blob, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return all, nil
	}
	if err != nil {
		return nil, fmt.Errorf("could not read how these sandboxes last booted (%s): %w", path, err)
	}
	if err := json.Unmarshal(blob, &all); err != nil || all == nil {
		return nil, fmt.Errorf("%s is not readable as a boot record. Move it aside to "+
			"start over; each sandbox is recorded again on its next boot", path)
	}
	return all, nil
}

// writeBoots replaces the file through a temporary and a rename.
func writeBoots(path string, all map[string]BootRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	blob, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".boots-*")
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

// LastBoot returns this sandbox's last boot, or false when none is recorded.
func LastBoot(name string) (BootRecord, bool, error) {
	path, err := bootsPath()
	if err != nil {
		return BootRecord{}, false, err
	}
	all, err := readBoots(path)
	if err != nil {
		return BootRecord{}, false, err
	}
	rec, ok := all[name]
	return rec, ok, nil
}

// RecordBoot replaces this sandbox's record. At is stored in UTC.
func RecordBoot(name string, rec BootRecord) error {
	path, err := bootsPath()
	if err != nil {
		return err
	}
	unlock, err := flock(path)
	if err != nil {
		return err
	}
	defer unlock()
	all, err := readBoots(path)
	if err != nil {
		return err
	}
	rec.At = rec.At.UTC()
	all[name] = rec
	return writeBoots(path, all)
}

// ForgetBoot drops the record of a removed sandbox. Errors are ignored.
func ForgetBoot(name string) {
	path, err := bootsPath()
	if err != nil {
		return
	}
	unlock, err := flock(path)
	if err != nil {
		return
	}
	defer unlock()
	all, err := readBoots(path)
	if err != nil {
		return
	}
	if _, ok := all[name]; !ok {
		return
	}
	delete(all, name)
	_ = writeBoots(path, all)
}

// PruneBoots drops every record whose sandbox is not in keep. Errors are
// ignored.
func PruneBoots(keep []string) {
	path, err := bootsPath()
	if err != nil {
		return
	}
	unlock, err := flock(path)
	if err != nil {
		return
	}
	defer unlock()
	all, err := readBoots(path)
	if err != nil || len(all) == 0 {
		return
	}
	have := make(map[string]bool, len(keep))
	for _, n := range keep {
		have[n] = true
	}
	dropped := false
	for n := range all {
		if !have[n] {
			delete(all, n)
			dropped = true
		}
	}
	if dropped {
		_ = writeBoots(path, all)
	}
}
