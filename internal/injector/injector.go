// Package injector is brig's side of the credential broker: it starts and
// stops brig-broker, the process that serves the routes of every sandbox on
// the host, registers a sandbox's routes with it, has it mint the session
// tokens of one exec, and says how the guest reaches a route.
//
// brig-broker is a binary of its own because it refreshes credentials. A
// refresh rotates a pair the host may share, and no package on the run path
// links the refresher (internal/hostsrc/arch_test.go). This package is on the
// run path, so it talks to brig-broker over a unix socket and never links
// internal/renew or internal/broker. The host key that signs the tokens and
// the credentials the routes inject stay in brig-broker's memory.
//
// docs/broker.md has the control protocol, the files in the broker directory
// and the lifecycle.
package injector

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/runtime"
)

// The files in the broker directory. Every one is the broker's.
const (
	// SocketName is the control socket.
	SocketName = "control.sock"
	// PIDName holds the pid of the running broker and its start time.
	PIDName = "broker.pid"
	// LockName is held by the running broker for its whole life, so a
	// second one started beside it exits.
	LockName = "broker.lock"
	// StartLockName is held by brig while it checks for a broker and starts
	// one, so two runs started at once start one broker.
	StartLockName = "start.lock"
	// PortName holds the loopback port the broker listens on. The broker
	// writes it once, and binds the same port at every later start.
	PortName = "port"
	// LogName is the broker's own output.
	LogName = "broker.log"
	// AuditName is the audit log: one line per request, never a value.
	AuditName = "audit.log"
	// HostKeyName is the host key, 32 random bytes the tokens are signed
	// with. Only the broker reads it.
	HostKeyName = "hostkey"
	// CACertName and CAKeyName are the brig CA, which signs only
	// brig.internal. Only the broker reads the key.
	CACertName = "ca.pem"
	CAKeyName  = "ca.key"
)

// TokenTTL is the lifetime of a session token minted for one exec.
const TokenTTL = 12 * time.Hour

// Dir returns the broker directory: broker under ~/.brig/host, or under
// BRIG_STATE_DIR/host.
func Dir() (string, error) {
	host, err := runtime.HostDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(host, runtime.HostBrokerDir), nil
}

// CheckDir refuses a broker directory that is a link, not a directory, not
// yours, or open to your group or to others, and one whose host or state
// directory above it is a link. Every file in it is a secret or controls one,
// so a directory anyone else can enter or redirect is not one to trust.
func CheckDir(dir string) error {
	if err := runtime.CheckHostDirLinks(filepath.Dir(dir)); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing the broker directory %s: it is a link. Remove it, and brig creates a "+
			"directory there", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("refusing the broker directory %s: it is not a directory", dir)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("refusing the broker directory %s: its mode is %#o, and it holds the broker's "+
			"keys. Run chmod 700 %s", dir, info.Mode().Perm(), dir)
	}
	if !ownedByMe(info) {
		return fmt.Errorf("refusing the broker directory %s: it belongs to another user", dir)
	}
	return nil
}

// PortHeldMessage starts the error of a broker that could not bind the port
// recorded in its directory, because another process holds it.
const PortHeldMessage = "another process holds the credential broker's port"

// ErrNotRunning is what a call to a broker that is not running returns.
var ErrNotRunning = errors.New("the credential broker is not running")

// Route is one route of a sandbox, as the control socket carries it.
type Route struct {
	// Policy is the name of the policy that declares the route.
	Policy string `json:"policy"`
	// Route is the route as policy.Routes expanded it. The broker expands
	// it again, so a lifecycle comes only from a preset.
	Route v1alpha1.Route `json:"route"`
}

// Sandbox is a sandbox and its routes, the body of add-sandbox.
type Sandbox struct {
	ID      string  `json:"id"`
	Profile string  `json:"profile"`
	Routes  []Route `json:"routes"`
	// BrigInternal is the address the sandbox's guest resolves brig.internal
	// to, or "" for a sandbox that reaches the broker over loopback. The
	// runtime diverts that address on BrigInternalPort to the sandbox's
	// divert socket, and the broker serves brig.internal there, with TLS. A
	// sandbox registered with it is refused over loopback.
	BrigInternal string `json:"brigInternal,omitempty"`
}

// DivertDirName is the directory of the divert sockets, under the broker
// directory.
const DivertDirName = "divert"

// DivertSocket returns the divert socket of sandbox id under the broker
// directory dir. It depends on the id alone, so a broker that restarts
// listens where a running diverter connects, and brig names the socket to
// the diverter before it registers the sandbox.
func DivertSocket(dir, id string) (string, error) {
	// 48 bits of the hash keep the path short. Two sandboxes whose ids
	// collide would share a socket, and the tokens of one fail the other's
	// checks.
	sum := sha256.Sum256([]byte(id))
	sock := filepath.Join(dir, DivertDirName, hex.EncodeToString(sum[:6])+".sock")
	if len(sock) > sockaddrUnMax {
		return "", fmt.Errorf("the divert socket %s is %d bytes, and a unix socket path is at most %d; "+
			"point BRIG_STATE_DIR at a shorter directory", sock, len(sock), sockaddrUnMax)
	}
	return sock, nil
}

// AddResponse is the answer to add-sandbox.
type AddResponse struct {
	// DivertSocket is the unix socket the broker listens on for the
	// sandbox's diverted connections, or "" for a sandbox with no
	// BrigInternal address. It is the path DivertSocket returns. The
	// diverter connects to it once per guest connection.
	DivertSocket string `json:"divertSocket,omitempty"`
	// CA is the brig CA certificate, in PEM, for a sandbox with a
	// BrigInternal address. The guest trusts it for brig.internal. Its key
	// never leaves the broker.
	CA string `json:"ca,omitempty"`
}

// MintRequest is the body of mint.
type MintRequest struct {
	Sandbox string `json:"sandbox"`
	Session string `json:"session,omitempty"`
	Route   string `json:"route"`
	// TTLSeconds is the token's lifetime. The broker caps it at TokenTTL.
	TTLSeconds int64 `json:"ttlSeconds"`
}

// MintResponse is the answer to mint.
type MintResponse struct {
	Token string `json:"token"`
}

// RemoveResponse is the answer to remove-sandbox.
type RemoveResponse struct {
	// Removed is whether the sandbox was registered.
	Removed bool `json:"removed"`
	// Remaining is the number of sandboxes still registered.
	Remaining int `json:"remaining"`
}

// StopRequest is the body of stop.
type StopRequest struct {
	// IfIdle stops the broker only when no sandbox is registered, so a
	// sandbox registered since the caller looked keeps its routes.
	IfIdle bool `json:"ifIdle"`
}

// StopResponse is the answer to stop.
type StopResponse struct {
	Stopping bool `json:"stopping"`
}

// Status is the answer to status. It names secrets and sandboxes, and never
// holds a value or a token.
type Status struct {
	PID  int `json:"pid"`
	Port int `json:"port"`
	// Sandboxes are the registered sandbox ids, sorted.
	Sandboxes []string `json:"sandboxes"`
	// Pending are the secrets whose refreshed value is held in memory and
	// not yet written back to the store.
	Pending []string `json:"pending,omitempty"`
	// Failing are the secrets whose last refresh failed.
	Failing []string `json:"failing,omitempty"`
	// Placeholder counts, per sandbox id, the requests refused for a
	// session token sent outside its carrier. A sandbox with none is left
	// out.
	Placeholder map[string]int64 `json:"placeholder,omitempty"`
	// Recovering are the secrets with a renewal journal left by a broker
	// that stopped, or held by another one, not yet settled.
	Recovering []string `json:"recovering,omitempty"`
}
