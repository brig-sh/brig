package injector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"
)

// callTimeout bounds one control call. The broker answers from memory, and a
// call that takes longer than this is not coming back.
const callTimeout = 10 * time.Second

// maxReply bounds a control answer. The largest is a status listing every
// sandbox on the host.
const maxReply = 1 << 20

// Client calls the control socket of a running broker.
type Client struct {
	// Socket is the path of the control socket.
	Socket string
}

// ownedByMe reports whether the file info belongs to the user brig runs as.
func ownedByMe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// call sends one request and decodes the answer into out, when out is not
// nil. A broker that is not listening is ErrNotRunning. A refusal carries the
// broker's own words.
func (c Client) call(method, path string, body, out any) error {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: callTimeout}
	var payload io.Reader
	if body != nil {
		blob, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(blob)
	}
	req, err := http.NewRequest(method, "http://broker"+path, payload)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return ErrNotRunning
		}
		return fmt.Errorf("the credential broker at %s did not answer: %w", c.Socket, err)
	}
	defer func() { _ = resp.Body.Close() }()
	blob, err := io.ReadAll(io.LimitReader(resp.Body, maxReply))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(blob))
		if msg == "" {
			msg = resp.Status
		}
		return &RefusedError{Status: resp.StatusCode, Message: msg}
	}
	// A broker older than the answer to add-sandbox sends none.
	if out == nil || len(blob) == 0 {
		return nil
	}
	return json.Unmarshal(blob, out)
}

// RefusedError is a control call the broker answered with an error.
type RefusedError struct {
	Status  int
	Message string
}

func (e *RefusedError) Error() string { return "the credential broker refused: " + e.Message }

// StoppingMessage is the refusal of a broker on its way out, with 503. The
// caller waits for it to exit and starts another.
const StoppingMessage = "the credential broker is stopping"

func stopping(err error) bool {
	var r *RefusedError
	return errors.As(err, &r) && r.Status == http.StatusServiceUnavailable
}

// AddSandbox registers s, or replaces the sandbox registered under s.ID.
// Registering the same routes again keeps the sandbox's key, so the tokens of
// an agent already running stay valid.
func (c Client) AddSandbox(s Sandbox) error {
	_, err := c.Add(s)
	return err
}

// Add is AddSandbox, and returns the broker's answer.
func (c Client) Add(s Sandbox) (AddResponse, error) {
	var out AddResponse
	err := c.call(http.MethodPost, "/sandboxes", s, &out)
	return out, err
}

// RemoveSandbox stops serving the sandbox id. Its tokens are refused from
// the next request on.
func (c Client) RemoveSandbox(id string) (RemoveResponse, error) {
	var out RemoveResponse
	err := c.call(http.MethodDelete, "/sandboxes/"+id, nil, &out)
	return out, err
}

// Mint returns a session token for one route of a registered sandbox.
func (c Client) Mint(r MintRequest) (string, error) {
	var out MintResponse
	if err := c.call(http.MethodPost, "/tokens", r, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", errors.New("the credential broker minted an empty token")
	}
	return out.Token, nil
}

// Status asks the broker what it serves.
func (c Client) Status() (Status, error) {
	var out Status
	err := c.call(http.MethodGet, "/status", nil, &out)
	return out, err
}

// Stop asks the broker to exit. With ifIdle it exits only when no sandbox is
// registered.
func (c Client) Stop(ifIdle bool) (bool, error) {
	var out StopResponse
	err := c.call(http.MethodPost, "/stop", StopRequest{IfIdle: ifIdle}, &out)
	return out.Stopping, err
}
