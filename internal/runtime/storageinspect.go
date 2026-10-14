package runtime

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// OwnsItsDisk reads the VMM argv hull persisted at boot, the record
// SandboxNetwork reads too. hvi takes the root as `--disk PATH` on a block
// boot and each host directory as `--share-rw` or `--share-ro`, including a
// shared root, so a block root with no share flag at all is a sandbox that
// owns its storage.
func (h *hull) OwnsItsDisk(name string) (bool, error) {
	out, stderr, err := inspectNetwork(h.bin, hullArgs("inspect", name)...)
	if err != nil {
		if strings.Contains(stderr, "instance not found") {
			return false, nil
		}
		return false, fmt.Errorf("inspect storage of %s: %w: %s", name, err, firstLines(stderr, 3))
	}
	return ownsItsDisk(name, out)
}

// ownsItsDisk is OwnsItsDisk's reading of one `hull inspect` record, apart
// from the call so a test can hand it records hull would print.
//
// It answers true only on a positive match: an hvi argv with one --disk and
// no share flag of any spelling. Every other shape, vz and qemu included, is
// false or an error, never true, because the caller refuses on true and a
// sandbox no teleportable session could have booted must not be stranded by
// a record this version does not recognise.
func ownsItsDisk(name string, record []byte) (bool, error) {
	var state struct {
		ID      string   `json:"id"`
		Backend string   `json:"backend"`
		CmdLine []string `json:"cmdLine"`
	}
	if err := json.Unmarshal(record, &state); err != nil {
		return false, fmt.Errorf("inspect storage of %s: invalid hull response: %w", name, err)
	}
	if state.ID != name || len(state.CmdLine) < 2 {
		return false, fmt.Errorf("inspect storage of %s: hull returned no matching VMM configuration", name)
	}
	if filepath.Base(state.CmdLine[0]) != "hvi" {
		return false, nil
	}
	if state.Backend != "" && state.Backend != "hvi" {
		return false, fmt.Errorf("inspect storage of %s: backend %q disagrees with its hvi argv", name, state.Backend)
	}
	args := state.CmdLine[1:]
	for _, arg := range args {
		if strings.HasPrefix(arg, "--share") {
			return false, nil
		}
	}
	disk, ok := argvValue(args, "--disk")
	return ok && disk != "", nil
}
