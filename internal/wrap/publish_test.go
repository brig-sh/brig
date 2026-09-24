package wrap

import (
	"bytes"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
)

func publications(t *testing.T, specs ...string) []runtime.Publication {
	t.Helper()
	ps, err := runtime.ParsePublications(specs)
	if err != nil {
		t.Fatalf("ParsePublications(%v): %v", specs, err)
	}
	return ps
}

// A published port is a hole in the sandbox boundary, so the block that names
// the boundary names it. Every one of them, not only the ones this command
// line asked for.
func TestEnvelopeNamesEveryPublishedPort(t *testing.T) {
	c := envelopeConfig()
	c.Publish = publications(t, "8080:80", "0.0.0.0:443:443")
	block := &bytes.Buffer{}
	c.renderEnvelope(block, creds.Set{})
	got := block.String()

	if !strings.Contains(got, "PORTS") {
		t.Fatalf("no PORTS row:\n%s", got)
	}
	for _, want := range []string{"127.0.0.1:8080 -> 80", "0.0.0.0:443 -> 443"} {
		if !strings.Contains(got, want) {
			t.Errorf("the block does not name %q:\n%s", want, got)
		}
	}
	// A port offered beyond this machine says so, because that is the wider
	// hole and the one a reader has to notice.
	if !strings.Contains(got, "reachable from the network this host is on") {
		t.Errorf("0.0.0.0 is not said out loud:\n%s", got)
	}
}

// A run that publishes nothing has no row. A row reading "none" on every run
// is a row people learn to skip, which is what would make the ones that matter
// invisible.
func TestEnvelopeHasNoPortsRowWhenNothingIsPublished(t *testing.T) {
	c := envelopeConfig()
	block := &bytes.Buffer{}
	c.renderEnvelope(block, creds.Set{})
	if strings.Contains(block.String(), "PORTS") {
		t.Errorf("a run with no published ports still prints the row:\n%s", block.String())
	}
}

// A --publish on this line is laid over what the sandbox already publishes
// rather than appended, so moving a host port moves the one listener on it.
func TestMergePublicationsMovesAHostPortRatherThanDoublingIt(t *testing.T) {
	have := publications(t, "8080:80", "3000")
	asked := publications(t, "8080:90")
	got := mergePublications(have, asked)
	if len(got) != 2 {
		t.Fatalf("merge = %+v, want two ports", got)
	}
	if got[0].HostPort != 8080 || got[0].GuestPort != 90 {
		t.Errorf("host port 8080 was not moved: %+v", got[0])
	}
	if got[1].HostPort != 3000 {
		t.Errorf("the other port moved: %+v", got[1])
	}
	// A port the sandbox does not have yet is added, in the order it was
	// asked for.
	got = mergePublications(have, publications(t, "9090:90"))
	if len(got) != 3 || got[2].HostPort != 9090 {
		t.Errorf("a new port was not appended: %+v", got)
	}
	// 0.0.0.0 covers loopback, so it takes over the loopback listener on the
	// same port.
	got = mergePublications(have, publications(t, "0.0.0.0:8080:80"))
	if len(got) != 2 || got[0].Addr() != "0.0.0.0" || got[0].HostPort != 8080 {
		t.Errorf("0.0.0.0:8080 did not replace the loopback 8080: %+v", got)
	}
}

// The spec a backend is allowed to refuse carries the ports, so a run that
// publishes what this backend cannot is stopped before anything boots -- on
// the path that joins a running sandbox as well as the one that boots.
func TestBackendSpecCarriesThePublishedPorts(t *testing.T) {
	c := envelopeConfig()
	c.Publish = publications(t, "3000")
	if got := c.backendSpec("hvi").Publish; len(got) != 1 || got[0].HostPort != 3000 {
		t.Fatalf("backendSpec dropped the ports: %+v", got)
	}
}
