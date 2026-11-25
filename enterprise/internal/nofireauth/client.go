// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package nofireauth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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
	return &Client{http: NewHTTPClient(requestTimeout), d: d, ua: "brig/" + version}
}

// NewHTTPClient returns the HTTP client every call to a deployment uses: it
// never follows a redirect, so a bearer token or a device code is never sent
// to a host the deployment did not name. timeout is the whole request's.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
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

// postForm posts form to endpoint and returns the status and body. addr is
// what the deployment calls the endpoint in an error, such as "token
// address", and action names the request, such as "request a token". A
// redirect is refused here, so the token and device endpoints say so in one
// way.
func (c *Client) postForm(ctx context.Context, endpoint, addr, action string, form url.Values) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, fmt.Errorf("the %s of %s is not valid. Ask its administrator to fix it", addr, c.d)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	status, body, err := c.do(ctx, req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s from %s: %w", action, c.d, err)
	}
	if status >= 300 && status < 400 {
		return 0, nil, fmt.Errorf("%s redirects its %s elsewhere, and Brig does not follow redirects. Ask its administrator to fix it", c.d, addr)
	}
	return status, body, nil
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
