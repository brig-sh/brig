package egress

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// The resolver reads bytes a guest chose, and an upstream answer. Neither may
// panic it.

func FuzzAnswer(f *testing.F) {
	f.Add(buildQuery(1, "api.anthropic.com", typeA))
	f.Add(buildQuery(2, "example.com", typeAAAA))
	f.Add([]byte{0, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0xc0, 12, 0, 1, 0, 1})
	p, err := Parse("deny", []string{"host=*.com"}, nil)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, query []byte) {
		r := &Resolver{
			Policy: p, Upstream: []string{"up:53"}, Pins: &fakePins{},
			exchange: func(_ context.Context, _, _ string, msg []byte) ([]byte, error) {
				return buildResponse(msg, []rr{aRecord("1.2.3.4", 300)}, nil), nil
			},
		}
		r.Answer(context.Background(), query, "udp", netip.Addr{})
	})
}

func FuzzInspectAnswer(f *testing.F) {
	query := buildQuery(1, "example.com", typeA)
	f.Add(buildResponse(query, []rr{aRecord("1.2.3.4", 300)}, []rr{{typeOPT, 0, nil}}))
	f.Add(buildResponse(query, nil, nil))
	q, err := parseQuery(query)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, resp []byte) {
		_, _, _ = inspectAnswer(resp, query, q, 60)
	})
}

func TestRefusalsAreLoggedOncePerName(t *testing.T) {
	var buf bytes.Buffer
	r := &Resolver{Policy: mustParse(t, "deny", nil, nil), Pins: &fakePins{}, Log: log.New(&buf, "", 0)}
	for i := 0; i < 50; i++ {
		r.Answer(context.Background(), buildQuery(uint16(i), "example.com", typeA), "udp", guest)
	}
	r.Answer(context.Background(), buildQuery(99, "example.org", typeA), "udp", guest)
	if n := strings.Count(buf.String(), "refused the query"); n != 2 {
		t.Errorf("logged %d refusals, want 2:\n%s", n, buf.String())
	}
}

// Every name a guest makes up is a new one, so a limit per name alone lets it
// write a line per query. The lines of one window are bounded, and so are the
// bytes over the resolver's life.
func TestGuestLinesHaveABudget(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	r := &Resolver{Policy: mustParse(t, "deny", nil, nil), Pins: &fakePins{}, Log: log.New(&buf, "", 0)}
	for i := 0; i < 500; i++ {
		r.Answer(ctx, buildQuery(uint16(i), fmt.Sprintf("n%d.example.com", i), typeA), "udp", guest)
	}
	if n := strings.Count(buf.String(), "refused the query"); n != guestLogBurst {
		t.Errorf("logged %d refusals in one window, want %d", n, guestLogBurst)
	}
	nextWindow := func() {
		r.mu.Lock()
		r.window = time.Now().Add(-guestLogEvery)
		r.mu.Unlock()
	}
	nextWindow()
	r.Answer(ctx, buildQuery(1, "late.example.com", typeA), "udp", guest)
	if !strings.Contains(buf.String(), "480 more refused or failed queries were not logged") {
		t.Errorf("the next window does not count what was dropped:\n%s", buf.String()[strings.LastIndex(buf.String(), "n499"):])
	}
	buf.Reset()
	r.logGuest("huge", "%s", strings.Repeat("x", guestLogBytes))
	nextWindow()
	r.Answer(ctx, buildQuery(2, "after.example.com", typeA), "udp", guest)
	if !strings.Contains(buf.String(), "is full") || strings.Contains(buf.String(), "after.example.com") {
		t.Error("the resolver logged past its byte budget")
	}
}
