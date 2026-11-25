// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package recording

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	// Format is the one wire format this package speaks.
	Format = "nofire.recording/v1"
	// OpenTimeout bounds the call that decides whether a run is recorded.
	OpenTimeout = 3 * time.Second

	apiPrefix    = "/api/v2/control-room/recordings"
	maxReply     = 64 << 10
	maxBatchCeil = 4 << 20
	maxWindow    = 1 << 20
	// maxIDLen bounds a recording id. The id comes from the deployment and
	// goes into a URL path, so it is a short token and nothing else.
	maxIDLen = 128
)

// validRecordingID reports whether id is safe to put in a URL path as one
// segment: 1 to 128 of letters, digits, hyphen and underscore. A deployment
// that sent a "/", a "..", a "?" or a control character there would steer the
// next requests, which carry the token, to a path the person never asked for.
func validRecordingID(id string) bool {
	if id == "" || len(id) > maxIDLen {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// OpenRequest is the body of POST /recordings. The server stores the strings as
// reported and trusts none of them for access.
type OpenRequest struct {
	RunKey      string `json:"run_key"`
	Sandbox     string `json:"sandbox"`
	Ref         string `json:"ref"`
	Profile     string `json:"profile"`
	Host        string `json:"host"`
	Runtime     string `json:"runtime"`
	BrigVersion string `json:"brig_version"`
	Cols        int    `json:"cols"`
	Rows        int    `json:"rows"`
	StartedAt   string `json:"started_at"`
}

// OpenResponse is the answer of POST /recordings.
type OpenResponse struct {
	Recorded      bool   `json:"recorded"`
	Reason        string `json:"reason"`
	RecordingID   string `json:"recording_id"`
	NextOffset    int64  `json:"next_offset"`
	MaxBatchBytes int    `json:"max_batch_bytes"`
	WindowBytes   int    `json:"window_bytes"`
	Format        string `json:"format"`
}

// CloseRequest is the body of POST /recordings/{id}/close.
type CloseRequest struct {
	EndReason string `json:"end_reason"`
	// ExitCode is nil (JSON null) when the status is unknown.
	ExitCode    *int   `json:"exit_code"`
	Signal      string `json:"signal"`
	FinalOffset int64  `json:"final_offset"`
}

// Client is a signed-in person's connection to one deployment. Base is the
// deployment's URL, path included and with no trailing slash. The token is sent
// only as a bearer header to Base.
type Client struct {
	Base      string
	Token     string
	HTTP      *http.Client
	UserAgent string
	Now       func() time.Time
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) do(ctx context.Context, method, path string, ctype string, body []byte, hdr map[string]string) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// The error text of net/http can quote the URL, not the token, but a
		// caller has no use for it: report only that the transport failed.
		return 0, nil, errTransport
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxReply+1))
	if err != nil || len(b) > maxReply {
		return 0, nil, errTransport
	}
	return resp.StatusCode, b, nil
}

// ErrAuth means the deployment answered 401 or 403: the token was revoked or has
// expired. The upload stops in this process and the spool is kept for a
// `brig recordings flush` after the user signs in again.
var ErrAuth = errors.New("recording: the login was refused")

var errTransport = errors.New("recording: the deployment could not be reached")

// openRetry is an open that failed in a way a later try may fix: a transport
// error, a timeout, 429 or 5xx. rateLimited is true for 429.
type openRetry struct{ rateLimited bool }

func (e openRetry) Error() string { return "recording: the open call failed and may be retried" }

// Open asks whether the run is recorded, and opens the recording when it is.
// It is idempotent for one run key. A non-200 answer, a timeout or a transport
// error is an error: the caller runs unrecorded.
func (c *Client) Open(ctx context.Context, req OpenRequest) (OpenResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, OpenTimeout)
	defer cancel()
	body, _ := json.Marshal(req)
	st, b, err := c.do(ctx, http.MethodPost, apiPrefix, "application/json", body, nil)
	if err != nil {
		return OpenResponse{}, openRetry{}
	}
	if st == http.StatusUnauthorized || st == http.StatusForbidden {
		return OpenResponse{}, ErrAuth
	}
	if st == http.StatusTooManyRequests || st >= 500 {
		return OpenResponse{}, openRetry{rateLimited: st == http.StatusTooManyRequests}
	}
	if st != http.StatusOK {
		return OpenResponse{}, fmt.Errorf("recording: open answered %d", st)
	}
	var r OpenResponse
	if err := json.Unmarshal(b, &r); err != nil {
		return OpenResponse{}, errors.New("recording: open answered with something that is not JSON")
	}
	if !r.Recorded {
		return r, nil
	}
	if !validRecordingID(r.RecordingID) || r.Format != Format || r.NextOffset < 0 {
		return OpenResponse{}, errors.New("recording: open answered with a recording this brig cannot write")
	}
	// The server's batch and window sizes are authoritative: the server decodes
	// each batch with a window of exactly window_bytes and bounds it by
	// max_batch_bytes, so a client that substituted its own values would send
	// batches the server cannot decode or refuses. A value of 0 or past the
	// ceilings below cannot be right, so the run goes unrecorded instead.
	if r.MaxBatchBytes <= 0 || r.MaxBatchBytes > maxBatchCeil || r.WindowBytes <= 0 || r.WindowBytes > maxWindow {
		return OpenResponse{}, errors.New("recording: open answered with batch or window sizes this brig refuses")
	}
	return r, nil
}

// put sends one batch.
func (c *Client) put(ctx context.Context, id string, offset int64, body []byte) (int, []byte, error) {
	return c.do(ctx, http.MethodPut, apiPrefix+"/"+url.PathEscape(id)+"/data?offset="+strconv.FormatInt(offset, 10),
		"application/octet-stream", body,
		map[string]string{"X-Recording-Host-Time": c.now().UTC().Format(time.RFC3339Nano)})
}

// closeRecording sends the close call.
func (c *Client) closeRecording(ctx context.Context, id string, req CloseRequest) (int, []byte, error) {
	body, _ := json.Marshal(req)
	return c.do(ctx, http.MethodPost, apiPrefix+"/"+url.PathEscape(id)+"/close", "application/json", body, nil)
}
