package runtime

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// A publication is a guest port offered on the host: the agent starts a dev
// server, and the person who asked for it opens it in a browser.
//
// It is a hole in the sandbox boundary, which is why brig makes two things
// true of every one of them. The host address defaults to loopback, so a
// published port is reachable from this machine and not from the network the
// machine is on. And every publication is named in the execution envelope,
// because a hole nobody was told about is the kind that matters.
//
// The mechanism is the gateway's, on both operating systems. On hvi brig owns
// a user-mode gateway and asks it to forward; see gatewayapi.go. On Linux the
// container runtime publishes, which it can only do when the sandbox is
// created; see nerdctl.runArgs.

// offlinePublishError refuses a sandbox that publishes a port and has no
// network to publish it from.
//
// The ports may come from the record and not from this command line, so the
// message names the first one and how to withdraw it.
func offlinePublishError(ports []Publication) error {
	return fmt.Errorf("this sandbox publishes %s, and --network offline gives it no "+
		"network to publish from. Run it on a network, or withdraw the port with "+
		"`brig network unpublish`. A publication outlives the run that made it, so this "+
		"applies whether or not --publish is on this line", ports[0])
}

// Publication is one guest port offered on the host.
type Publication struct {
	// Protocol is tcp or udp. Empty means tcp, which is what a dev server is.
	Protocol string `json:"protocol,omitempty"`
	// HostAddr is the address on the host the port is offered at. Empty means
	// 127.0.0.1, which keeps it on this machine.
	HostAddr string `json:"hostAddr,omitempty"`
	// HostPort is the port on the host, and GuestPort the port inside the
	// sandbox it carries to.
	HostPort  int `json:"hostPort"`
	GuestPort int `json:"guestPort"`
}

// loopback is where a published port goes when the person publishing it did
// not say. Every other address offers the guest's port to whatever can reach
// this machine, which is a wider hole than "look at it in a browser" asks for.
const loopback = "127.0.0.1"

// Addr is the host address this publication is offered at, with the default
// filled in.
func (p Publication) Addr() string {
	if p.HostAddr == "" {
		return loopback
	}
	return p.HostAddr
}

// Proto is the protocol, with the default filled in.
func (p Publication) Proto() string {
	if p.Protocol == "" {
		return "tcp"
	}
	return p.Protocol
}

// Local is the host side as an address, and Remote is the guest side once the
// guest's own address is known.
func (p Publication) Local() string {
	return p.Addr() + ":" + strconv.Itoa(p.HostPort)
}

func (p Publication) Remote(guestIP string) string {
	return guestIP + ":" + strconv.Itoa(p.GuestPort)
}

// Addressed reports whether a host address was written out, rather than left
// to the loopback default. `brig network unpublish 8080` names a port and no
// address; `brig network unpublish 0.0.0.0:8080:80` names both.
func (p Publication) Addressed() bool { return p.HostAddr != "" }

// Matches reports whether this publication is the one q names.
//
// A publication written without an address matches any address on that port,
// because the person typing `brig network unpublish 8080` is naming the port
// they saw and cannot be asked to remember which interface it went to. One
// written with an address matches only that address.
func (p Publication) Matches(q Publication) bool {
	if p.Proto() != q.Proto() || p.HostPort != q.HostPort {
		return false
	}
	return !q.Addressed() || p.Addr() == q.Addr()
}

// Same reports whether two publications name one listener: the same
// protocol, host address and host port, whatever guest port they carry to.
func (p Publication) Same(q Publication) bool {
	return p.Proto() == q.Proto() && p.Addr() == q.Addr() && p.HostPort == q.HostPort
}

// Overlaps reports whether two publications would compete for one host port.
// That is Same, and also a pair on one protocol and host port where either
// address is unspecified: 0.0.0.0 listens on every address the host has,
// 127.0.0.1 included.
//
// A new publication is checked with this, so that it replaces the one it
// competes with rather than joining it.
func (p Publication) Overlaps(q Publication) bool {
	if p.Proto() != q.Proto() || p.HostPort != q.HostPort {
		return false
	}
	return p.Addr() == q.Addr() || unspecified(p.Addr()) || unspecified(q.Addr())
}

func unspecified(addr string) bool {
	a, err := netip.ParseAddr(addr)
	return err == nil && a.IsUnspecified()
}

// String is the publication in the longest form --publish takes, whichever of
// the short spellings was typed. A reader of an envelope row or an error
// should not have to reconstruct the address half.
func (p Publication) String() string {
	s := fmt.Sprintf("%s:%d:%d", p.Addr(), p.HostPort, p.GuestPort)
	if p.Proto() != "tcp" {
		s += "/" + p.Proto()
	}
	return s
}

// Line is the publication as the envelope says it, host side first because
// that is the half the reader can open.
func (p Publication) Line() string {
	line := fmt.Sprintf("%s:%d -> %d", p.Addr(), p.HostPort, p.GuestPort)
	if p.Proto() != "tcp" {
		line += "/" + p.Proto()
	}
	if p.Addr() != loopback {
		line += " (reachable from the network this host is on)"
	}
	return line
}

// ParsePublication reads one published port, as --publish takes it and as
// `brig network publish` takes its bare words.
//
// The grammar is docker's, and deliberately: this is the one part of brig a
// person arrives at already knowing.
//
//	3000                 the same port on both sides, on loopback
//	8080:80              a host port and the guest port behind it
//	127.0.0.1:8080:80    an explicit host address
//	0.0.0.0:8080:80      offered beyond this machine
//	5353:53/udp          a protocol other than tcp
//
// Refused rather than guessed at, on the rule the security switches follow: a
// typo in the address half of this decides who can reach the sandbox, so it
// has to stop the run.
func ParsePublication(s string) (Publication, error) { return parsePublication(s, false) }

// ParseHostSide reads a port as `brig network unpublish` names one, by its
// host side.
//
// It takes every spelling ParsePublication does, and ADDR:PORT as well, which
// is how the HOST column of `brig network ls` prints a port. That form has no
// guest half, so the guest port is set to the host port. Matches never
// compares guest ports, so the value does not change what it names.
//
// --publish does not take ADDR:PORT. docker refuses it too, and publishing
// needs a guest port.
func ParseHostSide(s string) (Publication, error) { return parsePublication(s, true) }

func parsePublication(s string, hostSide bool) (Publication, error) {
	spec := strings.TrimSpace(s)
	if spec == "" {
		return Publication{}, errors.New("a published port cannot be empty, " +
			"for example 3000 or 8080:80")
	}
	var p Publication
	if rest, proto, ok := strings.Cut(spec, "/"); ok {
		spec = rest
		p.Protocol = strings.ToLower(proto)
		if p.Protocol != "tcp" && p.Protocol != "udp" {
			return Publication{}, fmt.Errorf("%s: %q is not a protocol brig publishes, use tcp or udp", s, proto)
		}
	}

	parts := strings.Split(spec, ":")
	var hostPort, guestPort string
	switch len(parts) {
	case 1:
		hostPort, guestPort = parts[0], parts[0]
	case 2:
		hostPort, guestPort = parts[0], parts[1]
		// The HOST column's ADDR:PORT. See ParseHostSide.
		if _, err := netip.ParseAddr(parts[0]); hostSide && err == nil {
			p.HostAddr, hostPort, guestPort = parts[0], parts[1], parts[1]
		}
	case 3:
		p.HostAddr, hostPort, guestPort = parts[0], parts[1], parts[2]
		if _, err := netip.ParseAddr(p.HostAddr); err != nil {
			return Publication{}, fmt.Errorf("%s: %q is not a host address; "+
				"publish on 127.0.0.1 to keep the port on this machine, or on 0.0.0.0 to "+
				"offer it to the network this host is on", s, p.HostAddr)
		}
	default:
		return Publication{}, fmt.Errorf("%s is not a port mapping; write it as PORT, "+
			"HOST:GUEST, or ADDR:HOST:GUEST with an IPv4 ADDR", s)
	}

	for _, f := range []struct {
		what string
		raw  string
		dst  *int
	}{{"host", hostPort, &p.HostPort}, {"guest", guestPort, &p.GuestPort}} {
		n, err := strconv.Atoi(f.raw)
		if err != nil || n < 1 || n > 65535 {
			return Publication{}, fmt.Errorf("%s: the %s port %q is not a number between 1 and 65535",
				s, f.what, f.raw)
		}
		*f.dst = n
	}
	return p, nil
}

// ParsePublications reads every published port on a command line, and refuses
// two that would want the same host listener.
//
// The duplicate is caught here rather than at the gateway, because the gateway
// sees them one at a time and would report the second as a conflict with
// something already published -- which is true, and says nothing about the
// command line that asked for both.
func ParsePublications(specs []string) ([]Publication, error) {
	return parsePublications(specs, ParsePublication)
}

// ParseHostSides is ParsePublications for the ports `brig network unpublish`
// names. See ParseHostSide.
func ParseHostSides(specs []string) ([]Publication, error) {
	return parsePublications(specs, ParseHostSide)
}

func parsePublications(specs []string, parse func(string) (Publication, error)) ([]Publication, error) {
	var out []Publication
	for _, s := range specs {
		p, err := parse(s)
		if err != nil {
			return nil, err
		}
		for _, have := range out {
			if have.Overlaps(p) {
				// Name 0.0.0.0 when one of them is on it, since that listener
				// covers the other.
				listener := p.Local()
				if unspecified(have.Addr()) {
					listener = have.Local()
				}
				return nil, fmt.Errorf("%s and %s both want %s on the host; "+
					"one host port carries one guest port", have, p, listener)
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// Publisher is a runtime that can publish a guest port on the host while the
// sandbox runs, and take the publication away again.
//
// Optional, on the same terms as NetworkPruner. A container runtime fixes its
// published ports when the container is created and has no way to add one
// afterwards, so it implements none of this and cmd/brig says why rather than
// calling a stub that always fails.
type Publisher interface {
	// Publish offers a guest port on the host, and Unpublish withdraws one.
	Publish(name string, p Publication) error
	Unpublish(name string, p Publication) error
	// Published is what this sandbox is offering, as the gateway serving it
	// reports, not as brig recorded it. The two agreeing is the thing worth
	// checking, so this asks the side that would be wrong.
	Published(name string) ([]Publication, error)
}
