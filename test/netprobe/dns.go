package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
)

// The DNS wire format, only as much as a probe needs: one question out, and
// the reply header back. The alternate resolver, DoT and DoH cases all carry
// the same message, and the standard library has no exported encoder for it.

const (
	typeA   = 1
	classIN = 1
)

const (
	rcodeNXDomain = 3
	rcodeRefused  = 5
)

var rcodeNames = map[int]string{
	0: "NOERROR", 1: "FORMERR", 2: "SERVFAIL", rcodeNXDomain: "NXDOMAIN", 4: "NOTIMP", rcodeRefused: "REFUSED",
}

func rcodeName(rcode int) string {
	if s, ok := rcodeNames[rcode]; ok {
		return s
	}
	return fmt.Sprintf("RCODE%d", rcode)
}

// newID draws a DNS message ID. The ID only has to match its reply, so it
// comes from math/rand/v2. crypto/rand blocks until the kernel's random pool
// is ready, and in a guest that has just booted that can take minutes.
func newID() uint16 {
	return uint16(rand.Uint32())
}

// buildQuery encodes one recursive question for name. A name DNS cannot carry
// is refused here, so a typo in a case never reaches the wire as a malformed
// packet the far end then rejects.
func buildQuery(id uint16, name string, qtype uint16) ([]byte, error) {
	name = strings.TrimSuffix(name, ".")
	if name == "" {
		return nil, errors.New("empty name")
	}
	if len(name) > 253 {
		return nil, fmt.Errorf("name %q is longer than 253 bytes", name)
	}
	b := make([]byte, 12, 12+len(name)+6)
	binary.BigEndian.PutUint16(b[0:], id)
	b[2] = 0x01 // RD
	binary.BigEndian.PutUint16(b[4:], 1)
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 {
			return nil, fmt.Errorf("name %q has a label of %d bytes", name, len(label))
		}
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	b = append(b, 0)
	b = binary.BigEndian.AppendUint16(b, qtype)
	b = binary.BigEndian.AppendUint16(b, classIN)
	return b, nil
}

type reply struct {
	rcode   int
	answers int
}

func (r reply) String() string {
	return fmt.Sprintf("%s with %d answers", rcodeName(r.rcode), r.answers)
}

// parseReply reads the header of a reply to the query with id. The records
// themselves are not needed: that a resolver answered at all is the finding.
func parseReply(b []byte, id uint16) (reply, error) {
	if len(b) < 12 {
		return reply{}, fmt.Errorf("reply of %d bytes is shorter than a DNS header", len(b))
	}
	if got := binary.BigEndian.Uint16(b[0:]); got != id {
		return reply{}, fmt.Errorf("reply ID %d does not match query ID %d", got, id)
	}
	if b[2]&0x80 == 0 {
		return reply{}, errors.New("message is a query, not a reply")
	}
	return reply{rcode: int(b[3] & 0x0f), answers: int(binary.BigEndian.Uint16(b[6:]))}, nil
}
