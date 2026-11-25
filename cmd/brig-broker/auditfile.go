package main

import (
	"fmt"
	"net/http"
	"os"
	"sync"
)

// cappedFile is the audit file a platform hands to the broker with
// --audit-fd. The broker appends to it and never rotates it: the platform
// owns the file and its directory, so the broker cannot point the file at
// anything else. Once it holds max bytes, full reports true and the broker
// refuses every route, since a request it cannot audit is one it does not
// serve. The line that crosses max is still written whole.
type cappedFile struct {
	mu   sync.Mutex
	f    *os.File
	max  int64
	size int64
}

func newCappedFile(f *os.File, max int64) (*cappedFile, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("the audit file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("the audit file is a %s, not a regular file", info.Mode().Type())
	}
	return &cappedFile{f: f, max: max, size: info.Size()}, nil
}

func (c *cappedFile) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.size >= c.max {
		return 0, fmt.Errorf("the audit file holds %d bytes, its limit is %d", c.size, c.max)
	}
	n, err := c.f.Write(p)
	c.size += int64(n)
	return n, err
}

func (c *cappedFile) full() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size >= c.max
}

// auditFullMessage is the answer to a route request once the audit file is
// full.
const auditFullMessage = "the broker's audit log is full, so it serves no route until it is restarted"

// audited refuses every request with 503 once the audit file handed over
// with --audit-fd is full. A broker with its own rotating audit.log never
// refuses.
func (s *server) audited(next http.Handler) http.Handler {
	if s.auditCap == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auditCap.full() {
			http.Error(w, auditFullMessage, http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}
