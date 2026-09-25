package runtime

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A host port can be taken by something other than a sandbox: a dev server
// started on the host, or any program that listened there first. Both
// runtimes are refused such a port, and neither says who holds it. brig names
// that case apart from a port another sandbox publishes, because the two have
// different fixes.

// holderTimeout bounds the lsof call that names the process holding a port.
// A refusal waits no longer than this for the name, and goes without one.
const holderTimeout = 3 * time.Second

// portHolder returns the process listening on p's host port, as "nc, pid
// 951", or "" when brig cannot tell.
//
// It asks lsof, which macOS always has and a Linux host may not. Without root,
// lsof sees only the caller's own processes, so a port held by another user
// comes back unnamed. A variable so a test can stand in for lsof.
var portHolder = func(p Publication) string {
	args := []string{"-nP", "-Fpc"}
	if p.Proto() == "udp" {
		args = append(args, "-iUDP:"+strconv.Itoa(p.HostPort))
	} else {
		args = append(args, "-iTCP:"+strconv.Itoa(p.HostPort), "-sTCP:LISTEN")
	}
	cmd := exec.Command("lsof", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		return ""
	}
	// Waited for in the background. lsof can block in the kernel on a process
	// it inspects, and a process blocked there does not exit when it is
	// killed, so a Wait here could hang the refusal with it.
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(holderTimeout):
		_ = cmd.Process.Kill()
		return ""
	}
	// lsof exits 1 when it finds nothing, so the output decides, not the status.
	return lsofHolder(out.String())
}

// lsofHolder returns the first process in lsof's -F output, as "nc, pid 951",
// or "" when there is none. Each process is a 'p' line followed by its 'c'
// line.
func lsofHolder(out string) string {
	pid := ""
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "p"):
			pid = line[1:]
		case strings.HasPrefix(line, "c") && pid != "":
			return fmt.Sprintf("%s, pid %s", line[1:], pid)
		}
	}
	return ""
}

// hostPortHeld reports whether something holds p's host address and port, by
// trying to bind it. Only EADDRINUSE counts: a port below 1024 refuses an
// unprivileged bind with EACCES whether or not anything listens there.
func hostPortHeld(p Publication) bool {
	addr := net.JoinHostPort(p.Addr(), strconv.Itoa(p.HostPort))
	var err error
	if p.Proto() == "udp" {
		var c net.PacketConn
		if c, err = net.ListenPacket("udp", addr); err == nil {
			_ = c.Close()
		}
	} else {
		var l net.Listener
		if l, err = net.Listen("tcp", addr); err == nil {
			_ = l.Close()
		}
	}
	return errors.Is(err, syscall.EADDRINUSE)
}

// portInUse is the refusal for a host port that a process outside brig holds.
func portInUse(p Publication) error {
	who := "another process"
	if holder := portHolder(p); holder != "" {
		who += " (" + holder + ")"
	}
	return fmt.Errorf("%s is in use by %s on this host. Stop it, or publish on "+
		"another host port, for example `%d:%d`", p.Local(), who, p.HostPort+1, p.GuestPort)
}

// publishedBy is the refusal for a host port another sandbox publishes. release
// says how that sandbox gives the port up, which differs by runtime.
func publishedBy(p Publication, owner, release string) error {
	return fmt.Errorf("%s is already published by %s; publish on another host port, "+
		"for example `%d:%d`, or %s", p.Local(), owner, p.HostPort+1, p.GuestPort, release)
}
