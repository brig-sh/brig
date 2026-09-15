package runtime

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestParsePublicationReadsEverySpelling(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Publication
	}{
		{"3000", Publication{HostPort: 3000, GuestPort: 3000}},
		{"8080:80", Publication{HostPort: 8080, GuestPort: 80}},
		{"127.0.0.1:8080:80", Publication{HostAddr: "127.0.0.1", HostPort: 8080, GuestPort: 80}},
		{"0.0.0.0:443:443", Publication{HostAddr: "0.0.0.0", HostPort: 443, GuestPort: 443}},
		{"5353:53/udp", Publication{Protocol: "udp", HostPort: 5353, GuestPort: 53}},
		{"5353:53/UDP", Publication{Protocol: "udp", HostPort: 5353, GuestPort: 53}},
	} {
		got, err := ParsePublication(tc.in)
		if err != nil {
			t.Fatalf("ParsePublication(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ParsePublication(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

// A port with no host address is on loopback, which is what keeps a published
// port from being offered to the network the host is on.
func TestAPublishedPortDefaultsToLoopback(t *testing.T) {
	p, err := ParsePublication("3000")
	if err != nil {
		t.Fatalf("ParsePublication: %v", err)
	}
	if p.Addr() != "127.0.0.1" || p.Local() != "127.0.0.1:3000" {
		t.Fatalf("addr = %q, local = %q", p.Addr(), p.Local())
	}
	if p.Proto() != "tcp" {
		t.Fatalf("proto = %q", p.Proto())
	}
	wide, err := ParsePublication("0.0.0.0:3000:3000")
	if err != nil {
		t.Fatalf("ParsePublication: %v", err)
	}
	// The envelope says so, because this one is the wider hole.
	if !strings.Contains(wide.Line(), "reachable from the network") {
		t.Fatalf("0.0.0.0 reads as %q", wide.Line())
	}
	if strings.Contains(p.Line(), "reachable from the network") {
		t.Fatalf("loopback reads as %q", p.Line())
	}
}

func TestParsePublicationRefusesWhatCannotBePublished(t *testing.T) {
	for _, in := range []string{
		"", "0", "70000", "abc", "8080:", ":80", "1:2:3:4",
		"localhost:8080:80", // a name, not an address
		"8080:80/sctp",
	} {
		if _, err := ParsePublication(in); err == nil {
			t.Fatalf("ParsePublication(%q) was accepted", in)
		}
	}
}

// Two -p flags on one line that want the same host listener are the command
// line's mistake, and the message names both rather than reporting a conflict
// with something already published.
func TestParsePublicationsRefusesOneHostPortTwice(t *testing.T) {
	_, err := ParsePublications([]string{"8080:80", "8080:90"})
	if err == nil {
		t.Fatal("two mappings on host port 8080 were accepted")
	}
	for _, want := range []string{"8080:80", "8080:90", "127.0.0.1:8080"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%v does not name %q", err, want)
		}
	}
	// A different host port for the same guest port is fine: two listeners.
	if _, err := ParsePublications([]string{"8080:80", "8081:80"}); err != nil {
		t.Fatalf("two host ports onto one guest port: %v", err)
	}
	// So is udp beside tcp on the same number.
	if _, err := ParsePublications([]string{"53:53", "53:53/udp"}); err != nil {
		t.Fatalf("tcp and udp on one number: %v", err)
	}
}

// published is Publications with the error asserted away, for a test that has
// just written the record it reads.
func published(t *testing.T, name string) []Publication {
	t.Helper()
	got, err := Publications(name)
	if err != nil {
		t.Fatalf("Publications(%s): %v", name, err)
	}
	return got
}

func TestPublicationRecordSurvivesAndIsForgotten(t *testing.T) {
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	const name = "brig-claude-web"

	if got := published(t, name); len(got) != 0 {
		t.Fatalf("a sandbox nothing published for has %d ports", len(got))
	}
	first, _ := ParsePublication("8080:80")
	if _, err := RecordPublications(name, []Publication{first}); err != nil {
		t.Fatalf("RecordPublications: %v", err)
	}
	second, _ := ParsePublication("3000")
	if _, err := RecordPublications(name, []Publication{second}); err != nil {
		t.Fatalf("RecordPublications: %v", err)
	}
	// Ordered by host port, so two reads of an unchanged record render the
	// same.
	want := []Publication{second, first}
	if got := published(t, name); !reflect.DeepEqual(got, want) {
		t.Fatalf("Publications = %+v, want %+v", got, want)
	}

	// Recording a host port that already carries something moves it rather
	// than adding a second listener on it.
	moved, _ := ParsePublication("8080:90")
	if _, err := RecordPublications(name, []Publication{moved}); err != nil {
		t.Fatalf("RecordPublications: %v", err)
	}
	if got := published(t, name); len(got) != 2 || got[1].GuestPort != 90 {
		t.Fatalf("after moving 8080: %+v", got)
	}

	left, gone, err := ForgetSomePublications(name, []Publication{second})
	if err != nil {
		t.Fatalf("ForgetSomePublications: %v", err)
	}
	if len(gone) != 1 || gone[0].HostPort != 3000 || len(left) != 1 {
		t.Fatalf("forget 3000: left %+v, gone %+v", left, gone)
	}

	ForgetPublications(name)
	if got := published(t, name); len(got) != 0 {
		t.Fatalf("after ForgetPublications: %+v", got)
	}
}

// The record is keyed by sandbox, so one sandbox's ports are not another's.
func TestPublicationRecordIsPerSandbox(t *testing.T) {
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	web, _ := ParsePublication("8080:80")
	api, _ := ParsePublication("9090:90")
	if _, err := RecordPublications("brig-a", []Publication{web}); err != nil {
		t.Fatalf("RecordPublications: %v", err)
	}
	if _, err := RecordPublications("brig-b", []Publication{api}); err != nil {
		t.Fatalf("RecordPublications: %v", err)
	}
	if got := published(t, "brig-a"); len(got) != 1 || got[0].HostPort != 8080 {
		t.Fatalf("brig-a: %+v", got)
	}
	if got := published(t, "brig-b"); len(got) != 1 || got[0].HostPort != 9090 {
		t.Fatalf("brig-b: %+v", got)
	}
	ForgetPublications("brig-a")
	if got := published(t, "brig-b"); len(got) != 1 {
		t.Fatalf("removing brig-a took brig-b's ports: %+v", got)
	}
}

// A backend with no gateway of brig's cannot publish, and says so with the
// setting that put the run there.
func TestPublishingIsRefusedOffTheGatewayBackend(t *testing.T) {
	p, _ := ParsePublication("3000")
	err := supports(RunSpec{Publish: []Publication{p}, Net: "shared"}, "vz")
	if err == nil {
		t.Fatal("publishing on vz was accepted")
	}
	if !strings.Contains(err.Error(), "hvi") {
		t.Fatalf("%v does not name the backend that can", err)
	}
	// And an offline sandbox has no network to publish from.
	err = supports(RunSpec{Publish: []Publication{p}, Net: "none"}, "hvi")
	if err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("offline with a published port: %v", err)
	}
	// Nothing published is nothing to refuse.
	if err := supports(RunSpec{Net: "shared"}, "vz"); err != nil {
		t.Fatalf("a run with no ports on vz: %v", err)
	}
}

// A record that cannot be read is an error, not an empty set.
//
// Reading it as empty is what let one sandbox's trouble reach another: the
// write persisted the guess, dropping every other sandbox's record, and their
// next boot reconciled against nothing and withdrew forwards nobody closed.
func TestAnUnreadableRecordIsAnErrorRatherThanNoPublications(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_GATEWAY_DIR", dir)

	// Nothing published yet is not a failure.
	if got, err := Publications("brig-a"); err != nil || len(got) != 0 {
		t.Fatalf("a fresh record: %+v, %v", got, err)
	}

	path, err := publishedPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Publications("brig-a"); err == nil {
		t.Fatal("a corrupt record read as no publications")
	}
	// And the write refuses rather than replacing it with a guess.
	if _, err := RecordPublications("brig-a", []Publication{{HostPort: 1, GuestPort: 1}}); err == nil {
		t.Fatal("a corrupt record was overwritten")
	}
	blob, err := os.ReadFile(path)
	if err != nil || string(blob) != "{not json" {
		t.Fatalf("the record was rewritten: %q (%v)", blob, err)
	}
}

// A port named without an address matches whatever is published on it, so the
// HOST column reads back and `brig unpublish 8080` reaches a 0.0.0.0 port.
func TestAPortNamedWithoutAnAddressMatchesAnyAddress(t *testing.T) {
	wide, _ := ParsePublication("0.0.0.0:8080:80")
	loop, _ := ParsePublication("8080:80")
	bare, _ := ParsePublication("8080")

	if !wide.Matches(bare) {
		t.Error("a bare port does not name the 0.0.0.0 publication")
	}
	if !loop.Matches(bare) {
		t.Error("a bare port does not name the loopback publication")
	}
	// An address written out matches only that address. `8080:80` writes none,
	// so it names any; `127.0.0.1:8080:80` names one.
	explicit, _ := ParsePublication("127.0.0.1:8080:80")
	if wide.Matches(explicit) {
		t.Error("127.0.0.1:8080:80 named the 0.0.0.0 publication")
	}
	if !wide.Matches(loop) {
		t.Error("8080:80 writes no address, so it should name any")
	}
	if !wide.Matches(wide) {
		t.Error("0.0.0.0:8080 does not name itself")
	}
	// The protocol still separates them.
	udp, _ := ParsePublication("8080/udp")
	if loop.Matches(udp) {
		t.Error("a udp port named the tcp publication")
	}
}
