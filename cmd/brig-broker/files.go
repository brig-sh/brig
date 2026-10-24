package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/brig-sh/brig/internal/injector"
)

// portHeldError is a recorded port that another process holds.
type portHeldError struct {
	port int
	err  error
}

func (e *portHeldError) Error() string {
	return fmt.Sprintf("%s 127.0.0.1:%d: %v", injector.PortHeldMessage, e.port, e.err)
}

// listenRoutes listens on the port recorded in dir, or on a free port when
// none is recorded, and then records it.
//
// A broker never moves to another port. If another process holds the
// recorded one, it refuses to start, and that process is the one a client of
// the port would send its tokens to.
func listenRoutes(dir string) (net.Listener, error) {
	path := filepath.Join(dir, injector.PortName)
	blob, err := os.ReadFile(path)
	switch {
	case err == nil:
		port, err := strconv.Atoi(strings.TrimSpace(string(blob)))
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("the port file %s does not hold a port: %q", path, blob)
		}
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, &portHeldError{port: port, err: err}
		}
		if err != nil {
			return nil, fmt.Errorf("cannot serve routes on 127.0.0.1:%d: %w", port, err)
		}
		return ln, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("cannot serve routes on a loopback port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := writeReplace(path, []byte(strconv.Itoa(port)+"\n")); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// writeReplace writes path whole, 0600, through a temporary file and a
// rename, so a reader never sees half of it.
func writeReplace(path string, data []byte) error {
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// rotatingFile is a log that is renamed to <path>.1 when the next write would
// take it past max, so it never holds more than max bytes and the previous
// file is kept. A request anyone on the host can send writes a line to the
// audit log, so the log has to be bounded.
type rotatingFile struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
	// warn takes the one line that says the log could not rotate. nil is
	// stderr.
	warn io.Writer
	// warned is whether that line was written.
	warned bool
	// reopened, when set, is called with each file the log rotates to.
	reopened func(*os.File)
}

func openRotating(path string, max int64) (*rotatingFile, error) {
	f, err := openPrivate(path, os.O_WRONLY|os.O_APPEND)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &rotatingFile{path: path, max: max, f: f, size: info.Size()}, nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return 0, os.ErrClosed
	}
	if r.size > 0 && r.size+int64(len(p)) > r.max {
		// A log that cannot rotate keeps the file it has, so the audit
		// goes on, and the next write tries again.
		if err := r.rotate(); err != nil && !r.warned {
			r.warned = true
			w := r.warn
			if w == nil {
				w = os.Stderr
			}
			_, _ = fmt.Fprintf(w, "brig-broker: cannot rotate %s, writing on to it: %v\n", r.path, err)
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate renames the log to <path>.1, over the previous one, and starts an
// empty one. Until the new one is open, the old one is kept.
func (r *rotatingFile) rotate() error {
	if err := os.Rename(r.path, r.path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(r.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_ = r.f.Close()
	r.f, r.size, r.warned = f, 0, false
	if r.reopened != nil {
		r.reopened(f)
	}
	return nil
}

// followStderr points the broker's stdout and stderr at f, the broker log
// it rotated to. brig started the broker with both on the log, and a panic
// or the broker's last words belong in the log brig reads. Best effort: a
// failure leaves them on the previous file.
func followStderr(f *os.File) {
	_ = unix.Dup2(int(f.Fd()), 1)
	_ = unix.Dup2(int(f.Fd()), 2)
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

var _ io.WriteCloser = (*rotatingFile)(nil)
