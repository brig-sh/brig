package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The seam a teleport is built on: stop a sandbox's guest where it stands,
// write it down, and bring it back from what was written. No brig verb
// pauses, checkpoints or restores yet; brig info and a run that reuses a
// running sandbox read the migration record. The rest is here so the verb
// that moves a session is written against the runtime's answers and not
// against hull's command line.
//
// Each is optional, the way StorageInspector is: a runtime that cannot hold a
// teleportable session has nothing to checkpoint, and a caller asks for the
// interface before it relies on one.

// CheckpointContinue and CheckpointFence are what a checkpoint leaves the
// guest doing. Continue resumes it on this host once the bundle is written.
// Fence keeps it stopped for good in this process: it is the half of a move
// that makes "at most one host runs this guest" true, because the source can
// never run again after the destination may have started.
const (
	CheckpointContinue = "continue"
	CheckpointFence    = "fence"
)

// MigrationFenced and MigrationRestoring are the two states of hull's record
// in which a sandbox's VMM can be up with a guest that is not running: a
// checkpoint fenced it, or a restore has not finished, or failed before the
// guest ran. A caller that would exec into a running sandbox asks for these.
const (
	MigrationFenced    = "fenced"
	MigrationRestoring = "restoring"
)

// CheckpointSpec is a request to checkpoint a running sandbox.
type CheckpointSpec struct {
	// Dir is where the bundle is written, as an absolute path. Empty leaves
	// the place to the runtime, which keeps it with the sandbox.
	Dir string
	// Then is CheckpointContinue or CheckpointFence. Empty is the runtime's
	// own default, which on hull is continue.
	Then string
	// Timeout bounds how long the runtime waits for the bundle. Zero is its
	// own default.
	Timeout time.Duration
}

// CheckpointResult is what a checkpoint left behind, read back from the
// runtime's record of the sandbox once the checkpoint returned, so a caller
// acts on what the runtime recorded rather than on what it asked for.
type CheckpointResult struct {
	// Migration is the runtime's record after the checkpoint: the bundle
	// directory, the id it gave this checkpoint, and the state it left the
	// sandbox in. It is the record only when RecordErr is nil and State is
	// set. With no Dir it is the only place the bundle's location exists, so
	// a caller that finds it unread must not act on an empty BundleDir.
	Migration Migration
	// RecordErr is why the record could not be read back after a checkpoint
	// that worked. It is not the Checkpoint's error: the bundle is written
	// and, under CheckpointFence, the guest is fenced, and a caller that
	// took a failed read for a failed checkpoint could resume or restore a
	// source a destination may already be running. Read the record again
	// with Migration rather than checkpoint again.
	RecordErr error
	// Summary is the runtime's own line about the checkpoint, for a person.
	Summary string
}

// RestoreSpec is a request to bring a stopped sandbox back from its last
// checkpoint.
type RestoreSpec struct {
	// Paused leaves the restored guest stopped until Resume. A move restores
	// paused, so the destination is ready before the source is fenced and
	// runs only after.
	Paused bool
	// Timeout bounds how long the runtime waits for the restored VM. Zero is
	// its own default.
	Timeout time.Duration
}

// Migration is where a sandbox stands in a checkpoint, restore or move, as
// the runtime records it on the host. The zero value is a sandbox with no
// such record.
type Migration struct {
	// State is the runtime's word for it. hull's are checkpointed, fenced,
	// restoring, restored-paused and restored.
	State string
	// BundleDir is the checkpoint the state refers to.
	BundleDir string
	// TxID names that checkpoint.
	TxID string
}

// Pauser stops and restarts a running sandbox's guest in place: its vCPUs and
// its device I/O, with its memory held. Nothing is written.
type Pauser interface {
	Pause(name string) error
	Resume(name string) error
}

// Checkpointer writes a running sandbox's whole state, memory and devices,
// into a bundle the runtime can restore from.
type Checkpointer interface {
	Checkpoint(name string, spec CheckpointSpec) (CheckpointResult, error)
}

// Restorer brings a sandbox back from its last checkpoint, on its own disk,
// without booting it.
type Restorer interface {
	Restore(name string, spec RestoreSpec) error
}

// MigrationInspector reads a sandbox's migration record from the runtime's
// own record of it. The zero Migration with a nil error means there is no
// record, or no sandbox. An error means the record could not be read.
type MigrationInspector interface {
	Migration(name string) (Migration, error)
}

// A caller finds each of these through a type assertion, which an adapter
// whose signature drifted fails without a word. These fail the build instead.
var (
	_ Pauser             = (*hull)(nil)
	_ Checkpointer       = (*hull)(nil)
	_ Restorer           = (*hull)(nil)
	_ MigrationInspector = (*hull)(nil)
)

// Pause is `hull pause NAME`.
func (h *hull) Pause(name string) error { return h.quiet("pause", name, false) }

// Resume is `hull resume NAME`. hull refuses it for a sandbox a checkpoint
// fenced, which is what keeps a moved guest from running in two places.
func (h *hull) Resume(name string) error { return h.quiet("resume", name, false) }

// Checkpoint runs `hull checkpoint`, then reads hull's record back for where
// the bundle went: with no Dir, hull picks the place, and only its record
// says which.
func (h *hull) Checkpoint(name string, spec CheckpointSpec) (CheckpointResult, error) {
	args, err := checkpointArgs(name, spec)
	if err != nil {
		return CheckpointResult{}, err
	}
	out, err := h.call(args)
	if err != nil {
		return CheckpointResult{}, err
	}
	res := CheckpointResult{Summary: strings.TrimSpace(out)}
	// The checkpoint has happened by now, and the bundle is on disk whether
	// or not this read works, so a failure here is not the checkpoint's. It
	// is said on the result rather than dropped, because an empty BundleDir
	// that reads as "no record" is one a caller would act on.
	res.Migration, res.RecordErr = h.Migration(name)
	if res.RecordErr == nil && res.Migration.State == "" {
		res.RecordErr = fmt.Errorf("hull recorded no checkpoint for %s after it said it wrote one", name)
	}
	return res, nil
}

// Restore runs `hull restore`, detached: brig hands a restored sandbox back
// to its own commands and never holds the terminal for the VMM.
func (h *hull) Restore(name string, spec RestoreSpec) error {
	_, err := h.call(restoreArgs(name, spec))
	return err
}

// Migration reads the migration object of `hull inspect`. A sandbox hull
// does not have, and one with no migration record, are both the zero value.
func (h *hull) Migration(name string) (Migration, error) {
	out, stderr, err := inspectNetwork(h.bin, hullArgs("inspect", name)...)
	if err != nil {
		if strings.Contains(stderr, "instance not found") {
			return Migration{}, nil
		}
		return Migration{}, fmt.Errorf("inspect migration of %s: %w: %s", name, err, firstLines(stderr, 3))
	}
	return migrationOf(name, out)
}

// migrationOf is Migration's reading of one `hull inspect` record, apart from
// the call so a test can hand it records hull would print.
func migrationOf(name string, record []byte) (Migration, error) {
	var state struct {
		ID        string `json:"id"`
		Migration *struct {
			State     string `json:"state"`
			BundleDir string `json:"bundleDir"`
			TxID      string `json:"txid"`
		} `json:"migration"`
	}
	if err := json.Unmarshal(record, &state); err != nil {
		return Migration{}, fmt.Errorf("inspect migration of %s: invalid hull response: %w", name, err)
	}
	if state.ID != "" && state.ID != name {
		return Migration{}, fmt.Errorf("inspect migration of %s: hull answered for %s", name, state.ID)
	}
	if state.Migration == nil {
		return Migration{}, nil
	}
	return Migration{State: state.Migration.State, BundleDir: state.Migration.BundleDir,
		TxID: state.Migration.TxID}, nil
}

// checkpointArgs is the one place the checkpoint command line is built.
//
// Then and Dir are checked here, before hull is asked, so a typo cannot
// reach a guest that is about to be paused. A relative Dir is refused rather
// than resolved: hull would resolve it against its own working directory,
// and the bundle would land somewhere the caller did not name.
func checkpointArgs(name string, spec CheckpointSpec) ([]string, error) {
	args := []string{"checkpoint"}
	switch spec.Then {
	case "":
	case CheckpointContinue, CheckpointFence:
		args = append(args, "--then", spec.Then)
	default:
		return nil, fmt.Errorf("a checkpoint then continues or fences, not %q", spec.Then)
	}
	if spec.Dir != "" {
		if !filepath.IsAbs(spec.Dir) {
			return nil, fmt.Errorf("a checkpoint directory must be absolute, not %q", spec.Dir)
		}
		args = append(args, "--dir", spec.Dir)
	}
	if spec.Timeout > 0 {
		args = append(args, "--timeout", seconds(spec.Timeout))
	}
	return append(args, name), nil
}

// restoreArgs is the one place the restore command line is built.
func restoreArgs(name string, spec RestoreSpec) []string {
	args := []string{"restore", "--detach"}
	if spec.Paused {
		args = append(args, "--paused")
	}
	if spec.Timeout > 0 {
		args = append(args, "--timeout", seconds(spec.Timeout))
	}
	return append(args, name)
}

// seconds is d as hull's whole-second flags take it, rounded up: a timeout
// cut short by rounding down fails a checkpoint that would have finished.
func seconds(d time.Duration) string {
	return strconv.FormatInt(int64((d+time.Second-1)/time.Second), 10)
}

// call runs one hull verb and returns its standard output. Its standard
// error is folded into the error, as quiet does, so a refusal reaches the
// caller in hull's words.
func (h *hull) call(args []string) (string, error) {
	cmd := exec.Command(h.bin, hullArgs(args...)...)
	cmd.Env = mergeEnv(h.countedEnv(false))
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if said := strings.TrimSpace(errb.String()); said != "" {
			return out.String(), fmt.Errorf("%s %s: %w: %s", h.bin, strings.Join(args, " "), err, firstLines(said, 3))
		}
		return out.String(), fmt.Errorf("%s %s: %w", h.bin, strings.Join(args, " "), err)
	}
	return out.String(), nil
}
