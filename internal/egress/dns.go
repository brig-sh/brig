package egress

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// The DNS wire format, as far as the resolver needs it. brig keeps its
// dependencies to three, so this reads and rewrites messages by hand instead
// of pulling in a DNS library. It never builds a record. It forwards the
// upstream answer, rewrites the TTLs and reads the A records out of it.

const headerLen = 12

// Record types the resolver forwards. hull's gateway answers these and gives
// every other type an empty answer. AAAA is among the others: the sandbox
// network carries no IPv6.
const (
	typeA     = 1
	typeNS    = 2
	typeCNAME = 5
	typeMX    = 15
	typeTXT   = 16
	typeSRV   = 33
	typeOPT   = 41
	classIN   = 1
)

const (
	rcodeFormErr  = 1
	rcodeServFail = 2
	rcodeNotImp   = 4
	rcodeRefused  = 5
)

// maxPointers bounds how many compression pointers one name may follow, so a
// message whose pointers form a loop is an error.
const maxPointers = 32

// maxNameLen is the longest name DNS allows, in octets on the wire.
const maxNameLen = 255

var (
	errShort  = errors.New("message is truncated")
	errOpcode = errors.New("not a standard query")
)

// question is the single question of a query.
type question struct {
	name  string
	qtype uint16
	// end is the offset just past the question.
	end int
}

// forwarded returns whether the resolver asks upstream for this type.
func (q question) forwarded() bool {
	switch q.qtype {
	case typeA, typeNS, typeCNAME, typeMX, typeTXT, typeSRV:
		return true
	}
	return false
}

// parseQuery reads the question of a standard query with exactly one
// question.
func parseQuery(msg []byte) (question, error) {
	if len(msg) < headerLen {
		return question{}, errShort
	}
	if msg[2]&0x80 != 0 {
		return question{}, errors.New("message is a response")
	}
	if opcode := (msg[2] >> 3) & 0x0f; opcode != 0 {
		return question{}, fmt.Errorf("opcode %d: %w", opcode, errOpcode)
	}
	if n := binary.BigEndian.Uint16(msg[4:]); n != 1 {
		return question{}, fmt.Errorf("query carries %d questions", n)
	}
	q, err := readQuestion(msg, headerLen)
	if err != nil {
		return question{}, err
	}
	if err := checkQueryTail(msg, q.end); err != nil {
		return question{}, err
	}
	return q, nil
}

// checkQueryTail reports that the bytes after the question are a well-formed
// tail and nothing more. A query carries no answer or authority records, and
// its additional records, such as an EDNS OPT, account for every remaining
// byte. The resolver forwards the query's own bytes, so a byte it did not
// account for is a byte it did not judge. Record owner names are read without
// compression, which no query uses, so none can be made to read differently
// once the forwarded ID changes.
func checkQueryTail(msg []byte, off int) error {
	if binary.BigEndian.Uint16(msg[6:]) != 0 || binary.BigEndian.Uint16(msg[8:]) != 0 {
		return errors.New("a query carries answer or authority records")
	}
	records := int(binary.BigEndian.Uint16(msg[10:]))
	for i := 0; i < records; i++ {
		_, next, err := nameLabels(msg, off, false)
		if err != nil {
			return err
		}
		if next+10 > len(msg) {
			return errShort
		}
		off = next + 10 + int(binary.BigEndian.Uint16(msg[next+8:]))
		if off > len(msg) {
			return errShort
		}
	}
	if off != len(msg) {
		return errors.New("a query carries trailing bytes after its records")
	}
	return nil
}

func readQuestion(msg []byte, off int) (question, error) {
	name, off, err := readName(msg, off)
	if err != nil {
		return question{}, err
	}
	if off+4 > len(msg) {
		return question{}, errShort
	}
	return question{name: name, qtype: binary.BigEndian.Uint16(msg[off:]), end: off + 4}, nil
}

// readName reads the name at off and returns it with the offset just past it.
//
// A label holds letters, digits, '-' and '_' alone. A label with a dot would
// read as a different name to the policy than the one the upstream resolver
// is asked for. A '/' would stop a '*' in a glob from matching it, so a name
// under a denied glob could escape it.
func readName(msg []byte, off int) (string, int, error) {
	labels, end, err := nameLabels(msg, off, false)
	if err != nil {
		return "", 0, err
	}
	names := make([]string, len(labels))
	for i, label := range labels {
		for _, c := range label {
			if !hostnameByte(c) {
				return "", 0, fmt.Errorf("label %q is not a plain hostname label", label)
			}
		}
		names[i] = string(label)
	}
	return strings.Join(names, "."), end, nil
}

func hostnameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

// nameLabels returns the labels of the name at off, and the offset just past
// it. It checks the structure of the name and not its bytes, so it also reads
// the owner names of records, which DNSSEC fills with any byte.
//
// compress says whether a compression pointer is allowed. A question name is
// read with it false. A question is the first name in the message, so a
// pointer in it can only point at the header or at bytes later in the
// message, which no well-formed client sends. brig forwards the query's own
// bytes with a fresh ID, so a question name that resolved through the ID
// bytes would read as one name to brig and another to the upstream once the
// ID changed. The owner names of records in a response are read with it true,
// where compression is ordinary and brig rewrites nothing they depend on.
func nameLabels(msg []byte, off int, compress bool) ([][]byte, int, error) {
	var labels [][]byte
	end := -1
	wire := 1
	for pointers := 0; ; {
		if off >= len(msg) {
			return nil, 0, errShort
		}
		n := int(msg[off])
		switch {
		case n == 0:
			if end < 0 {
				end = off + 1
			}
			return labels, end, nil
		case n&0xc0 == 0xc0:
			if !compress {
				return nil, 0, errors.New("a question name is compressed")
			}
			if off+2 > len(msg) {
				return nil, 0, errShort
			}
			if end < 0 {
				end = off + 2
			}
			if pointers++; pointers > maxPointers {
				return nil, 0, errors.New("name has a compression loop")
			}
			off = int(binary.BigEndian.Uint16(msg[off:]) & 0x3fff)
		case n&0xc0 != 0:
			return nil, 0, fmt.Errorf("label type %#x is not supported", n&0xc0)
		default:
			if off+1+n > len(msg) {
				return nil, 0, errShort
			}
			if wire += 1 + n; wire > maxNameLen {
				return nil, 0, fmt.Errorf("name is longer than %d octets", maxNameLen)
			}
			labels = append(labels, msg[off+1:off+1+n])
			off += 1 + n
		}
	}
}

// reply returns a response to query that carries the question and no records.
func reply(query []byte, q question, rcode byte) []byte {
	out := make([]byte, q.end)
	copy(out, query[:q.end])
	// QR set; opcode and RD copied from the query; AA and TC clear.
	out[2] = 0x80 | query[2]&0x79
	// RA set, Z clear.
	out[3] = 0x80 | rcode&0x0f
	binary.BigEndian.PutUint16(out[4:], 1)
	binary.BigEndian.PutUint16(out[6:], 0)
	binary.BigEndian.PutUint16(out[8:], 0)
	binary.BigEndian.PutUint16(out[10:], 0)
	return out
}

// headerReply returns a response to a query whose question could not be
// read. It carries the header alone.
func headerReply(query []byte, rcode byte) []byte {
	out := make([]byte, headerLen)
	copy(out, query[:headerLen])
	out[2] = 0x80 | query[2]&0x79
	out[3] = 0x80 | rcode&0x0f
	clear(out[4:])
	return out
}

// inspectAnswer checks that resp answers query and returns the response the
// guest gets, with the IPv4 addresses of the A records in its answer section.
// With ttl above zero, it also rewrites the TTL of every record but OPT to
// ttl, in place.
//
// The resolver advertises the same lifetime that it pins an address for. A
// guest that honors the TTL then asks again before the pin lapses.
//
// The guest's response drops the additional section but its OPT record. An
// MX or SRV answer carries the addresses of the hosts it names there, and
// the resolver judges only the addresses of the name the guest asked for.
// So a guest that wants an address asks for its A record, as on hull's
// gateway, which answers with no additional records.
func inspectAnswer(resp, query []byte, q question, ttl uint32) ([]byte, []netip.Addr, error) {
	if len(resp) < headerLen {
		return nil, nil, errShort
	}
	if resp[0] != query[0] || resp[1] != query[1] {
		return nil, nil, errors.New("response ID does not match the query")
	}
	if resp[2]&0x80 == 0 {
		return nil, nil, errors.New("upstream sent a query, not a response")
	}
	if n := binary.BigEndian.Uint16(resp[4:]); n != 1 {
		return nil, nil, fmt.Errorf("response carries %d questions", n)
	}
	rq, err := readQuestion(resp, headerLen)
	if err != nil {
		return nil, nil, err
	}
	if !strings.EqualFold(rq.name, q.name) || rq.qtype != q.qtype {
		return nil, nil, fmt.Errorf("response is for %s type %d, not %s type %d", rq.name, rq.qtype, q.name, q.qtype)
	}
	answers := int(binary.BigEndian.Uint16(resp[6:]))
	kept := answers + int(binary.BigEndian.Uint16(resp[8:]))
	total := kept + int(binary.BigEndian.Uint16(resp[10:]))
	var addrs []netip.Addr
	var opt [][]byte
	cut := -1
	off := rq.end
	for i := 0; i < total; i++ {
		if i == kept {
			cut = off
		}
		_, next, err := nameLabels(resp, off, true)
		if err != nil {
			return nil, nil, err
		}
		if next+10 > len(resp) {
			return nil, nil, errShort
		}
		rtype := binary.BigEndian.Uint16(resp[next:])
		class := binary.BigEndian.Uint16(resp[next+2:])
		rdlen := int(binary.BigEndian.Uint16(resp[next+8:]))
		rdata := next + 10
		if rdata+rdlen > len(resp) {
			return nil, nil, errShort
		}
		if ttl > 0 && rtype != typeOPT {
			binary.BigEndian.PutUint32(resp[next+4:], ttl)
		}
		if i < answers && rtype == typeA && class == classIN && rdlen == 4 {
			addrs = append(addrs, netip.AddrFrom4([4]byte(resp[rdata:rdata+4])))
		}
		// An OPT record is owned by the root, a single zero byte, so it
		// carries no pointer into the records that are dropped.
		if i >= kept && rtype == typeOPT && next == off+1 {
			opt = append(opt, resp[off:rdata+rdlen])
		}
		off = rdata + rdlen
	}
	if cut < 0 {
		return resp, addrs, nil
	}
	out := append([]byte(nil), resp[:cut]...)
	for _, r := range opt {
		out = append(out, r...)
	}
	binary.BigEndian.PutUint16(out[10:], uint16(len(opt)))
	return out, addrs, nil
}
