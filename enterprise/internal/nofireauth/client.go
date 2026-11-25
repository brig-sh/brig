// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package nofireauth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	requestTimeout = 30 * time.Second
	// maxBody bounds every response read: a deployment is not trusted to
	// send a small body.
	maxBody = 64 << 10
)

// Client talks to one deployment's authorization server.
type Client struct {
	http *http.Client
	d    Deployment
	ua   string
}

// NewClient returns a client for d. It never follows a redirect: an
// endpoint that redirects is answering for someone else, and a redirect would
// carry the request, and in the device flow the device code, along with it.
func NewClient(d Deployment, version string) *Client {
	return &Client{
		http: &http.Client{
			Timeout:       requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		d:  d,
		ua: "brig/" + version,
	}
}

// do sends req and returns the status and at most 64 KiB of the body. A longer
// body is an error, not a truncation, so a cut-off document is never parsed.
func (c *Client) do(ctx context.Context, req *http.Request) (int, []byte, error) {
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.http.Do(req.WithContext(ctx))
	if err != nil {
		return 0, nil, transportError{err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return 0, nil, transportError{err}
	}
	if len(body) > maxBody {
		return 0, nil, fmt.Errorf("response from %s is larger than %d KiB", req.URL.Host, maxBody>>10)
	}
	return resp.StatusCode, body, nil
}

// transportError is a request that failed in net/http or crypto/tls. Its text
// quotes bytes the server or the network chose: a malformed header line, a
// proxy's status line, a certificate's DNS names, which on Linux Go reports
// before it checks the chain. Those can hold terminal escapes that forge
// Brig's output, so the text is cleaned. Unwrap keeps errors.Is working for
// context.Canceled and context.DeadlineExceeded.
type transportError struct{ err error }

func (e transportError) Error() string { return CleanLine(e.err.Error()) }
func (e transportError) Unwrap() error { return e.err }
