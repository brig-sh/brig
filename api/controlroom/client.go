package controlroom

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Base is the Control Room API prefix.
const Base = "/api/v2/control-room"

// The router's CSRF double-submit check and the agent's credential.
const (
	csrfCookie   = "CSRF-TOKEN"
	csrfHeader   = "X-CSRFToken"
	apiKeyHeader = "X-API-Key"
)

// maxResponseBytes bounds what is read of an answer.
const maxResponseBytes = 1 << 20

// ErrUnreachable is a request that got no answer.
var ErrUnreachable = errors.New("controlroom: the Control Room did not answer")

// StatusError is a non-2xx answer.
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("controlroom: HTTP %d", e.Status)
	}
	return fmt.Sprintf("controlroom: HTTP %d: %s", e.Status, e.Message)
}

// Client posts to the agent routes of one Control Room deployment.
type Client struct {
	base string
	key  string
	hc   *http.Client
}

// NewClient returns a client for the deployment at base (scheme and host,
// e.g. https://my.nofire.ai) that authenticates with an agent key.
func NewClient(base, key string, hc *http.Client) (*Client, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://") {
		return nil, fmt.Errorf("controlroom: %q is not an http(s) URL", base)
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("controlroom: no agent key")
	}
	if hc == nil {
		hc = &http.Client{
			Timeout: 30 * time.Second,
			// An agent key goes to the deployment it was minted for and
			// nowhere a redirect points.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &Client{base: base, key: key, hc: hc}, nil
}

// URL is the deployment the client talks to.
func (c *Client) URL() string { return c.base }

func (c *Client) postJSON(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("controlroom: encode %s: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+Base+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("controlroom: build %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(apiKeyHeader, c.key)
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Errorf("controlroom: csrf token: %w", err)
	}
	tok := hex.EncodeToString(b[:])
	req.AddCookie(&http.Cookie{Name: csrfCookie, Value: tok})
	req.Header.Set(csrfHeader, tok)

	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrUnreachable
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return ErrUnreachable
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &StatusError{Status: resp.StatusCode, Message: errorText(data)}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("controlroom: decode %s: %w", path, err)
		}
	}
	return nil
}

// errorText reads the Control Room's {"error":{"code","message"}} or a short
// plain body.
func errorText(data []byte) string {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && (e.Error.Code != "" || e.Error.Message != "") {
		return strings.TrimSpace(e.Error.Code + " " + e.Error.Message)
	}
	s := strings.TrimSpace(string(data))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// Sandbox is one sandbox as R20 takes it.
type Sandbox struct {
	Sandbox string `json:"sandbox"`
	Ref     string `json:"ref"`
	Agent   string `json:"agent"`
	Label   string `json:"label"`
	State   string `json:"state"`
	Runtime string `json:"runtime"`
}

// ReportSandboxes is R20 without a snapshot: it adds or updates the sandboxes
// given and leaves the host's others alone, so it can run beside brainctl's
// agent, which reports full snapshots.
func (c *Client) ReportSandboxes(ctx context.Context, sb []Sandbox) error {
	return c.postJSON(ctx, "/agent/sandboxes", struct {
		Snapshot  bool      `json:"snapshot"`
		Sandboxes []Sandbox `json:"sandboxes"`
	}{false, sb}, nil)
}

// Event is one event as R21 takes it.
type Event struct {
	Kind    string         `json:"kind"`
	Sandbox string         `json:"sandbox"`
	File    string         `json:"file"`
	Offset  int64          `json:"offset"`
	Line    string         `json:"line"`
	Parsed  map[string]any `json:"parsed,omitempty"`
}

// EventsResult is R21's answer.
type EventsResult struct {
	Accepted   int `json:"accepted"`
	Duplicates int `json:"duplicates"`
	Rejected   int `json:"rejected"`
}

// IngestEvents is R21.
func (c *Client) IngestEvents(ctx context.Context, evs []Event) (EventsResult, error) {
	var out EventsResult
	err := c.postJSON(ctx, "/agent/events", struct {
		Events []Event `json:"events"`
	}{evs}, &out)
	return out, err
}
