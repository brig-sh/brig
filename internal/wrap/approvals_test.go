package wrap

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
)

// untouchedRuntime is a runtime that no call may reach: every method is the
// nil interface's, and panics.
type untouchedRuntime struct{ runtime.Runtime }

// The sandbox of a target boots only on a Config the gate admitted. A path
// that skips the gate, as brigd does, is refused before the runtime is asked
// anything, with the class that exits 8.
func TestEnsureRunningRefusesATargetThatWasNotAdmitted(t *testing.T) {
	digestHost(t)
	c := approvalConfig(t)
	c.Runtime = untouchedRuntime{}
	var ar *ApprovalRequiredError
	if err := c.EnsureRunning(creds.Set{}); !errors.As(err, &ar) {
		t.Fatalf("an unadmitted target: %v", err)
	}

	other := approvalConfig(t)
	s := subjectOf(t, other)
	s.Sandbox = "brig--review-0123"
	if err := c.Admit(s); err == nil {
		t.Error("Admit took the approval of another sandbox")
	}
	// A profile is never asked: its sandbox is not a target's.
	p, err := Load(repoClaude(t), Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.admission(); err != nil {
		t.Errorf("a profile run was refused: %v", err)
	}
}

// The store keeps one approval per session. A file brig cannot read
// approves nothing and is not written over. A record whose digest is not
// its subject's is not read. A project that holds the state directory is
// refused.
func TestTheApprovalStore(t *testing.T) {
	digestHost(t)
	c := approvalConfig(t)
	s := subjectOf(t, c)
	if _, ok := Approved(c.VMName); ok {
		t.Fatal("approved before Approve")
	}
	if err := Approve(c.VMName, s); err != nil {
		t.Fatal(err)
	}
	r, ok := Approved(c.VMName)
	if !ok || r.Digest != ApprovalDigest(s) || r.Target != "dev" || r.Root != "/src/a" {
		t.Fatalf("stored %+v, %v", r, ok)
	}
	if err := Approve("brig--other-1", s); err == nil {
		t.Error("Approve filed a subject under another key")
	}

	path := filepath.Join(os.Getenv("BRIG_STATE_DIR"), "approvals.json")
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f approvalsFile
	if err := json.Unmarshal(blob, &f); err != nil {
		t.Fatal(err)
	}
	// The subject changed and the digest kept: what a hand edit of the file
	// would be.
	rec := f.Approvals[c.VMName]
	rec.Subject.Args = []string{"--evil"}
	f.Approvals[c.VMName] = rec
	tampered, _ := json.Marshal(f)
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := Approved(c.VMName); ok {
		t.Error("a record whose digest is not its subject's was read")
	}

	for _, corrupt := range []string{"{", `{"version": 2, "approvals": {}}`, "null"} {
		if err := os.WriteFile(path, []byte(corrupt), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := Approved(c.VMName); ok {
			t.Errorf("%q approved a session", corrupt)
		}
		if err := Approve(c.VMName, s); err == nil {
			t.Errorf("Approve wrote over %q", corrupt)
		}
		if _, err := Revoke(c.VMName); err == nil {
			t.Errorf("Revoke wrote over %q", corrupt)
		}
		if got, _ := os.ReadFile(path); string(got) != corrupt {
			t.Errorf("%q was replaced with %s", corrupt, got)
		}
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	inside := s
	inside.Root = filepath.Dir(os.Getenv("BRIG_STATE_DIR"))
	if err := Approve(c.VMName, inside); err == nil {
		t.Error("Approve took a project that holds the state directory")
	}
	if err := Approve(c.VMName, s); err != nil {
		t.Fatal(err)
	}
	if gone, err := Revoke(c.VMName); !gone || err != nil {
		t.Errorf("Revoke: %v %v", gone, err)
	}
	if _, ok := Approved(c.VMName); ok {
		t.Error("approved after Revoke")
	}
}
