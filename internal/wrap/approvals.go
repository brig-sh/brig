package wrap

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The approval store: the approval you gave each session of a Brigfile target,
// keyed by its sandbox name, which hashes the checkout, the target and the
// session together. An approval of target dev is therefore not one of target
// review, of another checkout of the same repository, or of another session.
//
// It is kept in the state directory, beside projects.json and with the same
// guard: CheckProjectState refuses a project that holds the state directory,
// since the guest writes to its project and could otherwise approve itself.
//
// A file that is there and cannot be read as this version approves nothing,
// and is never written over. An approval is a decision you made, and a file
// that silently lost every one of them would ask for none of them again
// without saying why. The writers refuse and name the file.

// approvalsName is the file, inside stateDir.
const approvalsName = "approvals.json"

// approvalsVersion is the version this release writes and reads.
const approvalsVersion = 1

// ApprovalRecord is one approval: the session it is for, its digest, when
// it was given, and the subject it was given for, so that a later mismatch
// can be named part by part.
type ApprovalRecord struct {
	Root    string          `json:"root"`
	Target  string          `json:"target"`
	Session string          `json:"session,omitempty"`
	Digest  string          `json:"digest"`
	At      time.Time       `json:"at"`
	Subject ApprovalSubject `json:"subject"`
}

type approvalsFile struct {
	Version   int                       `json:"version"`
	Approvals map[string]ApprovalRecord `json:"approvals"`
}

// errApprovals is wrapped by every refusal to write the approval store
// because the file there cannot be read.
var errApprovals = errors.New("brig cannot read its approvals")

// readApprovals returns the approvals the store holds, and an error when a
// file is there that this release cannot read. A missing file holds none.
//
// A record whose own digest is not that of its subject, or whose subject
// names another session than its key, is left out: brig never wrote it.
func readApprovals() (map[string]ApprovalRecord, error) {
	blob, err := readStateFile(approvalsName)
	if err != nil {
		return map[string]ApprovalRecord{}, fmt.Errorf("%w: %w", errApprovals, err)
	}
	if blob == nil {
		return map[string]ApprovalRecord{}, nil
	}
	var f approvalsFile
	if err := json.Unmarshal(blob, &f); err != nil {
		return map[string]ApprovalRecord{}, fmt.Errorf("%w: %w", errApprovals, err)
	}
	if f.Version != approvalsVersion {
		return map[string]ApprovalRecord{}, fmt.Errorf("%w: it is version %d, and this release reads version %d",
			errApprovals, f.Version, approvalsVersion)
	}
	out := make(map[string]ApprovalRecord, len(f.Approvals))
	for key, r := range f.Approvals {
		if r.Subject.Sandbox != key || r.Digest != ApprovalDigest(r.Subject) {
			continue
		}
		out[key] = r
	}
	return out, nil
}

// approvalsError names the file a writer refused to replace.
func approvalsError(err error) error {
	path, perr := indexPath(approvalsName)
	if perr != nil {
		return err
	}
	return fmt.Errorf("%w. brig leaves %s as it is; move it aside to start a new one", err, path)
}

// Approved returns the approval stored for the session whose sandbox is key,
// and false when there is none or the store cannot be read.
func Approved(key string) (ApprovalRecord, bool) {
	approvals, err := readApprovals()
	if err != nil {
		return ApprovalRecord{}, false
	}
	r, ok := approvals[key]
	return r, ok
}

// Approve stores s as the approval of the session whose sandbox is key,
// replacing the one there.
//
// It refuses a subject of another session, a project whose root and the
// state directory contain one another, and a store it cannot read.
func Approve(key string, s ApprovalSubject) error {
	if s.Sandbox != key || !IsProjectSandbox(key) {
		return fmt.Errorf("%s is not the sandbox of this approval", key)
	}
	if err := CheckProjectState(s.Root); err != nil {
		return err
	}
	approvals, err := readApprovals()
	if err != nil {
		return approvalsError(err)
	}
	approvals[key] = ApprovalRecord{Root: s.Root, Target: s.Target, Session: s.Session,
		Digest: ApprovalDigest(s), At: approvalNow().UTC(), Subject: s}
	return writeStateFile(approvalsName, approvalsFile{Version: approvalsVersion, Approvals: approvals})
}

// Revoke drops the approval of the session whose sandbox is key. It reports
// whether there was one, and an error when the store cannot be read or
// rewritten.
func Revoke(key string) (bool, error) {
	approvals, err := readApprovals()
	if err != nil {
		return false, approvalsError(err)
	}
	if _, ok := approvals[key]; !ok {
		return false, nil
	}
	delete(approvals, key)
	return true, writeStateFile(approvalsName, approvalsFile{Version: approvalsVersion, Approvals: approvals})
}

// approvalNow is the clock an approval is stamped with. A test sets it.
var approvalNow = time.Now

// ApprovalRequiredError is a run of a target that no approval covers. It is
// a class of its own, so a script can tell "approve this first" from a run
// that started and failed.
type ApprovalRequiredError struct{ Err error }

func (e *ApprovalRequiredError) Error() string { return e.Err.Error() }
func (e *ApprovalRequiredError) Unwrap() error { return e.Err }

// Admit marks this run of a target as approved for s, the subject the gate
// approved. EnsureRunning refuses the sandbox of a target on a Config that
// was not admitted, so a path that boots without going through the gate
// boots nothing: brigd, or a verb added later that forgets to ask.
//
// It refuses a subject of another sandbox.
func (c *Config) Admit(s ApprovalSubject) error {
	if s.Sandbox != c.VMName || c.target == nil {
		return fmt.Errorf("the approval is for %s, not for %s", s.Sandbox, c.VMName)
	}
	c.admitted = ApprovalDigest(s)
	return nil
}

// admission refuses the sandbox of a Brigfile target on a run that the gate
// did not admit. A profile's sandbox never starts with that prefix: Load
// refuses a BRIG_NAME that does.
func (c *Config) admission() error {
	if !IsProjectSandbox(c.VMName) || c.admitted != "" {
		return nil
	}
	return &ApprovalRequiredError{Err: fmt.Errorf("%s is the sandbox of a Brigfile target, and only "+
		"`brig run --target` starts one, after you approve what it runs", c.VMName)}
}

// Refusal is why `brig run` refuses this run before it prepares anything, or
// nil: the checks EnsureRunning makes first that a plan can ask without
// writing. A run of a target asks them before it asks for an approval, since
// a run that would be refused is nothing to approve. The error keeps its
// class, so the exit status is the one the run would have.
func (c *Config) Refusal() error {
	if err := c.sessionRefusal(); err != nil {
		return err
	}
	if c.Runtime != nil {
		if err := c.backendRefusal(c.hypervisor()); err != nil {
			return err
		}
	}
	return c.routeStoreRefusal(c.checkRouteStore())
}

// TargetOf is the Brigfile target this run was resolved from, and false on a
// run of a profile.
func (c *Config) TargetOf() (Target, bool) {
	if c.target == nil {
		return Target{}, false
	}
	return *c.target, true
}
