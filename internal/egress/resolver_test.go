package egress

import (
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"
)

type fakePins struct {
	mu        sync.Mutex
	sets      map[string][]netip.Addr
	lifetimes map[string]time.Duration
	err       error
}

func (f *fakePins) Pin(set string, addrs []netip.Addr, lifetime time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if f.sets == nil {
		f.sets = map[string][]netip.Addr{}
		f.lifetimes = map[string]time.Duration{}
	}
	f.sets[set] = append(f.sets[set], addrs...)
	f.lifetimes[set] = lifetime
	return nil
}

// upstreamAnswering answers every A query with addr and counts the queries.
func upstreamAnswering(addr string, asked *int) func(context.Context, string, string, []byte) ([]byte, error) {
	return func(_ context.Context, _, _ string, msg []byte) ([]byte, error) {
		*asked++
		return buildResponse(msg, []rr{aRecord(addr, 300)}, nil), nil
	}
}

func rcode(resp []byte) byte { return resp[3] & 0x0f }

func ancount(resp []byte) int { return int(resp[6])<<8 | int(resp[7]) }

var guest = netip.MustParseAddr("10.4.1.2")

func TestAnswerUnderDenyDefault(t *testing.T) {
	pins := &fakePins{}
	asked := 0
	r := &Resolver{
		Policy:   mustParse(t, "deny", []string{"host=api.anthropic.com"}, nil),
		Upstream: []string{"up:53"},
		Pins:     pins,
		exchange: upstreamAnswering("160.79.104.10", &asked),
	}
	ctx := context.Background()

	resp := r.Answer(ctx, buildQuery(1, "api.anthropic.com", typeA), "udp", guest)
	if rcode(resp) != 0 || ancount(resp) != 1 {
		t.Fatalf("allowed name: rcode %d, %d answers", rcode(resp), ancount(resp))
	}
	if got := pins.sets[SetAllowIP]; len(got) != 1 || got[0].String() != "160.79.104.10" {
		t.Errorf("allow pins = %v", got)
	}

	resp = r.Answer(ctx, buildQuery(2, "example.com", typeA), "udp", guest)
	if rcode(resp) != rcodeRefused || asked != 1 {
		t.Errorf("unlisted name: rcode %d, upstream asked %d times", rcode(resp), asked)
	}

	resp = r.Answer(ctx, buildQuery(3, "api.anthropic.com", typeAAAA), "udp", guest)
	if rcode(resp) != 0 || ancount(resp) != 0 || asked != 1 {
		t.Errorf("AAAA: rcode %d, %d answers, upstream asked %d times", rcode(resp), ancount(resp), asked)
	}
}

func TestAnswerUnderAllowDefaultPinsDenied(t *testing.T) {
	pins := &fakePins{}
	asked := 0
	r := &Resolver{
		Policy:   mustParse(t, "allow", nil, []string{"host=example.com"}),
		Upstream: []string{"up:53"},
		Pins:     pins,
		exchange: upstreamAnswering("93.184.216.34", &asked),
	}
	resp := r.Answer(context.Background(), buildQuery(1, "example.com", typeA), "udp", guest)
	if rcode(resp) != 0 || ancount(resp) != 1 {
		t.Fatalf("rcode %d, %d answers", rcode(resp), ancount(resp))
	}
	if len(pins.sets[SetDenyIP]) != 1 || len(pins.sets[SetAllowIP]) != 0 {
		t.Errorf("pins = %v", pins.sets)
	}
	// A name no rule covers needs no pin under an allow default.
	r.Answer(context.Background(), buildQuery(2, "github.com", typeA), "udp", guest)
	if len(pins.sets[SetAllowIP]) != 0 {
		t.Errorf("allow pins = %v", pins.sets[SetAllowIP])
	}
}

// Under an allow default an A query for a denied name pins its address as
// denied. An MX query must not hand the guest an address as glue instead.
func TestAnswerHandsOutNoGlue(t *testing.T) {
	r := &Resolver{
		Policy:   mustParse(t, "allow", nil, []string{"host=evil.example"}),
		Upstream: []string{"up:53"},
		Pins:     &fakePins{},
		exchange: func(_ context.Context, _, _ string, msg []byte) ([]byte, error) {
			mx := append([]byte{0, 10}, appendName(nil, "mail.evil.example")...)
			return buildResponse(msg, []rr{{typeMX, 300, mx}}, []rr{aRecord("6.6.6.6", 300)}), nil
		},
	}
	resp := r.Answer(context.Background(), buildQuery(1, "evil.example", typeMX), "udp", guest)
	if rcode(resp) != 0 || ancount(resp) != 1 || binary.BigEndian.Uint16(resp[10:]) != 0 {
		t.Errorf("rcode %d, %d answers, %d additional", rcode(resp), ancount(resp), binary.BigEndian.Uint16(resp[10:]))
	}
}

func TestAnswerFailsClosedWhenPinFails(t *testing.T) {
	asked := 0
	r := &Resolver{
		Policy:   mustParse(t, "deny", []string{"host=api.anthropic.com"}, nil),
		Upstream: []string{"up:53"},
		Pins:     &fakePins{err: errors.New("nft: no such table")},
		exchange: upstreamAnswering("160.79.104.10", &asked),
	}
	resp := r.Answer(context.Background(), buildQuery(1, "api.anthropic.com", typeA), "udp", guest)
	if rcode(resp) != rcodeServFail || ancount(resp) != 0 {
		t.Errorf("rcode %d, %d answers", rcode(resp), ancount(resp))
	}
}

func TestAnswerTriesEachUpstream(t *testing.T) {
	var tried []string
	r := &Resolver{
		Policy:   mustParse(t, "allow", nil, nil),
		Upstream: []string{"a:53", "b:53"},
		Pins:     &fakePins{},
		exchange: func(_ context.Context, _, server string, msg []byte) ([]byte, error) {
			tried = append(tried, server)
			if server == "a:53" {
				return nil, errors.New("timeout")
			}
			return buildResponse(msg, []rr{aRecord("1.2.3.4", 300)}, nil), nil
		},
	}
	resp := r.Answer(context.Background(), buildQuery(1, "example.com", typeA), "udp", guest)
	if rcode(resp) != 0 || len(tried) != 2 {
		t.Errorf("rcode %d, tried %v", rcode(resp), tried)
	}
}

func TestAnswerDropsResponsesAndAnswersGarbageWithFormErr(t *testing.T) {
	r := &Resolver{Policy: mustParse(t, "deny", nil, nil), Pins: &fakePins{}}
	resp := buildResponse(buildQuery(1, "example.com", typeA), nil, nil)
	if got := r.Answer(context.Background(), resp, "udp", guest); got != nil {
		t.Errorf("answered a response: %x", got)
	}
	bad := buildQuery(1, "example.com", typeA)
	bad[5] = 3
	if got := r.Answer(context.Background(), bad, "udp", guest); got == nil || rcode(got) != rcodeFormErr {
		t.Errorf("malformed query: %x", got)
	}
	status := buildQuery(1, "example.com", typeA)
	status[2] |= 2 << 3
	if got := r.Answer(context.Background(), status, "udp", guest); got == nil || rcode(got) != rcodeNotImp {
		t.Errorf("status query: %x", got)
	}
}

// answerTTL is the TTL of the first answer record of a response that names it
// by a pointer.
func answerTTL(t *testing.T, resp []byte) uint32 {
	t.Helper()
	q, err := readQuestion(resp, headerLen)
	if err != nil {
		t.Fatal(err)
	}
	return binary.BigEndian.Uint32(resp[q.end+6:])
}

// A pin outlives the TTL the guest is told by the grace, so a guest that
// connects on an answer about to expire still gets through. Without host
// rules nothing is pinned, and the TTL is upstream's.
func TestAnswerPinsForTheTTLAndTheGrace(t *testing.T) {
	asked := 0
	pins := &fakePins{}
	r := &Resolver{
		Policy:   mustParse(t, "deny", []string{"host=example.com"}, nil),
		Upstream: []string{"up:53"},
		Pins:     pins,
		exchange: upstreamAnswering("93.184.216.34", &asked),
	}
	resp := r.Answer(context.Background(), buildQuery(1, "example.com", typeA), "udp", guest)
	if got := answerTTL(t, resp); got != uint32(PinTTL/time.Second) {
		t.Errorf("TTL %d, want %d", got, PinTTL/time.Second)
	}
	if got := pins.lifetimes[SetAllowIP]; got != PinTTL+PinGrace {
		t.Errorf("pinned for %s, want %s", got, PinTTL+PinGrace)
	}
	r.Policy = mustParse(t, "deny", []string{"cidr=93.184.216.0/24"}, nil)
	resp = r.Answer(context.Background(), buildQuery(2, "example.com", typeA), "udp", guest)
	if rcode(resp) != rcodeRefused {
		t.Errorf("a name under a cidr-only deny policy: rcode %d", rcode(resp))
	}
	r.Policy = mustParse(t, "allow", nil, []string{"cidr=10.0.0.0/8"})
	resp = r.Answer(context.Background(), buildQuery(3, "example.com", typeA), "udp", guest)
	if got := answerTTL(t, resp); got != 300 {
		t.Errorf("TTL %d with no host rules, want upstream's 300", got)
	}
}

// The guest picks the ID of its query. The upstream query carries another,
// so a forged answer has to guess it, and the guest gets its own ID back.
func TestUpstreamQueriesCarryAnIDOfTheirOwn(t *testing.T) {
	var ids []uint16
	r := &Resolver{
		Policy:   mustParse(t, "allow", nil, nil),
		Upstream: []string{"up:53"},
		Pins:     &fakePins{},
		exchange: func(_ context.Context, _, _ string, msg []byte) ([]byte, error) {
			ids = append(ids, binary.BigEndian.Uint16(msg))
			return buildResponse(msg, []rr{aRecord("1.2.3.4", 300)}, nil), nil
		},
	}
	for i := 0; i < 4; i++ {
		resp := r.Answer(context.Background(), buildQuery(0x1234, "example.com", typeA), "udp", guest)
		if got := binary.BigEndian.Uint16(resp); got != 0x1234 || rcode(resp) != 0 {
			t.Fatalf("answer ID %#x, rcode %d", got, rcode(resp))
		}
	}
	if !slices.ContainsFunc(ids, func(id uint16) bool { return id != 0x1234 }) {
		t.Errorf("every upstream query carried the guest's ID: %#x", ids)
	}
}

func TestAnswerRefusesAnUpstreamAnswerWithAnotherID(t *testing.T) {
	r := &Resolver{
		Policy:   mustParse(t, "allow", nil, nil),
		Upstream: []string{"up:53"},
		Pins:     &fakePins{},
		exchange: func(_ context.Context, _, _ string, msg []byte) ([]byte, error) {
			resp := buildResponse(msg, []rr{aRecord("6.6.6.6", 300)}, nil)
			resp[1] ^= 1
			return resp, nil
		},
	}
	resp := r.Answer(context.Background(), buildQuery(1, "example.com", typeA), "udp", guest)
	if rcode(resp) != rcodeServFail || ancount(resp) != 0 {
		t.Errorf("rcode %d, %d answers", rcode(resp), ancount(resp))
	}
}
