package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrSandboxGone is RunningNestedVirt's answer when the runtime no longer has
// the sandbox: it exited between the caller seeing it running and asking how
// it booted. That is not a question left unanswered, and a caller treats the
// sandbox as not running.
var ErrSandboxGone = errors.New("the sandbox is no longer there")

// NestedInspector is a runtime that can say whether a sandbox already running
// was booted with nested virtualization.
//
// A guest's EL2 is fixed at boot. A run that joins a sandbox left running from
// before the profile's capabilities changed would otherwise print an envelope
// about the profile and not about the guest: no CAPABILITIES row over a guest
// that still has /dev/kvm, or a kvm row over one that never had it.
//
// Asked of the runtime's own record of the instance, which is the one thing
// that was there when the guest booted. A record kept by brig would drift: an
// older brig sharing the same state directory boots and removes sandboxes
// without knowing the record exists.
//
// Optional: a runtime that cannot give a guest nested virtualization has
// nothing to report, and every sandbox it runs is not nested.
type NestedInspector interface {
	// RunningNestedVirt reports whether the sandbox was booted with nested
	// virtualization. It returns ErrSandboxGone when the sandbox is no
	// longer there, and another error when the runtime could not be asked.
	RunningNestedVirt(name string) (bool, error)
}

// RunningNestedVirt reads the instance record `hull inspect` prints. hull
// leaves nestedVirt out when it is false, and a hull released before the field
// never boots a nested guest, so a record without it is not nested.
func (h *hull) RunningNestedVirt(name string) (bool, error) {
	record, found, err := h.inspect(name)
	if err != nil {
		return false, err
	}
	if !found {
		return false, fmt.Errorf("%s inspect %s: %w", h.bin, name, ErrSandboxGone)
	}
	var rec struct {
		NestedVirt bool `json:"nestedVirt"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(record), &rec); err != nil {
		return false, fmt.Errorf("%s inspect %s printed a record brig cannot read: %w", h.bin, name, err)
	}
	return rec.NestedVirt, nil
}
