package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/brig-sh/brig/internal/broker"
	"github.com/brig-sh/brig/internal/injector"
)

// maxRevokeText bounds the by and reason of a revocation. They are written
// to the audit log and to revoked.json, and named in every refusal.
const maxRevokeText = 256

// loadRevoked reads the revocations recorded in dir. A missing file is none.
// A file that does not parse stops the broker: reading it as empty would
// serve every sandbox it revokes.
func loadRevoked(dir string) (map[string]injector.Revocation, error) {
	path := filepath.Join(dir, injector.RevokedName)
	f, err := openExisting(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]injector.Revocation{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var list []injector.Revocation
	if err := json.NewDecoder(f).Decode(&list); err != nil {
		return nil, fmt.Errorf("the revocations in %s do not parse, so the broker does not start: %w. "+
			"Fix the file, or remove it to reinstate every sandbox it names", path, err)
	}
	out := make(map[string]injector.Revocation, len(list))
	for _, r := range list {
		if err := broker.ValidID(r.ID); err != nil {
			return nil, fmt.Errorf("the revocations in %s name a sandbox that is not valid: %w", path, err)
		}
		out[r.ID] = r
	}
	return out, nil
}

// openExisting opens a file of the broker's own for reading, refusing a link
// and a file someone else can read.
func openExisting(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	if err := checkPrivate(f, path); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// saveRevoked writes the revocations whole. s.mu is held.
func (s *server) saveRevoked() error {
	list := s.revocations()
	blob, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return writeReplace(filepath.Join(s.cfg.dir, injector.RevokedName), append(blob, '\n'))
}

// revocations lists the revocations by id. s.mu is held.
func (s *server) revocations() []injector.Revocation {
	list := make([]injector.Revocation, 0, len(s.revoked))
	for _, r := range s.revoked {
		list = append(list, r)
	}
	slices.SortFunc(list, func(a, b injector.Revocation) int { return strings.Compare(a.ID, b.ID) })
	return list
}

// revokedRefusal is the refusal of a registration of a revoked sandbox. It
// names the revocation and how to lift it.
func revokedRefusal(r injector.Revocation) string {
	msg := fmt.Sprintf("sandbox %s: its broker access was revoked at %s", r.ID, r.At.UTC().Format(time.RFC3339))
	if r.By != "" {
		msg += " by " + r.By
	}
	if r.Reason != "" {
		msg += " (" + r.Reason + ")"
	}
	return msg + fmt.Sprintf(", and stays revoked until `brig broker reinstate %s`", r.ID)
}

// cleanText is free text of a revoke request, on one line and bounded.
func cleanText(s string) (string, error) {
	if len(s) > maxRevokeText {
		return "", fmt.Errorf("a revoke's by and reason are at most %d bytes", maxRevokeText)
	}
	if strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", errors.New("a revoke's by and reason are one line of text")
	}
	return s, nil
}

// revokeSandbox removes a sandbox as removeSandbox does, and records that
// its broker access is revoked, so every registration of it is refused until
// it is reinstated. The record is written before the answer: a revocation
// the broker could not keep is answered 500, with the sandbox removed all
// the same.
func (s *server) revokeSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := broker.ValidID(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req injector.RevokeRequest
	if !decode(w, r, &req) {
		return
	}
	by, err := cleanText(req.By)
	if err == nil {
		req.Reason, err = cleanText(req.Reason)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	removed, remaining := s.forget(id)
	rev, already := s.revoked[id]
	if !already {
		rev = injector.Revocation{ID: id, At: s.clock().UTC(), By: by, Reason: req.Reason}
		s.revoked[id] = rev
	}
	err = s.saveRevoked()
	s.mu.Unlock()
	if err != nil {
		s.logger.Printf("could not record the revocation of %s: %v", id, err)
		http.Error(w, fmt.Sprintf("sandbox %s was removed, and its revocation could not be recorded: %v", id, err),
			http.StatusInternalServerError)
		return
	}
	s.auditLog.Info("revoke", "sandbox", id, "by", rev.By, "reason", rev.Reason, "removed", removed,
		"already", already)
	reply(w, injector.RevokeResponse{Removed: removed, Remaining: remaining, Revocation: rev})
}

// reinstateSandbox lifts a sandbox's revocation. The next run of the
// sandbox registers it again, with a new key.
func (s *server) reinstateSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := broker.ValidID(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	old, ok := s.revoked[id]
	var err error
	if ok {
		delete(s.revoked, id)
		if err = s.saveRevoked(); err != nil {
			// The file still names it, so the next broker would revoke it
			// again: keep this one in step with the file.
			s.revoked[id] = old
		}
	}
	s.mu.Unlock()
	if err != nil {
		http.Error(w, fmt.Sprintf("sandbox %s: the reinstatement could not be recorded: %v", id, err),
			http.StatusInternalServerError)
		return
	}
	if ok {
		s.auditLog.Info("reinstate", "sandbox", id)
	}
	reply(w, injector.ReinstateResponse{Reinstated: ok})
}

// clock is cfg.now, or time.Now.
func (s *server) clock() time.Time {
	if s.cfg.now != nil {
		return s.cfg.now()
	}
	return time.Now()
}
