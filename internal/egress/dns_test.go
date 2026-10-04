package egress

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/netip"
	"strings"
	"testing"
)

// buildQuery returns a standard query for name with RD set.
func buildQuery(id uint16, name string, qtype uint16) []byte {
	msg := make([]byte, headerLen)
	binary.BigEndian.PutUint16(msg, id)
	msg[2] = 0x01
	binary.BigEndian.PutUint16(msg[4:], 1)
	msg = appendName(msg, name)
	msg = binary.BigEndian.AppendUint16(msg, qtype)
	return binary.BigEndian.AppendUint16(msg, classIN)
}

func appendName(msg []byte, name string) []byte {
	for _, l := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if l == "" {
			continue
		}
		msg = append(msg, byte(len(l)))
		msg = append(msg, l...)
	}
	return append(msg, 0)
}

type rr struct {
	rtype uint16
	ttl   uint32
	data  []byte
}

// buildResponse answers query with the records, each named by a compression
// pointer to the question, but OPT, which the root owns.
func buildResponse(query []byte, answers, additional []rr) []byte {
	q, err := parseQuery(query)
	if err != nil {
		panic(err)
	}
	msg := append([]byte(nil), query[:q.end]...)
	msg[2] |= 0x80
	msg[3] = 0x80
	binary.BigEndian.PutUint16(msg[6:], uint16(len(answers)))
	binary.BigEndian.PutUint16(msg[10:], uint16(len(additional)))
	for _, r := range append(answers, additional...) {
		if r.rtype == typeOPT {
			msg = append(msg, 0)
		} else {
			msg = append(msg, 0xc0, headerLen)
		}
		msg = binary.BigEndian.AppendUint16(msg, r.rtype)
		msg = binary.BigEndian.AppendUint16(msg, classIN)
		msg = binary.BigEndian.AppendUint32(msg, r.ttl)
		msg = binary.BigEndian.AppendUint16(msg, uint16(len(r.data)))
		msg = append(msg, r.data...)
	}
	return msg
}

func aRecord(addr string, ttl uint32) rr {
	a := netip.MustParseAddr(addr).As4()
	return rr{typeA, ttl, a[:]}
}

func TestParseQuery(t *testing.T) {
	q, err := parseQuery(buildQuery(7, "Api.Example.com", typeA))
	if err != nil {
		t.Fatal(err)
	}
	if q.name != "Api.Example.com" || q.qtype != typeA {
		t.Errorf("got %q type %d", q.name, q.qtype)
	}
}

func TestParseQueryRefusesMalformed(t *testing.T) {
	good := buildQuery(7, "example.com", typeA)
	twoQ := append([]byte(nil), good...)
	binary.BigEndian.PutUint16(twoQ[4:], 2)
	resp := append([]byte(nil), good...)
	resp[2] |= 0x80
	// A label holding a dot would match "api.example.com" in the policy and
	// ask upstream for a different, one-label name.
	dotted := buildQuery(7, "x", typeA)
	dotted = append(dotted[:headerLen], append([]byte{15}, "api.example.com"...)...)
	dotted = append(dotted, 0, 0, 1, 0, 1)
	loop := append([]byte(nil), good[:headerLen]...)
	loop = append(loop, 0xc0, headerLen, 0, 1, 0, 1)
	for name, msg := range map[string][]byte{
		"short":     good[:5],
		"two":       twoQ,
		"response":  resp,
		"truncated": good[:len(good)-2],
		"dotted":    dotted,
		"loop":      loop,
	} {
		if _, err := parseQuery(msg); err == nil {
			t.Errorf("%s: parseQuery succeeded", name)
		}
	}
}

// A question name that is a compression pointer is refused, and never
// forwarded. brig forwards the query's bytes with a fresh ID, and a question
// name that resolved through the ID bytes would read as an allowed name to
// brig and, once the ID changed, as another name to the upstream. That is a
// lookup the policy did not allow, sent to whatever server answers it.
func TestParseQueryRefusesACompressedQuestion(t *testing.T) {
	// QNAME at offset 12 is a pointer to the header, where the ID bytes are
	// read as a second pointer to an allowed name planted later. brig must
	// not read a name here at all.
	msg := make([]byte, 280)
	msg[0], msg[1] = 0xc0, 0xfa // ID; as a pointer, to offset 250
	msg[2] = 0x01               // RD
	msg[5] = 0x01               // QDCOUNT = 1
	msg[12], msg[13] = 0xc0, 0x00
	msg[14], msg[15], msg[16], msg[17] = 0x00, 0x01, 0x00, 0x01
	copy(msg[250:], append([]byte{7}, "allowed"...))
	msg[258] = 3
	copy(msg[259:], "com")
	msg[262] = 0
	if _, err := parseQuery(msg); err == nil {
		t.Fatal("parseQuery read a name from a compressed question")
	}

	var forwarded bool
	r := &Resolver{
		Policy:   mustParse(t, "deny", []string{"host=allowed.com"}, nil),
		Upstream: []string{"up:53"},
		Pins:     &fakePins{},
		exchange: func(_ context.Context, _, _ string, _ []byte) ([]byte, error) {
			forwarded = true
			return buildResponse(buildQuery(1, "allowed.com", typeA), nil, nil), nil
		},
	}
	if resp := r.Answer(context.Background(), msg, "udp", guest); rcode(resp) != rcodeFormErr {
		t.Errorf("a compressed question got rcode %d, want FORMERR", rcode(resp))
	}
	if forwarded {
		t.Error("a compressed question was forwarded upstream")
	}
}

// A query's bytes after the question must be a well-formed tail and nothing
// more, because brig forwards them. An EDNS OPT record is fine; answer or
// authority records, and trailing bytes after the records, are refused.
func TestParseQueryChecksTheTail(t *testing.T) {
	good := buildQuery(7, "example.com", typeA)

	// A single EDNS OPT record: root name, type 41, UDP size, and no rdata.
	withOPT := append([]byte(nil), good...)
	binary.BigEndian.PutUint16(withOPT[10:], 1) // ARCOUNT
	opt := []byte{0, 0, 41, 16, 0, 0, 0, 0, 0, 0, 0}
	withOPT = append(withOPT, opt...)
	if _, err := parseQuery(withOPT); err != nil {
		t.Errorf("parseQuery refused a query with an EDNS OPT record: %v", err)
	}

	trailing := append(append([]byte(nil), good...), 0xc1, 0xc1, 0xc1)
	answer := append([]byte(nil), good...)
	binary.BigEndian.PutUint16(answer[6:], 1) // ANCOUNT, with no record to back it
	shortAR := append([]byte(nil), good...)
	binary.BigEndian.PutUint16(shortAR[10:], 1) // ARCOUNT, with no record
	for name, msg := range map[string][]byte{
		"trailing":       trailing,
		"answer record":  answer,
		"missing record": shortAR,
	} {
		if _, err := parseQuery(msg); err == nil {
			t.Errorf("%s: parseQuery accepted it", name)
		}
	}
}

func TestReply(t *testing.T) {
	query := buildQuery(0xbeef, "example.com", typeAAAA)
	q, _ := parseQuery(query)
	r := reply(query, q, rcodeRefused)
	if r[0] != 0xbe || r[1] != 0xef || r[2]&0x80 == 0 || r[2]&0x01 == 0 || r[3]&0x0f != rcodeRefused {
		t.Errorf("header %x", r[:4])
	}
	if binary.BigEndian.Uint16(r[4:]) != 1 || binary.BigEndian.Uint16(r[6:]) != 0 {
		t.Errorf("counts %x", r[4:12])
	}
}

const typeAAAA = 28

func TestInspectAnswerPinsAAndRewritesTTL(t *testing.T) {
	query := buildQuery(1, "example.com", typeA)
	q, _ := parseQuery(query)
	resp := buildResponse(query,
		[]rr{{typeCNAME, 300, appendName(nil, "edge.example.net")}, aRecord("93.184.216.34", 300), aRecord("93.184.216.35", 5)},
		[]rr{aRecord("10.9.9.9", 300), {typeOPT, 0, nil}})
	out, addrs, err := inspectAnswer(resp, query, q, 60)
	if err != nil {
		t.Fatal(err)
	}
	// Additional records are not pinned: only the answer counts.
	if len(addrs) != 2 || addrs[0].String() != "93.184.216.34" || addrs[1].String() != "93.184.216.35" {
		t.Errorf("addrs = %v", addrs)
	}
	// Three answers and the OPT record are left.
	off := q.end
	for i := 0; i < 4; i++ {
		_, next, _ := nameLabels(out, off, true)
		rtype := binary.BigEndian.Uint16(out[next:])
		ttl := binary.BigEndian.Uint32(out[next+4:])
		if rtype != typeOPT && ttl != 60 {
			t.Errorf("record %d type %d has ttl %d", i, rtype, ttl)
		}
		off = next + 10 + int(binary.BigEndian.Uint16(out[next+8:]))
	}
	if off != len(out) {
		t.Errorf("%d bytes follow the last record", len(out)-off)
	}
}

// An MX answer carries the address of its mail host as glue. The resolver
// judges only the addresses of the name the guest asked for, so the guest
// gets no glue, and asks for the A record it needs.
func TestInspectAnswerDropsTheGlue(t *testing.T) {
	query := buildQuery(1, "evil.example", typeMX)
	q, _ := parseQuery(query)
	mx := append([]byte{0, 10}, appendName(nil, "mail.evil.example")...)
	resp := buildResponse(query, []rr{{typeMX, 300, mx}},
		[]rr{aRecord("6.6.6.6", 300), {typeOPT, 0, nil}})
	out, addrs, err := inspectAnswer(resp, query, q, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 0 {
		t.Errorf("addrs = %v", addrs)
	}
	if got := binary.BigEndian.Uint16(out[10:]); got != 1 {
		t.Errorf("ARCOUNT = %d, want the OPT record alone", got)
	}
	if glue := netip.MustParseAddr("6.6.6.6").As4(); bytes.Contains(out, glue[:]) {
		t.Error("the glue address reached the guest")
	}
}

func TestInspectAnswerRefusesMismatch(t *testing.T) {
	query := buildQuery(1, "example.com", typeA)
	q, _ := parseQuery(query)
	other := buildResponse(buildQuery(1, "evil.test", typeA), []rr{aRecord("1.2.3.4", 1)}, nil)
	wrongID := buildResponse(buildQuery(2, "example.com", typeA), []rr{aRecord("1.2.3.4", 1)}, nil)
	short := buildResponse(query, []rr{aRecord("1.2.3.4", 1)}, nil)
	short = short[:len(short)-2]
	for name, resp := range map[string][]byte{"name": other, "id": wrongID, "short": short} {
		if _, _, err := inspectAnswer(resp, query, q, 0); err == nil {
			t.Errorf("%s: inspectAnswer succeeded", name)
		}
	}
}

// A '/' stops a '*' in a glob from matching, so "a/b.evil.com" would escape
// a deny rule for "*.evil.com". A name past 255 octets is not a DNS name.
func TestParseQueryRefusesNamesTheGlobsMisread(t *testing.T) {
	long := strings.Repeat(strings.Repeat("a", 63)+".", 4) + "com"
	for name, qname := range map[string]string{
		"slash": "a/b.evil.com",
		"space": "a b.evil.com",
		"star":  "*.evil.com",
		"long":  long,
	} {
		if _, err := parseQuery(buildQuery(1, qname, typeA)); err == nil {
			t.Errorf("%s: parseQuery took %q", name, qname)
		}
	}
	for _, qname := range []string{
		strings.Repeat(strings.Repeat("a", 62)+".", 4),
		"_sip._tcp.Example-1.com",
	} {
		if _, err := parseQuery(buildQuery(1, qname, typeA)); err != nil {
			t.Errorf("parseQuery refused %q: %v", qname, err)
		}
	}
}

// DNSSEC owner names hold any byte, as in the "\000." names some signers
// answer NSEC with. A response that carries one is read, and its A records
// pinned.
func TestInspectAnswerReadsAnyOwnerName(t *testing.T) {
	query := buildQuery(1, "example.com", typeA)
	q, _ := parseQuery(query)
	resp := buildResponse(query, []rr{aRecord("93.184.216.34", 300)}, nil)
	binary.BigEndian.PutUint16(resp[8:], 1)
	resp = append(resp, 1, 0, 0xc0, headerLen)
	resp = binary.BigEndian.AppendUint16(resp, 47)
	resp = binary.BigEndian.AppendUint16(resp, classIN)
	resp = binary.BigEndian.AppendUint32(resp, 300)
	resp = binary.BigEndian.AppendUint16(resp, 0)
	_, addrs, err := inspectAnswer(resp, query, q, 60)
	if err != nil || len(addrs) != 1 {
		t.Errorf("addrs = %v, %v", addrs, err)
	}
}
