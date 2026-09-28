package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// dohCurl asks the DoH question through the guest's own curl instead of the
// probe's client. That is the tool an agent reaches for, and it honors the
// proxy variables set in the guest where the probe's client does not.
//
// With no curl on PATH the result is client-missing, before any traffic. A
// guest that cannot speak DoH at all is not a policy that blocks it.
func (p *prober) dohCurl(ctx context.Context, raw, name string) result {
	curl, err := p.lookPath("curl")
	if err != nil {
		return result{clientMissing, "no curl on PATH: " + err.Error()}
	}
	if _, _, err := dohURL(raw); err != nil {
		return result{failed, err.Error()}
	}
	id := newID()
	q, err := buildQuery(id, name, typeA)
	if err != nil {
		return result{failed, err.Error()}
	}
	out, err := os.CreateTemp("", "netprobe-doh-")
	if err != nil {
		return result{failed, err.Error()}
	}
	out.Close()
	defer os.Remove(out.Name())

	ctx, cancel := context.WithTimeout(ctx, p.timeout+p.timeout/2)
	defer cancel()
	// -v because curl folds a refused port and an unreachable one into exit
	// 7, and only the verbose connect line says which it was.
	cmd := exec.CommandContext(ctx, curl, "-v", "-sS",
		"--max-time", fmt.Sprintf("%.3f", p.timeout.Seconds()),
		"-o", out.Name(), "-w", "%{http_code}",
		"-H", "content-type: application/dns-message",
		"-H", "accept: application/dns-message",
		"--data-binary", "@-", raw)
	// The verbose lines matched below are curl's untranslated ones.
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	cmd.Stdin = bytes.NewReader(q)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()

	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case ctx.Err() != nil:
		if curlConnected(stderr.String()) {
			return result{failedAfterConnect, "curl connected, then did not finish: " + ctx.Err().Error()}
		}
		return result{timedOut, "curl did not finish: " + ctx.Err().Error()}
	case errors.As(err, &exitErr):
		r := curlFailure(exitErr.ExitCode(), stderr.String())
		if exitErr.ExitCode() == curlCouldNotResolve {
			u, _, _ := dohURL(raw)
			r = p.unresolved(ctx, u.Hostname(), r.detail)
		}
		return r
	default:
		return result{failed, "running curl: " + err.Error()}
	}
	if code := strings.TrimSpace(stdout.String()); code != "200" {
		return result{failedAfterConnect, "curl got HTTP " + code}
	}
	b, err := os.ReadFile(out.Name())
	if err != nil {
		return result{failed, err.Error()}
	}
	rep, err := parseReply(b, id)
	if err != nil {
		return result{failedAfterConnect, "curl got HTTP 200, then: " + err.Error()}
	}
	return result{reached, fmt.Sprintf("curl got %s", rep)}
}

// curlCouldNotResolve is the exit status curl gives a host it could not
// resolve.
const curlCouldNotResolve = 6

// curlConnected reports whether curl's verbose output says it made a
// connection. curl 8.16 and later print "Established connection to", and
// older releases print "Connected to".
func curlConnected(stderr string) bool {
	return strings.Contains(stderr, "* Connected to ") ||
		strings.Contains(stderr, "* Established connection to ")
}

// curlFailure maps curl's exit codes onto the probe's outcomes. An exit 7
// with no connect line saying why is an error. A guess at refused or no
// route puts a failure mode in the record that nobody saw. curl also exits 28
// for a stall after it connected, which is an error for the same reason the
// probe's own client reports one.
func curlFailure(code int, stderr string) result {
	detail := fmt.Sprintf("curl exit %d: %s", code, lastLine(stderr))
	if curlConnected(stderr) {
		return result{failedAfterConnect, detail}
	}
	switch code {
	case curlCouldNotResolve:
		return result{resolveFailed, detail}
	case 7:
		switch {
		case strings.Contains(stderr, "Connection refused"):
			return result{connectRefused, detail}
		case strings.Contains(stderr, "No route to host"),
			strings.Contains(stderr, "Network is unreachable"):
			return result{noRoute, detail}
		}
	case 28:
		return result{timedOut, detail}
	}
	return result{failed, detail}
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
