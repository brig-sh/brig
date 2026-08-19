//go:build darwin

package hostsrc

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// codeNotFound is the exit code security(1) returns for errSecItemNotFound.
// internal/secret/keychain_darwin.go measures and names the same constant;
// it is duplicated here rather than imported because that package's status()
// and securityError() are unexported and this is a different package -- the
// pattern is reused, the code is not shared.
const codeNotFound = 44

// securityBin is pinned for the reason internal/secret and internal/creds pin
// theirs, and this package is the one where it matters most: a host source
// hands the tool the service name of a credential brig is about to store, and
// takes its stdout as that credential verbatim. A file called `security`
// earlier in $PATH therefore chooses what brig imports and delivers into the
// guest. The tool is part of macOS and lives at a fixed path, so there is
// nothing to look up.
const securityBin = "/usr/bin/security"

// readKeychain reads a generic-password item by service name only: a host
// source names the item the agent itself wrote, and the importer has no
// account name of its own to filter on the way internal/secret's namespaced
// items do.
//
// Distinguishing "no such item" from every other failure is the whole point
// of this function. A denied approval dialog and a locked login keychain
// both exit non-zero with a message on stderr, and NEITHER is exit 44 --
// Reader.read relies on that to keep a refusal from being treated as
// ordinary absence.
func readKeychain(service string) ([]byte, error) {
	cmd := exec.Command(securityBin, "find-generic-password", "-s", service, "-w")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if status(err) == codeNotFound {
			return nil, errNoSuchItem
		}
		// The tool not running at all is an *exec.Error, never an
		// *exec.ExitError, so status() reports -1 for it and it would
		// otherwise be described as a refusal -- telling the reader to
		// approve a dialog that no missing binary ever raised.
		var ee *exec.Error
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("%w: %s: %w", errToolMissing, securityBin, err)
		}
		return nil, securityError(err, errb.String())
	}
	return out.Bytes(), nil
}

// status is the exit code of a failed command, or -1 when it never ran.
func status(err error) int {
	var e *exec.ExitError
	if errors.As(err, &e) {
		return e.ExitCode()
	}
	return -1
}

// securityError keeps security's own explanation, which is the only account
// of anything not covered by an exit code -- a locked keychain, a denied
// access dialog. See internal/secret/keychain_darwin.go's securityError,
// which this mirrors: the last "security: " line is the one worth keeping,
// because the prompt form shares a line with it rather than starting a new
// one.
func securityError(err error, stderr string) error {
	var msg string
	for _, line := range strings.Split(stderr, "\n") {
		if i := strings.LastIndex(line, "security: "); i >= 0 {
			msg = strings.TrimSpace(line[i+len("security: "):])
		}
	}
	if msg == "" {
		return err
	}
	return fmt.Errorf("security: %s: %w", msg, err)
}
